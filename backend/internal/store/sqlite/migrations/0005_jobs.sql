CREATE TABLE jobs (
    id           TEXT PRIMARY KEY,
    user_id      TEXT    NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    trigger_kind TEXT    NOT NULL,
    trigger_ref  TEXT    NOT NULL DEFAULT '',
    goal         TEXT    NOT NULL,
    state        TEXT    NOT NULL CHECK (state IN ('queued', 'running', 'waiting_approval', 'succeeded', 'failed', 'cancelled')),
    attempt      INTEGER NOT NULL DEFAULT 0,
    not_before   INTEGER NOT NULL,
    checkpoint   TEXT    NOT NULL DEFAULT '{}',
    error        TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE INDEX jobs_claim ON jobs(state, not_before, created_at);
CREATE INDEX jobs_user ON jobs(user_id, created_at);

CREATE TABLE job_steps (
    job_id      TEXT    NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    n           INTEGER NOT NULL,
    kind        TEXT    NOT NULL CHECK (kind IN ('llm', 'tool')),
    tool        TEXT    NOT NULL DEFAULT '',
    call_id     TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL,
    tokens_in   INTEGER NOT NULL DEFAULT 0,
    tokens_out  INTEGER NOT NULL DEFAULT 0,
    summary     TEXT    NOT NULL DEFAULT '',
    approval_id TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (job_id, n)
);

-- One row per user, kept for the life of the user so that fence only grows.
-- A blank holder / expires_at = 0 means "no active lease".
CREATE TABLE leases (
    user_id    TEXT PRIMARY KEY,
    holder     TEXT    NOT NULL,
    job_id     TEXT    NOT NULL DEFAULT '',
    fence      INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
