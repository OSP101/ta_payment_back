DO $$ DECLARE r record; BEGIN
 FOR r IN SELECT event_object_table FROM information_schema.triggers WHERE trigger_name='edit_resource_changed' LOOP
  EXECUTE format('DROP TRIGGER IF EXISTS edit_resource_changed ON %I',r.event_object_table);
 END LOOP;
END $$;
DROP FUNCTION track_edit_resource();
DROP TABLE edit_commands;
DROP TABLE edit_resource_versions;
