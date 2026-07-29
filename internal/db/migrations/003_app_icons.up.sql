CREATE TABLE app_icons (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_key      TEXT NOT NULL CHECK (
                     app_key <> '' AND octet_length(app_key) <= 255),
    png          BYTEA NOT NULL CHECK (octet_length(png) <= 65536),
    sha256       BYTEA NOT NULL CHECK (octet_length(sha256) = 32),
    width        SMALLINT NOT NULL CHECK (width BETWEEN 1 AND 128),
    height       SMALLINT NOT NULL CHECK (height BETWEEN 1 AND 128),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, app_key)
);

CREATE INDEX idx_app_icons_user_last_seen
    ON app_icons (user_id, last_seen_at DESC, id DESC);
