package discountplan

import (
	"encoding/json"
	"testing"
	"time"
)

func input() Input {
	return Input{OrganizationID: "synthetic-org", Name: "Synthetic", MakerCode: "SAVE10", Mode: "percentage", Scope: "all_products", PercentageOff: 10, StartAt: "2030-01-01T00:00:00Z"}
}
func TestPlansValidateAndDetectEdits(t *testing.T) {
	now := time.Now()
	p, e := PrepareAt(input(), now)
	if e != nil {
		t.Fatal(e)
	}
	if e = ValidateAt(p, now); e != nil {
		t.Fatal(e)
	}
	p.Input.PercentageOff = 11
	if ValidateAt(p, now) == nil {
		t.Fatal("edited plan accepted")
	}
	p, _ = PrepareAt(input(), now)
	if ValidateAt(p, now.Add(24*time.Hour)) == nil {
		t.Fatal("expired plan accepted")
	}
}
func TestExplicitLimitsAndUnsupportedScope(t *testing.T) {
	b, _ := json.Marshal(input())
	if _, e := DecodeInput(b); e != nil {
		t.Fatal(e)
	}
	var fields map[string]any
	json.Unmarshal(b, &fields)
	delete(fields, "max_unique_users")
	b, _ = json.Marshal(fields)
	if _, e := DecodeInput(b); e == nil {
		t.Fatal("missing limit became unlimited")
	}
	for _, mutate := range []func(*Input){func(i *Input) { i.Mode = "fixed" }, func(i *Input) { i.Scope = "products" }, func(i *Input) { v := 0; i.MaxUniqueUsers = &v }, func(i *Input) { i.StartAt = "2030-01-01T00:00:00-08:00" }} {
		i := input()
		mutate(&i)
		if ValidateInput(i) == nil {
			t.Fatal("unsupported input accepted", i)
		}
	}
}
func TestIndependentLimits(t *testing.T) {
	i := input()
	one := 1
	i.MaxUniqueUsers = &one
	if ValidateInput(i) != nil {
		t.Fatal("one customer unlimited uses refused")
	}
	i.MaxUniqueUsers = nil
	i.MaxUsesPerUser = &one
	if ValidateInput(i) != nil {
		t.Fatal("unlimited customers one use refused")
	}
}

func TestLegacyPlanDigestRemainsValid(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	previous := Plan{
		Input:     input(),
		CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
		Digest:    "ba83d384ca5c71ad552a1a5a9756eb932270fc5319263aaac60cc6b03f0507ee",
	}
	if err := ValidateAt(previous, now); err != nil {
		t.Fatal(err)
	}
}

func TestCapturedDiscountInputMatrix(t *testing.T) {
	fixed := input()
	fixed.Mode, fixed.PercentageOff, fixed.AmountMinor, fixed.Currency = "fixed", 0, 500, "USD"
	largest := fixed
	largest.AmountMinor = maxAmountMinor
	targeted := input()
	targeted.Scope, targeted.ProductIDs = "specific_products", []string{"synthetic-product"}
	for _, tc := range []struct {
		name   string
		base   Input
		mutate func(*Input)
	}{
		{"fixed amount", fixed, nil},
		{"targeted percentage", targeted, nil},
		{"whole minor units at portable bound", largest, nil},
		{"fixed with percentage", fixed, func(i *Input) { i.PercentageOff = 10 }},
		{"fixed in another currency", fixed, func(i *Input) { i.Currency = "EUR" }},
		{"fixed fractional dollars", fixed, func(i *Input) { i.AmountMinor = 525 }},
		{"fixed zero dollars", fixed, func(i *Input) { i.AmountMinor = 0 }},
		{"fixed above portable bound", fixed, func(i *Input) { i.AmountMinor = maxAmountMinor + 1 }},
		{"fixed with target", fixed, func(i *Input) { i.Scope, i.ProductIDs = targeted.Scope, targeted.ProductIDs }},
		{"percentage with amount", targeted, func(i *Input) { i.AmountMinor = 500 }},
		{"percentage with currency", targeted, func(i *Input) { i.Currency = "USD" }},
		{"percentage outside range", targeted, func(i *Input) { i.PercentageOff = 101 }},
		{"type scope", targeted, func(i *Input) { i.Scope = "specific_types" }},
		{"target on all-products", targeted, func(i *Input) { i.Scope = "all_products" }},
		{"missing target", targeted, func(i *Input) { i.ProductIDs = nil }},
		{"multiple targets", targeted, func(i *Input) { i.ProductIDs = []string{"one", "two"} }},
		{"unsafe target", targeted, func(i *Input) { i.ProductIDs = []string{"../foreign"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := tc.base
			if tc.mutate != nil {
				tc.mutate(&i)
			}
			b, err := json.Marshal(i)
			if err != nil {
				t.Fatal(err)
			}
			_, err = DecodeInput(b)
			wantValid := tc.mutate == nil
			if (err == nil) != wantValid {
				t.Fatalf("DecodeInput(%s) = %v; valid = %v", b, err, wantValid)
			}
		})
	}
}

func TestCapturedDiscountTermsBindPlanDigest(t *testing.T) {
	for _, tc := range []struct {
		mode, scope, currency string
		percentage, amount    int
		products              []string
		tamper                func(*Input)
	}{
		{"fixed", "all_products", "USD", 0, 500, nil, func(i *Input) { i.AmountMinor = 600 }},
		{"percentage", "specific_products", "", 10, 0, []string{"synthetic-product"}, func(i *Input) { i.ProductIDs[0] = "another-product" }},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			i := input()
			i.Mode, i.Scope, i.Currency = tc.mode, tc.scope, tc.currency
			i.PercentageOff, i.AmountMinor, i.ProductIDs = tc.percentage, tc.amount, tc.products
			p, err := Prepare(i)
			if err != nil {
				t.Fatal(err)
			}
			tc.tamper(&p.Input)
			if err := Validate(p); err == nil {
				t.Fatal("modified reviewed terms accepted")
			}
		})
	}
}
