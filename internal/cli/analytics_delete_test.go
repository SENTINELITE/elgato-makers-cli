package cli

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
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

func TestAnalyticsCommandDefaultsDatesFiltersAndExports(t *testing.T) {
	dir, cfg, _, _ := fixture(t)
	private := filepath.Join(dir, "private")
	os.Mkdir(private, 0700)
	credsFile := filepath.Join(private, "session.json")
	c, _ := auth.Parse(strings.NewReader("__Host-kc_at=synthetic"))
	if err := auth.NewFileStore(credsFile).Save(c); err != nil {
		t.Fatal(err)
	}
	var ranges [][2]string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/organizations/org/analytics" {
			t.Error("unexpected request", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		s, e := r.URL.Query().Get("start"), r.URL.Query().Get("end")
		ranges = append(ranges, [2]string{s, e})
		free := maker.AnalyticsProduct{ProductID: "free", Name: "Free product", Extension: "plugins", IsFree: true, TotalsInRange: maker.Metric{Revenue: 0, Adoptions: 10}, Days: []maker.AnalyticsDay{{Date: s, Adoptions: 10}}}
		paid := maker.AnalyticsProduct{ProductID: "paid", Name: "Paid, product", Extension: "plugins", TotalsInRange: maker.Metric{Revenue: -250, Adoptions: 2}, Days: []maker.AnalyticsDay{{Date: s, Revenue: -250, Adoptions: 2}}}
		json.NewEncoder(w).Encode(maker.AnalyticsData{TimeRange: maker.TimeRange{Start: s, End: e}, TotalsInRange: maker.Metric{Revenue: -250, Adoptions: 12}, TotalsPerDay: []maker.AnalyticsDay{{Date: s, Revenue: -250, Adoptions: 12}}, Products: []maker.AnalyticsProduct{free, paid}})
	}))
	defer server.Close()
	factory := func(c []*http.Cookie, ro bool) (*maker.Client, error) {
		if !ro {
			t.Error("analytics client should be read-only")
		}
		return maker.New(maker.Config{BaseURL: server.URL, HTTPClient: server.Client(), Cookies: c, ReadOnly: ro, AllowLocalhost: true})
	}
	opts := Options{ClientFactory: factory, Now: func() time.Time { return time.Date(2026, 11, 2, 1, 0, 0, 0, time.FixedZone("Pacific", -8*3600)) }}
	common := []string{"--config", cfg, "--credentials-file", credsFile, "analytics"}
	run := func(args ...string) string {
		t.Helper()
		out, _, err := command(t, opts, append(append([]string{}, common...), args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	var report analyticsReport
	if err := json.Unmarshal([]byte(run("--json")), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Earnings.Estimated || report.Earnings.PlatformFeePercent != 30 || report.Earnings.CreatorSharePercent != 70 || report.Earnings.Current.EstimatedCreatorRevenueCents != -175 || report.Data.TotalsInRange.Revenue != -250 {
		t.Fatal("missing estimate or changed gross revenue", report)
	}
	if !strings.Contains(run(), "Estimated your share after 30% fee (USD): -1.75") {
		t.Fatal("table omitted estimated creator share")
	}
	if len(ranges) != 2 || ranges[0] != [2]string{"2025-10-01", "2026-11-01"} || report.Pricing != "all" || report.Data.TotalsInRange.Adoptions != 12 {
		t.Fatal("default did not fetch all available with all pricing", ranges, report)
	}
	json.Unmarshal([]byte(run("products", "--start", "2026-10-01", "--end", "2026-10-07", "--type", "paid", "--json")), &report)
	if report.Data.TotalsInRange.Revenue != -250 || report.Data.TotalsInRange.Adoptions != 2 || len(report.Data.Products) != 1 {
		t.Fatal("paid filter lost refund or counts")
	}
	rows, err := csv.NewReader(strings.NewReader(run("products", "--type", "paid", "--format", "csv"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1][2] != "Paid, product" || rows[1][6] != "-250" || rows[0][8] != "estimated_creator_revenue_cents" || rows[1][8] != "-175" {
		t.Fatal("CSV escaping/cents incorrect", rows)
	}
	before := len(ranges)
	for _, args := range [][]string{{"--start", "2026-02-30"}, {"--start", "2026-12-01", "--end", "2026-01-01"}, {"--type", "invalid"}, {"--format", "csv", "--json"}} {
		_, _, err := command(t, opts, append(append([]string{}, common...), args...)...)
		if err == nil {
			t.Fatal("invalid args accepted", args)
		}
	}
	if len(ranges) != before {
		t.Fatal("invalid options performed network calls")
	}
	json.Unmarshal([]byte(run("--start", "2026-10-26", "--end", "2026-11-01", "--compare-previous", "--json")), &report)
	if report.Previous == nil || ranges[len(ranges)-1] != [2]string{"2026-10-19", "2026-10-25"} {
		t.Fatal("wrong previous period", ranges)
	}
}

func TestDeleteCommandConfirmationsAndNoninteractiveGuards(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		tty, yes    bool
		expected    string
		wantPosts   int
		wantErr     bool
	}{
		{"blank cancels", "\n", true, false, "", 0, false},
		{"second no cancels", "yes\nno\n", true, false, "", 0, false},
		{"two yes", "yes\nyes\n", true, false, "", 1, false},
		{"pipe refused", "yes\nyes\n", false, false, "", 0, true},
		{"yes lacks name", "", false, true, "", 0, true},
		{"wrong name", "", false, true, "Other", 0, true},
		{"explicit automation", "", false, true, "Demo", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, cfg, _, _ := fixture(t)
			private := filepath.Join(dir, "private")
			os.Mkdir(private, 0700)
			file := filepath.Join(private, "cookies.json")
			c, _ := auth.Parse(strings.NewReader("__Host-kc_at=synthetic"))
			if err := auth.NewFileStore(file).Save(c); err != nil {
				t.Fatal(err)
			}
			posts := 0
			deleted := false
			product := map[string]any{"id": "product", "name": "Demo", "status": "DRAFT", "variants": []any{map[string]any{"id": "variant", "streamdeckid": "com.example.demo", "versions": []any{}}}}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts++
					if r.URL.Path != "/org/products/product" || r.Header.Get("Next-Action") != strings.Repeat("a", 40) {
						t.Error("wrong destructive request")
					}
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
					}
					if r.FormValue("_1_product_id") != "product" || r.FormValue("0") != `[{"success":false},"$K1"]` {
						t.Error("wrong deletion target or envelope")
					}
					deleted = true
					w.Header().Set("Location", "/org/products")
					w.WriteHeader(303)
					return
				}
				switch r.URL.Path {
				case "/org/products/product/versions":
					json.NewEncoder(w).Encode(map[string]any{"product": product})
				case "/org/products/product":
					fmt.Fprint(w, `<script src="/_next/static/delete.js"></script>`)
				case "/org/products":
					items := []any{}
					if !deleted {
						items = append(items, product)
					}
					b, _ := json.Marshal(map[string]any{"products": items})
					fmt.Fprintf(w, `<script src="/_next/static/delete.js"></script><script>self.__next_f.push([1,%q])</script>`, "1:"+string(b)+"\n")
				case "/_next/static/delete.js":
					fmt.Fprintf(w, `(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"deleteProductAction");`, strings.Repeat("a", 40))
				default:
					t.Error("unexpected route", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			factory := func(c []*http.Cookie, ro bool) (*maker.Client, error) {
				return maker.New(maker.Config{BaseURL: server.URL, HTTPClient: server.Client(), Cookies: c, ReadOnly: ro, AllowLocalhost: true})
			}
			args := []string{"--config", cfg, "--credentials-file", file, "--state-dir", filepath.Join(dir, "states"), "product", "delete", "--product", "demo"}
			if tc.yes {
				args = append(args, "--yes")
			}
			if tc.expected != "" {
				args = append(args, "--confirm-name", tc.expected)
			}
			out, _, err := command(t, Options{ClientFactory: factory, IsTerminal: func() bool { return tc.tty }, In: strings.NewReader(tc.input)}, args...)
			if (err != nil) != tc.wantErr {
				t.Fatalf("wrong error: %v, output %s", err, out)
			}
			if posts != tc.wantPosts {
				t.Fatalf("wrong deletion count %d expected %d", posts, tc.wantPosts)
			}
		})
	}
}
