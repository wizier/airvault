package storage

import (
	"context"

	"github.com/wizier/airvault/internal/model"
)

// Columns are named explicitly so scanning does not depend on the table's
// physical column set.
const (
	deviceColumns = `udid, name, product_type, ios_version, paired, encrypted, last_seen_at,
		cleanup, cleanup_keep_days, cleanup_thin`
	scheduleColumns = `udid, task, enabled, days, window_start, window_end`
)

type scheduleTask string

const (
	taskBackup scheduleTask = "backup"
	taskVerify scheduleTask = "verify"
)

// What a device has until its schedules are saved, so discovery writes no rows.
var (
	defaultAutoBackup = model.Schedule{Days: 1}
	defaultAutoVerify = model.Schedule{Days: 7, WindowStart: new(int64(2 * 60)), WindowEnd: new(int64(6 * 60))}
)

type DeviceRepo struct{ s *Store }

func (r *DeviceRepo) List(ctx context.Context) ([]model.Device, error) {
	devices, err := listOf[model.Device](ctx, r.s.ext(),
		`SELECT `+deviceColumns+` FROM devices ORDER BY name, udid`)
	if err != nil {
		return nil, err
	}
	schedules, err := listOf[scheduleRow](ctx, r.s.ext(), `SELECT `+scheduleColumns+` FROM device_schedules`)
	if err != nil {
		return nil, err
	}
	for i := range devices {
		readSchedules(&devices[i], schedules)
	}
	return devices, nil
}

func (r *DeviceRepo) GetByUDID(ctx context.Context, udid string) (*model.Device, error) {
	device, err := getOne[model.Device](ctx, r.s.ext(),
		`SELECT `+deviceColumns+` FROM devices WHERE udid = ?`, udid)
	if err != nil {
		return nil, err
	}
	schedules, err := listOf[scheduleRow](ctx, r.s.ext(),
		`SELECT `+scheduleColumns+` FROM device_schedules WHERE udid = ?`, udid)
	if err != nil {
		return nil, err
	}
	readSchedules(device, schedules)
	return device, nil
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

// SetAutomaticTasks saves all the settings or none. ErrNotFound when the device
// is gone (an unpair may have just removed it).
func (r *DeviceRepo) SetAutomaticTasks(ctx context.Context, udid string, tasks model.AutomaticTasks) error {
	return r.s.WithTx(ctx, func(tx *Store) error {
		cleanup := tasks.Cleanup
		result, err := tx.ext().ExecContext(ctx, `
			UPDATE devices SET
				cleanup           = ?,
				cleanup_keep_days = ?,
				cleanup_thin      = ?
			WHERE udid = ?`,
			cleanup.Enabled, cleanup.KeepDays, cleanup.Thin, udid)
		if err := updatedOne(result, err, "set cleanup"); err != nil {
			return err
		}
		if err := tx.Device.setSchedule(ctx, udid, taskBackup, tasks.AutoBackup); err != nil {
			return err
		}
		return tx.Device.setSchedule(ctx, udid, taskVerify, tasks.AutoVerify)
	})
}

func (r *DeviceRepo) setSchedule(ctx context.Context, udid string, task scheduleTask, schedule model.Schedule) error {
	_, err := r.s.ext().ExecContext(ctx, `
		INSERT INTO device_schedules (udid, task, enabled, days, window_start, window_end)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(udid, task) DO UPDATE SET
			enabled      = excluded.enabled,
			days         = excluded.days,
			window_start = excluded.window_start,
			window_end   = excluded.window_end`,
		udid, task, schedule.Enabled, schedule.Days, schedule.WindowStart, schedule.WindowEnd)
	return wrap(err, "set "+string(task)+" schedule")
}

// Backups are independent of the device row; its schedules go with it.
func (r *DeviceRepo) Delete(ctx context.Context, udid string) error {
	_, err := r.s.ext().ExecContext(ctx, `DELETE FROM devices WHERE udid = ?`, udid)
	return wrap(err, "delete device")
}

// readSchedules fills in the device's schedules from rows of any devices.
func readSchedules(device *model.Device, schedules []scheduleRow) {
	device.AutoBackup, device.AutoVerify = defaultAutoBackup, defaultAutoVerify
	for _, row := range schedules {
		if row.UDID != device.UDID {
			continue
		}
		switch row.Task {
		case taskBackup:
			device.AutoBackup = row.Schedule
		case taskVerify:
			device.AutoVerify = row.Schedule
		}
	}
}

type scheduleRow struct {
	UDID string       `db:"udid"`
	Task scheduleTask `db:"task"`
	model.Schedule
}
