-- the user's acknowledgement of an approval whose outcome is uncertain
-- (NULL: not acknowledged); the state and result stay as they are
ALTER TABLE approvals ADD COLUMN acknowledged_at INTEGER;
ALTER TABLE approvals ADD COLUMN acknowledged_by TEXT NOT NULL DEFAULT '';
