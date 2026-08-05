-- /ta/worklog (0062) was not the only dead one. A sweep of every literal link
-- the notify service sends, checked against the app router, found three more:
--
--   /lecturer/approvals        36 rows — sent to a lecturer every time a TA
--                              submitted hours. They approve on
--                              /lecturer/courses/{id}/reports.
--   /ta/courses                 4 rows — the TA's course list is /ta.
--   /lecturer/courses           — likewise, /lecturer.
--   /lecturer/courses/{id}/worklog — lecturers have no worklog page; the
--                              approval screen is .../reports, so those CAN be
--                              repaired exactly.
--
-- New notifications carry the right links (worklog.go, submission_period.go,
-- submission_review.go, ta_request_decide.go, worklog_edit_batch.go), and
-- notify_links_test.go now walks the app router and fails the build if a
-- literal link stops resolving.
--
-- The rows already delivered are pointed at the nearest page that exists. Only
-- the lecturer worklog→reports rewrite keeps its course; the rest lose a step,
-- because nothing on those rows records which course they were about.
UPDATE notifications
SET link = regexp_replace(link, '/worklog$', '/reports')
WHERE link LIKE '/lecturer/courses/%/worklog';

UPDATE notifications SET link = '/lecturer' WHERE link IN ('/lecturer/approvals', '/lecturer/courses');
UPDATE notifications SET link = '/ta'       WHERE link = '/ta/courses';
