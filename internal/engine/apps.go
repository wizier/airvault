package engine

import (
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/afc"
	airlog "github.com/wizier/airvault/internal/logging"
)

func (e *Engine) ListApps(ctx context.Context, device DeviceID) ([]App, error) {
	return call(ctx, deviceWorkTimeout, "app list", func(ctx context.Context) ([]App, error) {
		var lookup map[string]map[string]any
		err := e.installationProxy(ctx, device, func(proxy *ios.InstallationProxy) (err error) {
			lookup, err = proxy.LookupApps(ctx, "User")
			return err
		})
		if err != nil {
			return nil, err
		}
		apps := make([]App, 0, len(lookup))
		for bundleID, info := range lookup {
			fileSharing, _ := info["UIFileSharingEnabled"].(bool)
			apps = append(apps, App{
				BundleID:    bundleID,
				Name:        cmp.Or(text(info, "CFBundleDisplayName"), text(info, "CFBundleName"), bundleID),
				Version:     cmp.Or(text(info, "CFBundleShortVersionString"), text(info, "CFBundleVersion")),
				FileSharing: fileSharing,
			})
		}
		slices.SortFunc(apps, func(a, b App) int {
			return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.BundleID, b.BundleID))
		})
		return apps, nil
	})
}

func text(info map[string]any, key string) string {
	value, _ := info[key].(string)
	return value
}

// AppIcons skips icons the phone cannot give; when the connection fails or
// time runs out, the icons read so far are the answer.
func (e *Engine) AppIcons(ctx context.Context, device DeviceID, bundleIDs []string) (map[string][]byte, error) {
	return call(ctx, deviceWorkTimeout, "app icons", func(ctx context.Context) (map[string][]byte, error) {
		conn, err := e.openService(ctx, string(device), ios.SpringBoardService)
		if err != nil {
			return nil, err
		}
		springboard := ios.NewSpringBoard(conn)
		defer springboard.Close()
		icons := make(map[string][]byte, len(bundleIDs))
		for _, bundleID := range bundleIDs {
			png, err := springboard.IconPNG(ctx, bundleID)
			switch {
			case err == nil && len(png) > 0:
				icons[bundleID] = png
			case err != nil && !answered(err):
				return icons, nil
			}
		}
		return icons, nil
	})
}

// answered reports an error the device replied with, leaving the connection usable.
func answered(err error) bool {
	return errors.Is(err, ios.ErrProtocol) || errors.As(err, new(*ios.DeviceError))
}

const (
	stagedIPA         = "PublicStaging/airvault-install.ipa"
	appInstallTimeout = 5 * time.Minute // installd unpacking and verifying the package
)

// InstallApp's upload can be cancelled and fails only when a write stalls;
// the installation, once asked for, runs to the device's answer.
func (e *Engine) InstallApp(ctx context.Context, device DeviceID, ipaPath string, onProgress func(InstallProgress)) error {
	progress := newLatestDispatcher("install progress", onProgress)
	defer progress.close()
	report := func(phase InstallPhase, percent int) {
		progress.submit(InstallProgress{Phase: phase, Percent: percent})
	}
	cleanup := context.WithoutCancel(ctx)
	report(InstallPhaseStaging, 0)
	staged, err := e.stageIPA(ctx, device, ipaPath, report)
	if staged {
		defer e.removeStagedIPA(cleanup, device)
	}
	if err != nil {
		return failure(ctx, "stage app", err)
	}
	report(InstallPhaseStaging, 100)
	return do(cleanup, appInstallTimeout, "app install", func(ctx context.Context) error {
		report(InstallPhaseInstalling, 0)
		err := e.installationProxy(ctx, device, func(proxy *ios.InstallationProxy) error {
			return proxy.Install(ctx, stagedIPA, func(percent int) { report(InstallPhaseInstalling, percent) })
		})
		if err == nil {
			report(InstallPhaseInstalling, 100)
		}
		return err
	})
}

// stageIPA bounds each step on its own, so a slow link is fine while it moves.
func (e *Engine) stageIPA(ctx context.Context, device DeviceID, ipaPath string, report func(InstallPhase, int)) (staged bool, err error) {
	ipa, err := os.Open(ipaPath)
	if err != nil {
		return false, err
	}
	defer ipa.Close()
	info, err := ipa.Stat()
	if err != nil {
		return false, err
	}
	client, err := call(ctx, deviceWorkTimeout, "connect device files", func(ctx context.Context) (*afc.Client, error) {
		return e.dialAFC(ctx, afcKey{udid: string(device), source: AFCMedia})
	})
	if err != nil {
		return false, err
	}
	defer client.Close()
	step := func(op func(context.Context) error) error {
		ctx, cancel := context.WithTimeout(ctx, deviceWorkTimeout)
		defer cancel()
		return failure(ctx, "stage app", op(ctx))
	}
	_ = step(func(ctx context.Context) error { return client.MakeDir(ctx, "PublicStaging") }) // usually there
	var file *afc.File
	if err := step(func(ctx context.Context) (err error) {
		file, err = client.Open(ctx, stagedIPA, afc.WriteOnly)
		return err
	}); err != nil {
		return false, err
	}
	total := max(info.Size(), 1)
	buffer := make([]byte, afc.MaxTransfer)
	for copied := int64(0); ; {
		n, readErr := io.ReadFull(ipa, buffer)
		if n > 0 {
			if err := step(func(ctx context.Context) error { return file.Write(ctx, buffer[:n]) }); err != nil {
				return true, err
			}
			copied += int64(n)
			report(InstallPhaseStaging, int(min(copied, total)*100/total))
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return true, readErr
		}
	}
	return true, step(file.Close)
}

func (e *Engine) removeStagedIPA(ctx context.Context, device DeviceID) {
	err := do(ctx, deviceWorkTimeout, "remove staged app", func(ctx context.Context) error {
		client, err := e.dialAFC(ctx, afcKey{udid: string(device), source: AFCMedia})
		if err != nil {
			return err
		}
		defer client.Close()
		if err := client.Remove(ctx, stagedIPA); !errors.Is(err, afc.ErrObjectNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		airlog.Component("engine").WarnContext(ctx, "staged app cleanup failed", "udid", device, "error", err)
	}
}

func (e *Engine) UninstallApp(ctx context.Context, device DeviceID, bundleID string) error {
	if bundleID == "" {
		return &Error{Kind: ErrorInvalidArgument, Detail: "empty bundle id"}
	}
	return do(context.WithoutCancel(ctx), deviceWorkTimeout, "app uninstall", func(ctx context.Context) error {
		return e.installationProxy(ctx, device, func(proxy *ios.InstallationProxy) error {
			return proxy.Uninstall(ctx, bundleID, nil)
		})
	})
}

func (e *Engine) installationProxy(ctx context.Context, device DeviceID, use func(*ios.InstallationProxy) error) error {
	conn, err := e.openService(ctx, string(device), ios.InstallationProxyService)
	if err != nil {
		return err
	}
	proxy := ios.NewInstallationProxy(conn)
	defer proxy.Close()
	return use(proxy)
}
