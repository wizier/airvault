package storage

import (
	"context"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/model"
)

type DeviceRepo struct{ s *Store }

// Columns are named explicitly so scanning does not depend on the table's
// physical column set.
const deviceColumns = `udid, name, product_type, ios_version, paired, encrypted, last_seen_at,
	auto_backup, auto_backup_days, auto_backup_window_start, auto_backup_window_end, auto_backup_tz`

func (r *DeviceRepo) List(ctx context.Context) ([]model.Device, error) {
	return listOf[model.Device](ctx, r.s.ext(),
		`SELECT `+deviceColumns+` FROM devices ORDER BY name, udid`)
}

func (r *DeviceRepo) GetByUDID(ctx context.Context, udid string) (*model.Device, error) {
	return getOne[model.Device](ctx, r.s.ext(),
		`SELECT `+deviceColumns+` FROM devices WHERE udid = ?`, udid)
}

func (r *DeviceRepo) Upsert(ctx context.Context, d *model.Device) error {
	_, err := r.s.ext().ExecContext(ctx, `
		INSERT INTO devices (udid, name, product_type, ios_version, paired, encrypted, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(udid) DO UPDATE SET
			name         = excluded.name,
			product_type = excluded.product_type,
			ios_version  = excluded.ios_version,
			paired       = excluded.paired,
			encrypted    = excluded.encrypted,
			last_seen_at = excluded.last_seen_at`,
		d.UDID, d.Name, d.ProductType, d.IOSVersion, d.Paired, d.Encrypted, d.LastSeenAt)
	return wrap(err, "upsert device")
}

func (r *DeviceRepo) TouchLastSeen(ctx context.Context, udid string, at int64) error {
	_, err := r.s.ext().ExecContext(ctx,
		`UPDATE devices SET last_seen_at = ? WHERE udid = ?`, at, udid)
	return wrap(err, "touch last seen")
}

// The UI refetch that follows the emit must see the new flag without waiting
// for the next discover pass.
func (r *DeviceRepo) SetEncrypted(ctx context.Context, udid string, encrypted bool) error {
	_, err := r.s.ext().ExecContext(ctx,
		`UPDATE devices SET encrypted = ? WHERE udid = ?`, encrypted, udid)
	return wrap(err, "set encrypted")
}

// ErrNotFound when the device is gone (an unpair may have just removed it).
func (r *DeviceRepo) SetAutoBackup(ctx context.Context, udid string, settings model.AutoBackup) error {
	result, err := r.s.ext().ExecContext(ctx, `
		UPDATE devices SET
			auto_backup              = ?,
			auto_backup_days         = ?,
			auto_backup_window_start = ?,
			auto_backup_window_end   = ?,
			auto_backup_tz           = ?
		WHERE udid = ?`,
		settings.Enabled, settings.Days, settings.WindowStart, settings.WindowEnd,
		settings.TimeZone, udid)
	if err != nil {
		return wrap(err, "set auto backup")
	}
	if affected, err := result.RowsAffected(); err != nil {
		return wrap(err, "set auto backup")
	} else if affected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Backups are independent of the device row.
func (r *DeviceRepo) Delete(ctx context.Context, udid string) error {
	_, err := r.s.ext().ExecContext(ctx, `DELETE FROM devices WHERE udid = ?`, udid)
	return wrap(err, "delete device")
}
