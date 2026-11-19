package maker

import (
	"context"
	"errors"
	"math"
	"net/url"
	"reflect"
	"strings"
	"time"
)

type ProductDetailPrice struct {
	Plan       *string `json:"plan"`
	Currency   *string `json:"currency"`
	UnitAmount any     `json:"unit_amount"`
	AmountUnit string  `json:"amount_unit"`
}
type ProductDetailVariant struct {
	ID     string                `json:"id"`
	UUID   *string               `json:"uuid"`
	Name   *string               `json:"name"`
	IsFree *bool                 `json:"is_free"`
	Prices *[]ProductDetailPrice `json:"prices"`
}
type ProductDetailLink struct {
	ID          *string `json:"id"`
	DisplayText *string `json:"display_text"`
	URL         *string `json:"url"`
	URLStatus   string  `json:"url_status"`
}
type ProductDetails struct {
	ID            string                 `json:"id"`
	Name          *string                `json:"name"`
	Description   *string                `json:"description"`
	Status        *string                `json:"status"`
	Variants      []ProductDetailVariant `json:"variants"`
	TagIDs        *[]string              `json:"tag_ids"`
	Languages     *[]ListingOption       `json:"languages"`
	Platforms     *[]ListingOption       `json:"platforms"`
	Links         *[]ProductDetailLink   `json:"links"`
	Coverage      map[string]string      `json:"coverage"`
	UnknownFields []string               `json:"unknown_fields"`
	SourceKind    string                 `json:"source_kind"`
	ObservedAt    string                 `json:"observed_at"`
}

// ProductDetails selects typed listing facts from the established owned product
// page. It never forwards arbitrary provider objects, media URLs or credentials.
func (c *Client) ProductDetails(ctx context.Context, org, product string) (ProductDetails, error) {
	if safeID(org) != nil || safeID(product) != nil || len(org) > 128 || len(product) > 128 || org == "." || org == ".." || product == "." || product == ".." {
		return ProductDetails{}, errors.New("invalid product details target")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	page, err := c.get(ctx, "/"+org+"/products/"+product)
	if err != nil {
		return ProductDetails{}, err
	}
	return parseProductDetails(page, org, product, time.Now().UTC())
}
func parseProductDetails(page []byte, org, product string, now time.Time) (ProductDetails, error) {
	out := ProductDetails{ID: product, Coverage: map[string]string{}, UnknownFields: []string{}, SourceKind: "authenticated_maker_product_page", ObservedAt: now.Format(time.RFC3339)}
	var found bool
	groups := map[string]map[string]ListingOption{}
	err := walkListingPage(page, func(m map[string]any, resolve func(any) any) error {
		// The existing Details editor contract identifies compatibility and languages
		// with selected tag IDs and tagsets. Do not infer labels from ID spelling.
		if raw, exists := m["tagsets"]; exists {
			list, ok := resolve(raw).([]any)
			if !ok || len(list) > 100 {
				return errors.New("unrecognized product tagsets")
			}
			for _, rawGroup := range list {
				group, ok := resolve(rawGroup).(map[string]any)
				if !ok {
					return errors.New("invalid product tagset")
				}
				slug, _ := resolve(group["slug"]).(string)
				if slug != "language" && slug != "os" {
					continue
				}
				tags, ok := resolve(group["tags"]).([]any)
				if !ok || len(tags) > 1000 {
					return errors.New("unrecognized product tag options")
				}
				selected := map[string]ListingOption{}
				for _, rawTag := range tags {
					tag, ok := resolve(rawTag).(map[string]any)
					if !ok {
						return errors.New("invalid product tag option")
					}
					id, _ := resolve(tag["id"]).(string)
					name, _ := resolve(tag["name"]).(string)
					if safeID(id) != nil || len(id) > 256 || name == "" || len(name) > 1024 {
						return errors.New("invalid product tag identity")
					}
					if prior, exists := selected[id]; exists && prior.Name != name {
						return errors.New("conflicting product tag label")
					}
					selected[id] = ListingOption{ID: id, Name: name}
				}
				if prior, exists := groups[slug]; exists && !reflect.DeepEqual(prior, selected) {
					return errors.New("conflicting product tagsets")
				}
				groups[slug] = selected
			}
		}
		id, _ := resolve(m["id"]).(string)
		variants, ok := resolve(m["variants"]).([]any)
		if id != product || !ok {
			return nil
		}
		if len(variants) > 100 {
			return errors.New("too many product variants")
		}
		if raw, exists := m["organization_id"]; exists && resolve(raw) != org {
			return errors.New("product organization mismatch")
		}
		text := func(m map[string]any, key string) *string {
			value, ok := resolve(m[key]).(string)
			if !ok || len(value) > 100000 {
				return nil
			}
			return &value
		}
		record := ProductDetails{ID: product, Name: text(m, "name"), Description: text(m, "description"), Status: text(m, "status"), Variants: []ProductDetailVariant{}}
		seen := map[string]bool{}
		for _, raw := range variants {
			v, ok := resolve(raw).(map[string]any)
			if !ok {
				return errors.New("invalid product details variant")
			}
			vid, _ := resolve(v["id"]).(string)
			if safeID(vid) != nil || len(vid) > 256 || seen[vid] {
				return errors.New("invalid product details variant identity")
			}
			seen[vid] = true
			variant := ProductDetailVariant{ID: vid, UUID: text(v, "streamdeckid"), Name: text(v, "name")}
			if free, ok := resolve(v["is_free"]).(bool); ok {
				variant.IsFree = &free
			}
			if raw, exists := v["prices"]; exists && resolve(raw) != nil {
				prices, ok := resolve(raw).([]any)
				if !ok || len(prices) > 100 {
					return errors.New("unrecognized product prices")
				}
				selected := []ProductDetailPrice{}
				for _, rawPrice := range prices {
					price, ok := resolve(rawPrice).(map[string]any)
					if !ok {
						return errors.New("invalid product price")
					}
					p := ProductDetailPrice{Plan: text(price, "plan"), Currency: text(price, "currency"), AmountUnit: "provider_unit_amount"}
					amount := resolve(price["unit_amount"])
					if parsed, err := decimalCents(amount, false); err == nil && parsed.Sign() >= 0 {
						switch n := amount.(type) {
						case string:
							p.UnitAmount = n
						case float64:
							if n <= math.MaxInt64 && math.Trunc(n) == n {
								p.UnitAmount = n
							}
						}
					}
					selected = append(selected, p)
				}
				variant.Prices = &selected
			}
			record.Variants = append(record.Variants, variant)
		}
		if raw, exists := m["tags"]; exists && resolve(raw) != nil {
			tags, ok := resolve(raw).([]any)
			if !ok || len(tags) > 1000 {
				return errors.New("invalid product selected tags")
			}
			selected := []string{}
			for _, rawTag := range tags {
				tag, ok := resolve(rawTag).(string)
				if !ok || safeID(tag) != nil || len(tag) > 256 {
					return errors.New("invalid selected product tag")
				}
				selected = append(selected, tag)
			}
			record.TagIDs = &selected
		}
		if raw, exists := m["additional_links"]; exists && resolve(raw) != nil {
			links, ok := resolve(raw).([]any)
			if !ok || len(links) > 100 {
				return errors.New("invalid product links")
			}
			selected := []ProductDetailLink{}
			for _, rawLink := range links {
				link, ok := resolve(rawLink).(map[string]any)
				if !ok {
					return errors.New("invalid product link")
				}
				selectedLink := ProductDetailLink{ID: text(link, "id"), DisplayText: text(link, "display_text"), URLStatus: "missing_or_unsafe"}
				if value := text(link, "url"); value != nil {
					u, err := url.Parse(*value)
					if err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && len(*value) <= 4096 && !strings.Contains(*value, "\\") {
						selectedLink.URL = value
						selectedLink.URLStatus = "public_https_link"
					}
				}
				selected = append(selected, selectedLink)
			}
			record.Links = &selected
		}
		if found {
			before := out
			before.Coverage = nil
			before.UnknownFields = nil
			before.SourceKind = ""
			before.ObservedAt = ""
			before.Languages = nil
			before.Platforms = nil
			if !reflect.DeepEqual(before, record) {
				return errors.New("conflicting product details")
			}
			return nil
		}
		found = true
		record.Coverage = out.Coverage
		record.UnknownFields = out.UnknownFields
		record.SourceKind = out.SourceKind
		record.ObservedAt = out.ObservedAt
		out = record
		return nil
	})
	if err != nil {
		return out, err
	}
	if !found {
		return out, errors.New("owned product absent from recognized details page")
	}
	if out.TagIDs != nil {
		for slug, target := range map[string]**[]ListingOption{"language": &out.Languages, "os": &out.Platforms} {
			if group, exists := groups[slug]; exists {
				selected := []ListingOption{}
				for _, id := range *out.TagIDs {
					if tag, ok := group[id]; ok {
						selected = append(selected, tag)
					}
				}
				*target = &selected
			}
		}
	}
	known := map[string]bool{"description": out.Description != nil, "variants": true, "platforms": out.Platforms != nil, "languages": out.Languages != nil, "links": out.Links != nil, "pricing": len(out.Variants) > 0}
	for _, v := range out.Variants {
		if v.IsFree == nil || v.Prices == nil {
			known["pricing"] = false
		}
		if v.Prices != nil {
			if v.IsFree != nil && !*v.IsFree && len(*v.Prices) == 0 {
				known["pricing"] = false
			}
			for _, p := range *v.Prices {
				if p.UnitAmount == nil || p.Plan == nil || *p.Plan == "" || p.Currency == nil || *p.Currency == "" {
					known["pricing"] = false
				}
			}
		}
	}
	for _, field := range []string{"description", "variants", "platforms", "languages", "links", "pricing"} {
		if known[field] {
			out.Coverage[field] = "returned_by_authenticated_product_page"
		} else {
			out.Coverage[field] = "unknown_or_incomplete_in_authenticated_product_page"
			out.UnknownFields = append(out.UnknownFields, field)
		}
	}
	for _, link := range func() []ProductDetailLink {
		if out.Links == nil {
			return nil
		}
		return *out.Links
	}() {
		if link.URL == nil {
			out.Coverage["links"] = "unknown_or_incomplete_in_authenticated_product_page"
			out.UnknownFields = append(out.UnknownFields, "link_url")
			break
		}
	}
	return out, nil
}
