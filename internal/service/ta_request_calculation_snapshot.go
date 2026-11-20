package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// declaredWeeklyTotal is an eligibility total, not a payable-hours total.
// Keep each source section intact so a later policy can reinterpret it.
func declaredWeeklyTotal(ids []uuid.UUID, per map[uuid.UUID]WorkloadInput, level string) float64 {
	var total float64
	for _, id := range ids {
		total += weeklyWorkloadTotal(per[id], level)
	}
	return total
}

// snapshotRequestCalculation runs in the mutation's transaction: failure to
// preserve evidence also prevents the mutation. No deduplicated values replace
// original workloads. JSON schema version and rule identifiers make the stored
// inputs interpretable independently of future code or timetable/rate edits.
func snapshotRequestCalculation(ctx context.Context, tx pgx.Tx, requestID, actor uuid.UUID, event string) error {
	_, err := tx.Exec(ctx, `
        INSERT INTO ta_request_calculation_snapshots(request_id, actor_id, event, inputs)
        SELECT r.id, $2, $3, jsonb_build_object(
            'schema_version', 1,
            'workload_rule_version', r.workload_rule_version,
            'billing_rule', 'merged_sittings_by_track_activity_v1',
            'eligibility_limits', jsonb_build_object('graduate_min',10,'graduate_max',12,'undergraduate_min_exclusive',0),
            'request', to_jsonb(r),
            'course', (SELECT to_jsonb(tc) FROM teaching_courses tc WHERE tc.id=r.teaching_course_id),
            'term', (SELECT to_jsonb(t) FROM academic_terms t JOIN teaching_courses tc ON tc.term_id=t.id WHERE tc.id=r.teaching_course_id),
            'pay_rates', (SELECT to_jsonb(pay_rates) FROM `+payRatesInForce+`),
            'sections', COALESCE((
                SELECT jsonb_agg(jsonb_build_object(
                    'assignment', to_jsonb(a),
                    'workload', (SELECT to_jsonb(w) FROM ta_workload_forms w WHERE w.assignment_id=a.id),
                    'section', to_jsonb(sec),
                    'schedules', COALESCE((SELECT jsonb_agg(to_jsonb(ss) ORDER BY ss.day_of_week, ss.start_time, ss.id) FROM section_schedules ss WHERE ss.section_id=sec.id), '[]'::jsonb)
                ) ORDER BY a.ta_id, a.section_id)
                FROM ta_request_assignments a JOIN sections sec ON sec.id=a.section_id
                WHERE a.request_id=r.id
            ), '[]'::jsonb),
            'ta_class_schedules', COALESCE((
                SELECT jsonb_agg(to_jsonb(cs) ORDER BY cs.user_id, cs.id)
                FROM ta_class_schedules cs
                JOIN teaching_courses tc ON tc.id=r.teaching_course_id AND tc.term_id=cs.term_id
                WHERE cs.user_id IN (SELECT a.ta_id FROM ta_request_assignments a WHERE a.request_id=r.id)
            ), '[]'::jsonb)
        ) FROM ta_requests r WHERE r.id=$1`, requestID, actor, event)
	return err
}
