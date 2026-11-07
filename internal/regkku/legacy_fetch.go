package regkku

import (
	"context"
	"errors"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var searchTerm = regexp.MustCompile(`ปีการศึกษา\s+(\d{4})\s*/\s*(\d+)`)
var detailTerm = regexp.MustCompile(`เลือก\s*ปีการศึกษา\s*:\s*(\d+)\s*/\s*(\d{4})`)
var stripTags = regexp.MustCompile(`<[^>]*>`)

func validateLegacyTerm(body, path string, q url.Values) error {
	year, semester := q.Get("Acadyear"), q.Get("Semester")
	if year == "" {
		year = q.Get("acadyear")
	}
	if semester == "" {
		semester = q.Get("semester")
	}
	if path == "class_info_1.asp" {
		m := searchTerm.FindStringSubmatch(body)
		if len(m) != 3 || m[1] != year || m[2] != semester {
			return ErrLayout
		}
	}
	if path == "class_info_2.asp" {
		m := detailTerm.FindStringSubmatch(strings.Join(strings.Fields(html.UnescapeString(stripTags.ReplaceAllString(body, ""))), " "))
		if len(m) != 3 || m[1] != semester || m[2] != year {
			return ErrLayout
		}
	}
	return nil
}

// Fetch uses the mirror's existing search and detail pages. Details are read
// per course revision to get authoritative tracks rather than guessed ones.
func (c *Client) Fetch(ctx context.Context, pattern string, year, semester int, wanted map[string]bool) (map[string][]Section, error) {
	if year < 2500 || year > 3000 || semester < 1 || semester > 4 {
		return nil, ErrLayout
	}
	found := map[string]*Course{}
	if strings.HasSuffix(pattern, "*") {
		var err error
		found, err = c.Search(ctx, pattern, CollegeOfComputing, year, semester, 20)
		if err != nil {
			return nil, err
		}
	}
	var codes []string
	for code := range wanted {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	out := map[string][]Section{}
	for _, code := range codes {
		course := found[code]
		if course == nil {
			id, listed, err := c.List(ctx, code, year, semester)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			course = &Course{ID: id, Sections: listed}
		}
		byRevision := map[string]map[string]Section{}
		seen := map[string]bool{}
		for _, row := range course.Sections {
			id := row.CourseID
			if id == "" {
				id = course.ID
			}
			if byRevision[id] == nil {
				sections, err := c.Sections(ctx, id, code, year, semester)
				if err != nil {
					return nil, err
				}
				byRevision[id] = map[string]Section{}
				for _, sec := range sections {
					no := strings.TrimLeft(sec.No, "0")
					if no == "" || (!strings.Contains(sec.Level, "ปกติ") && !strings.Contains(sec.Level, "พิเศษ")) || sec.Open < 0 || sec.Enrolled < 0 {
						return nil, ErrLayout
					}
					if _, duplicate := byRevision[id][no]; duplicate {
						return nil, ErrLayout
					}
					sec.No = no
					byRevision[id][no] = sec
				}
			}
			key := id + "/" + row.No
			sec, ok := byRevision[id][row.No]
			if !ok || seen[key] || sec.Open != row.Open || sec.Enrolled != row.Enrolled {
				return nil, ErrLayout
			}
			seen[key] = true
			out[code] = append(out[code], sec)
		}
	}
	return out, nil
}

// Keep query casing consistent with the legacy form, including next pages.
func legacyTermQuery(q url.Values, year, semester int) {
	q.Set("Acadyear", strconv.Itoa(year))
	q.Set("Semester", strconv.Itoa(semester))
}
