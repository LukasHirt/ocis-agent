-- the live-job counts (proactive eligibility, the status view) by user and
-- state, without reading every job the user ever had
CREATE INDEX jobs_user_state ON jobs(user_id, state, trigger_kind);
