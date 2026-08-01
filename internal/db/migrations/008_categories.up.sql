CREATE TABLE categories (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    color_key   TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (id, user_id),
    CHECK (name = REGEXP_REPLACE(BTRIM(name), '[[:space:]]+', ' ', 'g')),
    CHECK (CHAR_LENGTH(name) BETWEEN 1 AND 64),
    CHECK (color_key IN (
        'coral', 'amber', 'leaf', 'teal',
        'sky', 'indigo', 'rose', 'slate'
    ))
);

CREATE UNIQUE INDEX categories_user_name_unique
    ON categories (user_id, LOWER(name));

CREATE TABLE application_category_assignments (
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_key     TEXT NOT NULL,
    category_id BIGINT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, app_key),
    FOREIGN KEY (category_id, user_id)
        REFERENCES categories(id, user_id) ON DELETE CASCADE,
    CHECK (
        app_key = LOWER(
            REGEXP_REPLACE(BTRIM(app_key), '[[:space:]]+', ' ', 'g')
        )
    ),
    CHECK (OCTET_LENGTH(app_key) BETWEEN 1 AND 255)
);

CREATE INDEX application_category_assignments_category
    ON application_category_assignments (category_id);

CREATE TABLE activity_record_category_overrides (
    activity_record_id BIGINT PRIMARY KEY,
    user_id            BIGINT NOT NULL,
    category_id        BIGINT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (activity_record_id)
        REFERENCES activity_records(id) ON DELETE CASCADE,
    FOREIGN KEY (category_id, user_id)
        REFERENCES categories(id, user_id) ON DELETE CASCADE
);

CREATE INDEX activity_record_category_overrides_category
    ON activity_record_category_overrides (category_id)
    WHERE category_id IS NOT NULL;

CREATE INDEX idx_activity_records_device_ended_app
    ON activity_records (device_id, ended_at DESC, id DESC)
    INCLUDE (producer, app_name);
