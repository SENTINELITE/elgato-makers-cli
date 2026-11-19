package cli

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

// Synthetic sparse data covers refunds and products with no sales in later buckets.
func analyticsShortcutsResponse(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodGet || r.URL.Path != "/api/organizations/org/analytics" {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	start, end := r.URL.Query().Get("start"), r.URL.Query().Get("end")
	paid := maker.AnalyticsDay{Date: start, Revenue: -123, Adoptions: 2}
	free := maker.AnalyticsDay{Date: start, Adoptions: 3}
	data := maker.AnalyticsData{
		TimeRange:     maker.TimeRange{Start: start, End: end},
		TotalsInRange: maker.Metric{Revenue: -123, Adoptions: 5},
		TotalsPerDay:  []maker.AnalyticsDay{{Date: start, Revenue: -123, Adoptions: 5}},
		Products: []maker.AnalyticsProduct{
			{ProductID: "paid", Name: "Paid, product", Extension: "plugins", TotalsInRange: maker.Metric{Revenue: -123, Adoptions: 2}, Days: []maker.AnalyticsDay{paid}},
			{ProductID: "free", Name: "Free product", Extension: "icons", IsFree: true, TotalsInRange: maker.Metric{Adoptions: 3}, Days: []maker.AnalyticsDay{free}},
		},
	}
	if err := json.NewEncoder(w).Encode(data); err != nil {
		t.Error(err)
	}
}

func TestAnalyticsProductIntervalShortcuts(t *testing.T) {
	opts, base, _ := analyticsStatusFixture(t, func(w http.ResponseWriter, r *http.Request) { analyticsShortcutsResponse(t, w, r) })
	run := func(args ...string) (string, string) {
		t.Helper()
		args = append(append([]string{}, base...), args...)
		args = append(args, "-d", "7")
		out, stderr, err := command(t, opts, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out, stderr
	}
	for _, tc := range []struct {
		view, group string
		buckets     int
	}{{"daily", "day", 7}, {"weekly", "week", 1}, {"monthly", "month", 2}, {"summary", "all", 1}} {
		for _, noun := range []string{"products", "product"} {
			t.Run(noun+"/"+tc.view, func(t *testing.T) {
				out, stderr := run(noun, tc.view, "--json", "--compare-previous")
				grouped, _ := run("products", "--group-by", tc.group, "--json", "--compare-previous")
				var got, want analyticsReport
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(grouped), &want); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("shortcut differs from --group-by %s\ngot %s\nwant %s", tc.group, out, grouped)
				}
				if got.GroupBy != tc.group || len(got.ProductBuckets) != 2 || len(got.ProductBuckets[0].Buckets) != tc.buckets || len(got.Buckets) != 0 || len(got.PreviousProductBuckets) != 2 {
					t.Fatalf("wrong per-product interval report: %+v", got)
				}
				if got.ProductBuckets[0].Buckets[0].Revenue != -123 || got.ProductBuckets[0].Buckets[0].EstimatedCreatorRevenueCents != -86 || got.Earnings.Current.EstimatedCreatorRevenueCents != -86 {
					t.Fatalf("refund or estimated earnings changed: %+v", got)
				}
				if strings.Count(stderr, "Success:") != 1 || strings.Contains(out, "Success:") {
					t.Fatalf("incorrect completion output: %q %q", out, stderr)
				}
			})
		}
	}
}

func TestAnalyticsProductDailyCSVIdentityAndZeroFill(t *testing.T) {
	opts, base, _ := analyticsStatusFixture(t, func(w http.ResponseWriter, r *http.Request) { analyticsShortcutsResponse(t, w, r) })
	out, stderr, err := command(t, opts, append(base, "products", "daily", "-d", "7", "--compare-previous", "--format", "csv")...)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 29 {
		t.Fatalf("expected 28 product/day data rows, got %d", len(rows)-1)
	}
	columns := map[string]int{}
	for i, name := range rows[0] {
		columns[name] = i
	}
	for _, name := range []string{"product_id", "name", "extension", "is_free", "period", "start", "end", "adoptions", "revenue_cents", "currency", "estimated_creator_revenue_cents"} {
		if _, ok := columns[name]; !ok {
			t.Fatalf("missing CSV column %q: %v", name, rows[0])
		}
	}
	seen := map[string]bool{}
	for _, row := range rows[1:] {
		value := func(name string) string { return row[columns[name]] }
		id, date, period := value("product_id"), value("start"), value("period")
		key := period + "/" + id + "/" + date
		if seen[key] {
			t.Fatalf("duplicate CSV row %s", key)
		}
		seen[key] = true
		if value("end") != date || value("currency") != "USD" {
			t.Fatalf("invalid daily bounds or currency: %v", row)
		}
		first := "2026-10-26"
		if period == "previous" {
			first = "2026-10-19"
		} else if period != "current" {
			t.Fatalf("unknown period: %v", row)
		}
		units, revenue, estimate := "0", "0", "0"
		switch id {
		case "paid":
			if value("name") != "Paid, product" || value("extension") != "plugins" || value("is_free") != "false" {
				t.Fatalf("paid identity lost: %v", row)
			}
			if date == first {
				units, revenue, estimate = "2", "-123", "-86"
			}
		case "free":
			if value("name") != "Free product" || value("extension") != "icons" || value("is_free") != "true" {
				t.Fatalf("free identity lost: %v", row)
			}
			if date == first {
				units = "3"
			}
		default:
			t.Fatalf("unknown product: %v", row)
		}
		if value("adoptions") != units || value("revenue_cents") != revenue || value("estimated_creator_revenue_cents") != estimate {
			t.Fatalf("incorrect sales, refund estimate, or zero fill: %v", row)
		}
	}
	if !regexp.MustCompile(fmt.Sprintf(`\b%d (?:CSV )?(?:data )?rows\b`, len(rows)-1)).MatchString(stderr) || strings.Count(stderr, "Success:") != 1 {
		t.Fatalf("completion differs from CSV row count: %q", stderr)
	}
}

func TestAnalyticsProductShortcutConflictsBeforeHTTP(t *testing.T) {
	opts, base, _ := analyticsStatusFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("conflicting flags dispatched HTTP") })
	for _, tc := range []struct{ view, conflict string }{{"daily", "week"}, {"weekly", "day"}, {"monthly", "all"}, {"summary", "month"}} {
		for _, noun := range []string{"products", "product"} {
			t.Run(noun+"/"+tc.view, func(t *testing.T) {
				out, stderr, err := command(t, opts, append(append([]string{}, base...), noun, tc.view, "--group-by", tc.conflict)...)
				if err == nil || !strings.Contains(err.Error(), "already groups by") {
					t.Fatalf("expected interval conflict, got %v", err)
				}
				if strings.Contains(out+stderr, "Success:") {
					t.Fatal("conflict reported success")
				}
			})
		}
	}
}

func TestAnalyticsMinifyRetainsReportAndCompletion(t *testing.T) {
	opts, base, _ := analyticsStatusFixture(t, func(w http.ResponseWriter, r *http.Request) { analyticsShortcutsResponse(t, w, r) })
	for _, view := range [][]string{{"daily"}, {"products", "daily"}} {
		t.Run(strings.Join(view, "/"), func(t *testing.T) {
			args := append(append([]string{}, base...), view...)
			args = append(args, "-d", "7", "--compare-previous")
			pretty, _, err := command(t, opts, append(append([]string{}, args...), "--json")...)
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(pretty), &want); err != nil {
				t.Fatal(err)
			}
			if strings.Count(strings.TrimSpace(pretty), "\n") == 0 {
				t.Fatal("default JSON is no longer pretty")
			}
			for _, flags := range [][]string{{"--minify"}, {"--json", "--minify"}, {"--format", "json", "--minify"}} {
				out, stderr, err := command(t, opts, append(append([]string{}, args...), flags...)...)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("invalid minified JSON: %v: %s", err, out)
				}
				if strings.Contains(strings.TrimSpace(out), "\n") || !reflect.DeepEqual(got, want) {
					t.Fatalf("minified JSON changed report or has multiple lines: %s", out)
				}
				for _, field := range []string{"data", "earnings", "previous"} {
					if _, ok := got[field]; !ok {
						t.Errorf("minified report lost %s", field)
					}
				}
				if _, a := got["buckets"]; !a {
					if _, b := got["product_buckets"]; !b {
						t.Error("minified report lost buckets")
					}
				}
				if strings.Count(stderr, "Success:") != 1 || !strings.Contains(strings.ToLower(stderr), "json") {
					t.Fatalf("missing JSON completion: %q", stderr)
				}
			}
			out, _, err := command(t, opts, append(append([]string{}, args...), "--json", "--minify=false")...)
			if err != nil || out != pretty {
				t.Fatalf("--minify=false changed pretty JSON: %v\n%s", err, out)
			}
		})
	}
}

func TestAnalyticsMinifyRejectsOtherFormatsBeforeHTTP(t *testing.T) {
	opts, base, _ := analyticsStatusFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid minify format dispatched HTTP") })
	for _, format := range []string{"csv", "table"} {
		for _, flags := range [][]string{{"--minify", "--format", format}, {"--json", "--minify", "--format", format}} {
			out, stderr, err := command(t, opts, append(append([]string{}, base...), flags...)...)
			if err == nil || !strings.Contains(err.Error(), "minify") {
				t.Fatalf("expected minify/%s rejection, got %v", format, err)
			}
			if strings.Contains(out+stderr, "Success:") {
				t.Fatal("invalid format reported success")
			}
		}
	}
}
