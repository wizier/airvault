-- +goose Up
-- AirVault schema for fresh installs

CREATE TABLE devices (
    udid          TEXT    PRIMARY KEY NOT NULL,
    name          TEXT    NOT NULL DEFAULT '',
    product_type  TEXT    NOT NULL DEFAULT '',
    ios_version   TEXT    NOT NULL DEFAULT '',
    paired        INTEGER NOT NULL DEFAULT 0,
    encrypted     INTEGER NOT NULL DEFAULT 0,
    last_seen_at  INTEGER
);

-- Immutable restore points owned by the object store
CREATE TABLE backups (
    id                TEXT    PRIMARY KEY NOT NULL,   -- canonical lowercase UUID
    source_udid       TEXT    NOT NULL,
    size_bytes        INTEGER NOT NULL,
    transferred_bytes INTEGER,
    encrypted         INTEGER NOT NULL,
    device_name       TEXT NOT NULL DEFAULT '',
    product_type      TEXT NOT NULL DEFAULT '',
    ios_version       TEXT    NOT NULL,
    created_at        INTEGER NOT NULL,
    started_at        INTEGER
);
CREATE INDEX idx_backups_source ON backups(source_udid, created_at DESC, id DESC);

-- Rebuildable on-disk footprint per source
CREATE TABLE backup_sources (
    source_udid TEXT    PRIMARY KEY NOT NULL,
    disk_bytes  INTEGER NOT NULL CHECK (disk_bytes >= 0)
);

-- +goose Down
SELECT 'no down migration for the consolidated initial schema';
