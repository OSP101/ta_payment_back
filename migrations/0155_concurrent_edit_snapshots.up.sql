-- PostgreSQL transaction snapshots let a reader prove that the resource has
-- not changed since it was read, including changes made by background jobs.
CREATE TABLE edit_resource_versions (
 scope TEXT PRIMARY KEY,
 changed_by XID8 NOT NULL
);
CREATE TABLE edit_commands (
 actor_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 command_id UUID NOT NULL,
 method TEXT NOT NULL,
 path TEXT NOT NULL,
 completed BOOLEAN NOT NULL DEFAULT false,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(actor_id,command_id)
);
CREATE INDEX edit_commands_created_idx ON edit_commands(created_at);

CREATE FUNCTION track_edit_resource() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 v jsonb; course_key uuid; person_key uuid; term_key uuid;
 scopes text[]; k text; row_id text; namespace text;
BEGIN
 IF TG_OP='DELETE' THEN v:=to_jsonb(OLD); ELSE v:=to_jsonb(NEW); END IF;
 course_key:=COALESCE(NULLIF(v->>'teaching_course_id',''),NULLIF(v->>'course_id',''))::uuid;
 person_key:=COALESCE(NULLIF(v->>'user_id',''),NULLIF(v->>'ta_id',''))::uuid;
 term_key:=NULLIF(v->>'term_id','')::uuid;
 IF TG_TABLE_NAME='teaching_courses' THEN course_key:=(v->>'id')::uuid; END IF;
 IF TG_TABLE_NAME='users' THEN person_key:=(v->>'id')::uuid; END IF;
 IF TG_TABLE_NAME='academic_terms' THEN term_key:=(v->>'id')::uuid; END IF;
 IF course_key IS NULL AND v ? 'section_id' THEN SELECT teaching_course_id INTO course_key FROM sections WHERE id=(v->>'section_id')::uuid; END IF;
 IF course_key IS NULL AND v ? 'request_id' THEN SELECT teaching_course_id INTO course_key FROM ta_requests WHERE id=(v->>'request_id')::uuid; END IF;
 IF course_key IS NULL AND v ? 'assignment_id' THEN
  SELECT sec.teaching_course_id,a.ta_id INTO course_key,person_key FROM ta_request_assignments a JOIN sections sec ON sec.id=a.section_id WHERE a.id=(v->>'assignment_id')::uuid;
 END IF;
 -- Course work does not revise the TA's identity/profile form.
 IF course_key IS NOT NULL THEN person_key:=NULL; END IF;
 scopes:=ARRAY[]::text[];
 IF course_key IS NOT NULL THEN scopes:=array_append(scopes,'course:'||course_key::text); END IF;
 IF person_key IS NOT NULL THEN scopes:=array_append(scopes,'user:'||person_key::text); END IF;
 -- A term's own configuration is distinct from individual course edits.
 IF term_key IS NOT NULL AND course_key IS NULL AND person_key IS NULL THEN scopes:=array_append(scopes,'term:'||term_key::text); END IF;
 IF TG_TABLE_NAME='announcements' THEN scopes:=array_append(scopes,'announcement:'||(v->>'id')); END IF;
 IF TG_TABLE_NAME='curricula' THEN scopes:=array_append(scopes,'curriculum:'||(v->>'code')); END IF;
 IF TG_TABLE_NAME IN ('admin_officers','ta_request_windows') THEN scopes:=array_append(scopes,'table:'||TG_TABLE_NAME); END IF;
 IF cardinality(scopes)=0 THEN scopes:=ARRAY['table:'||TG_TABLE_NAME]; END IF;
 FOR k IN SELECT DISTINCT s FROM unnest(scopes) s ORDER BY s LOOP
  INSERT INTO edit_resource_versions(scope,changed_by) VALUES(k,pg_current_xact_id())
  ON CONFLICT(scope) DO UPDATE SET changed_by=EXCLUDED.changed_by;
 END LOOP;
 RETURN NULL;
END $$;

DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY[
 'users','user_roles','ta_enrollments','ta_profiles','ta_documents','ta_class_schedules',
 'teaching_courses','teaching_lecturers','sections','section_schedules','makeup_schedules','lecture_review_dates',
 'ta_requests','ta_request_assignments','ta_request_counts','ta_workload_forms','ta_review_schedules','ta_request_drafts',
 'work_logs','academic_terms','submission_periods','submission_period_status','pay_rates','hour_caps','budget_caps',
 'curricula','course_groups','course_group_members','public_holidays','admin_officers','mail_settings',
 'announcements','announcement_recipients','announcement_attachments','announcement_backgrounds',
 'appointment_orders','appointment_order_items','document_progress','signature_checklist','document_progress_share_links',
 'export_batches','transfer_cover_exports','course_summary_exports','export_month_ledger','grad_lump_ledger','monthly_payout_batches','monthly_payout_signers',
 'data_deletion_requests','ta_request_windows'
 ] LOOP
  IF to_regclass(t) IS NOT NULL THEN
   EXECUTE format('CREATE TRIGGER edit_resource_changed AFTER INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION track_edit_resource()',t);
  END IF;
 END LOOP;
END $$;
