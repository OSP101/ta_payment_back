package service

// The appointment order (คำสั่งแต่งตั้งผู้ช่วยสอน) is what makes a TA's work
// official. A lecturer's request being approved in the app is an internal
// decision; the printed, signed order is the document the faculty and the
// finance office act on. Until it exists there is nothing to pay against, so the
// payout-review and export screens must not offer the course at all — a course
// that reaches export without an order produces a package the finance office
// will reject, after staff have already done the work.
//
// appointment_order_items is the record of a printed order: AppointmentOrderService
// writes it in the same call that renders the document, precisely so a produced
// document is never unrecorded (see recordRound). That makes "a row exists" and
// "it was printed" the same statement, and this file the single place that says
// so — every screen gated on it must use these helpers rather than write the
// EXISTS by hand, or the two menus will disagree about which courses are live.

// AppointedSQL tests that one (course, TA) pair appears in a printed appointment
// order. Pair-level rather than course-level because that is what the document
// actually asserts: a TA added to a course after the order was printed is not
// appointed yet, even though their colleagues on the same course are.
//
// Takes column expressions so callers can correlate on whatever aliases they
// have (`tc.id`, `a.ta_id`, …).
func AppointedSQL(courseCol, taCol string) string {
	return `EXISTS (
		SELECT 1 FROM appointment_order_items aoi
		 WHERE aoi.teaching_course_id = ` + courseCol + `
		   AND aoi.ta_id = ` + taCol + `
	)`
}

// CourseAppointedSQL tests that a course has any TA on a printed order. Used
// where the row is a course rather than a (course, TA) pair — the exports
// dashboard lists courses, and a course with at least one appointed TA has
// something to export.
func CourseAppointedSQL(courseCol string) string {
	return `EXISTS (
		SELECT 1 FROM appointment_order_items aoi
		 WHERE aoi.teaching_course_id = ` + courseCol + `
	)`
}
