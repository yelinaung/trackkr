-- Deleting a device that has ever reported activity fails without this:
-- the original foreign key has no ON DELETE action, so the device
-- management page cannot remove exactly the devices worth removing.
ALTER TABLE activity_records
    DROP CONSTRAINT activity_records_device_id_fkey;

ALTER TABLE activity_records
    ADD CONSTRAINT activity_records_device_id_fkey
    FOREIGN KEY (device_id) REFERENCES devices (id) ON DELETE CASCADE;
