ALTER TABLE activity_records
    ADD CONSTRAINT activity_records_positive_interval
    CHECK (ended_at > started_at) NOT VALID;
