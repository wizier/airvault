-- +goose Up
-- Per-device automatic backups, off by default. The window is minutes after
-- local midnight in auto_backup_tz and may cross midnight; NULL means any time.
ALTER TABLE devices ADD COLUMN auto_backup INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN auto_backup_days INTEGER NOT NULL DEFAULT 1;
ALTER TABLE devices ADD COLUMN auto_backup_window_start INTEGER;
ALTER TABLE devices ADD COLUMN auto_backup_window_end INTEGER;
ALTER TABLE devices ADD COLUMN auto_backup_tz TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE devices DROP COLUMN auto_backup_tz;
ALTER TABLE devices DROP COLUMN auto_backup_window_end;
ALTER TABLE devices DROP COLUMN auto_backup_window_start;
ALTER TABLE devices DROP COLUMN auto_backup_days;
ALTER TABLE devices DROP COLUMN auto_backup;
