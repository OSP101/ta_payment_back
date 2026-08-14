// Package handler's request bodies were, until this file, parsed with a bare
// c.BodyParser(&in) and nothing else — whatever JSON arrived became the
// struct, with no check that a required field was present, a string was
// within a sane length, or a code was one of the values the database (or the
// business rule two lines later) actually accepts. Malformed input mostly
// surfaced as a Postgres constraint violation, or a business-rule error from
// deep inside a service method — both work, but late, in a way that: (a)
// exercises a DB round trip for something that was never going to succeed,
// and (b) can produce a validation-adjacent report that reads as "this field
// is wrong" when it should read as "this field is missing".
//
// Bind is the fix: c.BodyParser(&in), then validator.Struct(&in) against the
// struct's own `validate:"..."` tags. It replaces the bodyparser call, not
// the business logic after it — a struct tag can express "not empty" or "one
// of these three codes", not "this course isn't over budget", so every
// service-layer check downstream is unchanged and still runs.
package handler

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
)

// validate is safe for concurrent use across requests — the package doc for
// go-playground/validator states struct validation itself does not mutate
// shared state, only the one-time tag/translator registration below does,
// and that runs once at package init.
var validate = validator.New()

func init() {
	// Field names in validation errors read as the JSON key ("email"), not
	// the Go struct field ("Email") — the caller sent JSON, so the error
	// should talk about the JSON they sent.
	validate.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "-" || name == "" {
			return f.Name
		}
		return name
	})
}

// Bind parses c's JSON body into dst and validates it against dst's
// `validate:"..."` struct tags in one step. Handlers should call this
// instead of c.BodyParser for any request body carrying user-editable
// fields — GET-only handlers and handlers with no body (id-only actions)
// have nothing to bind and don't need it.
//
// Both failure modes return the same 400 shape the rest of this codebase
// already uses ({"error": "..."}), so no frontend change was needed to
// surface either kind of rejection.
func Bind(c *fiber.Ctx, dst any) error {
	if err := c.BodyParser(dst); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := validate.Struct(dst); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, humanizeValidationError(err))
	}
	return nil
}

// humanizeValidationError turns the first failing field into a short,
// specific message ("email is required", "new_password must be at least 8
// characters") instead of validator's default Go-oriented dump. Only the
// first failure is reported — a form the user cannot see already told them
// nothing about which field, so naming the single most useful one beats a
// concatenated list they cannot map back to inputs.
func humanizeValidationError(err error) string {
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) || len(verrs) == 0 {
		return "invalid body"
	}
	e := verrs[0]
	field := e.Field()
	switch e.Tag() {
	case "required":
		return fmt.Sprintf("%s is required", field)
	case "min":
		if e.Kind().String() == "string" {
			return fmt.Sprintf("%s must be at least %s characters", field, e.Param())
		}
		return fmt.Sprintf("%s must be at least %s", field, e.Param())
	case "max":
		if e.Kind().String() == "string" {
			return fmt.Sprintf("%s must be at most %s characters", field, e.Param())
		}
		return fmt.Sprintf("%s must be at most %s", field, e.Param())
	case "email":
		return fmt.Sprintf("%s must be a valid email address", field)
	case "oneof":
		return fmt.Sprintf("%s must be one of: %s", field, e.Param())
	case "gte":
		return fmt.Sprintf("%s must be %s or greater", field, e.Param())
	case "lte":
		return fmt.Sprintf("%s must be %s or less", field, e.Param())
	case "gt":
		return fmt.Sprintf("%s must be greater than %s", field, e.Param())
	case "uuid4", "uuid":
		return fmt.Sprintf("%s must be a valid id", field)
	case "dive":
		return fmt.Sprintf("%s contains an invalid item", field)
	default:
		return fmt.Sprintf("%s is invalid", field)
	}
}
