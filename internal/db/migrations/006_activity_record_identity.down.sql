-- Restoring (device_id, started_at) can fail where two records legitimately
-- share that pair, which is the defect migration 006 exists to fix. Drop the
-- duplicates that only the new identity allowed, keeping the oldest row.
DELETE FROM activity_records a
USING activity_records b
WHERE a.device_id = b.device_id
  AND a.started_at = b.started_at
  AND a.id > b.id;

DROP INDEX IF EXISTS idx_activity_records_producer;

ALTER TABLE activity_records
    DROP CONSTRAINT activity_records_device_id_record_id_key;

ALTER TABLE activity_records
    ADD CONSTRAINT activity_records_device_id_started_at_key
        UNIQUE (device_id, started_at);

ALTER TABLE activity_records
    DROP CONSTRAINT activity_records_producer_known,
    DROP COLUMN producer,
    DROP COLUMN record_id;
