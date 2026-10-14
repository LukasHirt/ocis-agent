CREATE TABLE idempotency (
    idem_key    TEXT PRIMARY KEY,
    user_id     TEXT    NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    tool        TEXT    NOT NULL,
    state       TEXT    NOT NULL CHECK (state IN ('started', 'done', 'failed')),
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    result      TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idempotency_started ON idempotency(state, started_at);
