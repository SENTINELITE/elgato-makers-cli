package cli

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/auth"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

func TestAnalyticsRangesAndBucketsCLI(t *testing.T) {
	dir, cfg, _, _ := fixture(t)
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(private, "session.json")
	creds, _ := auth.Parse(strings.NewReader("__Host-kc_at=synthetic"))
	if err := auth.NewFileStore(file).Save(creds); err != nil {
		t.Fatal(err)
	}
	var calls [][2]string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/organizations/org/analytics" {
			t.Error("unexpected request")
			http.NotFound(w, r)
			return
		}
		start, end := r.URL.Query().Get("start"), r.URL.Query().Get("end")
		calls = append(calls, [2]string{start, end})
		day := maker.AnalyticsDay{Date: start, Revenue: -123, Adoptions: 2}
		p := maker.AnalyticsProduct{ProductID: "paid", Name: "Paid, product", Extension: "plugins", TotalsInRange: maker.Metric{Revenue: -123, Adoptions: 2}, Days: []maker.AnalyticsDay{day}}
		free := maker.AnalyticsProduct{ProductID: "free", Name: "Free product", Extension: "plugins", IsFree: true, TotalsInRange: maker.Metric{Adoptions: 3}, Days: []maker.AnalyticsDay{{Date: start, Adoptions: 3}}}
		json.NewEncoder(w).Encode(maker.AnalyticsData{TimeRange: maker.TimeRange{Start: start, End: end}, TotalsInRange: maker.Metric{Revenue: -123, Adoptions: 5}, TotalsPerDay: []maker.AnalyticsDay{{Date: start, Revenue: -123, Adoptions: 5}}, Products: []maker.AnalyticsProduct{p, free}})
	}))
	defer server.Close()
	opts := Options{Now: func() time.Time { return time.Date(2026, 11, 2, 2, 0, 0, 0, time.FixedZone("Pacific", -8*3600)) }, ClientFactory: func(cookies []*http.Cookie, ro bool) (*maker.Client, error) {
		if !ro {
			t.Error("analytics must be read-only")
		}
		return maker.New(maker.Config{BaseURL: server.URL, HTTPClient: server.Client(), Cookies: cookies, ReadOnly: ro, AllowLocalhost: true})
	}}
	common := []string{"--config", cfg, "--credentials-file", file, "analytics"}
	run := func(args ...string) (string, string, error) {
		t.Helper()
		return command(t, opts, append(append([]string{}, common...), args...)...)
	}
	for _, tc := range []struct {
		args []string
		want [2]string
	}{
		{[]string{"-d", "7"}, [2]string{"2026-10-26", "2026-11-01"}},
		{[]string{"-m", "3"}, [2]string{"2026-08-02", "2026-11-01"}},
		{[]string{"--range", "this-month"}, [2]string{"2026-11-01", "2026-11-01"}},
		{[]string{"--range", "last-month"}, [2]string{"2026-10-01", "2026-10-31"}},
		{[]string{"--range", "this-year"}, [2]string{"2026-01-01", "2026-11-01"}},
		{[]string{"--range", "last-year"}, [2]string{"2025-10-01", "2025-12-31"}},
		{[]string{"--range", "all-time"}, [2]string{"2025-10-01", "2026-11-01"}},
	} {
		out, stderr, err := run(append(tc.args, "--json")...)
		if err != nil {
			t.Fatal(err)
		}
		if calls[len(calls)-1] != tc.want {
			t.Fatalf("%v got %v want %v", tc.args, calls[len(calls)-1], tc.want)
		}
		if tc.args[len(tc.args)-1] == "last-year" {
			var report analyticsReport
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatal(err)
			}
			if !report.Selection.Truncated || report.Selection.RequestedStart != "2025-01-01" || !strings.Contains(stderr, "available history starts 2025-10-01") {
				t.Fatal("missing truncation disclosure", out, stderr)
			}
		}
	}
	before := len(calls)
	for _, args := range [][]string{
		{"-d", "0"}, {"-m", "0"}, {"-d", "-1"}, {"-m", "-1"},
		{"-d", "7", "-m", "1"}, {"-d", "7", "--start", "2026-10-01"},
		{"--range", "this-month", "--end", "2026-10-31"}, {"--range", ""}, {"--range", "nonesuch"},
		{"--group-by", "year"}, {"--group-by", ""}, {"weekly", "--group-by", "month"}, {"--start", ""},
	} {
		if _, _, err := run(args...); err == nil {
			t.Fatal("invalid options accepted", args)
		}
	}
	if before != len(calls) {
		t.Fatal("invalid flags dispatched requests")
	}
	for _, tc := range []struct {
		view, group string
		count       int
	}{{"daily", "day", 7}, {"weekly", "week", 1}, {"monthly", "month", 2}, {"summary", "all", 1}} {
		out, _, err := run(tc.view, "-d", "7", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var report analyticsReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if report.GroupBy != tc.group || len(report.Buckets) != tc.count || report.Buckets[0].Revenue != -123 || report.Buckets[0].EstimatedCreatorRevenueCents != -86 || report.Earnings.Current.EstimatedCreatorRevenueCents != -86 {
			t.Fatal("wrong buckets", tc, report)
		}
	}
	out, _, err := run("weekly", "-d", "7", "--compare-previous", "--format", "csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[1][0] != "current" || rows[1][1] != "2026-10-26" || rows[1][3] != "false" || rows[2][0] != "previous" || rows[2][1] != "2026-10-19" || rows[1][5] != "-123" || rows[0][7] != "estimated_creator_revenue_cents" || rows[1][7] != "-86" || rows[2][7] != "-86" {
		t.Fatal("wrong weekly comparison CSV", rows)
	}
	out, _, err = run("products", "-d", "7", "--group-by", "month", "--type", "paid", "--format", "csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err = csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[1][1] != "Paid, product" || rows[1][5] != "2026-10-26" || rows[1][7] != "true" || rows[1][9] != "-123" || rows[2][9] != "0" || rows[1][11] != "-86" || rows[2][11] != "0" {
		t.Fatal("wrong per-product CSV", rows)
	}
	out, _, err = run("--group-by", "all", "--type", "free", "--format", "csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err = csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil || len(rows) != 2 || rows[1][4] != "3" || rows[1][5] != "0" || rows[1][7] != "0" {
		t.Fatal("wrong filtered totals CSV", rows, err)
	}
	out, _, err = run("daily", "-d", "7", "--format", "csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err = csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil || len(rows) != 8 || rows[0][1] != "date" || rows[7][1] != "2026-11-01" || rows[7][2] != "0" || rows[1][5] != "-86" || rows[7][5] != "0" {
		t.Fatal("daily did not zero-fill", rows, err)
	}
}
