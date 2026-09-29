// Package model holds SQLite rows. Times are unix seconds. Rows never define an
// HTTP contract; the service maps them into read models.
package model

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

// The window is minutes after local midnight in TimeZone and may cross
// midnight; nil means any time.
type AutoBackup struct {
	Enabled     bool   `db:"auto_backup"`
	Days        int    `db:"auto_backup_days"`
	WindowStart *int64 `db:"auto_backup_window_start"`
	WindowEnd   *int64 `db:"auto_backup_window_end"`
	TimeZone    string `db:"auto_backup_tz"`
}

// SourceUDID survives device deletion; CreatedAt is the authoritative time.
// StartedAt and TransferredBytes are nil for rows rebuilt from the manifest.
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
	// Damage is why the backup cannot be restored ("" when it can), as the last
	// collection pass found it.
	Damage       string `db:"damage"`
	DamagedFiles int    `db:"damaged_files"`
	VerifiedAt   *int64 `db:"verified_at"`
}
