-- the answer the model of a running job is writing (untrusted, transient:
-- every state transition deletes the row), what the model is doing ('' |
-- 'thinking' | 'writing') and when that was last written. A table of its own
-- so that a write never rewrites the jobs row (its checkpoint may be 1 MiB)
-- and the change mark is an index-only lookup.
CREATE TABLE job_partials (
    job_id  TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL,
    chat    INTEGER NOT NULL, -- the job is a chat job (trigger_kind 'chat')
    state   TEXT NOT NULL,
    text    TEXT NOT NULL,
    at      INTEGER NOT NULL
);
CREATE INDEX job_partials_user ON job_partials(user_id, chat, at);
