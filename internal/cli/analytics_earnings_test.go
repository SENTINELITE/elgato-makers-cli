package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

func TestAnalyticsEarningsOutputAndIndependentRounding(t *testing.T) {
	data := maker.AnalyticsData{TimeRange: maker.TimeRange{Start: "2026-10-01", End: "2026-10-02"}, TotalsInRange: maker.Metric{Revenue: 10}, TotalsPerDay: []maker.AnalyticsDay{{Date: "2026-10-01", Revenue: 5}, {Date: "2026-10-02", Revenue: 5}}, Products: []maker.AnalyticsProduct{{ProductID: "paid", Name: "Demo", TotalsInRange: maker.Metric{Revenue: 10}, Days: []maker.AnalyticsDay{{Date: "2026-10-01", Revenue: 5}, {Date: "2026-10-02", Revenue: 5}}}}}
	previous := data
	previous.TotalsInRange.Revenue = -105
	for _, tc := range []struct {
		view, interval string
		want           int64
	}{{"overview", "all", 7}, {"daily", "day", 4}, {"weekly", "week", 7}, {"monthly", "month", 7}, {"products", "month", 7}} {
		t.Run(tc.view, func(t *testing.T) {
			report := analyticsReport{Data: data, Previous: &previous, GroupBy: tc.interval}
			report.estimateEarnings()
			if err := report.aggregate(tc.view); err != nil {
				t.Fatal(err)
			}
			if report.Earnings.Current.EstimatedCreatorRevenueCents != 7 || report.Earnings.Previous.EstimatedCreatorRevenueCents != -74 || report.Earnings.Current.Days[0].EstimatedCreatorRevenueCents != 4 || report.Earnings.Current.Products[0].EstimatedCreatorRevenueCents != 7 {
				t.Fatal("wrong current/previous earnings", report.Earnings)
			}
			// Buckets round once after aggregation, not the sum of individually rounded days.
			var out bytes.Buffer
			a := &app{o: Options{Out: &out}}
			if err := a.analyticsBucketCSV(report, tc.view); err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(&out).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if rows[0][len(rows[0])-1] != "estimated_creator_revenue_cents" || rows[1][len(rows[1])-1] != map[int64]string{4: "4", 7: "7"}[tc.want] {
				t.Fatal("wrong CSV creator amount", rows)
			}
			out.Reset()
			if err := a.analyticsBucketTable(report, tc.view); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "EST. YOUR SHARE (USD)") || !strings.Contains(out.String(), map[int64]string{4: "0.04", 7: "0.07"}[tc.want]) {
				t.Fatal("missing table estimate", out.String())
			}
			b, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(b, &decoded); err != nil {
				t.Fatal(err)
			}
			key := "buckets"
			if tc.view == "products" {
				key = "product_buckets"
			}
			rowsJSON := decoded[key].([]any)
			first := rowsJSON[0].(map[string]any)
			if tc.view == "products" {
				first = first["buckets"].([]any)[0].(map[string]any)
			}
			if first["estimated_creator_revenue_cents"] != float64(tc.want) || first["revenue"] == nil {
				t.Fatal("missing JSON gross/estimate", first)
			}
		})
	}
	if data.TotalsInRange.Revenue != 10 || data.TotalsPerDay[0].Revenue != 5 {
		t.Fatal("provider data mutated")
	}
}
