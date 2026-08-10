// course_group.go proposes and confirms course_groups: teaching_courses that
// are the same class taught under more than one registrar code (a curriculum
// reorganisation leaves the old and new code both running against the same
// lecturer, room and time slot — see migration 0073's comment for the real
// example this was built from, CP353301/SC313302 "Internetworking").
//
// The two payout documents in the staff interview (สรุปรายวิชาที่ขอใช้ TA and
// ปะหน้าจ่ายตรง) print such a group as ONE row with its money added together,
// never once per code. Detection only ever PROPOSES a group — nothing here
// writes to course_groups. A group is not used by an export until a human
// has confirmed it via ConfirmCourseGroup, because a wrong merge means a
// student count (and therefore a budget figure) gets counted for the wrong
// course.
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// CourseGroupCourseInfo is one course inside a candidate or confirmed group.
type CourseGroupCourseInfo struct {
	ID         uuid.UUID `json:"id"`
	Code       string    `json:"code"`
	NameTH     string    `json:"name_th"`
	Curriculum string    `json:"curriculum"`
}

// CourseGroupCandidate is a proposed merge, for staff to review before
// ConfirmCourseGroup writes it. SuggestedPrimaryID defaults to the course
// whose curriculum sorts first in the curricula table (ties broken by course
// code) — staff can pick a different member as primary when confirming.
type CourseGroupCandidate struct {
	Courses            []CourseGroupCourseInfo `json:"courses"`
	SuggestedPrimaryID uuid.UUID               `json:"suggested_primary_id"`
	// Reason is a Thai sentence a reviewing officer can read directly — the
	// shared name plus the day/time/room the match was found on.
	Reason string `json:"reason"`
}

type scheduleFingerprint struct {
	dayOfWeek int
	startTime string
	endTime   string
	kind      string
	room      string
}

func (f scheduleFingerprint) key() string {
	return fmt.Sprintf("%d|%s|%s|%s|%s", f.dayOfWeek, f.startTime, f.endTime, f.kind, f.room)
}

// sectionSignature collapses one section's schedule rows into a single
// string that is identical for two sections meeting at the same day, time,
// kind and room — the college's own "สอน วัน-เวลา เดียวกัน" test. Row order
// does not matter (a section's lecture-then-lab and lab-then-lecture are the
// same section), so the parts are sorted before joining.
func sectionSignature(rows []scheduleFingerprint) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = r.key()
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// DetectCourseGroups proposes merges among this term's teaching_courses that
// (a) are not already in a course_groups row for this term, (b) share the
// same name_th, and (c) have at least one section apiece whose schedule
// signature is identical. Requiring only ONE matching section pair (not
// every section) matters in practice: CP353301's regular section matches
// SC313302's regular section exactly, but their special sections run at
// different times — the college's own file still merges the course, going by
// the regular sections alone.
func (s *TeachingService) DetectCourseGroups(ctx context.Context, termID uuid.UUID) ([]CourseGroupCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tc.id, tc.code, tc.name_th,
		       COALESCE((SELECT s.curriculum FROM sections s
		                 WHERE s.teaching_course_id = tc.id AND s.curriculum IS NOT NULL
		                 ORDER BY s.sec_no LIMIT 1), '') AS curriculum,
		       sec.id AS section_id,
		       ss.day_of_week, ss.start_time::text, ss.end_time::text, ss.kind,
		       COALESCE(ss.room, '') AS room
		FROM teaching_courses tc
		JOIN sections sec           ON sec.teaching_course_id = tc.id
		JOIN section_schedules ss   ON ss.section_id = sec.id
		WHERE tc.term_id = $1
		  AND NOT EXISTS (
		      SELECT 1 FROM course_group_members m WHERE m.teaching_course_id = tc.id)
		ORDER BY tc.code`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type courseInfo struct {
		info       CourseGroupCourseInfo
		sectionSig map[uuid.UUID][]scheduleFingerprint // section_id -> its schedule rows
	}
	courses := map[uuid.UUID]*courseInfo{}
	var order []uuid.UUID

	for rows.Next() {
		var tcID, secID uuid.UUID
		var code, nameTH, curriculum, startTime, endTime, kind, room string
		var dow int
		if err := rows.Scan(&tcID, &code, &nameTH, &curriculum, &secID,
			&dow, &startTime, &endTime, &kind, &room); err != nil {
			return nil, err
		}
		c, ok := courses[tcID]
		if !ok {
			c = &courseInfo{
				info:       CourseGroupCourseInfo{ID: tcID, Code: code, NameTH: nameTH, Curriculum: curriculum},
				sectionSig: map[uuid.UUID][]scheduleFingerprint{},
			}
			courses[tcID] = c
			order = append(order, tcID)
		}
		c.sectionSig[secID] = append(c.sectionSig[secID],
			scheduleFingerprint{dayOfWeek: dow, startTime: startTime, endTime: endTime, kind: kind, room: room})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// signature -> course ids that have a section with that exact signature.
	bySignature := map[string][]uuid.UUID{}
	for _, tcID := range order {
		seen := map[string]bool{} // a course listed once per distinct signature, even with several matching sections
		for _, sig := range courses[tcID].sectionSig {
			key := sectionSignature(sig)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			bySignature[key] = append(bySignature[key], tcID)
		}
	}

	// Union-find over courses connected by a shared signature AND a shared
	// name — two different classes that happen to share a room and hour by
	// coincidence must not merge just because of the room/time match alone.
	parent := map[uuid.UUID]uuid.UUID{}
	var find func(uuid.UUID) uuid.UUID
	find = func(x uuid.UUID) uuid.UUID {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b uuid.UUID) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for _, tcID := range order {
		parent[tcID] = tcID
	}
	matchReason := map[[2]uuid.UUID]string{}
	for sig, ids := range bySignature {
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				a, b := ids[i], ids[j]
				if courses[a].info.NameTH != courses[b].info.NameTH {
					continue
				}
				union(a, b)
				pair := [2]uuid.UUID{a, b}
				if _, ok := matchReason[pair]; !ok {
					matchReason[pair] = sig
				}
			}
		}
	}

	groups := map[uuid.UUID][]uuid.UUID{}
	for _, tcID := range order {
		root := find(tcID)
		groups[root] = append(groups[root], tcID)
	}

	var candidates []CourseGroupCandidate
	for _, tcID := range order { // stable order
		root := find(tcID)
		if root != tcID {
			continue
		}
		members := groups[root]
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(i, j int) bool {
			return courses[members[i]].info.Code < courses[members[j]].info.Code
		})
		infos := make([]CourseGroupCourseInfo, len(members))
		for i, id := range members {
			infos[i] = courses[id].info
		}
		candidates = append(candidates, CourseGroupCandidate{
			Courses:            infos,
			SuggestedPrimaryID: infos[0].ID,
			Reason: fmt.Sprintf("ชื่อวิชาเดียวกัน (%s) และมีตารางสอนตรงกัน (%s)",
				courses[members[0]].info.NameTH, describeSignature(matchReason, members)),
		})
	}
	return candidates, nil
}

// describeSignature renders the FIRST matching schedule signature found
// between any two members, as "วัน X เวลา start-end ห้อง room" — good enough
// for a reviewer to sanity-check the merge without re-deriving it themselves.
func describeSignature(matchReason map[[2]uuid.UUID]string, members []uuid.UUID) string {
	dayNames := [...]string{"อา.", "จ.", "อ.", "พ.", "พฤ.", "ศ.", "ส."}
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			if sig, ok := matchReason[[2]uuid.UUID{members[i], members[j]}]; ok {
				parts := strings.Split(strings.Split(sig, ";")[0], "|")
				if len(parts) != 5 {
					return sig
				}
				var dow int
				fmt.Sscanf(parts[0], "%d", &dow)
				dayLabel := ""
				if dow >= 0 && dow < len(dayNames) {
					dayLabel = dayNames[dow]
				}
				return fmt.Sprintf("%s %s-%s ห้อง %s", dayLabel, parts[1], parts[2], parts[4])
			}
		}
	}
	return ""
}

// ConfirmCourseGroup writes a staff-confirmed merge. primaryID must be one of
// courseIDs — enforced here because the schema (course_group_members has no
// way to express "this member is also the primary") can't. curriculumCode
// decides which sheet the merged row prints under in the export documents;
// it need not match any single member's own derived curriculum (a group can
// legitimately span two curricula, as CS+IT does in the college's own file).
func (s *TeachingService) ConfirmCourseGroup(
	ctx context.Context, actor, termID, primaryID uuid.UUID, courseIDs []uuid.UUID, curriculumCode string,
) (uuid.UUID, error) {
	isPrimaryMember := false
	for _, id := range courseIDs {
		if id == primaryID {
			isPrimaryMember = true
			break
		}
	}
	if !isPrimaryMember {
		return uuid.Nil, Invalid("รายวิชาหลักต้องเป็นหนึ่งในรายวิชาที่รวมกลุ่ม")
	}
	if len(courseIDs) < 2 {
		return uuid.Nil, Invalid("ต้องเลือกอย่างน้อย 2 รายวิชาเพื่อรวมกลุ่ม")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	groupID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO course_groups (id, term_id, primary_course_id, curriculum_code, confirmed_by, confirmed_at)
		VALUES ($1, $2, $3, $4, $5, now())`,
		groupID, termID, primaryID, curriculumCode, actor); err != nil {
		return uuid.Nil, err
	}
	for _, id := range courseIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO course_group_members (course_group_id, teaching_course_id) VALUES ($1, $2)`,
			groupID, id); err != nil {
			// The unique index on teaching_course_id is what actually stops a
			// course joining two groups — surface it as a normal validation
			// error rather than a raw constraint-violation message.
			return uuid.Nil, Invalid("มีรายวิชาที่เลือกอยู่ในกลุ่มอื่นแล้ว")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return groupID, nil
}

// ConfirmedCourseGroup is one staff-confirmed merge, as the export builders
// need it: which courses feed one printed row, which of them is primary
// (decides the printed code/name/lecturer), and which sheet it prints under.
type ConfirmedCourseGroup struct {
	PrimaryCourseID uuid.UUID
	CurriculumCode  string
	MemberCourseIDs []uuid.UUID
}

// ListConfirmedCourseGroups returns every confirmed group for a term, keyed
// by EVERY member course id (including the primary) so a caller iterating
// teaching_courses can look up "is this course part of a group, and if so
// which" with a single map lookup, instead of re-deriving membership itself.
func (s *TeachingService) ListConfirmedCourseGroups(ctx context.Context, termID uuid.UUID) (map[uuid.UUID]*ConfirmedCourseGroup, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.id, g.primary_course_id, g.curriculum_code, m.teaching_course_id
		FROM course_groups g
		JOIN course_group_members m ON m.course_group_id = g.id
		WHERE g.term_id = $1 AND g.confirmed_at IS NOT NULL
		ORDER BY g.id`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := map[uuid.UUID]*ConfirmedCourseGroup{} // group id -> group
	byMember := map[uuid.UUID]*ConfirmedCourseGroup{}
	for rows.Next() {
		var groupID, primaryID, memberID uuid.UUID
		var curriculumCode string
		if err := rows.Scan(&groupID, &primaryID, &curriculumCode, &memberID); err != nil {
			return nil, err
		}
		g, ok := groups[groupID]
		if !ok {
			g = &ConfirmedCourseGroup{PrimaryCourseID: primaryID, CurriculumCode: curriculumCode}
			groups[groupID] = g
		}
		g.MemberCourseIDs = append(g.MemberCourseIDs, memberID)
		byMember[memberID] = g
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return byMember, nil
}
