package engine

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/backup2"
	airlog "github.com/wizier/airvault/internal/logging"
	"github.com/wizier/airvault/internal/objectstore"
)

const (
	assertionBackstop = 20 * time.Minute // the device drops a forgotten assertion by then
	assertionRenew    = 10 * time.Minute
	assertionRetry    = time.Minute
	syncCancelRequest = "com.apple.itunes-client.syncCancelRequest"
)

// BuildSnapshot backs the device up into a new staged snapshot and returns
// the bytes it added to the object pool; publishing it is the caller's.
func (e *Engine) BuildSnapshot(ctx context.Context, req BuildSnapshotRequest, onProgress func(Progress)) (int64, error) {
	udid := string(req.DeviceID)
	if err := validateUDID(udid); err != nil {
		return 0, err
	}
	session, err := e.objects.BeginSnapshot(udid, string(req.SnapshotID), string(req.BaseSnapshotID))
	if err != nil {
		return 0, storeFailure("backup", err)
	}
	progress := newTransferProgress(onProgress)
	defer progress.close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t := &transfer{engine: e, udid: udid, source: udid, session: session, progress: progress}
	if err := t.run(ctx, cancel); err != nil {
		return 0, transferResult(ctx, "backup", err)
	}
	progress.submit(ProgressPhaseSealing, -1, 0)
	added, err := session.Seal(ctx)
	if err == nil {
		err = ctx.Err() // a cancel racing the seal discards the backup
	}
	if err != nil {
		return 0, transferResult(ctx, "backup", storeFailure("seal backup", err))
	}
	return added, nil
}

// RestoreSnapshot restores the target from a published snapshot, possibly
// another device's. Once the device has accepted it, a late cancel is moot.
func (e *Engine) RestoreSnapshot(ctx context.Context, req RestoreSnapshotRequest, onProgress func(Progress)) error {
	udid := string(req.TargetID)
	if err := validateUDID(udid); err != nil {
		return err
	}
	source := string(req.Snapshot.SourceID)
	session, err := e.objects.OpenRestore(source, string(req.Snapshot.SnapshotID))
	if err != nil {
		return storeFailure("restore", err)
	}
	if err := e.checkFindMy(ctx, udid); err != nil {
		return err
	}
	progress := newTransferProgress(onProgress)
	defer progress.close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t := &transfer{engine: e, udid: udid, source: source, session: session, progress: progress,
		restore: &backup2.RestoreOptions{
			Reboot:                 req.Reboot,
			PreserveSettings:       !req.SettingsFromBackup,
			SystemFiles:            req.SystemFiles,
			RemoveItemsNotRestored: req.RemoveItemsNotRestored,
			Password:               req.Password,
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

// transfer is one mobilebackup2 backup or restore of udid, served from
// source's snapshots.
type transfer struct {
	engine   *Engine
	udid     string
	source   string
	session  *objectstore.Session
	progress *transferProgress
	restore  *backup2.RestoreOptions // nil for a backup
}

func (t *transfer) label() string {
	if t.restore == nil {
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
	if t.restore == nil {
		return false, t.engine.writeBackupInfo(ctx, t.udid, t.session)
	}
	return t.engine.stageRestoreApplications(ctx, t.udid, t.session)
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
	storage := &backupStorage{store: t.engine.objects, session: t.session, source: t.source}
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
	return verdictFailure(backup2.Verdict(outcome), t.restore == nil)
}

func (t *transfer) serve(ctx context.Context, conn *backup2.Conn, storage *backupStorage) (*backup2.Dict, error) {
	var err error
	if t.restore == nil {
		err = conn.Backup(ctx, t.udid, t.source)
	} else {
		err = conn.Restore(ctx, t.udid, t.source, *t.restore)
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
		airlog.Component("engine").WarnContext(ctx, t.label()+": "+stage+" failed", "udid", t.udid, "error", err)
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
		airlog.Component("engine").WarnContext(ctx, label+": proceeding without the phone's cancel request", "udid", udid, "error", err)
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
					airlog.Component("engine").InfoContext(ctx, label+": cancelled on the phone", "udid", udid)
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
			airlog.Component("engine").WarnContext(ctx, "no power assertion; an off-charger phone may sleep mid-transfer", "udid", udid, "error", err)
		}
		if !sleep(ctx, wait) {
			return
		}
	}
}
