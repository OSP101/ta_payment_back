package regkku

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// API fixtures intentionally use .0 for IDs and counts, as REG does.
func apiTestRow(id int) map[string]any {
	return map[string]any{"classid": float64(id), "courseid": 3613669428.0, "acadyear": 2569, "semester": 2, "coursecode": "CP001001", "section": strconv.Itoa(id), "totalseat": 100.0, "enrollseat": 10.0}
}

func TestAPI_PaginationTracksAndGzip(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		enc := json.NewEncoder(w)
		if r.URL.Path == "/Validate/tokenservice" {
			enc.Encode(map[string]string{"token": "guest"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer guest" {
			t.Error("missing guest bearer")
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/ComboLev/AllClassinfo" {
			enc.Encode([]map[string]any{{"comboid": 31.0, "comboshow": "ตรี ภาคปกติ"}, {"comboid": 34.0, "comboshow": "ตรี โครงการพิเศษ"}})
			return
		}
		p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(p) != 11 || p[4] != "2569" || p[5] != "2" {
			t.Error("wrong query", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		var rows []map[string]any
		for id := 1; id <= 101; id++ {
			if p[7] == "31" && id > 51 || p[7] == "34" && id <= 51 {
				continue
			}
			rows = append(rows, apiTestRow(id))
		}
		start, _ := strconv.Atoi(p[2])
		total := len(rows)
		data, _ := json.Marshal([]map[string]any{{"row": start, "limit": 100, "count": total, "classinfolist": rows[start-1 : min(start+99, total)]}})
		var buf bytes.Buffer
		z := gzip.NewWriter(&buf)
		z.Write(data)
		z.Close()
		enc.Encode(map[string]string{"result": base64.StdEncoding.EncodeToString(buf.Bytes())})
	}))
	defer srv.Close()
	c := NewAPI(srv.URL)
	c.Gap = 0
	c.Jitter = 0
	got, err := c.Fetch(context.Background(), "CP*", 2569, 2, map[string]bool{"CP001001": true})
	if err != nil {
		t.Fatal(err)
	}
	reg, spc := 0, 0
	for _, sec := range got["CP001001"] {
		if sec.Special {
			spc += sec.Enrolled
		} else {
			reg += sec.Enrolled
		}
	}
	if reg != 510 || spc != 500 || calls.Load() != 6 {
		t.Fatalf("regular=%d special=%d calls=%d", reg, spc, calls.Load())
	}
}

func TestAPI_RejectsIncompleteOrChangedData(t *testing.T) {
	for _, scenario := range []string{"wrong-year", "wrong-semester", "count-mismatch", "duplicate-id", "missing-count", "fractional-count", "missing-enrolled", "changed-snapshot", "unknown-level"} {
		t.Run(scenario, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				enc := json.NewEncoder(w)
				if r.URL.Path == "/Validate/tokenservice" {
					enc.Encode(map[string]string{"token": "guest"})
					return
				}
				if r.URL.Path == "/ComboLev/AllClassinfo" {
					name := "ตรี ภาคปกติ"
					if scenario == "unknown-level" {
						name = "ตรี ภาษาอังกฤษ"
					}
					enc.Encode([]map[string]any{{"comboid": 31, "comboshow": name}})
					return
				}
				row := apiTestRow(1)
				count := 1
				switch scenario {
				case "wrong-year":
					row["acadyear"] = 2568
				case "wrong-semester":
					row["semester"] = 1
				case "count-mismatch":
					count = 2
				case "fractional-count":
					row["enrollseat"] = 1.5
				case "missing-enrolled":
					delete(row, "enrollseat")
				case "changed-snapshot":
					if strings.Contains(r.URL.Path, "/-9/31/") {
						row["enrollseat"] = 11
					}
				}
				rows := []map[string]any{row}
				if scenario == "duplicate-id" {
					rows = append(rows, row)
					count = 2
				}
				page := map[string]any{"row": 1, "limit": 100, "count": count, "classinfolist": rows}
				if scenario == "missing-count" {
					delete(page, "count")
				}
				enc.Encode([]map[string]any{page})
			}))
			defer srv.Close()
			c := NewAPI(srv.URL)
			c.Gap = 0
			c.Jitter = 0
			got, err := c.Fetch(context.Background(), "CP001001", 2569, 2, map[string]bool{"CP001001": true})
			if !errors.Is(err, ErrLayout) || got != nil {
				t.Fatalf("got=%v err=%v", got, err)
			}
		})
	}
}

func TestAPI_TokenRefreshAndRateLimit(t *testing.T) {
	for _, scenario := range []string{"refresh", "busy", "forbidden"} {
		t.Run(scenario, func(t *testing.T) {
			var tokens, data atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/Validate/tokenservice" {
					json.NewEncoder(w).Encode(map[string]string{"token": fmt.Sprint(tokens.Add(1))})
					return
				}
				n := data.Add(1)
				if scenario == "busy" {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(429)
					return
				}
				if scenario == "forbidden" {
					w.WriteHeader(403)
					return
				}
				if n == 1 {
					w.WriteHeader(401)
					return
				}
				if r.Header.Get("Authorization") != "Bearer 2" {
					t.Error("did not refresh token")
				}
				json.NewEncoder(w).Encode([]map[string]any{{"row": 1, "limit": 100, "count": 0, "classinfolist": []any{}}})
			}))
			defer srv.Close()
			c := NewAPI(srv.URL)
			c.Gap = 0
			c.Jitter = 0
			_, err := c.Fetch(context.Background(), "CP001001", 2569, 2, map[string]bool{"CP001001": true})
			if scenario == "refresh" && (err != nil || tokens.Load() != 2 || data.Load() != 2) {
				t.Fatalf("err=%v tokens=%d data=%d", err, tokens.Load(), data.Load())
			}
			if scenario == "busy" && (!errors.Is(err, ErrBusy) || data.Load() != 2) {
				t.Fatalf("err=%v data=%d", err, data.Load())
			}
			if scenario == "forbidden" && (err == nil || data.Load() != 1) {
				t.Fatalf("err=%v data=%d", err, data.Load())
			}
		})
	}
}

func TestAPI_PacingCancellation(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		json.NewEncoder(w).Encode(map[string]string{"token": "guest"})
	}))
	defer srv.Close()
	c := NewAPI(srv.URL)
	c.Gap = time.Second
	c.Jitter = 0
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Fetch(ctx, "CP001001", 2569, 2, map[string]bool{"CP001001": true})
	if !errors.Is(err, context.DeadlineExceeded) || hits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestAPI_UnsupportedLevelDoesNotReturnPartialCourseTotal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		if r.URL.Path == "/Validate/tokenservice" {
			enc.Encode(map[string]string{"token": "guest"})
			return
		}
		if r.URL.Path == "/ComboLev/AllClassinfo" {
			enc.Encode([]map[string]any{{"comboid": 31, "comboshow": "ตรี ภาคปกติ"}, {"comboid": 37, "comboshow": "ตรี ภาษาอังกฤษ"}})
			return
		}
		rows := []map[string]any{apiTestRow(1), apiTestRow(2), apiTestRow(3)}
		rows[1]["coursecode"], rows[2]["coursecode"] = "CP002002", "CP002002"
		if strings.Contains(r.URL.Path, "/-9/31/") {
			rows = rows[:2] // CP002002's third class has an ambiguous level.
		}
		enc.Encode([]map[string]any{{"row": 1, "limit": 100, "count": len(rows), "classinfolist": rows}})
	}))
	defer srv.Close()
	c := NewAPI(srv.URL)
	c.Gap, c.Jitter = 0, 0
	got, err := c.Fetch(context.Background(), "CP*", 2569, 2, map[string]bool{"CP001001": true, "CP002002": true})
	if !errors.Is(err, ErrLayout) || len(got["CP001001"]) != 1 || len(got["CP002002"]) != 0 {
		t.Fatalf("partial course leaked: got=%+v err=%v", got, err)
	}
}

// Explicit opt-in only: ordinary tests never send traffic to the registrar.
func TestAPI_Live(t *testing.T) {
	if os.Getenv("RUN_REG_KKU_LIVE") != "1" {
		t.Skip("set RUN_REG_KKU_LIVE=1 for paced live read")
	}
	c := NewAPI("")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	got, err := c.Fetch(ctx, "CP001001", 2569, 2, map[string]bool{"CP001001": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["CP001001"]) == 0 {
		t.Fatal("no classes")
	}
	regular, special := 0, 0
	for _, s := range got["CP001001"] {
		if s.Special {
			special += s.Enrolled
		} else {
			regular += s.Enrolled
		}
	}
	t.Logf("2569/2 CP001001: %d classes, regular=%d special=%d", len(got["CP001001"]), regular, special)
}
