package maker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestReadPhaseReuseAndIsolation(t *testing.T) {
	c := &Client{}
	ctx := c.BeginReadPhase(context.Background())
	count := 0
	fetch := func() ([]byte, error) { count++; return []byte(fmt.Sprint(count)), nil }
	first, err := c.readPhaseGet(ctx, "/org/products/id", fetch)
	if err != nil {
		t.Fatal(err)
	}
	first[0] = 'x'
	second, err := c.readPhaseGet(c.BeginReadPhase(ctx), "/org/products/id", fetch)
	if err != nil || string(second) != "1" || count != 1 {
		t.Fatalf("nested read %q %d %v", second, count, err)
	}
	second[0] = 'y'
	third, _ := c.readPhaseGet(ctx, "/org/products/id", fetch)
	if string(third) != "1" {
		t.Fatalf("cached bytes mutated: %q", third)
	}
	_, _ = c.readPhaseGet(c.BeginReadPhase(context.Background()), "/org/products/id", fetch)
	if count != 2 {
		t.Fatalf("new phase reused stale data: %d", count)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = c.readPhaseGet(canceled, "/org/products/id", fetch); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled hit: %v", err)
	}
	if got := c.RequestStats().PageCacheHits; got != 2 {
		t.Fatalf("hits: %d", got)
	}
}

func TestReadPhaseRoutesAndFailures(t *testing.T) {
	c := &Client{}
	ctx := c.BeginReadPhase(context.Background())
	for _, tc := range []struct {
		path  string
		cache bool
	}{
		{"/org/products", true}, {"/org/products/id", true}, {"/org/products/id/versions", true}, {"/org/create/plugins", true},
		{"/auth/session", false}, {"/org/products/id/manifest", false}, {"/api/tags", false}, {"/_next/static/script.js", false}, {"/org/products?signature=secret", false}, {"https://storage.test/org/products", false}, {"/org/create/plugins#fragment", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			n := 0
			fetch := func() ([]byte, error) { n++; return []byte("page"), nil }
			_, _ = c.readPhaseGet(ctx, tc.path, fetch)
			_, _ = c.readPhaseGet(ctx, tc.path, fetch)
			want := 2
			if tc.cache {
				want = 1
			}
			if n != want {
				t.Fatalf("fetches %d, want %d", n, want)
			}
		})
	}
	n := 0
	fetch := func() ([]byte, error) { n++; return nil, errors.New("synthetic") }
	_, _ = c.readPhaseGet(ctx, "/org/products/failed", fetch)
	_, _ = c.readPhaseGet(ctx, "/org/products/failed", fetch)
	if n != 2 {
		t.Fatalf("failure cached %d", n)
	}
}

func TestReadPhaseInvalidationPreventsInflightRepopulation(t *testing.T) {
	c := &Client{}
	ctx := c.BeginReadPhase(context.Background())
	started := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.readPhaseGet(ctx, "/org/products/id", func() ([]byte, error) { close(started); <-finish; return []byte("old"), nil })
	}()
	<-started
	c.invalidateReadPhase(ctx)
	close(finish)
	<-done
	n := 0
	b, err := c.readPhaseGet(ctx, "/org/products/id", func() ([]byte, error) { n++; return []byte("new"), nil })
	if err != nil || string(b) != "new" || n != 1 {
		t.Fatalf("stale read %q %d %v", b, n, err)
	}
	c.invalidateReadPhase(ctx)
	_, _ = c.readPhaseGet(ctx, "/org/products/id", func() ([]byte, error) { n++; return []byte("newer"), nil })
	if n != 2 {
		t.Fatal("cache not invalidated")
	}
}

type phaseRoundTrip func(*http.Request) (*http.Response, error)

func (f phaseRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadPhaseHTTPInvalidatesAfterFailedWrite(t *testing.T) {
	var gets int
	transport := phaseRoundTrip(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusInternalServerError, "synthetic failure"
		if r.Method == http.MethodGet {
			gets++
			status, body = http.StatusOK, fmt.Sprintf("page-%d", gets)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	c, err := New(Config{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := c.BeginReadPhase(context.Background())
	a, err := c.get(ctx, "/org/products/id")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.get(ctx, "/org/products/id")
	if err != nil || string(a) != string(b) || gets != 1 {
		t.Fatalf("read reuse %q %q %d %v", a, b, gets, err)
	}
	_, err = c.request(ctx, http.MethodPost, c.endpoint("/org/products/id"), "", "", nil, true)
	if err == nil {
		t.Fatal("expected failed write")
	}
	b, err = c.get(ctx, "/org/products/id")
	if err != nil || string(b) != "page-2" || gets != 2 {
		t.Fatalf("read after failed write %q %d %v", b, gets, err)
	}
}

func TestReadPhaseEntryAndByteBounds(t *testing.T) {
	c := &Client{}
	ctx := c.BeginReadPhase(context.Background())
	for i := 0; i < readPhaseMaxEntries+1; i++ {
		path := fmt.Sprintf("/org/products/p%d", i)
		_, err := c.readPhaseGet(ctx, path, func() ([]byte, error) { return []byte("x"), nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	p := c.phase(ctx)
	p.mu.Lock()
	entries, bytes := len(p.pages), p.bytes
	p.mu.Unlock()
	if entries != readPhaseMaxEntries || bytes != readPhaseMaxEntries {
		t.Fatalf("entry bound %d bytes %d", entries, bytes)
	}
	ctx = c.BeginReadPhase(context.Background())
	calls := 0
	fetch := func() ([]byte, error) { calls++; return make([]byte, readPhaseMaxBytes+1), nil }
	_, _ = c.readPhaseGet(ctx, "/org/products/large", fetch)
	_, _ = c.readPhaseGet(ctx, "/org/products/large", fetch)
	if calls != 2 {
		t.Fatalf("oversized page cached: %d", calls)
	}
}
