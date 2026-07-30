-- Replace (device_id, started_at) as the record identity.
--
-- That pair cannot tell two real records apart: a desktop observation and a
-- browser observation of the same instant collide, and so do two browser
-- windows that become active together. Losing one to ON CONFLICT DO NOTHING
-- looks like the record was accepted.

ALTER TABLE activity_records
    ADD COLUMN record_id UUID,
    ADD COLUMN producer   TEXT;

-- Backfill before any constraint applies. gen_random_uuid() is pgcrypto in
-- older servers but core since Postgres 13, and this project pins 18.
UPDATE activity_records SET record_id = gen_random_uuid() WHERE record_id IS NULL;

-- A URL means a browser reported it, and Firefox was the only browser that
-- could until now. Everything else came from the native tracker.
UPDATE activity_records
SET producer = CASE
        WHEN url IS NOT NULL AND url <> '' AND lower(app_name) = 'firefox' THEN 'firefox'
        ELSE 'desktop'
    END
WHERE producer IS NULL;

ALTER TABLE activity_records
    ALTER COLUMN record_id SET NOT NULL,
    ALTER COLUMN producer  SET NOT NULL,
    ADD CONSTRAINT activity_records_producer_known
        CHECK (producer IN ('desktop', 'firefox', 'chrome'));

-- Only now that every row has an identity is it safe to drop the old one.
ALTER TABLE activity_records
    DROP CONSTRAINT activity_records_device_id_started_at_key;

ALTER TABLE activity_records
    ADD CONSTRAINT activity_records_device_id_record_id_key
        UNIQUE (device_id, record_id);

-- Deduplication and totals filter on the producer for a device's day.
CREATE INDEX idx_activity_records_producer ON activity_records (device_id, producer);
