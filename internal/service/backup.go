package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/engine"
	"github.com/wizier/airvault/internal/events"
	"github.com/wizier/airvault/internal/model"
)

// How long a launched backup waits for the phone to appear in the muxer.
const deviceAppearWindow = 2 * time.Minute

// awaitReachable waits for a standby Wi-Fi phone's next muxer visibility
// window, woken by the runtime store's presence-change signal rather than
// polling. netmuxd remains the sole owner of heartbeat/liveness throughout.
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

// StartBackup is the single launch API. HTTP returns the run id immediately.
func (s *Service) StartBackup(udid string) (string, error) {
	// Both leases key on the request's udid, so admission needs no lookup: the
	// device row is read once, under the lease that protects it.
	run, err := s.reserveRun(s.app, runKindBackup, udid,
		deviceReadResource(udid), snapshotWriteResource(udid))
	if err != nil {
		return "", err
	}
	device, err := s.pairedDevice(s.app, udid)
	if err != nil {
		s.discardRun(run)
		return "", err
	}
	if err := s.announceRun(run, StageWaiting); err != nil {
		s.discardRun(run)
		return "", err
	}
	s.bus.Emit(events.BackupStarted, map[string]any{"runId": run.id, "udid": udid})
	return s.launchRun(run, func() (runOutcome, error) {
		return s.executeBackup(run, device)
	}), nil
}

func (s *Service) executeBackup(run *runReservation, device *model.Device) (runOutcome, error) {
	ctx, app, udid := run.ctx, run.app, run.udid
	if !s.awaitReachable(ctx, udid) {
		if ctx.Err() != nil {
			if app.Err() != nil {
				return runOutcome{}, ctx.Err()
			}
			return runOutcome{}, domain.ErrCancelled
		}
		return runOutcome{errorCode: "device_never_came_online"},
			errors.New("backup: the phone didn't come online — make sure it's on Wi-Fi and awake")
	}
	s.setRunStage(run, StagePreparing)

	snapshot, baseSnapshotID, err := s.prepareSnapshot(ctx, run)
	if err != nil {
		if ctx.Err() != nil {
			return s.finalizeCancelledBackup(ctx, app, device, nil, err)
		}
		reason := "couldn't prepare an immutable backup snapshot"
		return runOutcome{}, fmt.Errorf("%s: %w", reason, err)
	}
	if ctx.Err() != nil {
		// The engine has not started, so no staging data or pooled objects exist.
		return s.finalizeCancelledBackup(ctx, app, device, nil, ctx.Err())
	}
	slog.DebugContext(ctx, "backup: starting", "device", device.Name, "udid", udid)
	request := engine.BuildSnapshotRequest{
		OperationID:    engine.OperationID(run.id),
		DeviceID:       engine.DeviceID(udid),
		SnapshotID:     engine.SnapshotID(snapshot.ID),
		BaseSnapshotID: engine.SnapshotID(baseSnapshotID),
	}
	addedBytes, engineErr := s.engine.BuildSnapshot(ctx, request, s.progressSink(run, StageBackingUp))
	var sizeBytes int64
	var projection model.Backup
	var transferredBytes int64
	finalCtx := context.WithoutCancel(ctx)

	if ctx.Err() != nil {
		return s.finalizeCancelledBackup(ctx, app, device, snapshot, engineErr)
	}

	if engineErr == nil {
		view, openErr := s.objects.OpenStaging(udid, snapshot.ID)
		if openErr != nil {
			engineErr = fmt.Errorf("open completed object snapshot: %w", openErr)
		} else if ctx.Err() != nil {
			return s.finalizeCancelledBackup(ctx, app, device, snapshot, ctx.Err())
		} else if candidate, validationErr := snapshotProjection(&view.View, udid, snapshot.ID); validationErr != nil {
			engineErr = fmt.Errorf("backup snapshot validation failed: %w", validationErr)
		} else {
			if !s.beginCommit(run) {
				return s.finalizeCancelledBackup(ctx, app, device, snapshot, ctx.Err())
			}
			candidate.StartedAt = snapshot.StartedAt
			projection = candidate
			sizeBytes = projection.SizeBytes
			transferredBytes = s.transferredBytes(run)
			projection.TransferredBytes = &transferredBytes
			_, engineErr = s.objects.Publish(view)
		}
	}
	if engineErr != nil {
		// Publish may have crossed its final-manifest commit point before a
		// subsequent fsync or staging cleanup failed. Resolve that boundary instead
		// of blindly aborting and deleting a snapshot that is already immutable.
		var transferred *int64
		if projection.ID != "" {
			transferred = &transferredBytes
		}
		recovered, snapshotErr := s.reconcileStagingSnapshot(finalCtx, snapshot, transferred)
		if recovered && snapshotErr == nil {
			slog.WarnContext(finalCtx, "backup: publication recovered after error",
				"device", device.Name, "snapshot_id", snapshot.ID, "error", engineErr)
			return runOutcome{sizeBytes: sizeBytes}, nil
		}
		errorCode := engineErrorCode(engineErr)
		slog.DebugContext(finalCtx, "backup: engine failed", "device", device.Name, "error", engineErr)
		return runOutcome{errorCode: errorCode}, errors.Join(
			fmt.Errorf("backup failed: %w", engineErr), snapshotErr)
	}

	// The footprint delta is the engine-reported pool payload plus the
	// published manifest itself; the catalog commit applies it atomically.
	var added *int64
	if manifestBytes, statErr := s.objects.SnapshotManifestBytes(udid, snapshot.ID); statErr == nil {
		delta := addedBytes + manifestBytes
		added = &delta
	}
	if catalogErr := s.publishSnapshot(finalCtx, projection, added); catalogErr != nil {
		current, getErr := s.store.Backup.Get(finalCtx, snapshot.ID)
		if getErr == nil {
			slog.WarnContext(finalCtx, "backup: catalog commit returned an error but is durable",
				"snapshotId", current.ID, "error", catalogErr)
		} else {
			// Publish is the durable commit point. Never discard its immutable objects
			// because SQLite failed: startup reconciliation can rebuild the catalog.
			const reason = "backup was saved, but its catalog entry couldn't be updated"
			return runOutcome{}, fmt.Errorf("%s: %w", reason, errors.Join(catalogErr, getErr))
		}
	}
	slog.DebugContext(finalCtx, "backup: done", "device", device.Name, "size_bytes", sizeBytes)
	return runOutcome{sizeBytes: sizeBytes}, nil
}

func (s *Service) finalizeCancelledBackup(
	ctx, app context.Context,
	device *model.Device,
	snapshot *model.Backup,
	cause error,
) (runOutcome, error) {
	finalCtx := context.WithoutCancel(ctx)
	if cause == nil {
		cause = ctx.Err()
	}
	var snapshotErr error
	if snapshot != nil {
		snapshotErr = s.discardSnapshot(finalCtx, snapshot)
	}
	if app.Err() != nil {
		return runOutcome{}, errors.Join(cause, snapshotErr)
	}
	slog.DebugContext(finalCtx, "backup: cancelled", "device", device.Name, "udid", device.UDID)
	return runOutcome{}, errors.Join(domain.ErrCancelled, cause, snapshotErr)
}
