package maker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

func TestPublicScriptDiscoveryFreshPageAndConflicts(t *testing.T) {
	const one = "/_next/static/chunks/main-0123456789abcdef.js"
	const two = "/_next/static/chunks/main-fedcba9876543210.js"
	current, conflict := one, false
	counts := map[string]int{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts[r.URL.Path]++
		if strings.HasPrefix(r.URL.Path, "/_next/static/") {
			fmt.Fprint(w, fixtureActions("a"))
			return
		}
		fmt.Fprintf(w, `<script src="%s"></script>`, current)
		if conflict {
			fmt.Fprint(w, fixtureActions("b"))
		}
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, AllowLocalhost: true})
	target := model.Target{OrgID: "org", ProductID: "product"}
	for i := 0; i < 2; i++ {
		if _, err := c.DiscoverActions(context.Background(), target); err != nil {
			t.Fatal(err)
		}
	}
	if counts[one] != 1 {
		t.Fatalf("script fetched %d times", counts[one])
	}
	conflict = true
	if _, err := c.DiscoverActions(context.Background(), target); err == nil {
		t.Fatal("warm cache hid conflict")
	}
	conflict = false
	current = two
	if _, err := c.DiscoverActions(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if counts[two] != 1 || counts["/org/products/product/versions"] != 4 {
		t.Fatalf("counts %v", counts)
	}
}

func TestPublicScriptErrorsCancellationAndEviction(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		n := counts[r.URL.Path]
		mu.Unlock()
		if strings.Contains(r.URL.Path, "failure-") && n == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, "script")
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, AllowLocalhost: true})
	script := func(name string) *url.URL {
		u, _ := url.Parse(s.URL + "/_next/static/chunks/" + name + ".js")
		return u
	}
	bad := script("failure-0123456789abcdef")
	if _, err := c.publicScript(context.Background(), bad); err == nil {
		t.Fatal("failure accepted")
	}
	if _, err := c.publicScript(context.Background(), bad); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.publicScript(ctx, bad); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled hit: %v", err)
	}
	for i := 0; i < publicScriptMaxEntries; i++ {
		if _, err := c.publicScript(context.Background(), script(fmt.Sprintf("chunk-%016x", i))); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.publicScripts.bodies) > publicScriptMaxEntries || c.publicScripts.bytes > publicScriptMaxBytes {
		t.Fatal("cache exceeds limit")
	}
	if _, err := c.publicScript(context.Background(), bad); err != nil {
		t.Fatal(err)
	}
	if counts[bad.Path] != 3 {
		t.Fatalf("failure or eviction cached incorrectly: %v", counts[bad.Path])
	}
}

func TestPublicScriptConcurrentMissIsShared(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	entered, release := make(chan struct{}), make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			close(entered)
			<-release
		}
		fmt.Fprint(w, "script")
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, AllowLocalhost: true})
	u, _ := url.Parse(s.URL + "/_next/static/chunks/main-0123456789abcdef.js")
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		go func() { _, err := c.publicScript(context.Background(), u); errs <- err }()
	}
	<-entered
	close(release)
	for i := 0; i < 12; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("concurrent fetches: %d", calls)
	}
}
