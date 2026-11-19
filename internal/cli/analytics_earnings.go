package cli

import (
	"github.com/SENTINELITE/elgato-makers-cli/internal/analytics"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

// Estimates are derived locally. Provider data remains unchanged.
type analyticsEarnings struct {
	Estimated           bool                     `json:"estimated"`
	PlatformFeePercent  int                      `json:"assumed_platform_fee_percent"`
	CreatorSharePercent int                      `json:"creator_share_percent"`
	Rounding            string                   `json:"rounding"`
	Current             analyticsPeriodEarnings  `json:"current"`
	Previous            *analyticsPeriodEarnings `json:"previous,omitempty"`
}
type analyticsPeriodEarnings struct {
	EstimatedCreatorRevenueCents int64                      `json:"estimated_creator_revenue_cents"`
	Products                     []analyticsProductEarnings `json:"products"`
	Days                         []analyticsDayEarnings     `json:"days"`
}
type analyticsProductEarnings struct {
	ProductID                    string `json:"product_id"`
	EstimatedCreatorRevenueCents int64  `json:"estimated_creator_revenue_cents"`
}
type analyticsDayEarnings struct {
	Date                         string `json:"date"`
	EstimatedCreatorRevenueCents int64  `json:"estimated_creator_revenue_cents"`
}
type analyticsBucketReport struct {
	analytics.Bucket
	EstimatedCreatorRevenueCents int64 `json:"estimated_creator_revenue_cents"`
}

func estimatePeriod(data maker.AnalyticsData) analyticsPeriodEarnings {
	e := analyticsPeriodEarnings{EstimatedCreatorRevenueCents: analytics.EstimatedCreatorRevenue(data.TotalsInRange.Revenue), Products: make([]analyticsProductEarnings, 0, len(data.Products)), Days: make([]analyticsDayEarnings, 0, len(data.TotalsPerDay))}
	for _, p := range data.Products {
		e.Products = append(e.Products, analyticsProductEarnings{p.ProductID, analytics.EstimatedCreatorRevenue(p.TotalsInRange.Revenue)})
	}
	for _, d := range data.TotalsPerDay {
		e.Days = append(e.Days, analyticsDayEarnings{d.Date, analytics.EstimatedCreatorRevenue(d.Revenue)})
	}
	return e
}
func (r *analyticsReport) estimateEarnings() {
	r.Earnings = analyticsEarnings{Estimated: true, PlatformFeePercent: analytics.PlatformFeePercent, CreatorSharePercent: analytics.CreatorSharePercent, Rounding: "nearest_cent_half_away_from_zero_per_row", Current: estimatePeriod(r.Data)}
	if r.Previous != nil {
		previous := estimatePeriod(*r.Previous)
		r.Earnings.Previous = &previous
	}
}
func estimatedBuckets(data maker.AnalyticsData, interval string) ([]analyticsBucketReport, error) {
	buckets, err := analytics.Aggregate(data, interval)
	if err != nil {
		return nil, err
	}
	result := make([]analyticsBucketReport, 0, len(buckets))
	for _, b := range buckets {
		result = append(result, analyticsBucketReport{b, analytics.EstimatedCreatorRevenue(b.Revenue)})
	}
	return result, nil
}
