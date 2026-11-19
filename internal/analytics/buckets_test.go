package analytics

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

func bucketData(start, end string, days ...maker.AnalyticsDay) maker.AnalyticsData {
	return maker.AnalyticsData{TimeRange: maker.AnalyticsTimeRange{Start: start, End: end}, TotalsPerDay: days}
}

func TestAggregateCalendarBuckets(t *testing.T) {
	tests := []struct {
		name, interval, start, end string
		days                       []maker.AnalyticsDay
		want                       []Bucket
	}{
		{"leap day and sparse unsorted input", "day", "2024-02-28", "2024-03-01",
			[]maker.AnalyticsDay{{Date: "2024-03-01", Revenue: -125, Adoptions: -1}, {Date: "2024-02-28", Revenue: 250, Adoptions: 2}},
			[]Bucket{{Start: "2024-02-28", End: "2024-02-28", Revenue: 250, Adoptions: 2}, {Start: "2024-02-29", End: "2024-02-29"}, {Start: "2024-03-01", End: "2024-03-01", Revenue: -125, Adoptions: -1}}},
		{"ISO week crosses year", "week", "2020-12-28", "2021-01-10",
			[]maker.AnalyticsDay{{Date: "2021-01-01", Revenue: 123, Adoptions: 1}},
			[]Bucket{{Start: "2020-12-28", End: "2021-01-03", Revenue: 123, Adoptions: 1}, {Start: "2021-01-04", End: "2021-01-10"}}},
		{"partial weeks across month", "week", "2024-01-31", "2024-02-06",
			[]maker.AnalyticsDay{{Date: "2024-01-31", Revenue: 150}, {Date: "2024-02-01", Revenue: -25}, {Date: "2024-02-06", Adoptions: 2}},
			[]Bucket{{Start: "2024-01-31", End: "2024-02-04", Revenue: 125, Partial: true}, {Start: "2024-02-05", End: "2024-02-06", Adoptions: 2, Partial: true}}},
		{"partial months and full leap month", "month", "2023-12-31", "2024-03-01", nil,
			[]Bucket{{Start: "2023-12-31", End: "2023-12-31", Partial: true}, {Start: "2024-01-01", End: "2024-01-31"}, {Start: "2024-02-01", End: "2024-02-29"}, {Start: "2024-03-01", End: "2024-03-01", Partial: true}}},
		{"single Sunday", "week", "2024-02-04", "2024-02-04", nil,
			[]Bucket{{Start: "2024-02-04", End: "2024-02-04", Partial: true}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Aggregate(bucketData(test.start, test.end, test.days...), test.interval)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestAggregateAllUsesAuthoritativeTotals(t *testing.T) {
	data := bucketData("2024-01-01", "2024-01-02", maker.AnalyticsDay{Date: "2024-01-01", Revenue: math.MaxInt64}, maker.AnalyticsDay{Date: "2024-01-02", Revenue: 1})
	data.TotalsInRange = maker.AnalyticsMetric{Revenue: -123, Adoptions: 12}
	got, err := Aggregate(data, "all")
	if err != nil {
		t.Fatal(err)
	}
	want := []Bucket{{Start: "2024-01-01", End: "2024-01-02", Revenue: -123, Adoptions: 12}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"start":"2024-01-01","end":"2024-01-02","adoptions":12,"revenue":-123,"partial":false}` {
		t.Fatalf("unexpected JSON %s", b)
	}
}

func TestAggregateRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name, interval string
		data           maker.AnalyticsData
	}{
		{"bad interval", "weekly", bucketData("2024-01-01", "2024-01-02")},
		{"bad start", "day", bucketData("2024-1-01", "2024-01-02")},
		{"invalid calendar day", "day", bucketData("2023-02-29", "2023-03-01")},
		{"bad end", "day", bucketData("2024-01-01", "bad")},
		{"reversed", "day", bucketData("2024-01-02", "2024-01-01")},
		{"huge window", "day", bucketData("0001-01-01", "9999-12-31")},
		{"bad daily date", "week", bucketData("2024-01-01", "2024-01-02", maker.AnalyticsDay{Date: "bad"})},
		{"outside range", "all", bucketData("2024-01-01", "2024-01-02", maker.AnalyticsDay{Date: "2023-12-31"})},
		{"duplicate", "all", bucketData("2024-01-01", "2024-01-02", maker.AnalyticsDay{Date: "2024-01-01"}, maker.AnalyticsDay{Date: "2024-01-01"})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Aggregate(test.data, test.interval); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestAggregateDetectsOverflow(t *testing.T) {
	for _, metric := range []string{"revenue", "adoptions"} {
		for _, sign := range []int64{1, -1} {
			first := int64(math.MaxInt64)
			if sign < 0 {
				first = math.MinInt64
			}
			days := []maker.AnalyticsDay{{Date: "2024-01-01"}, {Date: "2024-01-02"}}
			if metric == "revenue" {
				days[0].Revenue, days[1].Revenue = first, sign
			} else {
				days[0].Adoptions, days[1].Adoptions = first, sign
			}
			if _, err := Aggregate(bucketData("2024-01-01", "2024-01-02", days...), "month"); err == nil {
				t.Fatalf("expected %s overflow for sign %d", metric, sign)
			}
		}
	}
}
