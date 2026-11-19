// Package analytics resolves inclusive date ranges for the daily analytics API.
package analytics

import (
	"errors"
	"fmt"
	"time"
)

const HistoryStart = "2025-10-01"

const dateLayout = "2006-01-02"

// RangeOptions accepts one selector family: explicit dates, days, months, or range.
// Zero counts mean unset. Callers must reject explicitly supplied zero flags.
type RangeOptions struct {
	Start, End, Range string
	Days, Months      int
}

// DateRange retains the requested bounds when available history clips the start.
type DateRange struct {
	Start          string `json:"start"`
	End            string `json:"end"`
	RequestedStart string `json:"requested_start"`
	RequestedEnd   string `json:"requested_end"`
	Truncated      bool   `json:"truncated"`
}

// ResolveRange uses the local calendar date in now. Relative periods end yesterday,
// so they contain complete days only. Months preserve and clamp the day of month.
func ResolveRange(opts RangeOptions, now time.Time) (DateRange, error) {
	if opts.Days < 0 || opts.Days > 365000 {
		return DateRange{}, errors.New("--days must be a positive count no greater than 365000")
	}
	if opts.Months < 0 || opts.Months > 12000 {
		return DateRange{}, errors.New("--months must be a positive count no greater than 12000")
	}
	selectors := 0
	for _, present := range []bool{opts.Start != "" || opts.End != "", opts.Days != 0, opts.Months != 0, opts.Range != ""} {
		if present {
			selectors++
		}
	}
	if selectors > 1 {
		return DateRange{}, errors.New("choose only one of --start/--end, --days, --months, or --range")
	}
	// Calendar arithmetic in UTC avoids elapsed-hour and DST assumptions. The
	// initial year, month, and day still come from now's own location.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)
	start, _ := time.Parse(dateLayout, HistoryStart)
	end := yesterday
	switch {
	case opts.Days != 0:
		start = today.AddDate(0, 0, -opts.Days)
	case opts.Months != 0:
		start = subtractMonths(today, opts.Months)
	case opts.Range != "":
		switch opts.Range {
		case "yesterday":
			start = yesterday
		case "last-7-days", "7d":
			start = today.AddDate(0, 0, -7)
		case "last-30-days", "30d":
			start = today.AddDate(0, 0, -30)
		case "last-60-days", "60d":
			start = today.AddDate(0, 0, -60)
		case "last-90-days", "90d":
			start = today.AddDate(0, 0, -90)
		case "this-month":
			start = time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
		case "last-month":
			end = time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
			start = time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC)
		case "this-year":
			start = time.Date(today.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
		case "last-year":
			start = time.Date(today.Year()-1, time.January, 1, 0, 0, 0, 0, time.UTC)
			end = time.Date(today.Year(), time.January, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
		case "all-time", "all":
		default:
			return DateRange{}, fmt.Errorf("unknown analytics range %q", opts.Range)
		}
	default:
		var err error
		if opts.Start != "" {
			start, err = parseDate(opts.Start)
			if err != nil {
				return DateRange{}, fmt.Errorf("--start: %w", err)
			}
		}
		if opts.End != "" {
			end, err = parseDate(opts.End)
			if err != nil {
				return DateRange{}, fmt.Errorf("--end: %w", err)
			}
		}
	}
	history, _ := time.Parse(dateLayout, HistoryStart)
	if end.Before(history) {
		return DateRange{}, fmt.Errorf("requested period ends before available daily history begins on %s", HistoryStart)
	}
	if start.After(end) {
		if opts.Range == "this-month" || opts.Range == "this-year" {
			return DateRange{}, fmt.Errorf("--range %s has no complete days yet", opts.Range)
		}
		return DateRange{}, errors.New("provide inclusive dates with start <= end")
	}
	result := DateRange{Start: start.Format(dateLayout), End: end.Format(dateLayout), RequestedStart: start.Format(dateLayout), RequestedEnd: end.Format(dateLayout)}
	if start.Before(history) {
		result.Start = HistoryStart
		result.Truncated = true
	}
	return result, nil
}

func parseDate(value string) (time.Time, error) {
	date, err := time.Parse(dateLayout, value)
	if err != nil || date.Year() < 1 || date.Format(dateLayout) != value {
		return time.Time{}, errors.New("use a valid YYYY-MM-DD date")
	}
	return date, nil
}

func subtractMonths(today time.Time, months int) time.Time {
	first := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -months, 0)
	lastDay := first.AddDate(0, 1, -1).Day()
	day := today.Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC)
}
