// Package model holds the plain data structures persisted in SQLite. Times are
// int64 unix seconds; nullable columns are *int64. These rows never define an
// HTTP contract; the service maps them into application read models.
package model

// Device is a known iPhone (row of the devices table, keyed by UDID).
type Device struct {
	UDID        string `db:"udid"`
	Name        string `db:"name"`
	ProductType string `db:"product_type"`
	IOSVersion  string `db:"ios_version"`
	Paired      bool   `db:"paired"`
	Encrypted   bool   `db:"encrypted"`
	LastSeenAt  *int64 `db:"last_seen_at"`
	AutoBackup
}

// AutoBackup is a device's automatic-backup settings. The window is minutes
// after local midnight in TimeZone and may cross midnight; nil means any time.
type AutoBackup struct {
	Enabled     bool   `db:"auto_backup"`
	Days        int    `db:"auto_backup_days"`
	WindowStart *int64 `db:"auto_backup_window_start"`
	WindowEnd   *int64 `db:"auto_backup_window_end"`
	TimeZone    string `db:"auto_backup_tz"`
}

// Backup is one independently restorable snapshot; SourceUDID survives device
// deletion and CreatedAt is the authoritative creation time. StartedAt and
// TransferredBytes are nil for rows rebuilt from the portable manifest.
type Backup struct {
	ID               string `db:"id"`
	SourceUDID       string `db:"source_udid"`
	SizeBytes        int64  `db:"size_bytes"`
	TransferredBytes *int64 `db:"transferred_bytes"`
	Encrypted        bool   `db:"encrypted"`
	DeviceName       string `db:"device_name"`
	ProductType      string `db:"product_type"`
	IOSVersion       string `db:"ios_version"`
	CreatedAt        int64  `db:"created_at"`
	StartedAt        *int64 `db:"started_at"`
}
