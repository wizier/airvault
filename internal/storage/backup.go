package storage

import (
	"context"

	"github.com/wizier/airvault/internal/model"

	"github.com/jmoiron/sqlx"
)

// backup_sources caches each source's reachable bytes; no row means the size is
// unknown until a collection measures it.
type BackupRepo struct{ s *Store }

func (r *BackupRepo) InsertSnapshot(ctx context.Context, backup model.Backup) error {
	_, err := sqlx.NamedExecContext(ctx, r.s.ext(), `
		INSERT INTO backups
			(id, source_udid, size_bytes, transferred_bytes,
			 encrypted, ios_version, device_name, product_type, created_at, started_at)
		VALUES
			(:id, :source_udid, :size_bytes, :transferred_bytes,
			 :encrypted, :ios_version, :device_name, :product_type, :created_at, :started_at)`,
		backup)
	return wrap(err, "insert backup snapshot")
}

func (r *BackupRepo) DeleteSnapshot(ctx context.Context, source, id string) error {
	_, err := r.s.ext().ExecContext(ctx,
		`DELETE FROM backups WHERE id = ? AND source_udid = ?`, id, source)
	return wrap(err, "delete backup snapshot")
}

func (r *BackupRepo) DeleteSourceSnapshots(ctx context.Context, source string) error {
	_, err := r.s.ext().ExecContext(ctx,
		`DELETE FROM backups WHERE source_udid = ?`, source)
	return wrap(err, "delete source snapshots")
}

func (r *BackupRepo) Get(ctx context.Context, id string) (*model.Backup, error) {
	return getOne[model.Backup](ctx, r.s.ext(),
		`SELECT * FROM backups WHERE id = ?`, id)
}

func (r *BackupRepo) SourceHasSnapshots(ctx context.Context, source string) (bool, error) {
	var exists bool
	if err := sqlx.GetContext(ctx, r.s.ext(), &exists,
		`SELECT EXISTS(SELECT 1 FROM backups WHERE source_udid = ?)`, source); err != nil {
		return false, wrap(err, "check source snapshots")
	}
	return exists, nil
}

func (r *BackupRepo) LatestCreated(ctx context.Context, source string) (*int64, error) {
	var latest *int64
	if err := sqlx.GetContext(ctx, r.s.ext(), &latest,
		`SELECT MAX(created_at) FROM backups WHERE source_udid = ?`, source); err != nil {
		return nil, wrap(err, "latest source snapshot")
	}
	return latest, nil
}

func (r *BackupRepo) SnapshotSources(ctx context.Context) (map[string]string, error) {
	rows, err := listOf[struct {
		ID     string `db:"id"`
		Source string `db:"source_udid"`
	}](ctx, r.s.ext(), `SELECT id, source_udid FROM backups`)
	if err != nil {
		return nil, wrap(err, "list catalog snapshot sources")
	}
	index := make(map[string]string, len(rows))
	for _, row := range rows {
		index[row.ID] = row.Source
	}
	return index, nil
}

func (r *BackupRepo) ListCompleteBySource(ctx context.Context, source string) ([]model.Backup, error) {
	return listOf[model.Backup](ctx, r.s.ext(), `
		SELECT * FROM backups
		WHERE source_udid = ?
		ORDER BY created_at DESC, id DESC`, source)
}

const listCompleteLimit = 1000

// No join on devices, so snapshots from forgotten phones remain selectable as
// restore sources.
func (r *BackupRepo) ListComplete(ctx context.Context) ([]model.Backup, error) {
	return listOf[model.Backup](ctx, r.s.ext(), `
		SELECT * FROM backups
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, listCompleteLimit)
}

// Backups hold hundreds of thousands of files, so a request never walks
// manifests; that accounting belongs to the mutation and reconciliation paths.
type SourceSummary struct {
	SourceUDID    string `db:"source_udid"`
	RestorePoints int    `db:"restore_points"`
	LatestCreated int64  `db:"latest_created_at"`
	DiskBytes     *int64 `db:"disk_bytes"`
	// Identity of the newest snapshot, so a source with no registry row still
	// shows a real name and model.
	DeviceName  string `db:"device_name"`
	ProductType string `db:"product_type"`
	IOSVersion  string `db:"ios_version"`
}

// Nil DiskBytes means the size is unknown, never a fabricated zero.
func (r *BackupRepo) SummaryBySource(ctx context.Context) (map[string]SourceSummary, error) {
	rows, err := listOf[SourceSummary](ctx, r.s.ext(), `
		SELECT summary.source_udid, summary.restore_points,
			summary.latest_created_at, summary.device_name,
			summary.product_type, summary.ios_version, source.disk_bytes
		FROM (
			SELECT source_udid, device_name, product_type, ios_version,
				COUNT(*) OVER (PARTITION BY source_udid) AS restore_points,
				created_at AS latest_created_at,
				ROW_NUMBER() OVER (
					PARTITION BY source_udid ORDER BY created_at DESC, id DESC
				) AS rn
			FROM backups
		) AS summary
		LEFT JOIN backup_sources AS source ON source.source_udid = summary.source_udid
		WHERE summary.rn = 1`)
	if err != nil {
		return nil, wrap(err, "summarize backup sources")
	}
	out := make(map[string]SourceSummary, len(rows))
	for _, row := range rows {
		out[row.SourceUDID] = row
	}
	return out, nil
}

func (r *BackupRepo) SetSourceFootprint(ctx context.Context, source string, diskBytes int64) error {
	_, err := r.s.ext().ExecContext(ctx, `
		INSERT INTO backup_sources (source_udid, disk_bytes) VALUES (?, ?)
		ON CONFLICT(source_udid) DO UPDATE SET disk_bytes = excluded.disk_bytes`,
		source, diskBytes)
	return wrap(err, "set source footprint")
}

// A delta means nothing without a measured base, so an unknown size stays
// unknown instead of being seeded from one.
func (r *BackupRepo) AddSourceFootprint(ctx context.Context, source string, delta int64) error {
	_, err := r.s.ext().ExecContext(ctx, `
		UPDATE backup_sources SET disk_bytes = disk_bytes + ?
		WHERE source_udid = ?`, delta, source)
	return wrap(err, "add source footprint")
}

// For a change whose effect on disk the caller cannot price; the next
// collection measures it.
func (r *BackupRepo) ForgetSourceFootprint(ctx context.Context, source string) error {
	_, err := r.s.ext().ExecContext(ctx,
		`DELETE FROM backup_sources WHERE source_udid = ?`, source)
	return wrap(err, "forget source footprint")
}

// No size may outlive the snapshots it was measured from.
func (r *BackupRepo) DropUnreferencedFootprints(ctx context.Context) error {
	_, err := r.s.ext().ExecContext(ctx, `
		DELETE FROM backup_sources
		WHERE source_udid NOT IN (SELECT source_udid FROM backups)`)
	return wrap(err, "drop unreferenced source footprints")
}
