package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type WorkloadService struct {
	pool *pgxpool.Pool
}

// TA class schedule (drag-drop UI)
type ClassBlock struct {
	ID         uuid.UUID `json:"id"`
	TermID     uuid.UUID `json:"term_id"`
	CourseLabel string   `json:"course_label"`
	DayOfWeek  int       `json:"day_of_week"`
	StartTime  string    `json:"start_time"`
	EndTime    string    `json:"end_time"`
	Note       string    `json:"note"`
	IsWBA      bool      `json:"is_wba"`
}

func (s *WorkloadService) ListClasses(ctx context.Context, userID, termID uuid.UUID) ([]ClassBlock, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, term_id, COALESCE(course_label,''), day_of_week, start_time::text, end_time::text, COALESCE(note,''), is_wba
		 FROM ta_class_schedules WHERE user_id=$1 AND term_id=$2 ORDER BY day_of_week, start_time`,
		userID, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClassBlock
	for rows.Next() {
		var b ClassBlock
		if err := rows.Scan(&b.ID, &b.TermID, &b.CourseLabel, &b.DayOfWeek, &b.StartTime, &b.EndTime, &b.Note, &b.IsWBA); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// ReplaceClasses swaps the whole schedule for a term.
func (s *WorkloadService) ReplaceClasses(ctx context.Context, userID, termID uuid.UUID, blocks []ClassBlock) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`DELETE FROM ta_class_schedules WHERE user_id=$1 AND term_id=$2`, userID, termID); err != nil {
		return err
	}
	for _, b := range blocks {
		if b.DayOfWeek < 0 || b.DayOfWeek > 6 {
			return errors.New("invalid day_of_week")
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO ta_class_schedules (id, user_id, term_id, course_label, day_of_week, start_time, end_time, note, is_wba)
			 VALUES ($1,$2,$3,$4,$5,$6::time,$7::time,$8,$9)`,
			uuid.New(), userID, termID, b.CourseLabel, b.DayOfWeek, b.StartTime, b.EndTime, b.Note, b.IsWBA); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
