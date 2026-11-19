package cli

import (
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/analytics"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/spf13/cobra"
)

const analyticsHistoryStart = analytics.HistoryStart

type analyticsOptions struct {
	start, end, pricing, product, extension, format string
	compare                                         bool
	days, months                                    int
	dateRange, groupBy                              string
}
type analyticsReport struct {
	Earnings               analyticsEarnings         `json:"earnings"`
	Data                   maker.AnalyticsData       `json:"data"`
	Previous               *maker.AnalyticsData      `json:"previous,omitempty"`
	Currency               string                    `json:"currency"`
	RevenueUnit            string                    `json:"revenue_unit"`
	Pricing                string                    `json:"type"`
	Selection              analytics.DateRange       `json:"selection"`
	GroupBy                string                    `json:"group_by,omitempty"`
	Buckets                []analyticsBucketReport   `json:"buckets,omitempty"`
	PreviousBuckets        []analyticsBucketReport   `json:"previous_buckets,omitempty"`
	ProductBuckets         []analyticsProductBuckets `json:"product_buckets,omitempty"`
	PreviousProductBuckets []analyticsProductBuckets `json:"previous_product_buckets,omitempty"`
}

func dollars(cents int64) string {
	n := uint64(cents)
	sign := ""
	if cents < 0 {
		sign = "-"
		n = uint64(-(cents + 1)) + 1
	}
	return fmt.Sprintf("%s%d.%02d", sign, n/100, n%100)
}

func (a *app) analyticsCommand() *cobra.Command {
	o := &analyticsOptions{pricing: "all", format: "table"}
	r := &cobra.Command{Use: "analytics", Short: "Read all available analytics by default, or select dates and free/paid products", Args: cobra.NoArgs}
	r.PersistentFlags().StringVar(&o.start, "start", "", "Inclusive first date YYYY-MM-DD (default: available daily history start)")
	r.PersistentFlags().StringVar(&o.end, "end", "", "Inclusive last date YYYY-MM-DD (default: yesterday in the local timezone)")
	r.PersistentFlags().IntVarP(&o.days, "days", "d", 0, "Trailing N complete days, ending yesterday")
	r.PersistentFlags().IntVarP(&o.months, "months", "m", 0, "Rolling N calendar months, ending yesterday; clamp month-end dates")
	r.PersistentFlags().StringVar(&o.dateRange, "range", "", "Date preset: yesterday, last-7-days, last-30-days, last-60-days, last-90-days, this-month, last-month, this-year, last-year, all-time")
	r.PersistentFlags().StringVar(&o.groupBy, "group-by", "", "Group daily metrics by day, week (Monday), month, or all (whole-period total)")
	r.PersistentFlags().StringVar(&o.pricing, "type", "all", "Monetization filter: all, free, paid")
	r.PersistentFlags().StringVar(&o.product, "product", "", "Product alias or ID")
	r.PersistentFlags().StringVar(&o.extension, "extension", "", "Product extension slug, e.g. plugins")
	r.PersistentFlags().StringVar(&o.format, "format", "table", "Output: table, json, csv; revenue CSV values are integer cents")
	r.PersistentFlags().BoolVar(&o.compare, "compare-previous", false, "Fetch the immediately preceding equal-length date range")
	r.RunE = a.analyticsRun(o, "overview")
	for _, view := range []string{"overview", "products", "daily", "weekly", "monthly", "summary"} {
		child := &cobra.Command{Use: view, Short: "Read " + view + " analytics", Args: cobra.NoArgs, RunE: a.analyticsRun(o, view)}
		if view == "products" {
			child.Aliases = []string{"product"}
			for _, intervalView := range []string{"daily", "weekly", "monthly", "summary"} {
				child.AddCommand(&cobra.Command{Use: intervalView, Short: "Read " + intervalView + " analytics for each product", Args: cobra.NoArgs, RunE: a.analyticsRun(o, "products")})
			}
		}
		r.AddCommand(child)
	}
	return r
}

func (a *app) analyticsRun(o *analyticsOptions, view string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		if o.pricing != "all" && o.pricing != "free" && o.pricing != "paid" {
			return errors.New("--type must be all, free, or paid")
		}
		if o.format != "table" && o.format != "json" && o.format != "csv" {
			return errors.New("--format must be table, json, or csv")
		}
		if a.minify && cmd.Flags().Changed("format") && o.format != "json" {
			return errors.New("--minify requires JSON output; omit --format or use --format json")
		}
		if a.json && o.format == "csv" {
			return errors.New("choose --json or --format csv")
		}
		if (cmd.Flags().Changed("days") && o.days <= 0) || (cmd.Flags().Changed("months") && o.months <= 0) {
			return errors.New("--days and --months must be positive")
		}
		if cmd.Flags().Changed("range") && o.dateRange == "" {
			return errors.New("--range must name a date preset")
		}
		// Also reject explicitly supplied empty date flags or empty selectors mixed with another family.
		if (cmd.Flags().Changed("start") && o.start == "") || (cmd.Flags().Changed("end") && o.end == "") {
			return errors.New("--start and --end require YYYY-MM-DD dates")
		}
		intervalView := view
		if view == "products" && cmd.Parent() != nil && cmd.Parent().Name() == "products" {
			intervalView = cmd.Name()
		}
		interval, err := analyticsInterval(intervalView, o.groupBy, cmd.Flags().Changed("group-by"))
		if err != nil {
			return err
		}
		selection, err := analytics.ResolveRange(analytics.RangeOptions{Start: o.start, End: o.end, Range: o.dateRange, Days: o.days, Months: o.months}, a.o.Now())
		start, end := selection.Start, selection.End
		if err != nil {
			return err
		}
		if selection.Truncated {
			fmt.Fprintf(a.o.Err, "Warning: requested %s to %s; available history starts %s, so results cover %s to %s.\n", selection.RequestedStart, selection.RequestedEnd, analytics.HistoryStart, start, end)
		}
		var ps, pe string
		if o.compare {
			s, _ := time.Parse("2006-01-02", start)
			e, _ := time.Parse("2006-01-02", end)
			days := int(e.Sub(s)/(24*time.Hour)) + 1
			ps = s.AddDate(0, 0, -days).Format("2006-01-02")
			pe = s.AddDate(0, 0, -1).Format("2006-01-02")
			if ps < analyticsHistoryStart {
				return errors.New("previous comparison period predates available daily history; select a later --start")
			}
		}
		c, err := a.config()
		if err != nil {
			return err
		}
		org, err := a.organization(c)
		if err != nil {
			return err
		}
		product := o.product
		if t, ok := c.Products[product]; ok {
			if t.OrgID != "" && t.OrgID != org {
				return errors.New("product alias belongs to a different organization")
			}
			product = t.ProductID
		}
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return err
		}
		data, err := api.Analytics(cmd.Context(), org, start, end)
		if err != nil {
			return err
		}
		data, err = maker.FilterAnalytics(data, o.pricing, product, o.extension)
		if err != nil {
			return err
		}
		report := analyticsReport{Data: data, Currency: "USD", RevenueUnit: "cents", Pricing: o.pricing, Selection: selection, GroupBy: interval}
		if o.compare {
			previous, e := api.Analytics(cmd.Context(), org, ps, pe)
			if e != nil {
				return e
			}
			previous, e = maker.FilterAnalytics(previous, o.pricing, product, o.extension)
			if e != nil {
				return e
			}
			report.Previous = &previous
		}
		report.estimateEarnings()
		if interval != "" {
			if err := report.aggregate(view); err != nil {
				return err
			}
		}
		format := o.format
		if a.json {
			format = "json"
		}
		if err := a.renderAnalytics(report, view, format); err != nil {
			return fmt.Errorf("write analytics %s output: %w", format, err)
		}
		a.completion = analyticsSuccess(report, view, format)
		return nil
	}
}

func (a *app) renderAnalytics(report analyticsReport, view, format string) error {
	if format == "json" {
		return a.output(report)
	}
	if format == "csv" {
		return a.analyticsCSV(report, view)
	}
	data := report.Data
	if _, err := fmt.Fprintf(a.o.Out, "Period: %s to %s (inclusive); type: %s\n", data.TimeRange.Start, data.TimeRange.End, report.Pricing); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.o.Out, "Product units: %d\nTotal sales (USD): %s\nEstimated your share after 30%% fee (USD): %s\n", data.TotalsInRange.Adoptions, dollars(data.TotalsInRange.Revenue), dollars(analytics.EstimatedCreatorRevenue(data.TotalsInRange.Revenue))); err != nil {
		return err
	}
	if report.Previous != nil {
		p := report.Previous
		if _, err := fmt.Fprintf(a.o.Out, "Previous %s to %s: %d units, USD %s gross, estimated your share USD %s\n", p.TimeRange.Start, p.TimeRange.End, p.TotalsInRange.Adoptions, dollars(p.TotalsInRange.Revenue), dollars(analytics.EstimatedCreatorRevenue(p.TotalsInRange.Revenue))); err != nil {
			return err
		}
	}
	if report.GroupBy != "" {
		return a.analyticsBucketTable(report, view)
	}
	w := tabwriter.NewWriter(a.o.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PRODUCT ID\tNAME\tFREE\tUNITS\tSALES (USD)\tEST. YOUR SHARE (USD)")
	for _, p := range data.Products {
		fmt.Fprintf(w, "%s\t%s\t%t\t%d\t%s\t%s\n", p.ProductID, p.Name, p.IsFree, p.TotalsInRange.Adoptions, dollars(p.TotalsInRange.Revenue), dollars(analytics.EstimatedCreatorRevenue(p.TotalsInRange.Revenue)))
	}
	return w.Flush()
}

func (a *app) analyticsCSV(report analyticsReport, view string) error {
	if report.GroupBy != "" {
		return a.analyticsBucketCSV(report, view)
	}
	w := csv.NewWriter(a.o.Out)
	var err error
	write := func(row []string) {
		if err == nil {
			err = w.Write(row)
		}
	}
	write([]string{"period", "product_id", "name", "extension", "is_free", "adoptions", "revenue_cents", "currency", "estimated_creator_revenue_cents"})
	data := []struct {
		label string
		data  maker.AnalyticsData
	}{{"current", report.Data}}
	if report.Previous != nil {
		data = append(data, struct {
			label string
			data  maker.AnalyticsData
		}{"previous", *report.Previous})
	}
	for _, set := range data {
		for _, p := range set.data.Products {
			write([]string{set.label, p.ProductID, p.Name, p.Extension, strconv.FormatBool(p.IsFree), strconv.FormatInt(p.TotalsInRange.Adoptions, 10), strconv.FormatInt(p.TotalsInRange.Revenue, 10), "USD", strconv.FormatInt(analytics.EstimatedCreatorRevenue(p.TotalsInRange.Revenue), 10)})
		}
	}
	w.Flush()
	return errors.Join(err, w.Error())
}
