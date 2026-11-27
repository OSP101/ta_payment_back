-- Read-only MariaDB mirror metadata. Upstream identifiers are not local FKs.
ALTER TABLE tdbm_extra_teachings
 ADD COLUMN source_teaching_id INT,
 ADD COLUMN source_class_id INT,
 ADD COLUMN source_holiday_id INT,
 ADD COLUMN source_original_date DATE,
 ADD COLUMN source_original_start TIME,
 ADD COLUMN source_original_end TIME,
 ADD COLUMN source_original_kind TEXT,
 ADD COLUMN source_latest_status_id INT,
 ADD COLUMN source_updated_at TIMESTAMPTZ,
 ADD COLUMN source_status_updated_at TIMESTAMPTZ,
 ADD COLUMN source_missing BOOLEAN NOT NULL DEFAULT FALSE,
 ADD COLUMN reconciliation_error TEXT;
CREATE INDEX idx_tdbm_extra_original ON tdbm_extra_teachings (section_id, source_original_date, source_original_kind);
