package storage

import (
	"context"

	"github.com/wizier/airvault/internal/model"
)

// DeviceRepo is the devices table.
type DeviceRepo struct{ s *Store }

// Columns are named explicitly so scanning does not depend on the table's
// physical column set.
const deviceColumns = `udid, name, product_type, ios_version, paired, encrypted, last_seen_at`

// List returns all devices, ordered by name.
func (r *DeviceRepo) List(ctx context.Context) ([]model.Device, error) {
	return listOf[model.Device](ctx, r.s.ext(),
		`SELECT `+deviceColumns+` FROM devices ORDER BY name, udid`)
}

// GetByUDID returns one device by UDID.
func (r *DeviceRepo) GetByUDID(ctx context.Context, udid string) (*model.Device, error) {
	return getOne[model.Device](ctx, r.s.ext(),
		`SELECT `+deviceColumns+` FROM devices WHERE udid = ?`, udid)
}

// Upsert inserts a device by UDID or updates its mutable fields. Used by
// discovery and by the pairing flow.
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

// TouchLastSeen stamps a device's last_seen_at (unix seconds). Used when a
// device drops off the muxer, so "last seen" reflects the moment it left.
func (r *DeviceRepo) TouchLastSeen(ctx context.Context, udid string, at int64) error {
	_, err := r.s.ext().ExecContext(ctx,
		`UPDATE devices SET last_seen_at = ? WHERE udid = ?`, at, udid)
	return wrap(err, "touch last seen")
}

// SetEncrypted records a confirmed backup-encryption flag immediately — the UI
// refetch that follows the emit must see the new value, not wait out the next
// discover pass.
func (r *DeviceRepo) SetEncrypted(ctx context.Context, udid string, encrypted bool) error {
	_, err := r.s.ext().ExecContext(ctx,
		`UPDATE devices SET encrypted = ? WHERE udid = ?`, encrypted, udid)
	return wrap(err, "set encrypted")
}

// Delete removes a device from the discovery registry. Backups are independent.
func (r *DeviceRepo) Delete(ctx context.Context, udid string) error {
	_, err := r.s.ext().ExecContext(ctx, `DELETE FROM devices WHERE udid = ?`, udid)
	return wrap(err, "delete device")
}
