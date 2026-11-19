// Package discountplan prepares account-bound, expiring discount plans without network access.
package discountplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

type Input struct {
	OrganizationID string   `json:"organization_id"`
	Name           string   `json:"name"`
	MakerCode      string   `json:"maker_code"`
	Mode           string   `json:"mode"`
	Scope          string   `json:"scope"`
	PercentageOff  int      `json:"percentage_off"`
	StartAt        string   `json:"start_at"`
	EndAt          *string  `json:"end_at"`
	MaxUniqueUsers *int     `json:"max_unique_users"`
	MaxUsesPerUser *int     `json:"max_uses_per_user"`
	AmountMinor    int      `json:"amount_minor,omitempty"`
	Currency       string   `json:"currency,omitempty"`
	ProductIDs     []string `json:"product_ids,omitempty"`
}
type Plan struct {
	Input     Input     `json:"input"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Digest    string    `json:"digest"`
}

var id = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,255}$`)
var code = regexp.MustCompile(`^[A-Za-z0-9]{1,50}$`)

// The largest whole USD amount in minor units that fits a signed 32-bit integer.
const maxAmountMinor = 2_147_483_600

func ValidateInput(i Input) error {
	if !id.MatchString(i.OrganizationID) || i.OrganizationID == "." || i.OrganizationID == ".." {
		return errors.New("invalid organization_id")
	}
	if strings.TrimSpace(i.Name) == "" || !code.MatchString(i.MakerCode) {
		return errors.New("name and alphanumeric maker_code of 1-50 characters are required")
	}
	switch i.Mode {
	case "percentage":
		if i.PercentageOff < 1 || i.PercentageOff > 100 || i.AmountMinor != 0 || i.Currency != "" {
			return errors.New("percentage discounts require percentage_off from 1 to 100 and no amount_minor or currency")
		}
	case "fixed":
		if i.PercentageOff != 0 || i.Currency != "USD" || i.AmountMinor < 100 || i.AmountMinor > maxAmountMinor || i.AmountMinor%100 != 0 {
			return errors.New("fixed discounts require USD amount_minor from 100 to 2147483600 in increments of 100 and no percentage_off")
		}
	default:
		return errors.New("mode must be percentage or fixed")
	}
	switch i.Scope {
	case "all_products":
		if len(i.ProductIDs) != 0 {
			return errors.New("all_products cannot include product_ids")
		}
	case "specific_products":
		if i.Mode != "percentage" || len(i.ProductIDs) != 1 || len(i.ProductIDs[0]) > 128 || !id.MatchString(i.ProductIDs[0]) {
			return errors.New("specific_products requires a percentage discount and exactly one valid product ID")
		}
	default:
		return errors.New("scope must be all_products or specific_products")
	}
	start, e := time.Parse(time.RFC3339, i.StartAt)
	if e != nil || start.Second() != 0 || start.Nanosecond() != 0 || !strings.HasSuffix(i.StartAt, "Z") {
		return errors.New("start_at requires UTC RFC3339 with whole minutes")
	}
	if i.EndAt != nil {
		end, e := time.Parse(time.RFC3339, *i.EndAt)
		if e != nil || !end.After(start) || end.Second() != 0 || end.Nanosecond() != 0 || !strings.HasSuffix(*i.EndAt, "Z") {
			return errors.New("end_at must be UTC whole minutes later than start_at, or null")
		}
	}
	for _, v := range []*int{i.MaxUniqueUsers, i.MaxUsesPerUser} {
		if v != nil && *v < 1 {
			return errors.New("use limits must be positive integers or explicit null for unlimited")
		}
	}
	return nil
}
func digest(p Plan) string {
	p.Digest = ""
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Prepare(i Input) (Plan, error) { return PrepareAt(i, time.Now()) }
func PrepareAt(i Input, now time.Time) (Plan, error) {
	if e := ValidateInput(i); e != nil {
		return Plan{}, e
	}
	p := Plan{Input: i, CreatedAt: now.UTC(), ExpiresAt: now.UTC().Add(24 * time.Hour)}
	p.Digest = digest(p)
	return p, nil
}
func Validate(p Plan) error { return ValidateAt(p, time.Now()) }
func ValidateAt(p Plan, now time.Time) error {
	if e := ValidateInput(p.Input); e != nil {
		return e
	}
	if p.Digest != digest(p) || p.CreatedAt.IsZero() || !p.ExpiresAt.Equal(p.CreatedAt.Add(24*time.Hour)) || now.Before(p.CreatedAt.Add(-time.Minute)) || !now.Before(p.ExpiresAt) {
		return errors.New("discount plan changed or expired; prepare and review a new plan")
	}
	return nil
}

// DecodeInput rejects omitted limits, unknown fields, and trailing JSON.
func DecodeInput(b []byte) (Input, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return Input{}, errors.New("invalid discount input JSON")
	}
	for _, k := range []string{"end_at", "max_unique_users", "max_uses_per_user"} {
		if _, ok := fields[k]; !ok {
			return Input{}, errors.New("end_at and both use limits must be explicit; use null for no end or unlimited")
		}
	}
	var i Input
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e := d.Decode(&i); e != nil {
		return i, errors.New("invalid discount input fields")
	}
	return i, ValidateInput(i)
}
