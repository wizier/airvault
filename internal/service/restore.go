package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/library"
	"github.com/wizier/airvault/internal/model"
)

func (s *Service) reserveRestore(ctx context.Context, udid string, opts RestoreOptions) (*runReservation, *restorePlan, error) {
	// The snapshot's source is the only thing admission needs; everything else —
	// the device row, the large object manifest, the keybag — is read once, under
	// the leases, so no part of the plan can go stale between the two.
	snapshot, err := s.library.Lookup(ctx, opts.SnapshotID)
	if err != nil {
		return nil, nil, err
	}
	run, err := s.reserveRun(runKindRestore, udid,
		deviceWriteResource(udid), snapshotReadResource(snapshot.SourceUDID))
	if err != nil {
		return nil, nil, err
	}
	plan, err := s.buildRestorePlan(ctx, udid, opts)
	if err != nil {
		s.discardRun(run)
		return nil, nil, err
	}
	if err := s.announceRun(run, StageRestoring); err != nil {
		s.discardRun(run)
		return nil, nil, err
	}
	s.bus.Emit(runStarted(run, plan.backup.ID()))
	return run, plan, nil
}

// Admission runs on the request's ctx; the run itself lives as long as the app.
func (s *Service) StartRestore(ctx context.Context, udid string, opts RestoreOptions) (string, error) {
	run, plan, err := s.reserveRestore(ctx, udid, opts)
	if err != nil {
		return "", err
	}
	return s.launchRun(run, func() (runOutcome, error) {
		return s.executeRestore(run, plan)
	}), nil
}

func (s *Service) executeRestore(run *runReservation, plan *restorePlan) (runOutcome, error) {
	ctx, udid := run.ctx, run.udid
	dev, opts := plan.device, plan.opts
	slog.DebugContext(ctx, "restore: starting", "device", dev.Name, "udid", udid,
		"source", plan.backup.Source(), "snapshot_id", opts.SnapshotID,
		"systemFiles", opts.SystemFiles, "reboot", opts.Reboot,
		"settingsFromBackup", opts.SettingsFromBackup, "removeItemsNotRestored", opts.RemoveItemsNotRestored)

	// A phone on Setup Assistant's "Restore from Mac or PC" screen is paired but
	// unactivated, and mb2's AFC sync lock errors out until it is activated.
	// Finder activates first; so do we.
	if code, err := s.activateIfNeeded(run, dev.Name); err != nil {
		return runOutcome{errorCode: code}, err
	}

	restoreErr := s.engine.RestoreSnapshot(ctx, engine.DeviceID(udid), plan.backup, engine.RestoreOptions{
		Password: opts.Password, SystemFiles: opts.SystemFiles, Reboot: opts.Reboot,
		SettingsFromBackup: opts.SettingsFromBackup, RemoveItemsNotRestored: opts.RemoveItemsNotRestored,
	}, s.progressSink(run, "", StageRestoring, plan.backup.SizeBytes()))
	fctx := context.WithoutCancel(ctx)
	if restoreErr != nil {
		slog.DebugContext(fctx, "restore: engine failed", "device", dev.Name, "error", restoreErr)
		return runOutcome{errorCode: engineErrorCode(restoreErr)}, fmt.Errorf("restore failed: %w", restoreErr)
	}
	// A restore the engine reports as done is already applied and irreversible,
	// so a late cancel can't turn it into a cancellation. beginCommit still
	// latches the commit phase so CancelRun stops offering a dead cancel.
	_ = s.beginCommit(run)
	slog.DebugContext(fctx, "restore: done", "device", dev.Name)
	return runOutcome{}, nil
}

type RestoreOptions struct {
	SnapshotID             string `json:"snapshotId"`
	Password               string `json:"password"`
	SystemFiles            bool   `json:"systemFiles"`
	Reboot                 bool   `json:"reboot"`
	SettingsFromBackup     bool   `json:"settingsFromBackup"`
	RemoveItemsNotRestored bool   `json:"removeItemsNotRestored"`
}

// Finder's standard restore; callers override single fields on top of it.
func DefaultRestoreOptions() RestoreOptions {
	return RestoreOptions{
		SystemFiles:            true,
		Reboot:                 true,
		SettingsFromBackup:     true,
		RemoveItemsNotRestored: true,
	}
}

type restorePlan struct {
	device *model.Device
	backup *iosbackup.Backup
	opts   RestoreOptions
}

// Called under the run's leases, so nothing it reads can change underneath.
func (s *Service) buildRestorePlan(ctx context.Context, targetUDID string, opts RestoreOptions) (*restorePlan, error) {
	device, err := s.pairedDevice(ctx, targetUDID)
	if err != nil {
		return nil, err
	}
	backup, err := s.library.Open(ctx, opts.SnapshotID)
	if errors.Is(err, library.ErrIncomplete) {
		return nil, &domain.ValidationError{Code: "backup_not_restorable", Message: "the selected backup is not confirmed complete and can't be restored"}
	}
	if err != nil {
		return nil, err
	}
	encrypted, snapshotIOS := backup.Encrypted, backup.IOSVersion
	if encrypted && opts.Password == "" {
		return nil, &domain.ValidationError{Code: "backup_password_required", Message: "this backup is encrypted — its password is required"}
	}
	if encrypted {
		valid, verifyErr := backup.VerifyPassword(opts.Password)
		switch {
		case verifyErr != nil:
			slog.WarnContext(ctx, "backup password preflight unavailable; the device enforces",
				"snapshot_id", backup.ID(), "error", verifyErr)
		case !valid:
			return nil, &domain.ValidationError{Code: "invalid_backup_password",
				Message: "this password does not unlock the selected backup"}
		}
	}
	// iOS refuses to apply a backup made on a newer iOS; fail before the run
	// starts instead of minutes into it.
	if iosbackup.NewerVersion(snapshotIOS, device.IOSVersion) {
		return nil, &domain.ValidationError{Code: "backup_ios_too_new",
			Message: fmt.Sprintf("this backup was made on iOS %s, newer than the phone's iOS %s — update the phone first", snapshotIOS, device.IOSVersion)}
	}
	return &restorePlan{device: device, backup: backup, opts: opts}, nil
}
