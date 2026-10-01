package library

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/wizier/airvault/internal/model"
	"github.com/wizier/airvault/internal/objectstore"
)

// Reconcile runs at startup, before any operation: it settles every source's
// interrupted snapshots and rebuilds the catalog from the manifests. Every
// source is returned for collection, since agreeing ids cannot rule out
// orphaned objects. A source that cannot be read is logged and left as it is,
// never failing startup.
func (l *Library) Reconcile(ctx context.Context) ([]string, error) {
	sources, err := l.objects.ListSources()
	if err != nil {
		return nil, fmt.Errorf("list object sources: %w", err)
	}
	for _, source := range sources {
		// A stranded envelope only blocks this source's collection.
		if err := l.ReconcileStaging(source); err != nil {
			slog.ErrorContext(ctx, "staging reconcile: source skipped", "source", source, "error", err)
		}
	}
	sources, err = l.reconcileCatalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("rebuild backup catalog: %w", err)
	}
	return sources, nil
}

// ReconcileStaging settles the source's unfinished snapshots: published ones
// complete their publication, the rest are discarded. No operation may be in
// flight on the source, so each of them is abandoned.
func (l *Library) ReconcileStaging(source string) error {
	return l.objects.RecoverSource(source)
}

// Only manifests new to the catalog are opened: an admitted row was
// seal-verified when let in, and leaving it alone preserves its run facts.
func (l *Library) reconcileCatalog(ctx context.Context) ([]string, error) {
	diskSources, err := l.objects.ListSources()
	if err != nil {
		return nil, fmt.Errorf("list object sources: %w", err)
	}
	// Rows leave unaccounted as their manifest turns up; the rest have none.
	unaccounted, err := l.catalog.Backup.SnapshotSources(ctx)
	if err != nil {
		return nil, err
	}
	admit := make([]model.Backup, 0)
	claimed := make(map[string]string, len(unaccounted))
	for _, source := range diskSources {
		snapshots, err := l.objects.ListSnapshots(source)
		if err != nil {
			// Without a listing none of this source's rows can be judged stale.
			slog.ErrorContext(ctx, "catalog reconcile: source skipped", "source", source, "error", err)
			maps.DeleteFunc(unaccounted, func(_, owner string) bool { return owner == source })
			continue
		}
		for _, snapshot := range snapshots {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			id := snapshot.ID
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
			if row, ok := l.projectNew(ctx, source, snapshot); ok {
				admit = append(admit, row)
			}
		}
	}
	stale := make([]staleSnapshot, 0, len(unaccounted))
	for id, source := range unaccounted {
		stale = append(stale, staleSnapshot{source: source, id: id})
	}
	slices.SortFunc(stale, func(a, b staleSnapshot) int { return strings.Compare(a.id, b.id) })
	if err := l.replaceCatalog(ctx, stale, admit); err != nil {
		return nil, fmt.Errorf("reconcile backup catalog: %w", err)
	}
	return diskSources, nil
}

// Every manifest becomes a row, so nothing on disk is out of sight. An
// unreadable one is admitted damaged; one whose backup files do not read is
// admitted with what its manifest says, for an integrity check to judge.
func (l *Library) projectNew(ctx context.Context, source string, snapshot objectstore.SnapshotFile) (model.Backup, bool) {
	id := snapshot.ID
	view, err := l.objects.OpenSnapshot(source, id)
	if errors.Is(err, objectstore.ErrManifestCorrupt) {
		slog.ErrorContext(ctx, "catalog reconcile: manifest unreadable",
			"source", source, "snapshot_id", id, "error", err)
		return model.Backup{ID: id, SourceUDID: source, Damage: objectstore.DamageManifestUnreadable,
			CreatedAt: snapshot.Modified.Unix()}, true
	}
	if err != nil {
		// A newer format or a failed read says nothing about the backup.
		slog.WarnContext(ctx, "catalog reconcile: snapshot skipped",
			"source", source, "snapshot_id", id, "error", err)
		return model.Backup{}, false
	}
	row, err := Project(view)
	if err != nil {
		slog.WarnContext(ctx, "catalog reconcile: backup files unreadable",
			"source", source, "snapshot_id", id, "error", err)
		row = model.Backup{ID: id, SourceUDID: source, SizeBytes: view.SizeBytes(), CreatedAt: view.CreatedUnix()}
	}
	return row, true
}
