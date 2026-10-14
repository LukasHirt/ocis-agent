CREATE TABLE cursors (
    user_id           TEXT PRIMARY KEY REFERENCES users(user_id) ON DELETE CASCADE,
    sse_last_event_id TEXT    NOT NULL DEFAULT '',
    last_sweep_at     INTEGER,
    memory_etags      TEXT    NOT NULL DEFAULT '{}'
);
