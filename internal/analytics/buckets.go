package analytics

import (
	"fmt"
	"math"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

// Bucket reports inclusive dates and integer USD cents. Partial marks a week or
// month whose calendar boundaries extend beyond the requested range.
type Bucket struct {
	Start     string `json:"start"`
	End       string `json:"end"`
	Adoptions int64  `json:"adoptions"`
	Revenue   int64  `json:"revenue"`
	Partial   bool   `json:"partial"`
}

const bucketDateLayout = "2006-01-02"

// Bound zero filling to about a century, including leap years.
const maxBucketDays = 36600

// Aggregate groups daily metrics into day, Monday-Sunday week, or calendar month
// buckets. Missing days count as zero. The all interval uses the authoritative
// server totals, which can differ from the sum of daily metrics.
func Aggregate(data maker.AnalyticsData, interval string) ([]Bucket, error) {
	if interval != "day" && interval != "week" && interval != "month" && interval != "all" {
		return nil, fmt.Errorf("analytics interval must be day, week, month, or all")
	}
	start, err := bucketDate(data.TimeRange.Start)
	if err != nil {
		return nil, fmt.Errorf("invalid analytics range start: %w", err)
	}
	end, err := bucketDate(data.TimeRange.End)
	if err != nil || end.Before(start) {
		return nil, fmt.Errorf("analytics range requires valid YYYY-MM-DD dates with start on or before end")
	}
	if end.After(start.AddDate(0, 0, maxBucketDays-1)) {
		return nil, fmt.Errorf("analytics range exceeds %d days", maxBucketDays)
	}
	days := make(map[string]maker.AnalyticsDay, len(data.TotalsPerDay))
	for _, row := range data.TotalsPerDay {
		date, err := bucketDate(row.Date)
		if err != nil || date.Before(start) || date.After(end) {
			return nil, fmt.Errorf("invalid or out-of-range analytics day %q", row.Date)
		}
		if _, exists := days[row.Date]; exists {
			return nil, fmt.Errorf("duplicate analytics day %q", row.Date)
		}
		days[row.Date] = row
	}
	if interval == "all" {
		return []Bucket{{Start: data.TimeRange.Start, End: data.TimeRange.End,
			Adoptions: data.TotalsInRange.Adoptions, Revenue: data.TotalsInRange.Revenue}}, nil
	}
	result := make([]Bucket, 0)
	for current := start; !current.After(end); {
		calendarStart, calendarEnd := current, current
		switch interval {
		case "week":
			mondayOffset := (int(current.Weekday()) + 6) % 7
			calendarStart = current.AddDate(0, 0, -mondayOffset)
			calendarEnd = calendarStart.AddDate(0, 0, 6)
		case "month":
			calendarStart = time.Date(current.Year(), current.Month(), 1, 0, 0, 0, 0, time.UTC)
			calendarEnd = calendarStart.AddDate(0, 1, -1)
		}
		actualEnd := calendarEnd
		if end.Before(actualEnd) {
			actualEnd = end
		}
		bucket := Bucket{Start: current.Format(bucketDateLayout), End: actualEnd.Format(bucketDateLayout),
			Partial: current.After(calendarStart) || actualEnd.Before(calendarEnd)}
		for date := current; !date.After(actualEnd); date = date.AddDate(0, 0, 1) {
			row := days[date.Format(bucketDateLayout)]
			bucket.Revenue, err = bucketAdd(bucket.Revenue, row.Revenue)
			if err != nil {
				return nil, fmt.Errorf("analytics revenue in bucket %s: %w", bucket.Start, err)
			}
			bucket.Adoptions, err = bucketAdd(bucket.Adoptions, row.Adoptions)
			if err != nil {
				return nil, fmt.Errorf("analytics adoptions in bucket %s: %w", bucket.Start, err)
			}
		}
		result = append(result, bucket)
		current = actualEnd.AddDate(0, 0, 1)
	}
	return result, nil
}

func bucketDate(value string) (time.Time, error) {
	date, err := time.Parse(bucketDateLayout, value)
	if err != nil || date.Year() < 1 || date.Format(bucketDateLayout) != value {
		return time.Time{}, fmt.Errorf("%q must be YYYY-MM-DD", value)
	}
	return date, nil
}

func bucketAdd(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("sum exceeds signed integer range")
	}
	return a + b, nil
}
