package maker

import (
	"context"
	"errors"
	"sort"
)

// CatalogPricing records only explicit pricing evidence from owned variants.
// Nil IsFree means absent, malformed, or conflicting evidence.
type CatalogPricing struct {
	ID       string                  `json:"id"`
	Variants []CatalogVariantPricing `json:"variants"`
}
type CatalogVariantPricing struct {
	ID     string `json:"id"`
	IsFree *bool  `json:"is_free"`
}

func (c *Client) CatalogMetadata(ctx context.Context, org string) ([]CatalogPricing, error) {
	if safeID(org) != nil {
		return nil, errors.New("invalid Maker organization")
	}
	page, err := c.get(ctx, "/"+org+"/products")
	if err != nil {
		return nil, err
	}
	return parseCatalogPricing(page)
}

func parseCatalogPricing(page []byte) ([]CatalogPricing, error) {
	type evidence struct {
		value   *bool
		seen    bool
		invalid bool
	}
	products := map[string]map[string]*evidence{}
	order := []string{}
	err := walkListingPage(page, func(m map[string]any, resolve func(any) any) error {
		id, _ := resolve(m["id"]).(string)
		name, _ := resolve(m["name"]).(string)
		variants, ok := resolve(m["variants"]).([]any)
		if id == "" || name == "" || !ok {
			return nil
		}
		if products[id] == nil {
			products[id] = map[string]*evidence{}
			order = append(order, id)
		}
		for _, raw := range variants {
			v, ok := resolve(raw).(map[string]any)
			if !ok {
				continue
			}
			vid, _ := resolve(v["id"]).(string)
			if vid == "" {
				continue
			}
			e := products[id][vid]
			if e == nil {
				e = &evidence{}
				products[id][vid] = e
			}
			value, ok := resolve(v["is_free"]).(bool)
			if !ok {
				e.invalid = true
				continue
			}
			if e.seen && e.value != nil && *e.value != value {
				e.invalid = true
			}
			copyValue := value
			e.value = &copyValue
			e.seen = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := []CatalogPricing{}
	for _, id := range order {
		p := CatalogPricing{ID: id, Variants: []CatalogVariantPricing{}}
		for vid, e := range products[id] {
			var b *bool
			if !e.invalid && e.seen {
				b = e.value
			}
			p.Variants = append(p.Variants, CatalogVariantPricing{ID: vid, IsFree: b})
		}
		sort.Slice(p.Variants, func(i, j int) bool { return p.Variants[i].ID < p.Variants[j].ID })
		out = append(out, p)
	}
	return out, nil
}
