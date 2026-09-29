package engine

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"github.com/wizier/airvault/internal/ios"
)

const (
	pairingTimeout = 45 * time.Second
	hostName       = "AirVault"
)

type PairingOutcome string

// Pairing outcomes reported by AdvancePairing.
const (
	TrustPaired                  PairingOutcome = "paired"
	TrustPending                 PairingOutcome = "trust_pending"             // Trust dialog is up; poll again
	TrustLocked                  PairingOutcome = "locked"                    // phone must be unlocked first
	TrustDenied                  PairingOutcome = "denied"                    // user tapped "Don't Trust"
	TrustWiFiAuthorizationFailed PairingOutcome = "wifi_authorization_failed" // USB trust saved, mandatory Wi-Fi setup failed
	TrustError                   PairingOutcome = "error"
)

// AdvancePairing takes one polled step and runs to the phone's answer:
// stopping between Pair and saving the record would orphan the pairing.
func (e *Engine) AdvancePairing(ctx context.Context, device DeviceID) (PairingOutcome, error) {
	outcome, err := call(context.WithoutCancel(ctx), pairingTimeout, "pairing", func(ctx context.Context) (PairingOutcome, error) {
		return e.advancePairing(ctx, string(device))
	})
	if err != nil {
		return TrustError, err
	}
	return outcome, nil
}

func (e *Engine) advancePairing(ctx context.Context, udid string) (PairingOutcome, error) {
	device, err := e.device(ctx, udid)
	if err != nil {
		return "", err
	}
	lockdown, err := ios.DialLockdown(ctx, e.mux, device)
	if err != nil {
		return "", err
	}
	defer func() { _ = lockdown.Close() }()

	record, err := e.pairs.Load(udid)
	if err != nil {
		return "", err
	}

	if record != nil {
		err := lockdown.StartSession(ctx, record)
		switch {
		case err == nil:
			if err := e.pairs.DeleteIdentity(udid); err != nil {
				return "", err
			}
			return e.finishPairing(ctx, lockdown, udid, record), nil
		case errors.Is(err, ios.ErrPasswordProtected), errors.Is(err, ios.ErrDeviceLocked):
			return TrustLocked, nil
		case !errors.Is(err, ios.ErrInvalidHostID):
			return "", err
		}
		// The phone forgot this host: pair again on a fresh connection.
		fresh, err := ios.DialLockdown(ctx, e.mux, device)
		if err != nil {
			return "", err
		}
		_ = lockdown.Close()
		lockdown = fresh
	}

	identity, err := e.pairingIdentity(ctx, udid)
	if err != nil {
		return "", err
	}
	record, err = lockdown.Pair(ctx, identity.HostID, identity.SystemBUID, hostName)
	switch {
	case errors.Is(err, ios.ErrPairingDialogResponsePending):
		return TrustPending, nil
	case errors.Is(err, ios.ErrUserDeniedPairing):
		return TrustDenied, nil
	case errors.Is(err, ios.ErrPasswordProtected):
		return TrustLocked, nil
	case err != nil:
		return "", err
	}
	if err := e.savePairing(udid, record); err != nil {
		return "", err
	}
	if err := e.pairs.DeleteIdentity(udid); err != nil {
		return "", err
	}
	if err := lockdown.StartSession(ctx, record); err != nil {
		slog.WarnContext(ctx, "paired, but the session for Wi-Fi setup failed", "udid", udid, "error", err)
		return TrustWiFiAuthorizationFailed, nil
	}
	return e.finishPairing(ctx, lockdown, udid, record), nil
}

func (e *Engine) pairingIdentity(ctx context.Context, udid string) (pairIdentity, error) {
	if identity, err := e.pairs.Identity(udid); identity != nil && err == nil {
		return *identity, nil
	}
	buid, err := e.mux.BUID(ctx)
	if err != nil {
		return pairIdentity{}, err
	}
	return e.pairs.ReserveIdentity(udid, pairIdentity{HostID: strings.ToUpper(uuid.NewV4().String()), SystemBUID: buid})
}

// finishPairing sets up Wi-Fi: the muxer needs a record to find the phone.
// A failure keeps AirVault's record, so a retry needs no Trust prompt.
func (e *Engine) finishPairing(ctx context.Context, lockdown *ios.Lockdown, udid string, record *ios.PairRecord) PairingOutcome {
	err := func() error {
		if _, err := e.mux.PairRecord(ctx, udid); err != nil {
			data, err := record.Marshal()
			if err == nil {
				err = e.mux.SavePairRecord(ctx, udid, data)
			}
			if err != nil {
				return err
			}
		}
		return lockdown.SetValue(ctx, "com.apple.mobile.wireless_lockdown", "EnableWifiConnections", true)
	}()
	if err != nil {
		slog.WarnContext(ctx, "paired, but Wi-Fi setup failed", "udid", udid, "error", err)
		return TrustWiFiAuthorizationFailed
	}
	return TrustPaired
}

func (e *Engine) savePairing(udid string, record *ios.PairRecord) error {
	e.afc.forget(udid)
	return e.pairs.Save(udid, record)
}

// Unpair removes AirVault's state whatever the phone answered, so no device
// becomes unremovable. The muxer's record may be Finder's and stays.
func (e *Engine) Unpair(ctx context.Context, device DeviceID) error {
	udid := string(device)
	if err := validateUDID(udid); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	switch record, err := e.pairs.Load(udid); {
	case err != nil:
		slog.WarnContext(ctx, "unpair: pairing record unreadable, cannot revoke on the device", "udid", udid, "error", err)
	case record == nil:
		slog.WarnContext(ctx, "unpair: no AirVault pairing record", "udid", udid)
	default:
		if err := e.revoke(ctx, udid, record); err != nil {
			slog.WarnContext(ctx, "unpair: the device did not acknowledge; removing host state anyway", "udid", udid, "error", err)
		} else {
			slog.InfoContext(ctx, "unpair: the device forgot this host", "udid", udid)
		}
	}
	return e.ForgetPairing(device)
}

// ForgetPairing drops this host's pairing with the device without telling it,
// for a device that has already forgotten this host, as an erased one has.
func (e *Engine) ForgetPairing(device DeviceID) error {
	udid := string(device)
	e.afc.forget(udid)
	if err := errors.Join(e.pairs.Delete(udid), e.pairs.DeleteIdentity(udid)); err != nil {
		return &Error{Kind: ErrorInternal, Detail: "remove pairing state: " + err.Error()}
	}
	return nil
}

func (e *Engine) revoke(ctx context.Context, udid string, record *ios.PairRecord) error {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	device, err := e.device(ctx, udid)
	if err != nil {
		return err
	}
	lockdown, err := ios.DialLockdown(ctx, e.mux, device)
	if err != nil {
		return err
	}
	defer lockdown.Close()
	// Over Wi-Fi the phone takes Unpair only inside a session.
	_ = lockdown.StartSession(ctx, record)
	if err := lockdown.Unpair(ctx, record.HostID); err != nil && !errors.Is(err, ios.ErrInvalidHostID) {
		return err
	}
	return nil
}
