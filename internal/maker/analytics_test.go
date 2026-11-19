package maker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func analyticsFixture() AnalyticsData {
	return AnalyticsData{
		TimeRange:     AnalyticsTimeRange{"2026-01-01", "2026-01-03"},
		TotalsInRange: AnalyticsMetric{75, 5},
		TotalsPerDay:  []AnalyticsDay{{"2026-01-01", 100, 3}, {"2026-01-02", -25, 2}, {"2026-01-03", 0, 0}},
		Products: []AnalyticsProduct{
			{ProductID: "paid-one", Name: "Synthetic paid", Extension: "plugin", TotalsInRange: AnalyticsMetric{75, 2}, Days: []AnalyticsDay{{"2026-01-01", 100, 1}, {"2026-01-02", -25, 1}}},
			{ProductID: "free-one", Name: "Synthetic free", Extension: "icon-pack", IsFree: true, TotalsInRange: AnalyticsMetric{0, 3}, Days: []AnalyticsDay{{"2026-01-01", 0, 2}, {"2026-01-02", 0, 1}}},
		},
	}
}
func analyticsFixtureJSON(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(analyticsFixture())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func TestAnalyticsEndpointAndDates(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != "GET" || r.URL.Path != "/api/organizations/synthetic-org/analytics" || r.URL.RawQuery != "end=2026-01-03&start=2026-01-01" {
			t.Errorf("wrong analytics request: %s %s", r.Method, r.URL.String())
		}
		fmt.Fprint(w, analyticsFixtureJSON(t))
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL, AllowLocalhost: true, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Analytics(context.Background(), "synthetic-org", "2026-01-01", "2026-01-03")
	if err != nil || !reflect.DeepEqual(got, analyticsFixture()) {
		t.Fatal("analytics decode", err)
	}
	for _, dates := range [][2]string{{"2026-1-01", "2026-01-03"}, {"2026-02-30", "2026-03-01"}, {"2026-01-03", "2026-01-01"}, {"", ""}, {"2026-01-01&token=x", "2026-01-03"}} {
		if _, err = c.Analytics(context.Background(), "synthetic-org", dates[0], dates[1]); err == nil {
			t.Fatal("bad dates accepted", dates)
		}
	}
	if _, err = c.Analytics(context.Background(), "org/path", "2026-01-01", "2026-01-03"); err == nil {
		t.Fatal("bad org accepted")
	}
	if hits != 1 {
		t.Fatal("invalid input dispatched request")
	}
}
func TestAnalyticsRejectsMalformedSchema(t *testing.T) {
	good := analyticsFixtureJSON(t)
	for _, body := range []string{
		`{}`, `null`, `<html>login</html>`, good + ` {}`,
		strings.Replace(good, `"revenue":75`, `"revenue":null`, 1),
		strings.Replace(good, `"revenue":75`, `"revenue":0.75`, 1),
		strings.Replace(good, `"revenue":75`, `"revenue":"75"`, 1),
		strings.Replace(good, `"revenue":75,`, ``, 1),
		strings.Replace(good, `"is_free":false,`, ``, 1),
		strings.Replace(good, `"is_free":false`, `"is_free":null`, 1),
		strings.Replace(good, `"products":[`, `"products":null,"ignored":[`, 1),
		strings.Replace(good, `"date":"2026-01-01"`, `"date":"2026-01-04"`, 1),
		strings.Replace(good, `"days":[`, `"days":null,"ignored":[`, 1),
	} {
		if _, err := parseAnalytics([]byte(body)); err == nil {
			t.Fatal("malformed analytics accepted", body)
		}
	}
	nullMetadata := strings.ReplaceAll(strings.ReplaceAll(good, `"app_icon":""`, `"app_icon":null`), `"thumbnail":""`, `"thumbnail":null`)
	if _, err := parseAnalytics([]byte(nullMetadata)); err != nil {
		t.Fatal("nullable metadata rejected", err)
	}
	empty := `{"time_range":{"start":"2026-01-01","end":"2026-01-03"},"totals_in_range":{"revenue":0,"adoptions":0},"totals_per_day":[],"products":[]}`
	if _, err := parseAnalytics([]byte(empty)); err != nil {
		t.Fatal("recognized empty analytics rejected", err)
	}
}
func TestAnalyticsHTTPFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusFound, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, "synthetic-private-token")
			}))
			defer server.Close()
			c, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			if _, err := c.Analytics(context.Background(), "org", "2026-01-01", "2026-01-03"); err == nil || strings.Contains(err.Error(), "synthetic-private-token") {
				t.Fatal("HTTP failure policy", err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, analyticsFixtureJSON(t)) }))
	defer server.Close()
	c, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	_, err := c.Analytics(context.Background(), "org", "2026-01-01", "2026-01-04")
	var mismatch *AnalyticsRangeError
	if !errors.As(err, &mismatch) || mismatch.Requested.End != "2026-01-04" || mismatch.Returned.End != "2026-01-03" {
		t.Fatalf("missing typed range mismatch: %v", err)
	}
}
func TestFilterAnalytics(t *testing.T) {
	original := analyticsFixture()
	for _, tc := range []struct {
		pricing, product, extension string
		count                       int
		metric                      AnalyticsMetric
		daily                       []AnalyticsDay
	}{
		{"all", "", "", 2, AnalyticsMetric{75, 5}, original.TotalsPerDay},
		{"paid", "", "", 1, AnalyticsMetric{75, 2}, []AnalyticsDay{{"2026-01-01", 100, 1}, {"2026-01-02", -25, 1}, {"2026-01-03", 0, 0}}},
		{"free", "", "", 1, AnalyticsMetric{0, 3}, []AnalyticsDay{{"2026-01-01", 0, 2}, {"2026-01-02", 0, 1}, {"2026-01-03", 0, 0}}},
		{"all", "paid-one", "plugin", 1, AnalyticsMetric{75, 2}, []AnalyticsDay{{"2026-01-01", 100, 1}, {"2026-01-02", -25, 1}, {"2026-01-03", 0, 0}}},
		{"paid", "", "icon-pack", 0, AnalyticsMetric{}, []AnalyticsDay{{"2026-01-01", 0, 0}, {"2026-01-02", 0, 0}, {"2026-01-03", 0, 0}}},
	} {
		got, err := FilterAnalytics(original, tc.pricing, tc.product, tc.extension)
		if err != nil || len(got.Products) != tc.count || got.TotalsInRange != tc.metric || !reflect.DeepEqual(got.TotalsPerDay, tc.daily) || got.TimeRange != original.TimeRange {
			t.Fatalf("filter %s/%s/%s: %#v %v", tc.pricing, tc.product, tc.extension, got, err)
		}
	}
	all, err := FilterAnalytics(original, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if all.Products[0].ProductID != "paid-one" || all.Products[1].ProductID != "free-one" {
		t.Fatal("product ordering changed")
	}
	all.Products[0].Days[0].Revenue = 12345
	all.TotalsPerDay[0].Revenue = 12345
	if !reflect.DeepEqual(original, analyticsFixture()) {
		t.Fatal("filter mutated input")
	}
	if _, err := FilterAnalytics(original, "invalid", "", ""); err == nil {
		t.Fatal("invalid pricing accepted")
	}
}

func TestFilterAnalyticsPreservesUnfilteredServerAggregates(t *testing.T) {
	data := analyticsFixture()
	// Server aggregates may include historical products omitted from product rows.
	data.TotalsInRange = AnalyticsMetric{999, 999}
	data.TotalsPerDay[0].Revenue = 999
	for _, pricing := range []string{"", "all"} {
		got, err := FilterAnalytics(data, pricing, "", "")
		if err != nil || !reflect.DeepEqual(got, data) {
			t.Fatal("unfiltered server aggregates changed", err)
		}
		got.TotalsPerDay[0].Revenue = -1
		got.Products[0].Days[0].Revenue = -1
		got.Products[0].Name = "changed"
		if data.TotalsPerDay[0].Revenue != 999 || data.Products[0].Days[0].Revenue != 100 || data.Products[0].Name != "Synthetic paid" {
			t.Fatal("unfiltered output aliases input")
		}
	}
	got, err := FilterAnalytics(data, "all", "paid-one", "")
	if err != nil || got.TotalsInRange != (AnalyticsMetric{75, 2}) || got.TotalsPerDay[0].Revenue != 100 {
		t.Fatal("filtered aggregates not recomputed", err)
	}
}

func TestFilterAnalyticsOverflowAndUnknownDay(t *testing.T) {
	data := analyticsFixture()
	data.Products[0].TotalsInRange.Revenue = math.MaxInt64
	data.Products[1].TotalsInRange.Revenue = 1
	data.Products[1].Extension = "plugin"
	if _, err := FilterAnalytics(data, "all", "", "plugin"); err == nil {
		t.Fatal("overflow accepted")
	}
	data = analyticsFixture()
	data.Products[0].Days[0].Date = "2026-01-04"
	if _, err := FilterAnalytics(data, "all", "", "plugin"); err == nil {
		t.Fatal("unknown organization day accepted")
	}
	data = analyticsFixture()
	data.Products[0].TotalsInRange.Revenue = -100
	data.Products[1].TotalsInRange.Revenue = -25
	data.Products[1].Extension = "plugin"
	out, err := FilterAnalytics(data, "all", "", "plugin")
	if err != nil || out.TotalsInRange.Revenue != -125 {
		t.Fatal("negative total lost", err)
	}
}
