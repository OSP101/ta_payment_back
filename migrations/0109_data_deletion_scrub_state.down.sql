ALTER TABLE data_deletion_requests
  DROP COLUMN scrub_completed_at,
  DROP COLUMN scrub_error,
  DROP COLUMN scrub_attempts,
  DROP COLUMN avatar_key_to_scrub;
