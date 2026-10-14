CREATE TABLE deks (
    dek_id      TEXT PRIMARY KEY,
    wrapped_key BLOB    NOT NULL,
    nonce       BLOB    NOT NULL,
    created_at  INTEGER NOT NULL,
    retired_at  INTEGER
);
