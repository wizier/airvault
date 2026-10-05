-- +goose Up
-- Per-device cleanup of old backups, off by default: restore points of the
-- last cleanup_keep_days days, counted back from the latest backup, stay; older
-- ones thin to one per cleanup_thin ('week' or 'month') or go ('none').
ALTER TABLE devices ADD COLUMN cleanup INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN cleanup_keep_days INTEGER NOT NULL DEFAULT 14;
ALTER TABLE devices ADD COLUMN cleanup_thin TEXT NOT NULL DEFAULT 'month';

-- +goose Down
ALTER TABLE devices DROP COLUMN cleanup_thin;
ALTER TABLE devices DROP COLUMN cleanup_keep_days;
ALTER TABLE devices DROP COLUMN cleanup;
