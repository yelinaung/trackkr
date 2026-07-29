CREATE TABLE site_icons (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    site         TEXT NOT NULL CHECK (
                     site <> '' AND octet_length(site) <= 253),
    png          BYTEA CHECK (png IS NULL OR octet_length(png) <= 65536),
    sha256       BYTEA CHECK (sha256 IS NULL OR octet_length(sha256) = 32),
    width        SMALLINT CHECK (width IS NULL OR width BETWEEN 1 AND 128),
    height       SMALLINT CHECK (height IS NULL OR height BETWEEN 1 AND 128),
    attempted_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    claim_until  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((png IS NULL) = (sha256 IS NULL)),
    CHECK ((png IS NULL) = (width IS NULL)),
    CHECK ((png IS NULL) = (height IS NULL)),
    UNIQUE (user_id, site)
);

CREATE INDEX idx_site_icons_user_expiry
    ON site_icons (user_id, expires_at, id);
