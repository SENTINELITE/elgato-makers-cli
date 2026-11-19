package maker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/pkg/discountplan"
)

// DiscountReceipt must be durably persisted by the caller before verification.
type DiscountReceipt struct {
	DiscountID string `json:"discount_id"`
	Success    bool   `json:"success"`
}
type AmbiguousDiscountError struct{}

func (*AmbiguousDiscountError) Error() string {
	return "discount mutation outcome is uncertain; reconcile the exact organization and maker_code before any retry"
}
func (c *Client) discoverDiscountAction(ctx context.Context, path, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	pattern := serverReferencePattern(name)
	queue := []string{c.endpoint(path)}
	seen := map[string]bool{}
	action := ""
	for len(queue) > 0 {
		raw := queue[0]
		queue = queue[1:]
		if seen[raw] {
			continue
		}
		if len(seen) >= 102 {
			return "", errors.New("too many Maker discount scripts")
		}
		seen[raw] = true
		u, e := url.Parse(raw)
		if e != nil {
			return "", e
		}
		var b []byte
		if strings.HasPrefix(u.Path, "/_next/static/") {
			b, e = c.publicScript(ctx, u)
		} else {
			b, e = c.request(ctx, http.MethodGet, raw, "", "", nil, true)
		}
		if e != nil {
			return "", e
		}
		for _, m := range pattern.FindAllSubmatch(b, -1) {
			v := string(m[1])
			if action != "" && action != v {
				return "", errors.New("conflicting current discount action references")
			}
			action = v
		}
		for _, m := range scripts.FindAllSubmatch(b, -1) {
			ref, e := url.Parse(string(m[1]))
			if e != nil {
				continue
			}
			ref = u.ResolveReference(ref)
			if ref.User == nil && ref.Fragment == "" && ref.Scheme == c.base.Scheme && ref.Host == c.base.Host && strings.HasPrefix(ref.Path, "/_next/static/") {
				queue = append(queue, ref.String())
			}
		}
	}
	if action == "" {
		return "", errors.New("current Maker discount action reference not recognized")
	}
	return action, nil
}

// CreateDiscount dispatches once. checkpoint is required to persist the provider ID
// before any readback. The caller must record uncertainty before invoking this method.
func (c *Client) CreateDiscount(ctx context.Context, p discountplan.Plan, checkpoint func(DiscountReceipt) error) (DiscountReceipt, error) {
	var out DiscountReceipt
	if c.readOnly {
		return out, ErrReadOnly
	}
	if e := discountplan.Validate(p); e != nil {
		return out, e
	}
	if checkpoint == nil {
		return out, errors.New("durable discount receipt checkpoint required")
	}
	i := p.Input
	action, e := c.discoverDiscountAction(ctx, "/"+i.OrganizationID+"/discounts/create", "createDiscount")
	if e != nil {
		return out, e
	}
	// A fresh complete read binds the organization and rejects existing exact codes.
	c.invalidateReadPhase(ctx)
	existing, e := c.Discounts(ctx, i.OrganizationID, "")
	if e != nil {
		return out, e
	}
	for _, d := range existing {
		code := d["code"].(map[string]any)
		if code["maker_code"] == i.MakerCode {
			return out, errors.New("maker_code already exists in this organization; reconcile instead of creating")
		}
	}
	if i.Scope == "specific_products" {
		if _, err := c.ProductDetails(ctx, i.OrganizationID, i.ProductIDs[0]); err != nil {
			return out, errors.New("discount target could not be verified as an owned product in this organization")
		}
	}
	if e := ctx.Err(); e != nil {
		return out, e
	}
	result, e := c.postDiscountCreation(ctx, action, i)
	if e != nil {
		return out, &AmbiguousDiscountError{}
	}
	candidateID, _ := result["discountId"].(string)
	if result["success"] != true || safeID(candidateID) != nil {
		return out, &AmbiguousDiscountError{}
	}
	out = DiscountReceipt{DiscountID: candidateID, Success: true}
	if checkpoint(out) != nil {
		return out, &AmbiguousDiscountError{}
	}
	c.invalidateReadPhase(ctx)
	d, e := c.Discount(ctx, i.OrganizationID, out.DiscountID)
	if e != nil || VerifyDiscount(p, d) != nil {
		return out, &AmbiguousDiscountError{}
	}
	return out, nil
}

func (c *Client) postDiscountCreation(ctx context.Context, action string, i discountplan.Input) (map[string]any, error) {
	start, _ := time.Parse(time.RFC3339, i.StartAt)
	fields := [][2]string{
		{"_1_method", "code"},
		{"_1_name", i.Name},
		{"_1_makerCode", i.MakerCode},
		{"_1_startDate", start.Format("2006-01-02")},
		{"_1_startTime", start.Format("15:04")},
		{"_1_hasEndDate", strconv.FormatBool(i.EndAt != nil)},
	}
	if i.EndAt != nil {
		end, _ := time.Parse(time.RFC3339, *i.EndAt)
		fields = append(fields, [2]string{"_1_endDate", end.Format("2006-01-02")}, [2]string{"_1_endTime", end.Format("15:04")})
	}
	if i.Mode == "fixed" {
		fields = append(fields, [2]string{"_1_mode", "absolute"}, [2]string{"_1_amount", strconv.Itoa(i.AmountMinor / 100)})
	} else {
		fields = append(fields, [2]string{"_1_mode", "percentage"}, [2]string{"_1_percentageOff", strconv.Itoa(i.PercentageOff)})
	}
	if i.Scope == "specific_products" {
		fields = append(fields, [2]string{"_1_appliesTo", "products"}, [2]string{"_1_items[0]", i.ProductIDs[0]})
	} else {
		fields = append(fields, [2]string{"_1_appliesTo", "makers"})
	}
	fields = append(fields, [2]string{"_1_limitUniqueUsers", strconv.FormatBool(i.MaxUniqueUsers != nil)})
	if i.MaxUniqueUsers != nil {
		fields = append(fields, [2]string{"_1_maxUniqueUsers", strconv.Itoa(*i.MaxUniqueUsers)})
	}
	fields = append(fields, [2]string{"_1_limitUsesPerUser", strconv.FormatBool(i.MaxUsesPerUser != nil)})
	if i.MaxUsesPerUser != nil {
		fields = append(fields, [2]string{"_1_maxUsesPerUser", strconv.Itoa(*i.MaxUsesPerUser)})
	}
	currency := i.Currency
	if i.Mode == "percentage" {
		currency = "USD"
	}
	fields = append(fields, [2]string{"_1_organizationId", i.OrganizationID}, [2]string{"_1_currency", currency}, [2]string{"0", `[null,"$K1"]`})
	path := "/" + i.OrganizationID + "/discounts/create"
	if i.Mode == "percentage" && i.Scope == "all_products" {
		// Keep the established field order for legacy plans.
		unordered := make(map[string]string, len(fields))
		for _, field := range fields {
			unordered[field[0]] = field[1]
		}
		return c.multipart(ctx, path, action, unordered)
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, field := range fields {
		if err := w.WriteField(field[0], field[1]); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	raw, err := c.request(ctx, http.MethodPost, c.endpoint(path), w.FormDataContentType(), action, &body, true)
	if err != nil {
		return nil, err
	}
	return actionResult(raw)
}

// VerifyDiscount checks the immediate creation result, including its live state.
func VerifyDiscount(p discountplan.Plan, d Discount) error {
	if err := VerifyDiscountTerms(p, d); err != nil {
		return err
	}
	if d["enabled"] != true || (d["state"] != "active" && d["state"] != "upcoming") {
		return errors.New("new discount is not enabled in an expected creation state")
	}
	return nil
}

// VerifyDiscountTerms compares immutable reviewed terms for historical
// reconciliation. A matching creation can now be expired or disabled.
func VerifyDiscountTerms(p discountplan.Plan, d Discount) error {
	i := p.Input
	bad := errors.New("fresh discount differs from reviewed plan")
	if discountplan.ValidateInput(i) != nil || d == nil || d["name"] != i.Name {
		return bad
	}
	if i.Mode == "fixed" {
		if d["mode"] != "absolute" || d["amount"] != json.Number(strconv.Itoa(i.AmountMinor)) || d["currency"] != i.Currency {
			return bad
		}
	} else if d["mode"] != "percentage" || d["percentage_off"] != strconv.Itoa(i.PercentageOff) {
		return bad
	}
	makers, ok := d["makers"].([]any)
	if !ok || len(makers) != 1 || makers[0] != i.OrganizationID {
		return bad
	}
	items, ok := d["items"].([]any)
	if !ok || len(items) != len(i.ProductIDs) {
		return bad
	}
	if i.Scope == "specific_products" {
		if d["discount_type"] != "products" || items[0] != i.ProductIDs[0] || d["currency"] != nil || d["amount"] != nil {
			return bad
		}
	} else if d["discount_type"] != "makers" {
		return bad
	}
	sameTime := func(raw any, expected string) bool {
		s, ok := raw.(string)
		if !ok {
			return false
		}
		a, e := time.Parse(time.RFC3339, s)
		b, f := time.Parse(time.RFC3339, expected)
		return e == nil && f == nil && a.Equal(b)
	}
	if !sameTime(d["valid_from"], i.StartAt) {
		return bad
	}
	if i.EndAt == nil {
		if v, ok := d["expires_at"]; !ok || v != nil {
			return bad
		}
	} else if !sameTime(d["expires_at"], *i.EndAt) {
		return bad
	}
	code, ok := d["code"].(map[string]any)
	if !ok || code["maker_code"] != i.MakerCode {
		return bad
	}
	for k, v := range map[string]*int{"max_unique_users": i.MaxUniqueUsers, "max_uses_per_user": i.MaxUsesPerUser} {
		raw, exists := code[k]
		if !exists {
			return bad
		}
		if v == nil {
			if raw != nil {
				return bad
			}
		} else if raw != float64(*v) {
			return bad
		}
	}
	return nil
}

// DeactivateDiscount only accepts an exact, freshly read code and records the
// acknowledgement before readback. It never reactivates or retries an action.
func (c *Client) DeactivateDiscount(ctx context.Context, org, id, expectedCode string, checkpoint func(DiscountReceipt) error) (DiscountReceipt, error) {
	var out DiscountReceipt
	if c.readOnly {
		return out, ErrReadOnly
	}
	if safeID(org) != nil || safeID(id) != nil || expectedCode == "" || checkpoint == nil {
		return out, errors.New("exact organization, ID, code and durable checkpoint required")
	}
	action, e := c.discoverDiscountAction(ctx, "/"+org+"/discounts", "toggleDiscountsEnabledAction")
	if e != nil {
		return out, e
	}
	c.invalidateReadPhase(ctx)
	d, e := c.Discount(ctx, org, id)
	if e != nil {
		return out, e
	}
	code := d["code"].(map[string]any)
	if code["maker_code"] != expectedCode || d["enabled"] != true || d["state"] == "disabled" {
		return out, errors.New("deactivation identity/state differs from reviewed target")
	}
	ids, _ := json.Marshal([]string{id})
	if e := ctx.Err(); e != nil {
		return out, e
	}
	result, e := c.multipart(ctx, "/"+org+"/discounts", action, map[string]string{"_1_organization_id": org, "_1_discount_ids": string(ids), "_1_enabled": "false", "0": `[{"success":false},"$K1"]`})
	if e != nil || result["success"] != true {
		return out, &AmbiguousDiscountError{}
	}
	out = DiscountReceipt{DiscountID: id, Success: true}
	if checkpoint(out) != nil {
		return out, &AmbiguousDiscountError{}
	}
	c.invalidateReadPhase(ctx)
	d, e = c.Discount(ctx, org, id)
	if e != nil || d["enabled"] != false || d["state"] != "disabled" || d["code"].(map[string]any)["maker_code"] != expectedCode {
		return out, &AmbiguousDiscountError{}
	}
	return out, nil
}
