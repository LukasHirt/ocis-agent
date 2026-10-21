-- the answer the model of a running job is writing (untrusted, transient:
-- every state transition clears it), what the model is doing ('' |
-- 'thinking' | 'writing') and when that was last written (0 when cleared)
ALTER TABLE jobs ADD COLUMN partial TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN partial_state TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN partial_at INTEGER NOT NULL DEFAULT 0;
