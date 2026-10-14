CREATE TABLE audit (
    user_id   TEXT    NOT NULL,
    seq       INTEGER NOT NULL,
    ts        INTEGER NOT NULL,
    job_id    TEXT    NOT NULL DEFAULT '',
    kind      TEXT    NOT NULL,
    tool      TEXT    NOT NULL DEFAULT '',
    risk      TEXT    NOT NULL DEFAULT '',
    args_hash TEXT    NOT NULL DEFAULT '',
    cred      TEXT    NOT NULL DEFAULT '',
    outcome   TEXT    NOT NULL DEFAULT '',
    detail    TEXT    NOT NULL DEFAULT '',
    prev_hash TEXT    NOT NULL,
    hash      TEXT    NOT NULL,
    PRIMARY KEY (user_id, seq)
);

CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit
BEGIN
    SELECT RAISE(ABORT, 'audit is append-only');
END;

CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit
BEGIN
    SELECT RAISE(ABORT, 'audit is append-only');
END;
