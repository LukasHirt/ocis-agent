CREATE TABLE conversations (
    id         TEXT PRIMARY KEY,
    user_id    TEXT    NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    title      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- serves the list (updated_at DESC, id DESC) and its keyset cursor
CREATE INDEX conversations_user ON conversations(user_id, updated_at, id);

CREATE TABLE messages (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT    NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    user_id         TEXT    NOT NULL,
    role            TEXT    NOT NULL CHECK (role IN ('user', 'assistant', 'system')),
    content         TEXT    NOT NULL,
    job_id          TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL
);

CREATE INDEX messages_conversation ON messages(conversation_id, created_at, id);
-- the change marks: newest message of a user
CREATE INDEX messages_user ON messages(user_id, created_at);
-- the change marks: newest job update of a user
CREATE INDEX jobs_user_updated ON jobs(user_id, updated_at);
