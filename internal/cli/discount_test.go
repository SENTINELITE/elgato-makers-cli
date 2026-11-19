package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/pkg/discountplan"
)

func TestDiscountPreparationLocalAndYesScoped(t *testing.T) {
	dir, cfg, _, _ := fixture(t)
	input := filepath.Join(dir, "discount.json")
	os.WriteFile(input, []byte(`{"organization_id":"org","name":"Synthetic","maker_code":"SAVE10","mode":"percentage","scope":"all_products","percentage_off":10,"start_at":"2030-01-01T00:00:00Z","end_at":null,"max_unique_users":1,"max_uses_per_user":null}`), 0600)
	o := Options{ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Fatal("local preparation or refused confirmation created a client")
		return nil, nil
	}}
	out, stderr, e := command(t, o, "--config", cfg, "--state-dir", filepath.Join(dir, "plans"), "discounts", "prepare", "--file", input)
	if e != nil {
		t.Fatal(e)
	}
	var p discountplan.Plan
	if json.Unmarshal([]byte(out), &p) != nil || p.Input.MaxUsesPerUser != nil {
		t.Fatal(out)
	}
	if stderr != "" {
		t.Fatal("local preparation polluted stderr", stderr)
	}
	_, stderr, e = command(t, o, "--config", cfg, "--state-dir", filepath.Join(dir, "plans"), "discounts", "create", p.Digest)
	if e == nil || !strings.Contains(stderr, "ALL PRODUCTS") || !strings.Contains(stderr, "Uses per customer: unlimited") {
		t.Fatal(stderr, e)
	}
	for _, args := range [][]string{{"--yes", "discounts", "list"}, {"discounts", "prepare", "--yes"}, {"discounts", "list", "--yes"}} {
		if _, _, e = command(t, o, args...); e == nil {
			t.Fatal("yes accepted outside mutation", args)
		}
	}
}

func TestDiscountReviewShowsCapturedMoneyAndTargetBeforeTwoConfirmations(t *testing.T) {
	for _, tc := range []struct {
		name, fields, scope, value string
	}{
		{"fixed", `"mode":"fixed","scope":"all_products","amount_minor":500,"currency":"USD"`, "ALL PRODUCTS", "USD 5.00"},
		{"targeted", `"mode":"percentage","scope":"specific_products","percentage_off":15,"product_ids":["synthetic-product"]`, "SPECIFIC PRODUCT (synthetic-product)", "15%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, cfg, _, _ := fixture(t)
			input := filepath.Join(dir, "discount.json")
			content := `{"organization_id":"org","name":"Synthetic","maker_code":"SAVE","start_at":"2030-01-01T00:00:00Z","end_at":null,"max_unique_users":1,"max_uses_per_user":1,` + tc.fields + `}`
			if err := os.WriteFile(input, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			o := Options{
				In:         strings.NewReader("yes\nno\n"),
				IsTerminal: func() bool { return true },
				ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
					t.Fatal("creation reached Maker before the second approval")
					return nil, nil
				},
			}
			state := filepath.Join(dir, "plans")
			out, _, err := command(t, o, "--config", cfg, "--state-dir", state, "discounts", "prepare", "--file", input)
			if err != nil {
				t.Fatal(err)
			}
			var plan discountplan.Plan
			if err := json.Unmarshal([]byte(out), &plan); err != nil {
				t.Fatal(err)
			}
			_, stderr, err := command(t, o, "--config", cfg, "--state-dir", state, "discounts", "create", plan.Digest)
			if err == nil {
				t.Fatal("second confirmation refusal was ignored")
			}
			want := "Scope: " + tc.scope + "\nDiscount: " + tc.value
			if !strings.Contains(stderr, want) || strings.Count(stderr, "[y/N]") != 2 {
				t.Fatalf("review or confirmation prompts missing from %q", stderr)
			}
		})
	}
}
