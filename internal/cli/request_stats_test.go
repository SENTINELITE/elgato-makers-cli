package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

type metricsTransport func(*http.Request) (*http.Response, error)

func (f metricsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVerboseOfflineJSONKeepsStatisticsOffStdout(t *testing.T) {
	_, _, pkg, _ := fixture(t)
	for _, flag := range []string{"--json", "--minify"} {
		out, stderr, err := command(t, Options{}, "--verbose", flag, "inspect", pkg)
		if err != nil || !json.Valid([]byte(out)) || !strings.Contains(stderr, "HTTP: 0 requests") || strings.Contains(out, "HTTP:") {
			t.Fatalf("invalid machine output or verbose stats: error=%v stderr=%q", err, stderr)
		}
	}
	_, stderr, err := command(t, Options{}, "inspect", pkg)
	if err != nil || strings.Contains(stderr, "HTTP:") {
		t.Fatalf("statistics appeared without --verbose: %v %q", err, stderr)
	}
}

func TestRequestStatisticsDeduplicateClientsAndExposeOnlyAggregates(t *testing.T) {
	secret := "synthetic-private-value"
	httpClient := &http.Client{Transport: metricsTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"products":[]}`)), Request: r}, nil
	})}
	c, err := maker.New(maker.Config{BaseURL: "http://127.0.0.1:9099", AllowLocalhost: true, ReadOnly: true, HTTPClient: httpClient, Cookies: []*http.Cookie{{Name: "__Host-kc_at", Value: secret}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Products(context.Background(), "synthetic-org"); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	a := &app{o: Options{Err: &stderr}}
	if err = a.writeRequestStats([]session{{client: c}, {client: c}}); err != nil {
		t.Fatal(err)
	}
	s := stderr.String()
	if !strings.Contains(s, "HTTP: 1 requests (Maker 1, storage 0)") || strings.Contains(s, secret) || strings.Contains(s, "127.0.0.1") || strings.Contains(s, "synthetic-org") {
		t.Fatalf("statistics duplicated clients or exposed request details: %q", s)
	}
}
