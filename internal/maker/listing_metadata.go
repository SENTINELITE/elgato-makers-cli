package maker

import (
	"context"
	"errors"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	metadataModuleHead = regexp.MustCompile(`(?:^|[}{,])\s*[0-9]+:\s*\([^)]{0,100}\)\s*=>\s*\{`)
	metadataAction     = serverReferencePattern(`updateProduct`)
	metadataDefaults   = regexp.MustCompile(`\{additional_links:([A-Za-z_$][A-Za-z0-9_$]*)\.additional_links,tags:\[\.\.\.([A-Za-z_$][A-Za-z0-9_$]*)\.tags\]\}`)
	metadataPayload    = regexp.MustCompile(`\{\.\.\.[A-Za-z_$][A-Za-z0-9_$]*,id:([A-Za-z_$][A-Za-z0-9_$]*)\.id,organization:[A-Za-z_$][A-Za-z0-9_$]*\}`)
)

// metadataListingActions distinguishes the Details editor from the unrelated
// About editor, which exports the same action name with a different contract.
// Deployment module identifiers and action references are never hardcoded.
func metadataListingActions(body []byte) ([]string, error) {
	heads := metadataModuleHead.FindAllIndex(body, -1)
	var actions []string
	for i, head := range heads {
		end := len(body)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		module := body[head[1]:end]
		if !strings.Contains(string(module), `title:"Details"`) {
			continue
		}
		defaults := metadataDefaults.FindAllSubmatch(module, -1)
		payloads := metadataPayload.FindAllSubmatch(module, -1)
		contract := false
		for _, d := range defaults {
			if string(d[1]) != string(d[2]) {
				continue
			}
			for _, p := range payloads {
				if string(p[1]) == string(d[1]) {
					contract = true
				}
			}
		}
		if !contract {
			continue
		}
		matches := metadataAction.FindAllSubmatch(module, -1)
		if len(matches) != 1 {
			return nil, errors.New("metadata-only Maker Details action is ambiguous")
		}
		actions = append(actions, string(matches[0][1]))
	}
	return actions, nil
}
func (c *Client) discoverMetadataListingAction(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	page, err := c.get(ctx, path)
	if err != nil {
		return "", err
	}
	action := ""
	scan := func(body []byte) error {
		candidates, err := metadataListingActions(body)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			if action != "" && candidate != action {
				return errors.New("conflicting metadata-only Maker Details actions")
			}
			action = candidate
		}
		return nil
	}
	if err = scan(page); err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for _, match := range scripts.FindAllSubmatch(page, -1) {
		u, err := url.Parse(html.UnescapeString(string(match[1])))
		if err != nil {
			continue
		}
		u = c.base.ResolveReference(u)
		if u.User != nil || u.Fragment != "" || u.Host != c.base.Host || u.Scheme != c.base.Scheme || !strings.HasPrefix(u.Path, "/_next/static/") || seen[u.String()] {
			continue
		}
		if len(seen) >= 100 {
			return "", errors.New("too many Maker script references")
		}
		seen[u.String()] = true
		body, err := c.publicScript(ctx, u)
		if err != nil {
			return "", err
		}
		if err = scan(body); err != nil {
			return "", err
		}
	}
	if action == "" {
		return "", errors.New("current metadata-only Maker Details action not recognized")
	}
	return action, nil
}

// listingNeedsInitialPrice permits the wizard price action only before any
// price exists on a recognized empty Free draft. Existing prices must match
// exactly; metadata saves never create or alter another price.
func listingNeedsInitialPrice(input ListingDetailsInput, product, variant map[string]any, resolve func(any) any) (bool, error) {
	free, ok := resolve(variant["is_free"]).(bool)
	if !ok {
		return false, errors.New("existing listing pricing is not recognized")
	}
	prices, ok := resolve(variant["prices"]).([]any)
	if !ok {
		return false, errors.New("existing listing prices schema is not recognized")
	}
	if len(prices) == 0 && free {
		versions, ok := resolve(variant["versions"]).([]any)
		status, _ := resolve(product["status"]).(string)
		if !ok || len(versions) != 0 || !strings.EqualFold(status, "DRAFT") {
			return false, errors.New("cannot initialize listing pricing outside a recognized empty Free draft")
		}
		return true, nil
	}
	if free != input.IsFree {
		return false, errors.New("existing listing paid/free status does not match reviewed plan")
	}
	if len(prices) != 1 {
		return false, errors.New("existing listing must expose exactly one authoritative once price")
	}
	price, ok := resolve(prices[0]).(map[string]any)
	if !ok {
		return false, errors.New("existing listing price is not recognized")
	}
	plan, _ := resolve(price["plan"]).(string)
	currency, _ := resolve(price["currency"]).(string)
	if plan != "once" || currency != "USD" {
		return false, errors.New("existing listing price plan or currency does not match reviewed plan")
	}
	wanted := input.Price
	if input.IsFree {
		wanted = "0"
	}
	want, err := decimalCents(wanted, true)
	if err != nil {
		return false, err
	}
	actual, err := decimalCents(resolve(price["unit_amount"]), false)
	if err != nil {
		return false, err
	}
	if actual.Cmp(want) != 0 {
		return false, errors.New("existing listing price amount does not match reviewed plan")
	}
	return false, nil
}
func validateListingPricePage(ctx context.Context, page []byte, input ListingDetailsInput) (bool, error) {
	found := false
	initialize := false
	err := walkListingPage(page, func(product map[string]any, resolve func(any) any) error {
		id, _ := resolve(product["id"]).(string)
		if id != input.Target.ProductID {
			return nil
		}
		variants, ok := resolve(product["variants"]).([]any)
		if !ok {
			return nil
		}
		for _, raw := range variants {
			variant, ok := resolve(raw).(map[string]any)
			if !ok {
				return errors.New("invalid listing price variant")
			}
			vid, _ := resolve(variant["id"]).(string)
			if vid != input.Target.VariantID {
				continue
			}
			if err := listingVariantIdentity(ctx, input.Target, product, variant, resolve); err != nil {
				return err
			}
			mode, err := listingNeedsInitialPrice(input, product, variant, resolve)
			if err != nil {
				return err
			}
			if found && mode != initialize {
				return errors.New("conflicting listing pricing state")
			}
			found = true
			initialize = mode
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if !found {
		return false, errors.New("listing price target absent from recognized Maker response")
	}
	return initialize, nil
}
