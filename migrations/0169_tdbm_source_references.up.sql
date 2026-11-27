ALTER TABLE tdbm_extra_teachings
 ADD COLUMN source_document_id INT,
 ADD COLUMN source_duration_minutes INT,
 ADD COLUMN source_reference BOOLEAN NOT NULL DEFAULT FALSE;
