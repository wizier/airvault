package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
)

// Engine records whose JSON shape is already the API's are served as they are.
type (
	App             = engine.App
	InstallProgress = engine.InstallProgress
)

func (s *Service) Apps(ctx context.Context, udid string) ([]App, error) {
	if err := s.reachableDevice(ctx, udid); err != nil {
		return nil, err
	}
	apps, err := s.engine.ListApps(ctx, engine.DeviceID(udid))
	if err != nil {
		slog.WarnContext(ctx, "apps: engine", "udid", udid, "error", err)
		return nil, newEngineActionError("app_list_failed", err)
	}
	return apps, nil
}

const maxAppIconBatch = 100

func (s *Service) AppIcons(ctx context.Context, udid string, bundleIDs []string) (map[string][]byte, error) {
	if len(bundleIDs) == 0 || slices.Contains(bundleIDs, "") {
		return nil, &domain.ValidationError{Code: "bundle_id_required", Message: "non-empty bundle ids are required"}
	}
	if len(bundleIDs) > maxAppIconBatch {
		return nil, &domain.ValidationError{Code: "too_many_bundle_ids", Message: "too many bundle ids in one batch"}
	}
	if err := s.reachableDevice(ctx, udid); err != nil {
		return nil, err
	}
	icons, err := s.engine.AppIcons(ctx, engine.DeviceID(udid), bundleIDs)
	if err != nil {
		// The web keeps its placeholders, so the failure is only logged.
		slog.DebugContext(ctx, "app icons: engine", "udid", udid, "count", len(bundleIDs), "error", err)
		return nil, newEngineActionError("app_icons_failed", err)
	}
	return icons, nil
}

func (s *Service) InstallApp(ctx context.Context, udid string, ipa io.Reader, onProgress func(InstallProgress)) error {
	if err := s.reachableDevice(ctx, udid); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.uploads, "airvault-*.ipa")
	if err != nil {
		return fmt.Errorf("create temporary ipa: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, ipa); err != nil {
		return fmt.Errorf("store uploaded ipa: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close uploaded ipa: %w", err)
	}
	ipaPath := tmp.Name()
	err = s.runCommand(ctx, runKindInstall, udid, func(ctx context.Context) error {
		last := InstallProgress{Percent: -1}
		reportChange := func(progress InstallProgress) {
			if progress == last {
				return
			}
			last = progress
			onProgress(progress)
		}
		if err := s.engine.InstallApp(ctx, engine.DeviceID(udid), ipaPath, reportChange); err != nil {
			slog.DebugContext(ctx, "app install: engine", "udid", udid, "error", err)
			return newEngineActionError("app_install_failed", err)
		}
		return nil
	}, deviceWriteResource(udid))
	if err == nil {
		s.bus.Emit(appCatalogChanged(udid))
	}
	return err
}

func (s *Service) UninstallApp(ctx context.Context, udid, bundleID string) error {
	if bundleID == "" {
		return &domain.ValidationError{Code: "bundle_id_required", Message: "bundle id is required"}
	}
	if err := s.reachableDevice(ctx, udid); err != nil {
		return err
	}
	err := s.runCommand(ctx, runKindUninstall, udid, func(ctx context.Context) error {
		if err := s.engine.UninstallApp(ctx, engine.DeviceID(udid), bundleID); err != nil {
			slog.DebugContext(ctx, "app uninstall: engine", "udid", udid, "bundle", bundleID, "error", err)
			return newEngineActionError("app_uninstall_failed", err)
		}
		slog.DebugContext(ctx, "app uninstall: done", "udid", udid, "bundle", bundleID)
		return nil
	}, deviceWriteResource(udid))
	if err == nil {
		s.bus.Emit(appCatalogChanged(udid))
	}
	return err
}
