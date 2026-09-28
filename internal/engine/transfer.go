package engine

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/backup2"
	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/objectstore"
)

const (
	assertionBackstop = 20 * time.Minute // the device drops a forgotten assertion by then
	assertionRenew    = 10 * time.Minute
	assertionRetry    = time.Minute
	syncCancelRequest = "com.apple.itunes-client.syncCancelRequest"
)

// BuildSnapshot backs the device up into session and seals it; the snapshot
// comes back ready to publish, with the bytes it added to the object pool.
func (e *Engine) BuildSnapshot(ctx context.Context, device DeviceID, session *objectstore.Session, onProgress func(Progress)) (*objectstore.StagingView, int64, error) {
	udid := string(device)
	if err := validateUDID(udid); err != nil {
		return nil, 0, err
	}
	progress := newTransferProgress(onProgress)
	defer progress.close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t := &transfer{engine: e, udid: udid, session: session, progress: progress}
	if err := t.run(ctx, cancel); err != nil {
		return nil, 0, transferResult(ctx, "backup", err)
	}
	progress.submit(ProgressPhaseSealing, -1, 0)
	staged, added, err := session.Seal(ctx)
	if err == nil {
		err = ctx.Err() // a cancel racing the seal discards the backup
	}
	if err != nil {
		return nil, 0, transferResult(ctx, "backup", storeFailure("seal backup", err))
	}
	return staged, added, nil
}

type RestoreOptions struct {
	Password    string
	SystemFiles bool
	Reboot      bool
	// Wire inverse of RestorePreserveSettings; true mirrors a Finder restore.
	SettingsFromBackup     bool
	RemoveItemsNotRestored bool
}

// RestoreSnapshot restores the device from a backup, possibly another
// device's. Once the device has accepted it, a late cancel is moot.
func (e *Engine) RestoreSnapshot(ctx context.Context, device DeviceID, from *iosbackup.Backup, options RestoreOptions, onProgress func(Progress)) error {
	udid := string(device)
	if err := validateUDID(udid); err != nil {
		return err
	}
	if err := e.checkFindMy(ctx, udid); err != nil {
		return err
	}
	progress := newTransferProgress(onProgress)
	defer progress.close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t := &transfer{engine: e, udid: udid, session: from.Session(), progress: progress, from: from,
		options: backup2.RestoreOptions{
			Reboot:                 options.Reboot,
			PreserveSettings:       !options.SettingsFromBackup,
			SystemFiles:            options.SystemFiles,
			RemoveItemsNotRestored: options.RemoveItemsNotRestored,
			Password:               options.Password,
		}}
	return transferResult(ctx, "restore", t.run(ctx, cancel))
}

// transferResult classifies what ended a transfer; once it was cancelled,
// by the caller or on the phone, any failure is the cancellation.
func transferResult(ctx context.Context, operation string, err error) error {
	if err == nil {
		return nil
	}
	err = failure(ctx, operation, err)
	var classified *Error
	if ctx.Err() != nil && errors.As(err, &classified) && classified.Kind != ErrorCancelled {
		return &Error{Kind: ErrorCancelled, Detail: classified.Detail}
	}
	return err
}

// transfer is one mobilebackup2 backup or restore of udid; the session's
// source names the snapshots to the device.
type transfer struct {
	engine   *Engine
	udid     string
	session  *objectstore.Session
	progress *transferProgress
	from     *iosbackup.Backup // what a restore applies; nil for a backup
	options  backup2.RestoreOptions
}

func (t *transfer) label() string {
	if t.from == nil {
		return "backup"
	}
	return "restore"
}

// run holds a sync session around the conversation; the phone can end it
// early through cancel.
func (t *transfer) run(ctx context.Context, cancel context.CancelFunc) error {
	e := t.engine
	e.afc.forget(t.udid) // idle for the whole transfer, and a restore reboots the phone
	lock, err := e.startSync(ctx, t.udid)
	if err != nil {
		return err
	}
	staged, err := t.prepare(ctx)
	if err == nil {
		err = t.converse(ctx, cancel)
	}
	if staged && err != nil {
		e.removeRestoreApplications(context.WithoutCancel(ctx), t.udid)
	}
	t.cleanup(ctx, "sync session teardown", lock.finish(context.WithoutCancel(ctx)))
	return err
}

// prepare refreshes Info.plist for a backup; a restore stages the apps to
// reinstall, and fails without them rather than restore none.
func (t *transfer) prepare(ctx context.Context) (staged bool, err error) {
	if t.from == nil {
		return false, t.engine.writeBackupInfo(ctx, t.udid, t.session)
	}
	return t.engine.stageRestoreApplications(ctx, t.udid, t.from)
}

// converse runs the mobilebackup2 conversation. A store failure outranks
// what the device then reported: it is the root cause.
func (t *transfer) converse(ctx context.Context, cancel context.CancelFunc) error {
	if err := t.session.Err(); err != nil {
		return storeFailure(t.label(), err)
	}
	conn, err := t.engine.openBackup2(ctx, t.udid)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := t.engine.guardTransfer(ctx, t.udid, t.label(), cancel)
	storage := &backupStorage{session: t.session}
	outcome, err := t.serve(ctx, conn, storage)
	stop()
	switch {
	case t.session.Err() != nil:
		return storeFailure(t.label(), t.session.Err())
	case storage.Violation() != nil:
		return &Error{Kind: ErrorIntegrity, Detail: storage.Violation().Error()}
	case err != nil:
		return failure(ctx, t.label()+" transfer", err)
	}
	return verdictFailure(backup2.Verdict(outcome), t.from == nil)
}

func (t *transfer) serve(ctx context.Context, conn *backup2.Conn, storage *backupStorage) (*backup2.Dict, error) {
	var err error
	if t.from == nil {
		err = conn.Backup(ctx, t.udid, t.session.Source())
	} else {
		err = conn.Restore(ctx, t.udid, t.session.Source(), t.options)
	}
	if err != nil {
		return nil, err
	}
	return conn.Serve(ctx, storage, func(p backup2.Progress) {
		t.progress.submit(ProgressPhaseTransfer, p.Percent, p.Bytes)
	})
}

// cleanup logs a teardown failure: it never replaces the transfer's result.
func (t *transfer) cleanup(ctx context.Context, stage string, err error) {
	if err != nil {
		slog.WarnContext(ctx, t.label()+": "+stage+" failed", "udid", t.udid, "error", err)
	}
}

// openBackup2 offers the escrow bag, which lets a locked phone back up; a
// device refusing it gets a plain start.
func (e *Engine) openBackup2(ctx context.Context, udid string) (*backup2.Conn, error) {
	return call(ctx, connectTimeout, "mobilebackup2 connect", func(ctx context.Context) (*backup2.Conn, error) {
		session, err := e.openSession(ctx, udid)
		if err != nil {
			return nil, err
		}
		defer session.Close()
		escrowBag := session.record.EscrowBag
		service, err := session.StartService(ctx, backup2.Service, escrowBag)
		if err != nil && escrowBag != nil && answered(err) {
			service, err = session.StartService(ctx, backup2.Service, nil)
		}
		if err != nil {
			return nil, err
		}
		conn, err := ios.DialService(ctx, e.mux, session.device, service, session.record)
		if err != nil {
			return nil, err
		}
		mb2, err := backup2.Open(ctx, conn)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return mb2, nil
	})
}

// checkFindMy fails a restore up front like Finder: the device refuses it
// while Find My is on. An unreadable flag leaves the decision to the device.
func (e *Engine) checkFindMy(ctx context.Context, udid string) error {
	on, err := call(ctx, connectTimeout, "Find My check", func(ctx context.Context) (bool, error) {
		session, err := e.openSession(ctx, udid)
		if err != nil {
			return false, err
		}
		defer session.Close()
		return ios.Value[bool](ctx, session.Lockdown, "com.apple.fmip", "IsAssociated")
	})
	if err == nil && on {
		return &Error{Kind: ErrorFindMyEnabled, Detail: "Find My iPhone is on; turn it off on the phone before restoring"}
	}
	return nil
}

// guardTransfer, until stop, cancels the transfer when the user cancels the
// sync on the phone and keeps the phone awake.
func (e *Engine) guardTransfer(ctx context.Context, udid, label string, cancel context.CancelFunc) (stop func()) {
	ctx, stopGuards := context.WithCancel(ctx)
	var wg sync.WaitGroup
	proxy, err := e.observeSyncCancel(ctx, udid)
	if err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, label+": proceeding without the phone's cancel request", "udid", udid, "error", err)
	}
	if proxy != nil {
		wg.Go(func() {
			defer proxy.Close()
			for {
				name, err := proxy.Next(ctx)
				if err != nil {
					return
				}
				if strings.Contains(name, "syncCancelRequest") {
					slog.InfoContext(ctx, label+": cancelled on the phone", "udid", udid)
					cancel()
					return
				}
			}
		})
	}
	wg.Go(func() { e.keepAwake(ctx, udid, "AirVault "+label) })
	return func() {
		stopGuards()
		wg.Wait()
	}
}

func (e *Engine) observeSyncCancel(ctx context.Context, udid string) (*ios.NotificationProxy, error) {
	return call(ctx, probeTimeout, "sync cancel observer", func(ctx context.Context) (*ios.NotificationProxy, error) {
		conn, err := e.openService(ctx, udid, ios.NotificationProxyService)
		if err != nil {
			return nil, err
		}
		proxy := ios.NewNotificationProxy(conn)
		if err := proxy.Observe(ctx, syncCancelRequest); err != nil {
			_ = proxy.Close()
			return nil, err
		}
		return proxy, nil
	})
}

// keepAwake holds a Wi-Fi sync power assertion until ctx ends: off the
// charger, iOS standby reaps service sockets about 15 minutes in.
func (e *Engine) keepAwake(ctx context.Context, udid, name string) {
	var held net.Conn
	defer func() {
		if held != nil {
			_ = held.Close()
		}
	}()
	warned := false
	for {
		conn, err := call(ctx, connectTimeout, "power assertion", func(ctx context.Context) (net.Conn, error) {
			conn, err := e.openService(ctx, udid, ios.AssertionAgentService)
			if err != nil {
				return nil, err
			}
			if err := ios.HoldWirelessSync(ctx, conn, name, assertionBackstop); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return conn, nil
		})
		wait := assertionRetry
		switch {
		case err == nil:
			if held != nil {
				_ = held.Close()
			}
			held, wait, warned = conn, assertionRenew, false
		case ctx.Err() != nil:
			return
		case !warned:
			warned = true
			slog.WarnContext(ctx, "no power assertion; an off-charger phone may sleep mid-transfer", "udid", udid, "error", err)
		}
		if !sleep(ctx, wait) {
			return
		}
	}
}
