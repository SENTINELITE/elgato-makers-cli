package maker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"time"
)

// Discount preserves provider values, including nullable limits and raw state.
type Discount map[string]any

func walkDiscountPage(page []byte, visit func(map[string]any, func(any) any) error) error {
	return walkFlightPage(page, visit, nil, func(data []byte, value any) error {
		if !json.Valid(data) {
			return errors.New("invalid discount JSON")
		}
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber() // Keep minor-unit values exact for monetary readback.
		return d.Decode(value)
	})
}

func discountPageNumber(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func discountRow(raw any, resolve func(any) any, org string) (Discount, error) {
	m, ok := resolve(raw).(map[string]any)
	if !ok {
		return nil, errors.New("invalid discount record")
	}
	var normalize func(any, int) (any, error)
	normalize = func(v any, depth int) (any, error) {
		if depth > 30 {
			return nil, errors.New("discount nesting exceeds limit")
		}
		switch v := resolve(v).(type) {
		case map[string]any:
			out := map[string]any{}
			for k, x := range v {
				if depth == 0 && k == "amount" {
					if amount, ok := resolve(x).(json.Number); ok {
						out[k] = amount
						continue
					}
				}
				n, e := normalize(x, depth+1)
				if e != nil {
					return nil, e
				}
				out[k] = n
			}
			return out, nil
		case []any:
			out := []any{}
			for _, x := range v {
				n, e := normalize(x, depth+1)
				if e != nil {
					return nil, e
				}
				out = append(out, n)
			}
			return out, nil
		case json.Number:
			return v.Float64()
		default:
			return v, nil
		}
	}
	n, e := normalize(m, 0)
	if e != nil {
		return nil, e
	}
	out := Discount(n.(map[string]any))
	id, _ := out["id"].(string)
	makers, ok := out["makers"].([]any)
	if safeID(id) != nil || !ok || len(makers) != 1 || makers[0] != org {
		return nil, errors.New("discount identity or organization mismatch")
	}
	if _, ok := out["enabled"].(bool); !ok {
		return nil, errors.New("discount enabled state missing")
	}
	if _, ok := out["state"].(string); !ok {
		return nil, errors.New("discount state missing")
	}
	c, ok := out["code"].(map[string]any)
	if !ok {
		return nil, errors.New("discount code missing")
	}
	for _, k := range []string{"maker_code", "code"} {
		if v, ok := c[k].(string); !ok || v == "" {
			return nil, errors.New("discount code identity missing")
		}
	}
	for _, k := range []string{"valid_from", "expires_at"} {
		v, exists := out[k]
		if !exists {
			return nil, errors.New("discount validity interval missing")
		}
		if v == nil && k == "expires_at" {
			continue
		}
		str, ok := v.(string)
		if !ok {
			return nil, errors.New("discount validity interval malformed")
		}
		if _, e := time.Parse(time.RFC3339, str); e != nil {
			return nil, errors.New("discount validity interval malformed")
		}
	}
	for _, k := range []string{"max_unique_users", "max_uses_per_user"} {
		v, ok := c[k]
		if !ok {
			return nil, errors.New("discount limit missing")
		}
		if v != nil {
			f, ok := v.(float64)
			if !ok || f < 1 || f != float64(int(f)) {
				return nil, errors.New("invalid discount limit")
			}
		}
	}
	return out, nil
}
func (c *Client) Discounts(ctx context.Context, org, status string) ([]Discount, error) {
	if safeID(org) != nil {
		return nil, errors.New("invalid organization")
	}
	if status == "upcoming" {
		status = "scheduled"
	}
	if status != "" && status != "scheduled" {
		return nil, errors.New("only scheduled/upcoming status filtering has a verified query contract")
	}
	out := []Discount{}
	seen := map[string]bool{}
	expectedTotal := -1
	expectedPages := 0
	for page := 1; page <= 1000; page++ {
		q := url.Values{}
		if status != "" {
			q.Set("status", status)
		}
		if page > 1 || status != "" {
			q.Set("page", strconv.Itoa(page))
		}
		path := "/" + org + "/discounts"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		b, e := c.get(ctx, path)
		if e != nil {
			return nil, e
		}
		var rows []Discount
		found := false
		pages, selected, total := 0, 0, -1
		e = walkDiscountPage(b, func(m map[string]any, resolve func(any) any) error {
			if v, ok := m["totalDiscounts"]; ok {
				f, ok := discountPageNumber(resolve(v))
				if !ok || f < 0 || f != float64(int(f)) {
					return errors.New("invalid discount total")
				}
				if total >= 0 && total != int(f) {
					return errors.New("conflicting discount totals")
				}
				total = int(f)
			}
			if _, ok := m["pageCount"]; ok {
				f, ok := discountPageNumber(resolve(m["pageCount"]))
				s, sok := discountPageNumber(resolve(m["selectedPage"]))
				if !ok || !sok || f < 1 || f != float64(int(f)) || s != float64(page) {
					return errors.New("unsupported discount pagination")
				}
				if pages != 0 && pages != int(f) {
					return errors.New("conflicting discount pagination")
				}
				pages, selected = int(f), int(s)
			}
			if raw, ok := m["discounts"]; ok {
				list, ok := resolve(raw).([]any)
				if !ok {
					return errors.New("discount collection missing")
				}
				v := []Discount{}
				for _, r := range list {
					d, e := discountRow(r, resolve, org)
					if e != nil {
						return e
					}
					v = append(v, d)
				}
				if found && !reflect.DeepEqual(v, rows) {
					return errors.New("conflicting discount collections")
				}
				rows, found = v, true
			}
			return nil
		})
		if e != nil {
			return nil, e
		}
		if !found || total < 0 || pages == 0 || selected != page {
			return nil, errors.New("discount list or pagination schema not recognized")
		}
		if expectedTotal >= 0 && total != expectedTotal {
			return nil, errors.New("discount total changed during pagination")
		}
		expectedTotal = total
		if expectedPages != 0 && expectedPages != pages {
			return nil, errors.New("discount pagination changed during read")
		}
		expectedPages = pages
		for _, d := range rows {
			id := d["id"].(string)
			if seen[id] {
				return nil, errors.New("duplicate discount across pages; list incomplete")
			}
			seen[id] = true
			out = append(out, d)
		}
		if page == pages {
			if status == "" && len(out) != total {
				return nil, errors.New("discount total does not match complete list")
			}
			return out, nil
		}
		if len(rows) == 0 {
			return nil, errors.New("empty discount page before last page")
		}
	}
	return nil, errors.New("discount pagination exceeds limit")
}
func (c *Client) Discount(ctx context.Context, org, id string) (Discount, error) {
	if safeID(org) != nil || safeID(id) != nil {
		return nil, errors.New("invalid discount resource identifier")
	}
	b, e := c.get(ctx, "/"+org+"/discounts/"+id)
	if e != nil {
		return nil, e
	}
	var found Discount
	e = walkDiscountPage(b, func(m map[string]any, r func(any) any) error {
		if r(m["id"]) != id {
			return nil
		}
		if _, ok := m["discount_type"]; !ok {
			return nil
		}
		d, e := discountRow(m, r, org)
		if e != nil {
			return e
		}
		if found != nil && !reflect.DeepEqual(found, d) {
			return errors.New("conflicting discount details")
		}
		found = d
		return nil
	})
	if e != nil {
		return nil, e
	}
	if found == nil {
		return nil, fmt.Errorf("discount detail schema not recognized for requested ID")
	}
	return found, nil
}
