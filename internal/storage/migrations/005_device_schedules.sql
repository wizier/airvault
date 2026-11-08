-- +goose Up
-- What a device does on a schedule, 'backup' or 'verify': every `days` days,
-- starting inside a window of minutes after midnight in the server's zone that may
-- cross midnight; NULL means any time. A device with no row for a task has the
-- task's default (see DeviceRepo).
CREATE TABLE device_schedules (
    udid         TEXT    NOT NULL REFERENCES devices(udid) ON DELETE CASCADE,
    task         TEXT    NOT NULL CHECK (task IN ('backup', 'verify')),
    enabled      INTEGER NOT NULL,
    days         INTEGER NOT NULL,
    window_start INTEGER,
    window_end   INTEGER,
    PRIMARY KEY (udid, task)
);

INSERT INTO device_schedules (udid, task, enabled, days, window_start, window_end)
SELECT udid, 'backup', auto_backup, auto_backup_days, auto_backup_window_start, auto_backup_window_end
FROM devices;

ALTER TABLE devices DROP COLUMN auto_backup_window_end;
ALTER TABLE devices DROP COLUMN auto_backup_window_start;
ALTER TABLE devices DROP COLUMN auto_backup_days;
ALTER TABLE devices DROP COLUMN auto_backup;

-- +goose Down
ALTER TABLE devices ADD COLUMN auto_backup INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN auto_backup_days INTEGER NOT NULL DEFAULT 1;
ALTER TABLE devices ADD COLUMN auto_backup_window_start INTEGER;
ALTER TABLE devices ADD COLUMN auto_backup_window_end INTEGER;

UPDATE devices SET (auto_backup, auto_backup_days, auto_backup_window_start, auto_backup_window_end) = (
    SELECT enabled, days, window_start, window_end FROM device_schedules
    WHERE device_schedules.udid = devices.udid AND task = 'backup'
) WHERE udid IN (SELECT udid FROM device_schedules WHERE task = 'backup');

DROP TABLE device_schedules;
