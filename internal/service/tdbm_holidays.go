package service

import (
	"context"
)

// Reconcile removed/deactivated holidays only inside the successfully read
// source term window. Preserve manual entries and closed financial evidence.
func (s *TDBMService) removeAbsentHolidays(ctx context.Context, start, end string, ids []int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id,holiday_date::text FROM public_holidays
	 WHERE source='tdbm' AND tdbm_holiday_id IS NOT NULL AND holiday_date BETWEEN $1::date AND $2::date
	 AND NOT(tdbm_holiday_id=ANY($3)) FOR UPDATE`, start, end, ids)
	if err != nil {
		return err
	}
	type holiday struct {
		id   string
		date string
	}
	var removed []holiday
	for rows.Next() {
		var h holiday
		if err = rows.Scan(&h.id, &h.date); err != nil {
			rows.Close()
			return err
		}
		removed = append(removed, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, h := range removed {
		var protected bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_logs WHERE work_date=$1::date AND status IN ('submitted','approved'))
		 OR EXISTS(SELECT 1 FROM submission_period_status st JOIN submission_periods sp ON sp.id=st.submission_period_id
		 JOIN academic_terms term ON term.id=sp.term_id
		 WHERE st.status IN ('exported','finance_sent') AND $1::date BETWEEN term.starts_on AND term.ends_on
		 AND sp.year_month=term.academic_year::text||'-'||to_char($1::date,'MM'))`, h.date).Scan(&protected)
		if err != nil {
			return err
		}
		if protected {
			return Conflict(tdbmReconcileConflict)
		}
		if _, err = tx.Exec(ctx, `DELETE FROM public_holidays WHERE id=$1`, h.id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
