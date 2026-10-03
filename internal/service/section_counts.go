package service

import (
	"context"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SectionCount is one registrar section's enrolment under one code.
type SectionCount struct {
	Code    string `json:"code,omitempty"`
	SecNo   string `json:"sec_no"`
	Special bool   `json:"special"`
	Count   int    `json:"count"`
}

type courseSection struct {
	id         uuid.UUID
	secNo      string
	track      string
	courseCode string // set on a section opened under an alternate code
	students   int
}

// syncSectionCounts makes the sections add up to the course's new track
// totals. Setting the real enrolment used to change only the course row, so
// the course read 85 while its sections still summed the file's 95 seats
// (SC362005, 03/10/2026) — and the TA planner and recommendation read the
// sections.
//
// With the registrar's per-section numbers (the REG fetch), each lands on its
// section: a code's sec N is the course's sec N of the same track, an
// alternate code's section folds where the import folded it (mergeCodeTx's
// rule). Without them (an Excel paste), or when any of them finds no section,
// each track's total is spread over its sections in proportion to what they
// hold now.
func syncSectionCounts(ctx context.Context, tx pgx.Tx, courseID uuid.UUID, regular, special int, regs []SectionCount) error {
	var primary string
	if err := tx.QueryRow(ctx, `SELECT UPPER(code) FROM teaching_courses WHERE id = $1`, courseID).Scan(&primary); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT id, sec_no, track::text, COALESCE(UPPER(course_code), ''), num_students
		  FROM sections WHERE teaching_course_id = $1 ORDER BY sec_no`, courseID)
	if err != nil {
		return err
	}
	var secs []courseSection
	for rows.Next() {
		var c courseSection
		if err := rows.Scan(&c.id, &c.secNo, &c.track, &c.courseCode, &c.students); err != nil {
			rows.Close()
			return err
		}
		secs = append(secs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(secs) == 0 {
		return nil
	}

	want := map[uuid.UUID]int{}
	exact := len(regs) > 0
	if exact {
		for _, r := range regs {
			sec, ok := matchRegSection(secs, primary, r)
			if !ok {
				exact = false
				break
			}
			want[sec] += r.Count
		}
	}
	if exact {
		// Only trust the per-section map when it accounts for exactly the
		// totals being saved — otherwise the two disagree and the totals win.
		sum := map[string]int{}
		for _, c := range secs {
			sum[c.track] += want[c.id]
		}
		exact = sum["regular"] == regular && sum["special"] == special
	}
	if !exact {
		want = map[uuid.UUID]int{}
		for track, total := range map[string]int{"regular": regular, "special": special} {
			for id, n := range spreadByWeight(secs, track, total) {
				want[id] = n
			}
		}
	}
	for _, c := range secs {
		n, ok := want[c.id]
		if !ok || n == c.students {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE sections SET num_students = $2 WHERE id = $1`, c.id, n); err != nil {
			return err
		}
	}
	return nil
}

func secNum(s string) string {
	if i := strings.LastIndex(s, "-"); i >= 0 {
		s = s[i+1:]
	}
	if t := strings.TrimLeft(s, "0"); t != "" {
		return t
	}
	return s
}

// matchRegSection finds the course section a registrar section belongs to.
func matchRegSection(secs []courseSection, primary string, r SectionCount) (uuid.UUID, bool) {
	track := "regular"
	if r.Special {
		track = "special"
	}
	code := strings.ToUpper(r.Code)
	num := secNum(r.SecNo)
	// A section still open separately under this alternate code.
	if code != "" && code != primary {
		for _, c := range secs {
			if c.courseCode == code && c.track == track && secNum(c.secNo) == num {
				return c.id, true
			}
		}
	}
	var only *courseSection
	n := 0
	for i := range secs {
		c := &secs[i]
		if c.courseCode != "" || c.track != track {
			continue
		}
		if secNum(c.secNo) == num {
			return c.id, true
		}
		only, n = c, n+1
	}
	// mergeCodeTx's fallback: the course's only section of that track.
	if code != primary && n == 1 {
		return only.id, true
	}
	return uuid.Nil, false
}

// spreadByWeight splits total over the track's sections in proportion to what
// each holds now (equally when none holds any), largest remainder first so the
// parts add up exactly.
func spreadByWeight(secs []courseSection, track string, total int) map[uuid.UUID]int {
	var idx []int
	weight := 0
	for i, c := range secs {
		if c.track == track {
			idx = append(idx, i)
			weight += c.students
		}
	}
	out := map[uuid.UUID]int{}
	if len(idx) == 0 {
		return out
	}
	type part struct {
		id  uuid.UUID
		rem float64
	}
	parts := make([]part, 0, len(idx))
	given := 0
	for _, i := range idx {
		w := float64(secs[i].students)
		if weight == 0 {
			w = 1
		}
		den := float64(weight)
		if weight == 0 {
			den = float64(len(idx))
		}
		exact := float64(total) * w / den
		n := int(exact)
		out[secs[i].id] = n
		given += n
		parts = append(parts, part{secs[i].id, exact - float64(n)})
	}
	sort.SliceStable(parts, func(a, b int) bool { return parts[a].rem > parts[b].rem })
	for i := 0; given < total; i++ {
		out[parts[i%len(parts)].id]++
		given++
	}
	return out
}
