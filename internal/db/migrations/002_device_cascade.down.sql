ALTER TABLE activity_records
    DROP CONSTRAINT activity_records_device_id_fkey;

ALTER TABLE activity_records
    ADD CONSTRAINT activity_records_device_id_fkey
    FOREIGN KEY (device_id) REFERENCES devices (id);
