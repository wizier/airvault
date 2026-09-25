package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/wizier/airvault/internal/iosbackup"
	"github.com/wizier/airvault/internal/model"
)

func (s *Service) prepareSnapshot(ctx context.Context, run *runReservation) (*model.Backup, string, error) {
	base, err := s.latestRestorableSnapshot(ctx, run.udid)
	if err != nil {
		return nil, "", fmt.Errorf("find base snapshot: %w", err)
	}
	baseSnapshotID := ""
	if base != nil {
		baseSnapshotID = base.ID
	}
	snapshotID, err := uuid.NewV7()
	if err != nil {
		return nil, "", fmt.Errorf("allocate snapshot id: %w", err)
	}
	startedAt := time.Now().Unix()
	return &model.Backup{
		ID: snapshotID.String(), SourceUDID: run.udid, StartedAt: &startedAt,
	}, baseSnapshotID, nil
}

// latestRestorableSnapshot skips a damaged newest snapshot when an older
// valid incremental base exists, but never silently falls back to a full copy.
func (s *Service) latestRestorableSnapshot(ctx context.Context, sourceUDID string) (*model.Backup, error) {
	snapshots, err := s.store.Backup.ListCompleteBySource(ctx, sourceUDID)
	if err != nil {
		return nil, err
	}
	if len(snapshots) == 0 {
		return nil, nil // first backup
	}
	for i := range snapshots {
		snapshot := &snapshots[i]
		view, inspectErr := s.objects.OpenSnapshot(sourceUDID, snapshot.ID)
		if inspectErr == nil {
			if _, inspectErr = iosbackup.Inspect(view); inspectErr == nil {
				return snapshot, nil
			}
		}
		slog.WarnContext(ctx, "backup snapshot is not usable as an incremental base",
			"snapshot_id", snapshot.ID, "source", sourceUDID, "error", inspectErr)
	}
	return nil, errors.New("no complete backup snapshot is usable as an incremental base")
}

func (s *Service) discardSnapshot(ctx context.Context, snapshot *model.Backup) error {
	fctx := context.WithoutCancel(ctx)
	if err := s.objects.DiscardStaging(snapshot.SourceUDID, snapshot.ID); err != nil {
		return err
	}
	if err := s.collectSource(fctx, snapshot.SourceUDID); err != nil {
		slog.WarnContext(fctx, "discard snapshot: orphan collection deferred",
			"source", snapshot.SourceUDID, "error", err)
	}
	return nil
}

// reconcileStagingSnapshot resolves an interrupted publication: a final
// manifest is the durable commit point and is recovered, otherwise all
// snapshot-owned staging is discarded. True only on a complete recovery.
func (s *Service) reconcileStagingSnapshot(ctx context.Context, snapshot *model.Backup, transferredBytes *int64) (bool, error) {
	published, openErr := s.objects.OpenSnapshot(snapshot.SourceUDID, snapshot.ID)
	if openErr == nil {
		projection, validationErr := snapshotProjection(published, snapshot.SourceUDID, snapshot.ID)
		if validationErr != nil {
			return false, fmt.Errorf("validate interrupted published snapshot: %w", validationErr)
		}
		// The rename already committed; complete the durability boundary and
		// clear the mutable envelope left by the crash.
		if err := s.objects.FinishPublication(published); err != nil {
			return false, fmt.Errorf("finish interrupted snapshot publication: %w", err)
		}
		fctx := context.WithoutCancel(ctx)
		projection.TransferredBytes = transferredBytes
		projection.StartedAt = snapshot.StartedAt
		// The recovered snapshot's delta is unknown, so the collection below
		// measures the source instead.
		if err := s.publishSnapshot(fctx, projection, nil); err != nil {
			return false, fmt.Errorf("recover published snapshot: %w", err)
		}
		if err := s.collectSource(fctx, snapshot.SourceUDID); err != nil {
			slog.WarnContext(fctx, "recovered snapshot: object collection deferred",
				"source", snapshot.SourceUDID, "error", err)
		}
		return true, nil
	}
	if !errors.Is(openErr, fs.ErrNotExist) {
		// The run is over either way, so the envelope goes regardless of why the
		// manifest could not be read. Leaving it behind silently disables object
		// collection for this source until the process restarts.
		inspectErr := fmt.Errorf("inspect interrupted snapshot: %w", openErr)
		if err := s.objects.DiscardStaging(snapshot.SourceUDID, snapshot.ID); err != nil {
			return false, errors.Join(inspectErr, fmt.Errorf("discard interrupted staging: %w", err))
		}
		return false, inspectErr
	}
	if err := s.discardSnapshot(ctx, snapshot); err != nil {
		return false, fmt.Errorf("discard interrupted snapshot: %w", err)
	}
	return false, nil
}
