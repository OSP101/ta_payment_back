package service

import "errors"

var (
	ErrNotFound     = errors.New("not found")
	ErrForbidden    = errors.New("forbidden")
	ErrConflict     = errors.New("conflict")
	ErrInvalidInput = errors.New("invalid input")
)

// UserError carries a message that is safe (and intended) to show to the end
// user, together with the HTTP status it should map to. Service methods return
// these for business-rule failures so the handler layer can surface the Thai
// message without leaking internal errors. Plain errors.New/fmt.Errorf values
// are also treated as user-facing 400s by the error handler, but UserError lets
// callers pick a more specific status (409/403/…).
type UserError struct {
	Status int
	Msg    string
}

func (e *UserError) Error() string { return e.Msg }

// Invalid builds a 400 user-facing error.
func Invalid(msg string) *UserError { return &UserError{Status: 400, Msg: msg} }

// Conflict builds a 409 user-facing error (state clash / duplicate).
func Conflict(msg string) *UserError { return &UserError{Status: 409, Msg: msg} }

// Forbidden builds a 403 user-facing error.
func Forbidden(msg string) *UserError { return &UserError{Status: 403, Msg: msg} }
