package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/library"
	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/objectstore"
	"log/slog"
	"time"
	"uuid"
)

const deviceAppearWindow = 2 * time.Minute

// Woken by presence-change signals rather than polling: netmuxd stays the sole
// owner of heartbeat and liveness.
func (s *Service) awaitReachable(ctx context.Context, udid string) bool {
	reachable, changed := s.live.connectionWait(udid)
	if reachable {
		return true
	}
	deadline := time.NewTimer(deviceAppearWindow)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			reachable, _ = s.live.connectionWait(udid)
			return reachable
		case <-changed:
			// A snapshot was applied — re-read presence and the fresh channel.
			reachable, changed = s.live.connectionWait(udid)
			if reachable {
				return true
			}
		}
	}
}

// Admission runs on the request's ctx; the run itself lives as long as the app.
func (s *Service) StartBackup(ctx context.Context, udid string) (string, error) {
	return s.startBackup(ctx, udid, false)
}

func (s *Service) startBackup(ctx context.Context, udid string, auto bool) (string, error) {
	// Both leases key on the request's udid, so admission needs no lookup: the
	// device row is read once, under the lease that protects it.
	run, err := s.reserveRun(runKindBackup, udid,
		deviceReadResource(udid), snapshotWriteResource(udid))
	if err != nil {
		return "", err
	}
	run.auto = auto
	device, err := s.pairedDevice(ctx, udid)
	if err != nil {
		s.discardRun(run)
		return "", err
	}
	if err := s.runs.announce(run, StageWaiting); err != nil {
		s.discardRun(run)
		return "", err
	}
	s.bus.Emit(runStarted(run, ""))
	return s.launchRun(run, func() (runOutcome, error) {
		return s.executeBackup(run, device)
	}), nil
}

func (s *Service) executeBackup(run *runReservation, device *model.Device) (runOutcome, error) {
	ctx, udid := run.ctx, run.udid
	if !s.awaitReachable(ctx, udid) {
		if err := ctx.Err(); err != nil {
			return runOutcome{}, err
		}
		return runOutcome{errorCode: "device_never_came_online"},
			errors.New("backup: the phone didn't come online — make sure it's on Wi-Fi and awake")
	}
	s.runs.setStage(run, StagePreparing)

	base, err := s.library.LatestBase(ctx, udid)
	if err != nil {
		return runOutcome{}, fmt.Errorf("couldn't prepare an immutable backup snapshot: %w", err)
	}
	id, startedAt := uuid.NewV7().String(), time.Now().Unix()
	if err := ctx.Err(); err != nil {
		// The engine has not started, so no staging data or pooled objects exist.
		return runOutcome{}, err
	}
	slog.DebugContext(ctx, "backup: starting", "device", device.Name, "udid", udid)
	staged, err := s.buildSnapshot(ctx, run, id, base)
	finalCtx := context.WithoutCancel(ctx)
	var row model.Backup
	if err == nil && ctx.Err() == nil {
		if row, err = library.Project(&staged.Snapshot); err != nil {
			err = fmt.Errorf("backup snapshot validation failed: %w", err)
		}
	}
	// beginCommit is the cancellation boundary: past it the backup is published.
	if err == nil && s.runs.beginCommit(run) {
		row.StartedAt, row.TransferredBytes = &startedAt, new(s.runs.transferred(run))
		if err := s.library.Publish(finalCtx, staged, row); err != nil {
			return runOutcome{}, fmt.Errorf("publish backup: %w", err)
		}
		slog.DebugContext(finalCtx, "backup: done", "device", device.Name, "size_bytes", row.SizeBytes)
		return runOutcome{sizeBytes: row.SizeBytes}, nil
	}
	// Nothing was published: the snapshot goes, and what only it pooled.
	discardErr := s.library.Discard(finalCtx, udid, id)
	if ctx.Err() != nil {
		return runOutcome{}, errors.Join(cmp.Or(err, ctx.Err()), discardErr)
	}
	slog.DebugContext(finalCtx, "backup: engine failed", "device", device.Name, "error", err)
	return runOutcome{errorCode: engineErrorCode(err)}, errors.Join(fmt.Errorf("backup failed: %w", err), discardErr)
}

// buildSnapshot backs the device up into a new snapshot on top of base, nil
// for a full backup. Only an incremental backup pauses on the phone long
// enough to name: it diffs against the previous manifest first.
func (s *Service) buildSnapshot(ctx context.Context, run *runReservation, id string, base *iosbackup.Backup) (*objectstore.StagedSnapshot, error) {
	var idleStage RunStage
	if base != nil {
		idleStage = StageCalculating
	}
	draft, err := s.library.Begin(run.udid, id, base)
	if err != nil {
		return nil, err
	}
	return s.engine.BuildSnapshot(ctx, engine.DeviceID(run.udid), draft, s.runs.progressSink(run, idleStage, StageBackingUp, 0))
}
