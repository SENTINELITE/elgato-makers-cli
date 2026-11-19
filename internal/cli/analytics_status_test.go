package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/auth"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/testutil"
)

// All credentials and responses in these tests are synthetic; only localhost is used.
func analyticsStatusFixture(t *testing.T, handler http.HandlerFunc) (Options, []string, string) {
	t.Helper()
	dir, config, _, _ := fixture(t)
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(private, "session.json")
	creds, err := auth.Parse(strings.NewReader("__Host-kc_at=synthetic"))
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.NewFileStore(file).Save(creds); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	opts := Options{Now: func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }, ClientFactory: func(cookies []*http.Cookie, readOnly bool) (*maker.Client, error) {
		if !readOnly {
			t.Error("analytics must use a read-only client")
		}
		return maker.New(maker.Config{BaseURL: server.URL, HTTPClient: server.Client(), Cookies: cookies, ReadOnly: readOnly, AllowLocalhost: true})
	}}
	return opts, []string{"--config", config, "--credentials-file", file, "analytics"}, file
}

func analyticsStatusResponse(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodGet || r.URL.Path != "/api/organizations/org/analytics" {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	start, end := r.URL.Query().Get("start"), r.URL.Query().Get("end")
	day := maker.AnalyticsDay{Date: start, Revenue: 1000, Adoptions: 2}
	if err := json.NewEncoder(w).Encode(maker.AnalyticsData{
		TimeRange:     maker.TimeRange{Start: start, End: end},
		TotalsInRange: maker.Metric{Revenue: 1000, Adoptions: 2},
		TotalsPerDay:  []maker.AnalyticsDay{day},
		Products:      []maker.AnalyticsProduct{{ProductID: "paid", Name: "Paid, product", Extension: "plugins", TotalsInRange: maker.Metric{Revenue: 1000, Adoptions: 2}, Days: []maker.AnalyticsDay{day}}},
	}); err != nil {
		t.Error(err)
	}
}

func TestAnalyticsCompletionKeepsRedirectedOutputClean(t *testing.T) {
	opts, base, _ := analyticsStatusFixture(t, func(w http.ResponseWriter, r *http.Request) { analyticsStatusResponse(t, w, r) })
	for _, view := range []string{"overview", "products", "daily", "weekly", "monthly", "summary"} {
		for _, format := range []string{"csv", "json", "table"} {
			t.Run(view+"/"+format, func(t *testing.T) {
				args := append([]string{}, base...)
				if view != "overview" {
					args = append(args, view)
				}
				args = append(args, "--start", "2026-10-26", "--end", "2026-11-01", "--type", "paid", "--compare-previous", "--format", format)
				out, stderr, err := command(t, opts, args...)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(out, "Success:") {
					t.Fatalf("completion contaminated stdout: %s", out)
				}
				if strings.Count(stderr, "Success:") != 1 {
					t.Fatalf("expected one completion on stderr: %q", stderr)
				}
				lower := strings.ToLower(stderr)
				for _, want := range []string{format, "stdout", "row", "2026-10-26", "2026-11-01", "inclusive", "paid", "previous", "2026-10-19", "2026-10-25"} {
					if !strings.Contains(lower, want) {
						t.Errorf("completion missing %q: %q", want, stderr)
					}
				}
				switch format {
				case "csv":
					rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
					if err != nil {
						t.Fatal("stdout is not clean CSV", err)
					}
					if len(rows) < 2 || !strings.Contains(strings.Join(rows[0], ","), "estimated_creator_revenue_cents") {
						t.Fatal("CSV lacks earnings rows", rows)
					}
					if !regexp.MustCompile(fmt.Sprintf(`\b%d (?:csv )?(?:data )?rows?\b`, len(rows)-1)).MatchString(lower) {
						t.Errorf("completion row count differs from CSV: %q (%d rows)", stderr, len(rows)-1)
					}
				case "json":
					var report analyticsReport
					if err := json.Unmarshal([]byte(out), &report); err != nil {
						t.Fatal("stdout is not clean JSON", err)
					}
					if !report.Earnings.Estimated || report.Earnings.CreatorSharePercent != 70 || report.Earnings.PlatformFeePercent != 30 || report.Earnings.Current.EstimatedCreatorRevenueCents != 700 || report.Earnings.Previous == nil || report.Earnings.Previous.EstimatedCreatorRevenueCents != 700 || report.Data.TotalsInRange.Revenue != 1000 {
						t.Fatalf("JSON earnings missing or gross changed: %+v", report)
					}
				case "table":
					if !strings.Contains(out, "EST. YOUR SHARE (USD)") {
						t.Fatal("table earnings missing", out)
					}
				}
			})
		}
	}
}

func TestAnalyticsCompletionExplicitlyReportsZeroRows(t *testing.T) {
	opts, base, _ := analyticsStatusFixture(t, func(w http.ResponseWriter, r *http.Request) { analyticsStatusResponse(t, w, r) })
	for _, format := range []string{"csv", "json", "table"} {
		t.Run(format, func(t *testing.T) {
			out, stderr, err := command(t, opts, append(append([]string{}, base...), "products", "--type", "free", "--format", format)...)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr, "Success:") || !regexp.MustCompile(`\b0 (?:CSV )?(?:data )?rows\b`).MatchString(stderr) {
				t.Fatalf("empty success must explicitly report zero rows: %q", stderr)
			}
			if format == "csv" {
				rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
				if err != nil || len(rows) != 1 {
					t.Fatalf("expected CSV header only: %v %v", rows, err)
				}
			}
		})
	}
}

type analyticsStatusFailWriter struct{ err error }

func (w analyticsStatusFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestAnalyticsFailuresNeverReportSuccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  int
		flags []string
	}{
		{"http", http.StatusInternalServerError, nil},
		{"invalid type", http.StatusOK, []string{"--type", "invalid"}},
		{"invalid format", http.StatusOK, []string{"--format", "invalid"}},
		{"invalid dates", http.StatusOK, []string{"--start", "2026-02-30"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, base, _ := analyticsStatusFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.code != http.StatusOK {
					http.Error(w, "synthetic failure", tc.code)
					return
				}
				t.Error("invalid flags must not make an HTTP request")
			})
			out, stderr, err := command(t, opts, append(append([]string{}, base...), tc.flags...)...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(out+stderr, "Success:") {
				t.Fatal("failure reported success", out, stderr)
			}
		})
	}
	for _, format := range []string{"csv", "json", "table"} {
		t.Run("write/"+format, func(t *testing.T) {
			opts, base, _ := analyticsStatusFixture(t, func(w http.ResponseWriter, r *http.Request) { analyticsStatusResponse(t, w, r) })
			sentinel := errors.New("synthetic stdout write failure")
			var stderr bytes.Buffer
			opts.Out, opts.Err = analyticsStatusFailWriter{sentinel}, &stderr
			root := New(opts)
			root.SetArgs(append(append([]string{}, base...), "--format", format))
			if err := root.ExecuteContext(context.Background()); !errors.Is(err, sentinel) {
				t.Fatalf("expected output write error, got %v", err)
			}
			if strings.Contains(stderr.String(), "Success:") {
				t.Fatal("write failure reported success", stderr.String())
			}
		})
	}
	t.Run("authentication", func(t *testing.T) {
		opts, base, file := analyticsStatusFixture(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("missing credentials must not make an HTTP request")
		})
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		out, stderr, err := command(t, opts, base...)
		if err == nil || strings.Contains(out+stderr, "Success:") {
			t.Fatalf("missing credentials must fail without success: %v %q %q", err, out, stderr)
		}
	})
	t.Run("session persistence", func(t *testing.T) {
		var file string
		opts, base, path := analyticsStatusFixture(t, func(w http.ResponseWriter, r *http.Request) {
			// Invalidate only this temporary credential store after the initial load.
			testutil.Widen(t, filepath.Dir(file))
			http.SetCookie(w, &http.Cookie{Name: "__Host-kc_at", Value: "synthetic-rotated", Path: "/", Secure: true, HttpOnly: true})
			analyticsStatusResponse(t, w, r)
		})
		file = path
		out, stderr, err := command(t, opts, append(append([]string{}, base...), "--json")...)
		if err == nil || !strings.Contains(err.Error(), "could not persist") {
			t.Fatalf("expected session persistence error: %v", err)
		}
		if strings.Contains(out+stderr, "Success:") {
			t.Fatal("session save failure reported success", stderr)
		}
		if !json.Valid([]byte(out)) {
			t.Fatal("rendered output should still be valid JSON", out)
		}
	})
}
