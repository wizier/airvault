-- +goose Up
-- A restore point's health, derived from the object store by each collection
-- pass: damage names why it cannot be restored ('' when it can), verified_at is
-- when an integrity check last read every object it needs.
ALTER TABLE backups ADD COLUMN damage TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN damaged_files INTEGER NOT NULL DEFAULT 0;
ALTER TABLE backups ADD COLUMN verified_at INTEGER;

-- +goose Down
ALTER TABLE backups DROP COLUMN verified_at;
ALTER TABLE backups DROP COLUMN damaged_files;
ALTER TABLE backups DROP COLUMN damage;
