package service

import (
	"github.com/google/uuid"
	"ta-payment-back/internal/audit"
	"testing"
)

func TestREGTrustedLecturerValidation(t *testing.T) {
	cs, _, _, _ := parseRawRows(regReviewRows())
	a, b := uuid.New(), uuid.New()
	for _, tc := range []struct {
		name              string
		selected, trusted []uuid.UUID
		wantWarning       bool
	}{
		{"exact single", []uuid.UUID{a}, []uuid.UUID{a}, false},
		{"exact ordered team", []uuid.UUID{a, b}, []uuid.UUID{a, b}, false},
		{"primary changed", []uuid.UUID{b, a}, []uuid.UUID{a, b}, true},
		{"teacher replaced", []uuid.UUID{b}, []uuid.UUID{a}, true},
		{"teacher removed", []uuid.UUID{a}, []uuid.UUID{a, b}, true},
		{"teacher duplicated", []uuid.UUID{a, a}, []uuid.UUID{a}, true},
		{"no verified lookup", []uuid.UUID{a}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newREGReview(cs, []byte("file"))
			r.Courses[0].LecturerIDs = tc.selected
			r.Courses[0].LecturerMatchVerified = true // untrusted client metadata alone cannot waive review
			trusted := map[string][]uuid.UUID{r.Courses[0].Code: tc.trusted}
			validateREGReviewWithTrustedMatches(r, trusted)
			found := false
			for _, i := range r.Issues {
				if i.Kind == "lecturer_match" {
					found = true
				}
			}
			if found != tc.wantWarning {
				t.Fatalf("lecturer warning = %v, want %v", found, tc.wantWarning)
			}
			if !tc.wantWarning && !r.Ready {
				t.Fatalf("exact source match still requires acknowledgement: %+v", r.Issues)
			}
			r.Courses[0].Sections[0].Curriculum = ""
			validateREGReviewWithTrustedMatches(r, trusted)
			if r.Ready {
				t.Fatal("trusted teacher match waived an unrelated required field")
			}
		})
	}
}

func TestREGSourceTrustRequiresCompleteNames(t *testing.T) {
	a := uuid.New()
	for _, tc := range []struct {
		raw       string
		unmatched []string
		want      bool
	}{
		{"Exact", nil, true}, {"อาจารย์ Exact", nil, true},
		{"Exact Typo", []string{"Typo"}, false}, {"Exact และคณะ", nil, false},
		{"Exact อจ.พิเศษ", nil, false}, {"", nil, false},
	} {
		if got := trustedREGSourceLecturers(tc.raw, []uuid.UUID{a}, tc.unmatched); got != tc.want {
			t.Fatalf("%q: got %v", tc.raw, got)
		}
	}
	if trustedREGSourceLecturers("Unknown", nil, nil) {
		t.Fatal("empty lookup marked trusted")
	}
}

func TestREGExactLecturerMatchNeedsNoAcknowledgement(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	svc := &TeachingService{pool: f.Pool, aud: audit.New(f.Pool)}
	actor := f.insertUser("admin", "admin")
	exact := lecturerNamed(f, "ExactTeacher", "UniqueFamily")
	lecturerNamed(f, "DuplicateTeacher", "FamilyOne")
	lecturerNamed(f, "DuplicateTeacher", "FamilyTwo")
	inactive := lecturerNamed(f, "InactiveTeacher", "InactiveFamily")
	f.exec(`UPDATE users SET is_active=false WHERE id=$1`, inactive)
	for _, tc := range []struct {
		name, raw string
		verified  bool
	}{
		{"exact", "ExactTeacher", true}, {"duplicate", "DuplicateTeacher", false},
		{"misspelled", "ExactTeacherr", false}, {"partial", "ExactTeacher MissingTeacher", false},
		{"unnamed team", "ExactTeacher และคณะ", false}, {"inactive", "InactiveTeacher", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := regReviewRows()
			rows[3][13] = tc.raw
			rows[4][13] = tc.raw
			body := buildREGXlsx(t, rows)
			p, err := svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, nil)
			if err != nil {
				t.Fatal(err)
			}
			r := p.Review
			if r.Courses[0].LecturerMatchVerified != tc.verified {
				t.Fatalf("unexpected trust for %q: %+v", tc.raw, r.Issues)
			}
			if !tc.verified {
				if r.Ready {
					t.Fatalf("uncertain names silently accepted: %q", tc.raw)
				}
				// A forged metadata flag and a valid but unrelated account cannot bypass review.
				r.Courses[0].LecturerMatchVerified = true
				r.Courses[0].LecturerIDs = []uuid.UUID{f.LecturerID}
				p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
				if err != nil {
					t.Fatal(err)
				}
				if p.Review.Courses[0].LecturerMatchVerified || p.Review.Ready {
					t.Fatal("client trust flag was accepted")
				}
				return
			}
			if !r.Ready || len(r.Acknowledged) != 0 || len(r.Issues) != 0 {
				t.Fatalf("exact match still creates work: %+v", r.Issues)
			}
			if r.Courses[0].LecturerIDs[0] != exact {
				t.Fatal("wrong unique lecturer selected")
			}
			// Replacing the primary teacher is a new decision, requiring review again.
			r.Courses[0].LecturerIDs = []uuid.UUID{f.LecturerID}
			p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, r)
			if err != nil {
				t.Fatal(err)
			}
			if p.Review.Courses[0].LecturerMatchVerified || p.Review.Ready {
				t.Fatal("manual override incorrectly retained source trust")
			}
		})
	}
	// A fresh exact preview can be committed without any per-teacher acknowledgement.
	rows := regReviewRows()
	rows[3][13] = "ExactTeacher"
	rows[4][13] = "ExactTeacher"
	body := buildREGXlsx(t, rows)
	p, err := svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Review.Confirmed = true
	duplicate := lecturerNamed(f, "ExactTeacher", "NewNamesake")
	if _, err = svc.CommitREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, p.Review); err == nil {
		t.Fatal("source became ambiguous after preview but commit was accepted")
	}
	f.exec(`UPDATE users SET is_active=false WHERE id=$1`, duplicate)
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Review.Confirmed = true
	f.exec(`UPDATE users SET is_active=false WHERE id=$1`, exact)
	if _, err = svc.CommitREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, p.Review); err == nil {
		t.Fatal("lecturer was deactivated after preview but commit was accepted")
	}
	f.exec(`UPDATE users SET is_active=true WHERE id=$1`, exact)
	p, err = svc.PreviewREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Review.Confirmed = true
	if _, err = svc.CommitREGReview(f.ctx, actor, f.TermID, "reg.xlsx", body, p.Review); err != nil {
		t.Fatalf("exact source match commit required extra acknowledgement: %v", err)
	}
}
