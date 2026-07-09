package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// authz.go centralises object-level access checks that must hold at the service
// layer. Route-level RBAC only proves "this user has role X"; it does not prove
// "this lecturer owns this course" or "this TA owns this assignment". Those
// object-level checks live here so every mutating path can enforce them
// consistently and IDOR bugs cannot creep back in per-endpoint.

// lecturerOwnsCourse reports whether actor teaches the given teaching course.
func lecturerOwnsCourse(ctx context.Context, pool *pgxpool.Pool, actor, tcID uuid.UUID) (bool, error) {
	var ok bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM teaching_lecturers WHERE teaching_course_id = $1 AND lecturer_id = $2)`,
		tcID, actor).Scan(&ok)
	return ok, err
}

// hasRole reports whether the actor holds any of the given role codes.
func hasRole(ctx context.Context, pool *pgxpool.Pool, actor uuid.UUID, roles ...string) (bool, error) {
	var ok bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM user_roles WHERE user_id = $1 AND role::text = ANY($2))`,
		actor, roles).Scan(&ok)
	return ok, err
}

// isPrivileged reports whether the actor is an admin or staff member — the two
// roles allowed to manage any course regardless of ownership.
func isPrivileged(ctx context.Context, pool *pgxpool.Pool, actor uuid.UUID) (bool, error) {
	return hasRole(ctx, pool, actor, "admin", "staff")
}

// assertCourseManager allows the action when the actor is privileged
// (admin/staff) or teaches the course. Returns ErrForbidden otherwise.
func assertCourseManager(ctx context.Context, pool *pgxpool.Pool, actor, tcID uuid.UUID) error {
	priv, err := isPrivileged(ctx, pool, actor)
	if err != nil {
		return err
	}
	if priv {
		return nil
	}
	owns, err := lecturerOwnsCourse(ctx, pool, actor, tcID)
	if err != nil {
		return err
	}
	if !owns {
		return ErrForbidden
	}
	return nil
}

// assignmentContext resolves the parent teaching course, request status and the
// owning TA for a work-log assignment.
type assignmentContext struct {
	TeachingCourseID uuid.UUID
	TAID             uuid.UUID
	RequestStatus    string
}

func loadAssignmentContext(ctx context.Context, pool *pgxpool.Pool, assignmentID uuid.UUID) (*assignmentContext, error) {
	var ac assignmentContext
	err := pool.QueryRow(ctx, `
		SELECT sec.teaching_course_id, a.ta_id, r.status::text
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN ta_requests r ON r.id = a.request_id
		WHERE a.id = $1`, assignmentID).Scan(&ac.TeachingCourseID, &ac.TAID, &ac.RequestStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &ac, nil
}
