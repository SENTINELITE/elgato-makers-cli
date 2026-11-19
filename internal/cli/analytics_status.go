package cli

import (
	"fmt"
	"strings"
)

func analyticsSuccess(r analyticsReport, view, format string) string {
	count := len(r.Data.Products)
	if r.Previous != nil {
		count += len(r.Previous.Products)
	}
	if r.GroupBy != "" {
		count = 0
		for _, set := range r.bucketSets() {
			if view == "products" {
				for _, p := range set.products {
					count += len(p.Buckets)
				}
			} else {
				count += len(set.buckets)
			}
		}
	}
	kind := strings.ToUpper(format)
	action := fmt.Sprintf("wrote %d %s data rows to stdout", count, kind)
	if format == "json" {
		action = fmt.Sprintf("wrote JSON report to stdout (%d data rows)", count)
	}
	if format == "table" {
		action = fmt.Sprintf("wrote table output to stdout (%d data rows)", count)
	}
	if view == "products" && r.GroupBy != "" {
		view += " grouped by " + r.GroupBy
	}
	message := fmt.Sprintf("Success: analytics %s; %s; %s to %s inclusive; type: %s", view, action, r.Data.TimeRange.Start, r.Data.TimeRange.End, r.Pricing)
	if r.Previous != nil {
		message += fmt.Sprintf("; previous: %s to %s inclusive", r.Previous.TimeRange.Start, r.Previous.TimeRange.End)
	}
	return message + "."
}
