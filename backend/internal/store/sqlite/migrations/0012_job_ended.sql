-- the time a job reached a terminal state (0 while it is live); jobs that
-- ended before this column existed take their last change
ALTER TABLE jobs ADD COLUMN ended_at INTEGER NOT NULL DEFAULT 0;
UPDATE jobs SET ended_at = updated_at WHERE state IN ('succeeded', 'failed', 'cancelled');
