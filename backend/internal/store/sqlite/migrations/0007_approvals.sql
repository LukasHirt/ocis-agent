CREATE TABLE approvals (
    id           TEXT PRIMARY KEY,
    user_id      TEXT    NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    job_id       TEXT    NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    call_id      TEXT    NOT NULL,
    tool         TEXT    NOT NULL,
    risk         TEXT    NOT NULL CHECK (risk IN ('write', 'destructive', 'outward')),
    args         TEXT    NOT NULL,
    args_hash    TEXT    NOT NULL,
    target_etag  TEXT    NOT NULL DEFAULT '',
    rationale    TEXT    NOT NULL DEFAULT '',
    preview      TEXT    NOT NULL DEFAULT '{}',
    idem_key     TEXT    NOT NULL UNIQUE,
    state        TEXT    NOT NULL CHECK (state IN ('pending', 'approved', 'rejected', 'expired', 'executing', 'executed', 'failed', 'unknown')),
    requested_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    decided_by   TEXT    NOT NULL DEFAULT '',
    decided_at   INTEGER,
    executed_at  INTEGER,
    result       TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX approvals_user ON approvals(user_id, requested_at);
CREATE INDEX approvals_job ON approvals(job_id, state);
CREATE INDEX approvals_pending ON approvals(state, expires_at);
