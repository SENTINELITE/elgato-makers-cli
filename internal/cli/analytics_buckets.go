package cli

import (
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

type analyticsProductBuckets struct {
	ProductID string                  `json:"product_id"`
	Name      string                  `json:"name"`
	Extension string                  `json:"extension"`
	IsFree    bool                    `json:"is_free"`
	Buckets   []analyticsBucketReport `json:"buckets"`
}

func analyticsInterval(view, requested string, explicit bool) (string, error) {
	implied := map[string]string{"daily": "day", "weekly": "week", "monthly": "month", "summary": "all"}[view]
	if explicit && requested != "day" && requested != "week" && requested != "month" && requested != "all" {
		return "", errors.New("--group-by must be day, week, month, or all")
	}
	if implied != "" && explicit && implied != requested {
		return "", fmt.Errorf("%s already groups by %s; use analytics --group-by %s", view, implied, requested)
	}
	if explicit {
		return requested, nil
	}
	return implied, nil
}

func (r *analyticsReport) aggregate(view string) error {
	var err error
	if view == "products" {
		r.ProductBuckets, err = productAnalyticsBuckets(r.Data, r.GroupBy)
		if err != nil {
			return err
		}
		if r.Previous != nil {
			r.PreviousProductBuckets, err = productAnalyticsBuckets(*r.Previous, r.GroupBy)
		}
	} else {
		r.Buckets, err = estimatedBuckets(r.Data, r.GroupBy)
		if err != nil {
			return err
		}
		if r.Previous != nil {
			r.PreviousBuckets, err = estimatedBuckets(*r.Previous, r.GroupBy)
		}
	}
	return err
}

func productAnalyticsBuckets(data maker.AnalyticsData, interval string) ([]analyticsProductBuckets, error) {
	result := make([]analyticsProductBuckets, 0, len(data.Products))
	for _, p := range data.Products {
		buckets, err := estimatedBuckets(maker.AnalyticsData{TimeRange: data.TimeRange, TotalsInRange: p.TotalsInRange, TotalsPerDay: p.Days}, interval)
		if err != nil {
			return nil, fmt.Errorf("product %s: %w", p.ProductID, err)
		}
		result = append(result, analyticsProductBuckets{ProductID: p.ProductID, Name: p.Name, Extension: p.Extension, IsFree: p.IsFree, Buckets: buckets})
	}
	return result, nil
}

type analyticsBucketSet struct {
	label    string
	buckets  []analyticsBucketReport
	products []analyticsProductBuckets
}

func (r analyticsReport) bucketSets() []analyticsBucketSet {
	result := []analyticsBucketSet{{"current", r.Buckets, r.ProductBuckets}}
	if r.Previous != nil {
		result = append(result, analyticsBucketSet{"previous", r.PreviousBuckets, r.PreviousProductBuckets})
	}
	return result
}

func (a *app) analyticsBucketTable(report analyticsReport, view string) error {
	w := tabwriter.NewWriter(a.o.Out, 0, 4, 2, ' ', 0)
	if view == "products" {
		fmt.Fprint(w, "PRODUCT ID\tNAME\tFREE\t")
	}
	fmt.Fprintln(w, "PERIOD\tSTART\tEND\tPARTIAL\tUNITS\tSALES (USD)\tEST. YOUR SHARE (USD)")
	for _, set := range report.bucketSets() {
		write := func(prefix string, rows []analyticsBucketReport) {
			for _, b := range rows {
				fmt.Fprintf(w, "%s%s\t%s\t%s\t%t\t%d\t%s\t%s\n", prefix, set.label, b.Start, b.End, b.Partial, b.Adoptions, dollars(b.Revenue), dollars(b.EstimatedCreatorRevenueCents))
			}
		}
		if view == "products" {
			for _, p := range set.products {
				write(fmt.Sprintf("%s\t%s\t%t\t", p.ProductID, p.Name, p.IsFree), p.Buckets)
			}
		} else {
			write("", set.buckets)
		}
	}
	return w.Flush()
}

func (a *app) analyticsBucketCSV(report analyticsReport, view string) error {
	w := csv.NewWriter(a.o.Out)
	var err error
	write := func(row []string) {
		if err == nil {
			err = w.Write(row)
		}
	}
	header := []string{"period", "start", "end", "partial", "adoptions", "revenue_cents", "currency", "estimated_creator_revenue_cents"}
	// Keep the existing daily CSV column names while filling sparse dates with zero.
	legacyDaily := view == "daily"
	if legacyDaily {
		header = []string{"period", "date", "adoptions", "revenue_cents", "currency", "estimated_creator_revenue_cents"}
	}
	if view == "products" {
		header = append([]string{"product_id", "name", "extension", "is_free"}, header...)
	}
	write(header)
	for _, set := range report.bucketSets() {
		rows := func(prefix []string, buckets []analyticsBucketReport) {
			for _, b := range buckets {
				row := []string{set.label, b.Start, b.End, strconv.FormatBool(b.Partial), strconv.FormatInt(b.Adoptions, 10), strconv.FormatInt(b.Revenue, 10), "USD", strconv.FormatInt(b.EstimatedCreatorRevenueCents, 10)}
				if legacyDaily {
					row = []string{set.label, b.Start, strconv.FormatInt(b.Adoptions, 10), strconv.FormatInt(b.Revenue, 10), "USD", strconv.FormatInt(b.EstimatedCreatorRevenueCents, 10)}
				}
				write(append(append([]string{}, prefix...), row...))
			}
		}
		if view == "products" {
			for _, p := range set.products {
				rows([]string{p.ProductID, p.Name, p.Extension, strconv.FormatBool(p.IsFree)}, p.Buckets)
			}
		} else {
			rows(nil, set.buckets)
		}
	}
	w.Flush()
	return errors.Join(err, w.Error())
}
