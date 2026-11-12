package regkku

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"ta-payment-back/internal/telemetry"
	"time"
)

// DefaultAPIBase is the same guest API used by REG's replacement website.
const DefaultAPIBase = "https://reg4.kku.ac.th/regapiweb1/api/th"

// APIClient serializes ALL requests, including token refreshes and retries.
// It never switches API servers in response to rate limiting.
type APIClient struct {
	Base                    string
	HTTP                    *http.Client
	Gap, Jitter, MaxBackoff time.Duration
	mu                      sync.Mutex
	last                    time.Time
	cooldownUntil           time.Time
	lastAccess              *AccessError
	probing                 bool
	token                   string
	tokenUntil              time.Time
	levels                  []apiLevel
	levelsUntil             time.Time
}

func NewAPI(base string) *APIClient {
	if base == "" {
		base = DefaultAPIBase
	}
	return &APIClient{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Transport: telemetry.HTTPTransport{Integration: "reg-api"}, Timeout: 20 * time.Second}, Gap: time.Second, Jitter: 500 * time.Millisecond, MaxBackoff: time.Minute}
}

// apiInt accepts JSON's 31 and 31.0 without rounding identifiers or counts.
type apiInt int64

func (n *apiInt) UnmarshalJSON(b []byte) error {
	s := string(b)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		if strings.Trim(s[i+1:], "0") != "" {
			return ErrLayout
		}
		s = s[:i]
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return ErrLayout
	}
	*n = apiInt(v)
	return nil
}

type apiLevel struct {
	ID   apiInt `json:"comboid"`
	Name string `json:"comboshow"`
}
type apiClass struct {
	ID       apiInt  `json:"classid"`
	CourseID apiInt  `json:"courseid"`
	Year     apiInt  `json:"acadyear"`
	Semester apiInt  `json:"semester"`
	Code     string  `json:"coursecode"`
	Section  string  `json:"section"`
	Open     *apiInt `json:"totalseat"`
	Enrolled *apiInt `json:"enrollseat"`
}
type apiPage struct {
	Row     *apiInt     `json:"row"`
	Limit   *apiInt     `json:"limit"`
	Count   *apiInt     `json:"count"`
	Classes *[]apiClass `json:"classinfolist"`
}

const apiMaxBody = 8 << 20

func limitedRead(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, apiMaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > apiMaxBody {
		return nil, ErrLayout
	}
	return b, nil
}

// requestLocked implements the normal guest flow, with one token retry and
// one busy retry at most. The caller owns mu for the whole fetch operation.
func (c *APIClient) requestLocked(ctx context.Context, path string, guest bool, out any) error {
	refreshed, waited := false, false
	for {
		if !guest && (c.token == "" || time.Now().After(c.tokenUntil)) {
			var t struct {
				Token string `json:"token"`
			}
			if err := c.requestLocked(ctx, "Validate/tokenservice", true, &t); err != nil {
				return err
			}
			if t.Token == "" {
				return ErrLayout
			}
			c.token, c.tokenUntil = t.Token, time.Now().Add(45*time.Second)
		}
		wait := c.Gap - time.Since(c.last)
		if c.Jitter > 0 {
			wait += time.Duration(rand.Int64N(int64(c.Jitter)))
		}
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/"+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "COCO-TAS/1.0 (College of Computing KKU; enrolment check)")
		if !guest {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		res, err := c.HTTP.Do(req)
		c.last = time.Now()
		if err != nil {
			return err
		}
		body, readErr := limitedRead(res.Body)
		res.Body.Close()
		c.last = time.Now()
		if res.StatusCode == http.StatusUnauthorized && !guest && !refreshed {
			c.token = ""
			refreshed = true
			continue
		}
		if res.StatusCode == 429 || res.StatusCode == 503 {
			if waited {
				c.tokenUntil = time.Time{}
				c.cooldownUntil = time.Now().Add(c.MaxBackoff)
				c.lastAccess = &AccessError{Status: res.StatusCode, RetryAt: c.cooldownUntil}
				return c.lastAccess
			}
			delay := retryDelay(res.Header)
			if c.probing || delay > c.MaxBackoff {
				c.cooldownUntil = time.Now().Add(delay)
				c.lastAccess = &AccessError{Status: res.StatusCode, RetryAt: c.cooldownUntil}
				return c.lastAccess
			}
			if delay < 0 {
				delay = 0
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
			waited = true
			continue
		}
		if res.StatusCode != 200 {
			if res.StatusCode == 401 || res.StatusCode == 403 {
				c.cooldownUntil = time.Now().Add(c.MaxBackoff)
			}
			c.lastAccess = &AccessError{Status: res.StatusCode, RetryAt: c.cooldownUntil}
			return c.lastAccess
		}
		if readErr != nil {
			return readErr
		}
		var envelope struct {
			Result string `json:"result"`
		}
		if json.Unmarshal(body, &envelope) == nil && envelope.Result != "" {
			z, err := base64.StdEncoding.DecodeString(envelope.Result)
			if err != nil {
				return ErrLayout
			}
			reader, err := gzip.NewReader(bytes.NewReader(z))
			if err != nil {
				return ErrLayout
			}
			body, err = limitedRead(reader)
			reader.Close()
			if err != nil {
				return err
			}
		}
		if err := json.Unmarshal(body, out); err != nil {
			if challengePage(string(body)) {
				c.cooldownUntil = time.Now().Add(time.Minute)
				c.lastAccess = &AccessError{Status: 403, Challenge: true, RetryAt: c.cooldownUntil}
				return c.lastAccess
			}
			return ErrLayout
		}
		return nil
	}
}

func (c *APIClient) pages(ctx context.Context, code string, year, semester int, level apiInt) (map[apiInt]apiClass, error) {
	const size = 100
	classes := map[apiInt]apiClass{}
	total := -1
	for start := 1; start <= 10000; start += size {
		path := fmt.Sprintf("Classinfo/Classinfo/%d/%d/%d/%d/-9/%d/%s/null/1", start, size, year, semester, level, url.PathEscape(code))
		var pages []apiPage
		if err := c.requestLocked(ctx, path, false, &pages); err != nil {
			return nil, err
		}
		if len(pages) != 1 {
			return nil, ErrLayout
		}
		p := pages[0]
		if p.Row == nil || p.Limit == nil || p.Count == nil || p.Classes == nil || int(*p.Row) != start || int(*p.Limit) != size || *p.Count < 0 {
			return nil, ErrLayout
		}
		if total < 0 {
			total = int(*p.Count)
		}
		if int(*p.Count) != total || len(*p.Classes) > size {
			return nil, ErrLayout
		}
		for _, row := range *p.Classes {
			if int(row.Year) != year || int(row.Semester) != semester || row.ID <= 0 || row.CourseID <= 0 || row.Section == "" || row.Code == "" || row.Open == nil || row.Enrolled == nil || *row.Open < 0 || *row.Enrolled < 0 {
				return nil, ErrLayout
			}
			if _, duplicate := classes[row.ID]; duplicate {
				return nil, ErrLayout
			}
			classes[row.ID] = row
		}
		if start+size > total {
			if len(classes) != total {
				return nil, ErrLayout
			}
			return classes, nil
		}
		if len(*p.Classes) == 0 {
			return nil, ErrLayout
		}
	}
	return nil, ErrLayout
}

// Fetch searches a prefix or exact code, then uses level-filtered searches
// to classify the requested class IDs. No cached ASP track guesses are used.
// Unknown levels, incomplete pages and changed term/counts fail closed.
func (c *APIClient) Fetch(ctx context.Context, pattern string, year, semester int, wanted map[string]bool) (map[string][]Section, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.cooldownUntil) {
		if c.lastAccess != nil {
			return nil, c.lastAccess
		}
		return nil, ErrBusy
	}
	if year < 2500 || year > 3000 || semester < 1 || semester > 4 {
		return nil, ErrLayout
	}
	pattern = strings.TrimSuffix(strings.ToUpper(pattern), "*")
	if pattern == "" || strings.ContainsAny(pattern, "/?#") {
		return nil, ErrLayout
	}
	all, err := c.pages(ctx, pattern, year, semester, -9)
	if err != nil {
		return nil, err
	}
	needed := map[apiInt]apiClass{}
	for id, row := range all {
		if wanted[strings.ToUpper(row.Code)] {
			needed[id] = row
		}
	}
	out := map[string][]Section{}
	if len(needed) == 0 {
		return out, nil
	}
	if len(c.levels) == 0 || time.Now().After(c.levelsUntil) {
		var levels []apiLevel
		if err := c.requestLocked(ctx, "ComboLev/AllClassinfo", false, &levels); err != nil {
			return nil, err
		}
		if len(levels) == 0 {
			return nil, ErrLayout
		}
		c.levels, c.levelsUntil = levels, time.Now().Add(time.Hour)
	}
	levels := append([]apiLevel(nil), c.levels...)
	// Common tracks first; finish as soon as every requested class is placed.
	priority := map[apiInt]int{31: 1, 34: 2, 51: 3, 55: 4, 71: 5, 75: 6}
	sort.SliceStable(levels, func(i, j int) bool {
		a, b := priority[levels[i].ID], priority[levels[j].ID]
		if a == 0 {
			a = 100
		}
		if b == 0 {
			b = 100
		}
		return a < b
	})
	assigned := map[apiInt]bool{}
	for _, level := range levels {
		regular, special := strings.Contains(level.Name, "ปกติ"), strings.Contains(level.Name, "พิเศษ")
		if !regular && !special {
			continue
		} // cannot silently classify ambiguous levels
		rows, err := c.pages(ctx, pattern, year, semester, level.ID)
		if err != nil {
			return nil, err
		}
		for id, row := range rows {
			original, need := needed[id]
			if !need {
				continue
			}
			if assigned[id] || row.Code != original.Code || row.Section != original.Section || row.CourseID != original.CourseID || *row.Enrolled != *original.Enrolled || *row.Open != *original.Open {
				return nil, ErrLayout
			}
			assigned[id] = true
			code := strings.ToUpper(row.Code)
			out[code] = append(out[code], Section{No: row.Section, Level: level.Name, Special: special, Open: int(*row.Open), Enrolled: int(*row.Enrolled)})
		}
		if len(assigned) == len(needed) {
			for code := range out {
				sort.Slice(out[code], func(i, j int) bool { return out[code][i].No < out[code][j].No })
			}
			return out, nil
		}
	}
	// Only fully classified courses may survive an unsupported level in a
	// different course. Never return the partial total of the affected code.
	for id, row := range needed {
		if !assigned[id] {
			delete(out, strings.ToUpper(row.Code))
		}
	}
	if len(out) == 0 {
		return nil, ErrLayout
	}
	return out, ErrLayout
}
