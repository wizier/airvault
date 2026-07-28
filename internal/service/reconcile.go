package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/objectstore"
)

// ReconcileBackupStore resolves interrupted transitions, then rebuilds the
// catalog from the manifests on disk. It returns every source — agreeing IDs
// cannot rule out orphaned pool objects — for the background collection pass.
func (s *Service) ReconcileBackupStore(ctx context.Context) ([]string, error) {
	if err := s.objects.ReconcileStaging(); err != nil {
		return nil, fmt.Errorf("resolve object staging state: %w", err)
	}
	sources, err := s.reconcileCatalogFromStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("rebuild backup catalog: %w", err)
	}
	return sources, nil
}

// reconcileCatalogFromStore projects the published manifests onto the catalog.
// Only manifests new to it are opened — an admitted row was seal-verified when it
// was let in, and leaving it alone is what preserves its runtime-only facts.
func (s *Service) reconcileCatalogFromStore(ctx context.Context) ([]string, error) {
	diskSources, err := s.objects.ListSources()
	if err != nil {
		return nil, fmt.Errorf("list object sources: %w", err)
	}
	// Rows leave unaccounted as their manifest turns up; the rest have none.
	unaccounted, err := s.store.Backup.SnapshotSources(ctx)
	if err != nil {
		return nil, err
	}
	admit := make([]model.Backup, 0)
	claimed := make(map[string]string, len(unaccounted))
	for _, source := range diskSources {
		ids, err := s.objects.ListSnapshotIDs(source)
		if err != nil {
			return nil, fmt.Errorf("list manifests for source %s: %w", source, err)
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if owner, taken := claimed[id]; taken {
				// Sources arrive sorted, so the same copy wins every restart.
				slog.ErrorContext(ctx, "catalog reconcile: snapshot id is published twice",
					"snapshot_id", id, "kept_source", owner, "ignored_source", source)
				continue
			}
			claimed[id] = source
			if unaccounted[id] == source {
				delete(unaccounted, id)
				continue
			}
			if projection, ok := s.projectNewSnapshot(ctx, source, id); ok {
				admit = append(admit, projection)
			}
		}
	}
	stale := make([]staleSnapshot, 0, len(unaccounted))
	for id, source := range unaccounted {
		stale = append(stale, staleSnapshot{source: source, id: id})
	}
	slices.SortFunc(stale, func(a, b staleSnapshot) int { return strings.Compare(a.id, b.id) })
	if err := s.replaceCatalog(ctx, stale, admit); err != nil {
		return nil, fmt.Errorf("reconcile backup catalog: %w", err)
	}
	return diskSources, nil
}

// projectNewSnapshot opens a manifest not yet in the catalog. A provably corrupt
// one is unlinked; an otherwise invalid one is skipped, never advertised.
func (s *Service) projectNewSnapshot(ctx context.Context, source, id string) (model.Backup, bool) {
	view, err := s.objects.OpenSnapshot(source, id)
	if err != nil {
		if !s.removeIfCorrupt(ctx, source, id, err) {
			slog.WarnContext(ctx, "catalog reconcile: invalid snapshot",
				"source", source, "snapshot_id", id, "error", err)
		}
		return model.Backup{}, false
	}
	projection, err := snapshotProjection(view, source, id)
	if err != nil {
		slog.WarnContext(ctx, "catalog reconcile: snapshot is not valid",
			"source", source, "snapshot_id", id, "error", err)
		return model.Backup{}, false
	}
	return projection, true
}

// removeIfCorrupt unlinks a manifest whose seal is provably corrupt, reporting
// whether it went. Other open errors are left for the caller to classify.
func (s *Service) removeIfCorrupt(ctx context.Context, source, id string, openErr error) bool {
	if !errors.Is(openErr, objectstore.ErrManifestCorrupt) {
		return false
	}
	if err := s.objects.RemoveSnapshot(source, id); err != nil {
		slog.WarnContext(ctx, "drop corrupt manifest failed",
			"source", source, "snapshot_id", id, "error", errors.Join(openErr, err))
		return false
	}
	slog.ErrorContext(ctx, "corrupt restore point removed",
		"source", source, "snapshot_id", id, "error", openErr)
	return true
}
