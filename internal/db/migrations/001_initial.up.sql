CREATE TABLE users (
    id          BIGSERIAL PRIMARY KEY,
    username    TEXT NOT NULL UNIQUE,
    password    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE devices (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id),
    name        TEXT NOT NULL,
    device_type TEXT NOT NULL DEFAULT 'desktop',
    api_key     TEXT NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE activity_records (
    id          BIGSERIAL PRIMARY KEY,
    device_id   BIGINT NOT NULL REFERENCES devices(id),
    app_name    TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    url         TEXT,
    started_at  TIMESTAMPTZ NOT NULL,
    ended_at    TIMESTAMPTZ NOT NULL,
    duration_s  INTEGER NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_activity_records_device_started ON activity_records (device_id, started_at);
CREATE INDEX idx_activity_records_started ON activity_records (started_at);
