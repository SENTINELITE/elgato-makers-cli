package maker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"time"
)

// AnalyticsMetric reports revenue in integer USD cents and product adoptions.
// Revenue can be negative when refunds exceed sales.
type AnalyticsMetric struct {
	Revenue   int64 `json:"revenue"`
	Adoptions int64 `json:"adoptions"`
}
type Metric = AnalyticsMetric
type AnalyticsTimeRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}
type TimeRange = AnalyticsTimeRange
type AnalyticsDay struct {
	Date      string `json:"date"`
	Revenue   int64  `json:"revenue"`
	Adoptions int64  `json:"adoptions"`
}
type AnalyticsProduct struct {
	ProductID     string          `json:"product_id"`
	Name          string          `json:"name"`
	Extension     string          `json:"extension"`
	AppIcon       string          `json:"app_icon"`
	Thumbnail     string          `json:"thumbnail"`
	PublishDate   string          `json:"publish_date"`
	IsFree        bool            `json:"is_free"`
	TotalsInRange AnalyticsMetric `json:"totals_in_range"`
	Days          []AnalyticsDay  `json:"days"`
}
type AnalyticsData struct {
	TimeRange     AnalyticsTimeRange `json:"time_range"`
	TotalsInRange AnalyticsMetric    `json:"totals_in_range"`
	TotalsPerDay  []AnalyticsDay     `json:"totals_per_day"`
	Products      []AnalyticsProduct `json:"products"`
}

func analyticsDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return err == nil && t.Format("2006-01-02") == s
}
func analyticsRange(start, end string) error {
	if !analyticsDate(start) || !analyticsDate(end) || start > end {
		return errors.New("analytics dates must be YYYY-MM-DD with start on or before end")
	}
	return nil
}

// AnalyticsRangeError carries only validated calendar dates, never response contents.
type AnalyticsRangeError struct{ Requested, Returned AnalyticsTimeRange }

func (e *AnalyticsRangeError) Error() string {
	return "Maker analytics returned a different date range"
}

// Analytics fetches the inclusive interval without changing remote state.
func (c *Client) Analytics(ctx context.Context, org, start, end string) (AnalyticsData, error) {
	if err := safeID(org); err != nil {
		return AnalyticsData{}, err
	}
	if err := analyticsRange(start, end); err != nil {
		return AnalyticsData{}, err
	}
	q := url.Values{"start": {start}, "end": {end}}
	b, err := c.get(ctx, "/api/organizations/"+org+"/analytics?"+q.Encode())
	if err != nil {
		return AnalyticsData{}, err
	}
	data, err := parseAnalytics(b)
	if err != nil {
		return AnalyticsData{}, errors.New("unrecognized Maker analytics response")
	}
	if data.TimeRange.Start != start || data.TimeRange.End != end {
		return AnalyticsData{}, &AnalyticsRangeError{Requested: AnalyticsTimeRange{Start: start, End: end}, Returned: data.TimeRange}
	}
	return data, nil
}

// Required fields must be present and non-null; ordinary struct decoding alone
// would silently turn missing metrics or pricing flags into zero values.
func analyticsObject(b json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	err := json.Unmarshal(b, &obj)
	if err != nil || obj == nil {
		return nil, errors.New("expected analytics object")
	}
	return obj, nil
}
func analyticsRequired[T any](obj map[string]json.RawMessage, key string) (T, error) {
	var value T
	b, ok := obj[key]
	if !ok || string(b) == "null" {
		return value, fmt.Errorf("missing analytics %s", key)
	}
	err := json.Unmarshal(b, &value)
	return value, err
}
func parseAnalyticsMetric(b json.RawMessage) (AnalyticsMetric, error) {
	obj, err := analyticsObject(b)
	if err != nil {
		return AnalyticsMetric{}, err
	}
	revenue, err := analyticsRequired[int64](obj, "revenue")
	if err != nil {
		return AnalyticsMetric{}, err
	}
	adoptions, err := analyticsRequired[int64](obj, "adoptions")
	return AnalyticsMetric{revenue, adoptions}, err
}
func parseAnalyticsDays(b json.RawMessage, start, end string) ([]AnalyticsDay, error) {
	var rows []json.RawMessage
	if len(b) == 0 || string(b) == "null" || json.Unmarshal(b, &rows) != nil {
		return nil, errors.New("missing analytics days")
	}
	days := make([]AnalyticsDay, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		obj, err := analyticsObject(row)
		if err != nil {
			return nil, err
		}
		date, err := analyticsRequired[string](obj, "date")
		if err != nil || !analyticsDate(date) || date < start || date > end || seen[date] {
			return nil, errors.New("invalid analytics day")
		}
		seen[date] = true
		metric, err := parseAnalyticsMetric(row)
		if err != nil {
			return nil, err
		}
		days = append(days, AnalyticsDay{date, metric.Revenue, metric.Adoptions})
	}
	return days, nil
}
func parseAnalytics(b []byte) (AnalyticsData, error) {
	var out AnalyticsData
	obj, err := analyticsObject(b)
	if err != nil {
		return out, err
	}
	rangeObj, err := analyticsObject(obj["time_range"])
	if err != nil {
		return out, err
	}
	out.TimeRange.Start, err = analyticsRequired[string](rangeObj, "start")
	if err != nil {
		return out, err
	}
	out.TimeRange.End, err = analyticsRequired[string](rangeObj, "end")
	if err != nil {
		return out, err
	}
	if err = analyticsRange(out.TimeRange.Start, out.TimeRange.End); err != nil {
		return out, err
	}
	out.TotalsInRange, err = parseAnalyticsMetric(obj["totals_in_range"])
	if err != nil {
		return out, err
	}
	out.TotalsPerDay, err = parseAnalyticsDays(obj["totals_per_day"], out.TimeRange.Start, out.TimeRange.End)
	if err != nil {
		return out, err
	}
	products, err := analyticsRequired[[]json.RawMessage](obj, "products")
	if err != nil {
		return out, err
	}
	out.Products = make([]AnalyticsProduct, 0, len(products))
	seen := map[string]bool{}
	for _, row := range products {
		p, err := analyticsObject(row)
		if err != nil {
			return out, err
		}
		var product AnalyticsProduct
		for key, dest := range map[string]*string{"product_id": &product.ProductID, "name": &product.Name, "extension": &product.Extension} {
			*dest, err = analyticsRequired[string](p, key)
			if err != nil || *dest == "" {
				return out, errors.New("invalid analytics product")
			}
		}
		if seen[product.ProductID] {
			return out, errors.New("duplicate analytics product")
		}
		seen[product.ProductID] = true
		for key, dest := range map[string]*string{"app_icon": &product.AppIcon, "thumbnail": &product.Thumbnail, "publish_date": &product.PublishDate} {
			if raw, ok := p[key]; ok && string(raw) != "null" {
				if err = json.Unmarshal(raw, dest); err != nil {
					return out, err
				}
			}
		}
		product.IsFree, err = analyticsRequired[bool](p, "is_free")
		if err != nil {
			return out, err
		}
		product.TotalsInRange, err = parseAnalyticsMetric(p["totals_in_range"])
		if err != nil {
			return out, err
		}
		product.Days, err = parseAnalyticsDays(p["days"], out.TimeRange.Start, out.TimeRange.End)
		if err != nil {
			return out, err
		}
		out.Products = append(out.Products, product)
	}
	return out, nil
}

func analyticsAdd(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, errors.New("analytics aggregate exceeds integer range")
	}
	return a + b, nil
}

// FilterAnalytics preserves product and daily ordering while deriving aggregates
// from the selected products. With no filters, server aggregates remain authoritative.
// Empty daily buckets remain visible.
func FilterAnalytics(data AnalyticsData, pricing, productID, extension string) (AnalyticsData, error) {
	if pricing == "" {
		pricing = "all"
	}
	if pricing != "all" && pricing != "free" && pricing != "paid" {
		return AnalyticsData{}, errors.New("analytics type must be all, free, or paid")
	}
	if pricing == "all" && productID == "" && extension == "" {
		out := data
		out.TotalsPerDay = append([]AnalyticsDay{}, data.TotalsPerDay...)
		out.Products = make([]AnalyticsProduct, len(data.Products))
		for i, product := range data.Products {
			out.Products[i] = product
			out.Products[i].Days = append([]AnalyticsDay{}, product.Days...)
		}
		return out, nil
	}
	out := AnalyticsData{TimeRange: data.TimeRange, Products: []AnalyticsProduct{}, TotalsPerDay: make([]AnalyticsDay, len(data.TotalsPerDay))}
	index := map[string]int{}
	for i, day := range data.TotalsPerDay {
		out.TotalsPerDay[i].Date = day.Date
		index[day.Date] = i
	}
	var err error
	for _, p := range data.Products {
		if pricing == "free" && !p.IsFree || pricing == "paid" && p.IsFree || productID != "" && p.ProductID != productID || extension != "" && p.Extension != extension {
			continue
		}
		copyProduct := p
		copyProduct.Days = append([]AnalyticsDay{}, p.Days...)
		out.Products = append(out.Products, copyProduct)
		out.TotalsInRange.Revenue, err = analyticsAdd(out.TotalsInRange.Revenue, p.TotalsInRange.Revenue)
		if err != nil {
			return AnalyticsData{}, err
		}
		out.TotalsInRange.Adoptions, err = analyticsAdd(out.TotalsInRange.Adoptions, p.TotalsInRange.Adoptions)
		if err != nil {
			return AnalyticsData{}, err
		}
		for _, day := range p.Days {
			i, ok := index[day.Date]
			if !ok {
				return AnalyticsData{}, errors.New("product day absent from organization analytics")
			}
			out.TotalsPerDay[i].Revenue, err = analyticsAdd(out.TotalsPerDay[i].Revenue, day.Revenue)
			if err != nil {
				return AnalyticsData{}, err
			}
			out.TotalsPerDay[i].Adoptions, err = analyticsAdd(out.TotalsPerDay[i].Adoptions, day.Adoptions)
			if err != nil {
				return AnalyticsData{}, err
			}
		}
	}
	return out, nil
}
