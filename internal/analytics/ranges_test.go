package analytics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestResolveRange(t *testing.T) {
	tests := []struct {
		name, now, start, end string
		opts                  RangeOptions
	}{
		{"default", "2026-11-02", HistoryStart, "2026-11-01", RangeOptions{}},
		{"days", "2026-11-02", "2026-10-26", "2026-11-01", RangeOptions{Days: 7}},
		{"one day", "2026-11-02", "2026-11-01", "2026-11-01", RangeOptions{Days: 1}},
		{"month end", "2026-03-31", "2026-02-28", "2026-03-30", RangeOptions{Months: 1}},
		{"leap month end", "2028-03-31", "2028-02-29", "2028-03-30", RangeOptions{Months: 1}},
		{"months cross year", "2027-01-31", "2026-11-30", "2027-01-30", RangeOptions{Months: 2}},
		{"twelve months", "2029-02-28", "2028-02-28", "2029-02-27", RangeOptions{Months: 12}},
		{"leap day minus year", "2028-02-29", "2027-02-28", "2028-02-28", RangeOptions{Months: 12}},
		{"yesterday", "2026-11-02", "2026-11-01", "2026-11-01", RangeOptions{Range: "yesterday"}},
		{"seven", "2026-11-02", "2026-10-26", "2026-11-01", RangeOptions{Range: "last-7-days"}},
		{"thirty", "2026-11-02", "2026-10-03", "2026-11-01", RangeOptions{Range: "last-30-days"}},
		{"sixty", "2026-11-02", "2026-09-03", "2026-11-01", RangeOptions{Range: "last-60-days"}},
		{"ninety", "2026-11-02", "2026-08-04", "2026-11-01", RangeOptions{Range: "last-90-days"}},
		{"this month", "2026-11-02", "2026-11-01", "2026-11-01", RangeOptions{Range: "this-month"}},
		{"last month", "2026-03-01", "2026-02-01", "2026-02-28", RangeOptions{Range: "last-month"}},
		{"last leap month", "2028-03-20", "2028-02-01", "2028-02-29", RangeOptions{Range: "last-month"}},
		{"last month crosses year", "2027-01-01", "2026-12-01", "2026-12-31", RangeOptions{Range: "last-month"}},
		{"this year", "2026-11-02", "2026-01-01", "2026-11-01", RangeOptions{Range: "this-year"}},
		{"last year", "2027-01-01", "2026-01-01", "2026-12-31", RangeOptions{Range: "last-year"}},
		{"all time", "2026-11-02", HistoryStart, "2026-11-01", RangeOptions{Range: "all-time"}},
		{"only start", "2026-11-02", "2026-10-01", "2026-11-01", RangeOptions{Start: "2026-10-01"}},
		{"only end", "2026-11-02", HistoryStart, "2026-10-01", RangeOptions{End: "2026-10-01"}},
		{"explicit", "2026-11-02", "2026-01-01", "2026-01-31", RangeOptions{Start: "2026-01-01", End: "2026-01-31"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now, _ := time.Parse(dateLayout, tt.now)
			got, err := ResolveRange(tt.opts, now)
			if err != nil {
				t.Fatal(err)
			}
			want := DateRange{Start: tt.start, End: tt.end, RequestedStart: tt.start, RequestedEnd: tt.end}
			if got != want {
				t.Fatalf("got %+v; want %+v", got, want)
			}
		})
	}
}

func TestRangeCoverage(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for _, opts := range []RangeOptions{{Range: "last-year"}, {Start: "2025-01-01", End: "2025-12-31"}} {
		got, err := ResolveRange(opts, now)
		want := DateRange{Start: HistoryStart, End: "2025-12-31", RequestedStart: "2025-01-01", RequestedEnd: "2025-12-31", Truncated: true}
		if err != nil || got != want {
			t.Fatalf("got %+v, %v; want %+v", got, err, want)
		}
	}
	got, err := ResolveRange(RangeOptions{Days: 90}, time.Date(2025, 10, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || !got.Truncated || got.Start != HistoryStart || got.RequestedStart != "2025-07-04" {
		t.Fatalf("trailing period coverage: %+v, %v", got, err)
	}
}

func TestRangeErrors(t *testing.T) {
	tests := []struct {
		opts RangeOptions
		now  string
		text string
	}{
		{RangeOptions{Days: -1}, "2026-11-02", "--days"},
		{RangeOptions{Days: 365001}, "2026-11-02", "--days"},
		{RangeOptions{Months: -1}, "2026-11-02", "--months"},
		{RangeOptions{Months: 12001}, "2026-11-02", "--months"},
		{RangeOptions{Range: "invalid"}, "2026-11-02", "unknown"},
		{RangeOptions{Range: "this-month"}, "2026-11-01", "no complete days"},
		{RangeOptions{Range: "this-year"}, "2026-01-01", "no complete days"},
		{RangeOptions{Range: "last-year"}, "2025-11-02", "before available"},
		{RangeOptions{End: "2025-09-30"}, "2026-11-02", "before available"},
		{RangeOptions{Start: "2025-01-01", End: "2025-09-30"}, "2026-11-02", "before available"},
		{RangeOptions{Start: "2026-11-02", End: "2026-11-01"}, "2026-11-02", "start <= end"},
		{RangeOptions{Start: "2026-2-01"}, "2026-11-02", "YYYY-MM-DD"},
		{RangeOptions{Start: "2026-02-29"}, "2026-11-02", "YYYY-MM-DD"},
		{RangeOptions{Start: "2026-13-01"}, "2026-11-02", "YYYY-MM-DD"},
		{RangeOptions{End: "2026-01-32"}, "2026-11-02", "YYYY-MM-DD"},
		{RangeOptions{Start: "0000-01-01"}, "2026-11-02", "YYYY-MM-DD"},
		{RangeOptions{Days: 7, Months: 1}, "2026-11-02", "choose only one"},
		{RangeOptions{Days: 7, Range: "7d"}, "2026-11-02", "choose only one"},
		{RangeOptions{Months: 1, Range: "all"}, "2026-11-02", "choose only one"},
		{RangeOptions{Start: "2026-01-01", Days: 7}, "2026-11-02", "choose only one"},
		{RangeOptions{End: "2026-01-01", Months: 1}, "2026-11-02", "choose only one"},
		{RangeOptions{End: "2026-01-01", Range: "all"}, "2026-11-02", "choose only one"},
	}
	for _, tt := range tests {
		now, _ := time.Parse(dateLayout, tt.now)
		if _, err := ResolveRange(tt.opts, now); err == nil || !strings.Contains(err.Error(), tt.text) {
			t.Errorf("%+v at %s: got %v; want error containing %q", tt.opts, tt.now, err, tt.text)
		}
	}
}

func TestLocalDatesAndDST(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ now, start, end string }{
		{"2026-03-09T00:30:00-07:00", "2026-03-07", "2026-03-08"},
		{"2026-11-02T00:30:00-08:00", "2026-10-31", "2026-11-01"},
		{"2026-11-01T23:30:00-08:00", "2026-10-30", "2026-10-31"},
	} {
		now, _ := time.Parse(time.RFC3339, tt.now)
		got, err := ResolveRange(RangeOptions{Days: 2}, now.In(loc))
		if err != nil || got.Start != tt.start || got.End != tt.end {
			t.Errorf("%s: got %+v, %v", tt.now, got, err)
		}
	}
}

func TestRangeJSON(t *testing.T) {
	got, err := json.Marshal(DateRange{Start: "2025-10-01", End: "2025-12-31", RequestedStart: "2025-01-01", RequestedEnd: "2025-12-31", Truncated: true})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"start":"2025-10-01","end":"2025-12-31","requested_start":"2025-01-01","requested_end":"2025-12-31","truncated":true}`
	if string(got) != want {
		t.Fatalf("got %s; want %s", got, want)
	}
}
