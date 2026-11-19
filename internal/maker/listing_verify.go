package maker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

var providerCentsPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,7})(\.0{1,2})?$`)

// decimalCents compares monetary values without float arithmetic or rounding.
func decimalCents(value any, dollars bool) (*big.Int, error) {
	var s string
	switch value := value.(type) {
	case string:
		s = value
	case float64:
		s = strconv.FormatFloat(value, 'f', -1, 64)
	case json.Number:
		s = string(value)
	default:
		return nil, errors.New("listing price amount schema is not recognized")
	}
	if (dollars && !listingPricePattern.MatchString(s)) || (!dollars && !providerCentsPattern.MatchString(s)) {
		return nil, errors.New("listing price amount is not a recognized decimal")
	}
	amount, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, errors.New("invalid listing price amount")
	}
	if dollars {
		amount.Mul(amount, big.NewRat(100, 1))
	}
	if !amount.IsInt() {
		return nil, errors.New("listing price amount contains fractional cents")
	}
	return new(big.Int).Set(amount.Num()), nil
}

// VerifyListing reads current provider state. A successful update response does
// not substitute for this independent check of paid/free status and pricing.
func (c *Client) VerifyListing(ctx context.Context, input ListingDetailsInput) error {
	if _, err := listingFields(input); err != nil {
		return err
	}
	page, err := c.get(ctx, "/"+input.Target.OrgID+"/products/"+input.Target.ProductID)
	if err != nil {
		return err
	}
	return verifyListingPage(ctx, page, input)
}
func verifyListingPage(ctx context.Context, page []byte, input ListingDetailsInput) error {
	found := false
	err := walkListingPage(page, func(p map[string]any, resolve func(any) any) error {
		id, _ := resolve(p["id"]).(string)
		if id != input.Target.ProductID {
			return nil
		}
		variants, ok := resolve(p["variants"]).([]any)
		if !ok {
			return nil
		}
		for _, raw := range variants {
			v, ok := resolve(raw).(map[string]any)
			if !ok {
				return errors.New("invalid listing verification variant")
			}
			id, _ := resolve(v["id"]).(string)
			if id != input.Target.VariantID {
				continue
			}
			if err := listingVariantIdentity(ctx, input.Target, p, v, resolve); err != nil {
				return err
			}
			free, ok := resolve(v["is_free"]).(bool)
			if !ok || free != input.IsFree {
				return errors.New("remote paid/free listing status does not match requested plan")
			}
			prices, ok := resolve(v["prices"]).([]any)
			if !ok {
				return errors.New("remote listing prices schema is not recognized")
			}
			wanted := input.Price
			if input.IsFree {
				wanted = "0"
			}
			want, err := decimalCents(wanted, true)
			if err != nil {
				return err
			}
			once := 0
			for _, rawPrice := range prices {
				price, ok := resolve(rawPrice).(map[string]any)
				if !ok {
					return errors.New("invalid remote listing price")
				}
				plan, ok := resolve(price["plan"]).(string)
				if !ok || plan != "once" {
					return errors.New("remote listing contains unsupported pricing plan")
				}
				once++
				currency, ok := resolve(price["currency"]).(string)
				if !ok || currency != "USD" {
					return errors.New("remote listing currency does not match USD")
				}
				actual, err := decimalCents(resolve(price["unit_amount"]), false)
				if err != nil {
					return err
				}
				if actual.Cmp(want) != 0 {
					return errors.New("remote listing price does not match requested amount")
				}
			}
			if once != 1 {
				return errors.New("remote listing must expose exactly one authoritative once price")
			}
			tags, ok := resolve(p["tags"]).([]any)
			if !ok {
				return errors.New("remote listing tags schema is not recognized")
			}
			remote := map[string]bool{}
			for _, tag := range tags {
				s, ok := resolve(tag).(string)
				if !ok {
					return errors.New("invalid remote listing tag")
				}
				remote[s] = true
			}
			for _, tag := range append(append(append([]string{}, input.LanguageIDs...), input.CategoryIDs...), input.ExtraTagIDs...) {
				if !remote[tag] {
					return errors.New("remote listing is missing a requested tag")
				}
			}
			if input.Links != nil {
				links, ok := resolve(p["additional_links"]).([]any)
				if !ok || len(links) != len(input.Links) {
					return errors.New("remote additional links do not match requested plan")
				}
				seen := map[string]bool{}
				for _, raw := range links {
					link, ok := resolve(raw).(map[string]any)
					if !ok {
						return errors.New("invalid remote listing link")
					}
					label, _ := resolve(link["display_text"]).(string)
					url, _ := resolve(link["url"]).(string)
					expected, exists := input.Links[label]
					if !exists || seen[label] || expected != url {
						return errors.New("remote additional links do not match requested plan")
					}
					seen[label] = true
				}
			}
			found = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("listing verification target absent from recognized Maker response")
	}
	return nil
}

// VerifyListingReady verifies the complete media layout, including surplus and
// duplicate slots. Acknowledged association IDs are checked by ReconcileMedia.
func (c *Client) VerifyListingReady(ctx context.Context, target model.Target, details ListingDetailsInput, inputs []MediaInput) error {
	if _, err := listingFields(details); err != nil {
		return err
	}
	if details.Target.OrgID != target.OrgID || details.Target.ProductID != target.ProductID || details.Target.VariantID != target.VariantID || details.Target.UUID != target.UUID {
		return errors.New("listing readiness target mismatch")
	}
	expected := map[string]string{}
	for _, input := range inputs {
		if input.Target.OrgID != target.OrgID || input.Target.ProductID != target.ProductID || input.Target.VariantID != target.VariantID || input.Target.UUID != target.UUID {
			return errors.New("media readiness target mismatch")
		}
		if err := ValidateMediaInput(input); err != nil {
			return err
		}
		key := fmt.Sprintf("%s:%d", input.Kind, input.Order)
		if _, ok := expected[key]; ok {
			return errors.New("duplicate desired media slot")
		}
		kind := "image"
		if strings.EqualFold(filepath.Ext(input.Path), ".mp4") {
			kind = "video"
		}
		expected[key] = kind
	}
	if _, ok := expected["icon:1"]; !ok {
		return errors.New("ready listing requires an icon")
	}
	if _, ok := expected["thumbnail:1"]; !ok {
		return errors.New("ready listing requires a thumbnail")
	}
	galleryCount := len(expected) - 2
	if galleryCount < 3 || galleryCount > 10 {
		return errors.New("ready listing requires 3 to 10 gallery assets")
	}
	for i := 1; i <= galleryCount; i++ {
		if _, ok := expected[fmt.Sprintf("gallery:%d", i)]; !ok {
			return errors.New("ready gallery order must be sequential")
		}
	}
	page, err := c.get(ctx, "/"+target.OrgID+"/products/"+target.ProductID)
	if err != nil {
		return err
	}
	if err = verifyListingPage(ctx, page, details); err != nil {
		return err
	}
	found := false
	err = walkListingPage(page, func(p map[string]any, resolve func(any) any) error {
		id, _ := resolve(p["id"]).(string)
		if id != target.ProductID {
			return nil
		}
		variants, ok := resolve(p["variants"]).([]any)
		if !ok {
			return nil
		}
		for _, raw := range variants {
			v, ok := resolve(raw).(map[string]any)
			if !ok {
				return errors.New("invalid readiness variant")
			}
			id, _ := resolve(v["id"]).(string)
			if id != target.VariantID {
				continue
			}
			if receipt, ok := ctx.Value(creationReceiptKey{}).(CreationReceipt); ok {
				name, _ := resolve(p["name"]).(string)
				description, _ := resolve(p["description"]).(string)
				if receipt.Target.OrgID != target.OrgID || receipt.Target.ProductID != target.ProductID || receipt.Target.VariantID != target.VariantID || receipt.Target.UUID != target.UUID || name != receipt.Name || !model.SameListingDescription(description, receipt.Description) {
					return errors.New("ready listing metadata differs from acknowledged creation receipt")
				}
			} else if target.Name != "" {
				name, _ := resolve(p["name"]).(string)
				if name != target.Name {
					return errors.New("ready listing product name differs from target")
				}
			}
			found = true
			seen := map[string]bool{}
			ids := map[string]bool{}
			for _, group := range []struct {
				key   string
				owner map[string]any
			}{{"media", p}, {"previews", v}} {
				rows, ok := resolve(group.owner[group.key]).([]any)
				if !ok {
					return errors.New("remote ready media schema is not recognized")
				}
				for _, raw := range rows {
					row, ok := resolve(raw).(map[string]any)
					if !ok {
						return errors.New("invalid ready media record")
					}
					id, _ := resolve(row["id"]).(string)
					if safeID(id) != nil || ids[id] {
						return errors.New("invalid or duplicate remote media identity")
					}
					ids[id] = true
					kind, _ := resolve(row["media_type"]).(string)
					order := 1
					typ := "image"
					if group.key == "previews" {
						kind = "gallery"
						var err error
						typ, err = galleryMediaKind(row, resolve)
						if err != nil {
							return err
						}
						n, ok := resolve(row["order"]).(float64)
						if !ok || n < 1 || n != float64(int(n)) {
							return errors.New("invalid remote gallery order")
						}
						order = int(n)
					} else if kind == "app-icon" {
						kind = "icon"
					} else if kind != "thumbnail" {
						return errors.New("unrecognized remote product media type")
					}
					key := fmt.Sprintf("%s:%d", kind, order)
					want, exists := expected[key]
					if !exists || seen[key] || want != typ {
						return errors.New("remote media layout differs from requested plan")
					}
					seen[key] = true
				}
			}
			if len(seen) != len(expected) {
				return errors.New("remote listing is missing requested media")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("remote readiness target not found")
	}
	return nil
}
