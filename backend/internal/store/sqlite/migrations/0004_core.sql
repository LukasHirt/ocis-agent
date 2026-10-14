CREATE TABLE users (
    user_id           TEXT PRIMARY KEY,
    user_name         TEXT    NOT NULL,
    personal_drive_id TEXT    NOT NULL,
    enrolled_at       INTEGER NOT NULL,
    last_seen_at      INTEGER NOT NULL
);

CREATE TABLE policies (
    user_id            TEXT PRIMARY KEY REFERENCES users(user_id) ON DELETE CASCADE,
    paused             INTEGER NOT NULL DEFAULT 0,
    pause_reason       TEXT    NOT NULL DEFAULT '',
    auto_approve_write INTEGER NOT NULL DEFAULT 0,
    proactive_enabled  INTEGER NOT NULL DEFAULT 0,
    watch              TEXT    NOT NULL DEFAULT '[]',
    sweep_interval_s   INTEGER NOT NULL DEFAULT 21600,
    quiet_hours        TEXT    NOT NULL DEFAULT '',
    daily_tokens       INTEGER NOT NULL DEFAULT 200000,
    dormancy_days      INTEGER NOT NULL DEFAULT 30,
    updated_at         INTEGER NOT NULL
);

CREATE TABLE app_tokens (
    user_id    TEXT PRIMARY KEY REFERENCES users(user_id) ON DELETE CASCADE,
    ciphertext BLOB    NOT NULL,
    nonce      BLOB    NOT NULL,
    dek_id     TEXT    NOT NULL REFERENCES deks(dek_id),
    label      TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    status     TEXT    NOT NULL CHECK (status IN ('valid', 'invalid', 'revoked'))
);

CREATE TABLE app_token_labels (
    user_id    TEXT    NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    label      TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    deleted_at INTEGER,
    PRIMARY KEY (user_id, label)
);
