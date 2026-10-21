-- the watch-folder poll: per folder what the last poll saw (JSON object),
-- and when a change no job has picked up yet was first and last seen
ALTER TABLE cursors ADD COLUMN watch_etags TEXT NOT NULL DEFAULT '{}';
ALTER TABLE cursors ADD COLUMN watch_dirty_since INTEGER;
ALTER TABLE cursors ADD COLUMN watch_changed_at INTEGER;
