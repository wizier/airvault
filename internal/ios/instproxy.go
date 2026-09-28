package ios

import (
	"context"
	"fmt"
	"net"

	"howett.net/plist"
)

const InstallationProxyService = "com.apple.mobile.installation_proxy"

type InstallationProxy struct {
	conn *PlistConn
}

func NewInstallationProxy(conn net.Conn) *InstallationProxy {
	return &InstallationProxy{conn: NewPlistConn(conn, plist.XMLFormat)}
}

func (p *InstallationProxy) Close() error { return p.conn.Close() }

// LookupApps returns the apps' Info.plist attributes by bundle id.
func (p *InstallationProxy) LookupApps(ctx context.Context, applicationType string) (map[string]map[string]any, error) {
	var reply struct {
		LookupResult map[string]any `plist:"LookupResult"`
	}
	request := map[string]any{
		"Command":       "Lookup",
		"ClientOptions": map[string]any{"ApplicationType": applicationType},
	}
	if err := p.conn.Exchange(ctx, request, &reply); err != nil {
		return nil, fmt.Errorf("lookup apps: %w", err)
	}
	if reply.LookupResult == nil {
		return nil, fmt.Errorf("lookup apps: %w: no LookupResult", ErrProtocol)
	}
	apps := make(map[string]map[string]any, len(reply.LookupResult))
	for bundleID, info := range reply.LookupResult {
		if attributes, ok := info.(map[string]any); ok {
			apps[bundleID] = attributes
		}
	}
	return apps, nil
}

// Browse lists the installed apps, each with the attributes options ask for.
func (p *InstallationProxy) Browse(ctx context.Context, options map[string]any) ([]map[string]any, error) {
	if err := p.conn.SendContext(ctx, map[string]any{"Command": "Browse", "ClientOptions": options}); err != nil {
		return nil, fmt.Errorf("browse apps: %w", err)
	}
	var apps []map[string]any
	for {
		var reply struct {
			Status      string `plist:"Status"`
			CurrentList []any  `plist:"CurrentList"`
		}
		if err := p.conn.RecvContext(ctx, &reply); err != nil {
			return nil, fmt.Errorf("browse apps: %w", err)
		}
		for _, item := range reply.CurrentList {
			if app, ok := item.(map[string]any); ok {
				apps = append(apps, app)
			}
		}
		if reply.Status == "Complete" {
			return apps, nil
		}
	}
}

// Install installs the package staged at packagePath, relative to the AFC root.
func (p *InstallationProxy) Install(ctx context.Context, packagePath string, progress func(percent int)) error {
	return p.run(ctx, map[string]any{"Command": "Install", "PackagePath": packagePath, "ClientOptions": map[string]any{}}, progress)
}

func (p *InstallationProxy) Uninstall(ctx context.Context, bundleID string, progress func(percent int)) error {
	return p.run(ctx, map[string]any{"Command": "Uninstall", "ApplicationIdentifier": bundleID, "ClientOptions": map[string]any{}}, progress)
}

func (p *InstallationProxy) run(ctx context.Context, request map[string]any, progress func(int)) error {
	command := request["Command"]
	if err := p.conn.SendContext(ctx, request); err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	for {
		var status struct {
			Status           string  `plist:"Status"`
			PercentComplete  *uint64 `plist:"PercentComplete"`
			ErrorDescription string  `plist:"ErrorDescription"`
		}
		if err := p.conn.RecvContext(ctx, &status); err != nil {
			return fmt.Errorf("%s: %w", command, err)
		}
		if status.ErrorDescription != "" {
			return fmt.Errorf("%s: %w", command, &DeviceError{Code: "OperationFailed", Description: status.ErrorDescription})
		}
		if status.PercentComplete != nil && progress != nil {
			progress(int(*status.PercentComplete))
		}
		if status.Status == "Complete" {
			return nil
		}
	}
}
