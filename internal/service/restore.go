package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/model"
)

func (s *Service) reserveRestore(app context.Context, udid string, opts RestoreOptions) (*runReservation, *restorePlan, error) {
	// The snapshot's source is the only thing admission needs; everything else —
	// the device row, the large object manifest, the keybag — is read once, under
	// the leases, so no part of the plan can go stale between the two.
	snapshot, err := s.lookupSnapshot(app, opts.SnapshotID)
	if err != nil {
		return nil, nil, err
	}
	run, err := s.reserveRun(app, runKindRestore, udid,
		deviceWriteResource(udid), snapshotReadResource(snapshot.SourceUDID))
	if err != nil {
		return nil, nil, err
	}
	plan, err := s.buildRestorePlan(app, udid, opts)
	if err != nil {
		s.discardRun(run)
		return nil, nil, err
	}
	if err := s.announceRun(run, StageRestoring); err != nil {
		s.discardRun(run)
		return nil, nil, err
	}
	s.bus.Emit(events.BackupStarted, map[string]any{
		"runId": run.id, "udid": udid, "restore": true, "snapshotId": plan.snapshot.ID})
	return run, plan, nil
}

func (s *Service) StartRestore(udid string, opts RestoreOptions) (string, error) {
	run, plan, err := s.reserveRestore(s.app, udid, opts)
	if err != nil {
		return "", err
	}
	return s.launchRun(run, func() (runOutcome, error) {
		return s.executeRestore(run, plan)
	}), nil
}

func (s *Service) executeRestore(run *runReservation, plan *restorePlan) (runOutcome, error) {
	ctx, app, udid := run.ctx, run.app, run.udid
	dev, opts := plan.device, plan.opts
	slog.DebugContext(ctx, "restore: starting", "device", dev.Name, "udid", udid,
		"source", plan.snapshot.SourceUDID, "snapshot_id", opts.SnapshotID,
		"systemFiles", opts.SystemFiles, "reboot", opts.Reboot,
		"settingsFromBackup", opts.SettingsFromBackup, "removeItemsNotRestored", opts.RemoveItemsNotRestored)

	// A phone on Setup Assistant's "Restore from Mac or PC" screen is paired but
	// unactivated, and mb2's AFC sync lock errors out until it is activated.
	// Finder activates first; so do we.
	if code, actErr := s.activateIfNeeded(run, dev.Name); actErr != nil {
		if ctx.Err() != nil {
			return runOutcome{}, domain.ErrCancelled
		}
		return runOutcome{errorCode: code}, actErr
	}

	restoreErr := s.engine.RestoreSnapshot(ctx, engine.RestoreSnapshotRequest{
		OperationID: engine.OperationID(run.id),
		TargetID:    engine.DeviceID(udid),
		Snapshot: engine.SnapshotRef{
			SourceID:   engine.DeviceID(plan.snapshot.SourceUDID),
			SnapshotID: engine.SnapshotID(plan.snapshot.ID),
		},
		Password: opts.Password, SystemFiles: opts.SystemFiles, Reboot: opts.Reboot,
		SettingsFromBackup: opts.SettingsFromBackup, RemoveItemsNotRestored: opts.RemoveItemsNotRestored,
	}, s.progressSink(run, StageRestoring))
	// A restore the engine reports as done is already applied and irreversible,
	// so a late cancel can't turn it into a cancellation. beginCommit still
	// latches the commit phase so CancelRun stops offering a dead cancel.
	if restoreErr == nil {
		_ = s.beginCommit(run)
	}
	fctx := context.WithoutCancel(ctx)
	if restoreErr != nil && ctx.Err() != nil && app.Err() != nil {
		return runOutcome{}, restoreErr
	}
	if restoreErr != nil && ctx.Err() != nil {
		slog.DebugContext(fctx, "restore: cancelled", "device", dev.Name, "udid", udid)
		return runOutcome{}, domain.ErrCancelled
	}
	if restoreErr != nil {
		errorCode := engineErrorCode(restoreErr)
		slog.DebugContext(fctx, "restore: engine failed", "device", dev.Name, "error", restoreErr)
		return runOutcome{errorCode: errorCode}, fmt.Errorf("restore failed: %w", restoreErr)
	}
	slog.DebugContext(fctx, "restore: done", "device", dev.Name)
	return runOutcome{}, nil
}

// RestoreOptions selects one immutable rollback point. Bound directly from the
// POST /api/devices/:udid/restore request body.
type RestoreOptions struct {
	SnapshotID             string `json:"snapshotId"`
	Password               string `json:"password"`
	SystemFiles            bool   `json:"systemFiles"`
	Reboot                 bool   `json:"reboot"`
	SettingsFromBackup     bool   `json:"settingsFromBackup"`
	RemoveItemsNotRestored bool   `json:"removeItemsNotRestored"`
}

type restorePlan struct {
	device   *model.Device
	snapshot *model.Backup
	opts     RestoreOptions
}

func (s *Service) lookupSnapshot(ctx context.Context, snapshotID string) (*model.Backup, error) {
	if snapshotID == "" {
		return nil, &domain.ValidationError{Code: "snapshot_required", Message: "a backup snapshot must be selected"}
	}
	snapshot, err := s.store.Backup.Get(ctx, snapshotID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, &domain.ValidationError{Code: "snapshot_not_found", Message: "the selected backup snapshot no longer exists"}
		}
		return nil, err
	}
	return snapshot, nil
}

// buildRestorePlan resolves and validates the whole plan in one pass, called
// under the run's leases so nothing it reads can change underneath.
func (s *Service) buildRestorePlan(ctx context.Context, targetUDID string, opts RestoreOptions) (*restorePlan, error) {
	device, err := s.pairedDevice(ctx, targetUDID)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.lookupSnapshot(ctx, opts.SnapshotID)
	if err != nil {
		return nil, err
	}
	view, err := s.objects.OpenSnapshot(snapshot.SourceUDID, snapshot.ID)
	if err != nil {
		// A delete that won the gap before the read lease was held leaves the
		// catalog row without its manifest: a rejected request, not a fault.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &domain.ValidationError{Code: "snapshot_not_found", Message: "the selected backup snapshot no longer exists"}
		}
		return nil, fmt.Errorf("open selected object snapshot: %w", err)
	}
	info, err := inspectBackupView(view)
	if err != nil {
		return nil, &domain.ValidationError{Code: "backup_not_restorable", Message: "the selected backup is not confirmed complete and can't be restored"}
	}
	encrypted, snapshotIOS := info.encrypted, info.iosVersion
	if encrypted && opts.Password == "" {
		return nil, &domain.ValidationError{Code: "backup_password_required", Message: "this backup is encrypted — its password is required"}
	}
	if encrypted {
		valid, verifyErr := verifySnapshotPassword(view, opts.Password)
		switch {
		case verifyErr != nil:
			slog.WarnContext(ctx, "backup password preflight unavailable; the device enforces",
				"snapshot_id", snapshot.ID, "error", verifyErr)
		case !valid:
			return nil, &domain.ValidationError{Code: "invalid_backup_password",
				Message: "this password does not unlock the selected backup"}
		}
	}
	// iOS refuses to apply a backup made on a newer iOS; fail before the run
	// starts instead of minutes into it.
	if newerIOSVersion(snapshotIOS, device.IOSVersion) {
		return nil, &domain.ValidationError{Code: "backup_ios_too_new",
			Message: fmt.Sprintf("this backup was made on iOS %s, newer than the phone's iOS %s — update the phone first", snapshotIOS, device.IOSVersion)}
	}
	return &restorePlan{device: device, snapshot: snapshot, opts: opts}, nil
}

// newerIOSVersion reports a strictly newer than b; unknown versions never block.
func newerIOSVersion(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := 0, 0
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			return av > bv
		}
	}
	return false
}
