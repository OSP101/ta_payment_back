package regkku

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestDescribeAccess(t *testing.T) {
	for _, tc := range []struct {
		err   error
		state string
	}{{nil, "available"}, {&AccessError{Status: 403}, "blocked"}, {&AccessError{Status: 429}, "limited"}, {&AccessError{Status: 503}, "unavailable"}, {&AccessError{Status: 500}, "unavailable"}, {context.DeadlineExceeded, "timeout"}, {ErrLayout, "invalid_response"}} {
		got := DescribeAccess(SourceLegacy, tc.err)
		if got.State != tc.state || got.Available != (tc.err == nil) || got.Action == "" {
			t.Fatalf("got %+v for %v", got, tc.err)
		}
	}
}

func TestProbeHonorsRetryAfterWithoutWaitingOrHammering(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
		}))
		old, newAPI := New(srv.URL), NewAPI(srv.URL)
		old.Gap, old.Jitter, newAPI.Gap, newAPI.Jitter = 0, 0, 0, 0
		probe := newAPI.Probe
		if legacy {
			probe = old.Probe
		}
		start := time.Now()
		for i := 0; i < 2; i++ {
			err := probe(context.Background())
			got := DescribeAccess(SourceNew, err)
			if !errors.Is(err, ErrBusy) || got.RetryAt == nil || time.Until(*got.RetryAt) < 119*time.Second || got.State != "limited" {
				t.Fatalf("got %+v err=%v", got, err)
			}
		}
		if hits.Load() != 1 || time.Since(start) > time.Second {
			t.Fatalf("hits=%d duration=%v", hits.Load(), time.Since(start))
		}
		srv.Close()
	}
}

func TestLegacyRejectsWrongAcademicTerm(t *testing.T) {
	page := load(t, "class_info_1_SC363001.tis620.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(page)) }))
	defer srv.Close()
	c := New(srv.URL)
	c.Gap, c.Jitter = 0, 0
	if _, _, err := c.List(context.Background(), "SC363001", 2568, 1); !errors.Is(err, ErrLayout) {
		t.Fatalf("wrong year accepted: %v", err)
	}
	if _, _, err := c.List(context.Background(), "SC363001", 2569, 2); !errors.Is(err, ErrLayout) {
		t.Fatalf("wrong semester accepted: %v", err)
	}
}

func TestLegacyMirrorLive(t *testing.T) {
	if os.Getenv("RUN_REG_KKU_LIVE") != "1" {
		t.Skip("opt-in live read only")
	}
	c := New("")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := c.Fetch(ctx, "SC363001", 2569, 1, map[string]bool{"SC363001": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["SC363001"]) == 0 {
		t.Fatal("no sections")
	}
	t.Logf("mirror 2569/1 SC363001: %+v", got["SC363001"])
}

func TestAPIProbeLive(t *testing.T) {
	if os.Getenv("RUN_REG_KKU_LIVE") != "1" {
		t.Skip("opt-in live probe only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := NewAPI("").Probe(ctx); err != nil {
		t.Fatal(err)
	}
}
