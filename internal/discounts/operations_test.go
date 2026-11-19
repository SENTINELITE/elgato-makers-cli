package discounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/pkg/discountplan"
)

func TestUncertaintyBlocksSamePlanAndNewPlanForCode(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	i := discountplan.Input{OrganizationID: "synthetic-org", Name: "Synthetic", MakerCode: "SAVE10", Mode: "percentage", Scope: "all_products", PercentageOff: 10, StartAt: "2030-01-01T00:00:00Z"}
	p, e := discountplan.Prepare(i)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SavePlan(p); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal("preflight refusal dispatched a mutation")
		}
		fmt.Fprint(w, "unrecognized synthetic page")
	}))
	defer server.Close()
	api, _ := maker.New(maker.Config{BaseURL: server.URL, AllowLocalhost: true})
	op, e := s.Create(context.Background(), p.Digest, api)
	if e == nil || op.State != "uncertain" {
		t.Fatal(op, e)
	}
	if _, e = s.Create(context.Background(), p.Digest, nil); e == nil {
		t.Fatal("same operation retried")
	}
	p2, _ := discountplan.PrepareAt(i, time.Now().Add(time.Second))
	if e = s.SavePlan(p2); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Create(context.Background(), p2.Digest, nil); e == nil {
		t.Fatal("new plan bypassed account/code uncertainty guard")
	}
}
func TestCheckpointAtomicPrivateAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt.json")
	if e := WriteCheckpoint(path, map[string]string{"discount_id": "synthetic-one"}); e != nil {
		t.Fatal(e)
	}
	if e := WriteCheckpoint(path, map[string]string{"discount_id": "synthetic-two"}); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(path)
	if e != nil || string(b) != "{\n  \"discount_id\": \"synthetic-two\"\n}" {
		t.Fatal(string(b), e)
	}
	if !filesec.IsPrivateFile(path) {
		t.Fatal("checkpoint not private")
	}
}

func TestUnknownPostOutcomeNeverRetries(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	p, e := discountplan.Prepare(discountplan.Input{OrganizationID: "synthetic-org", Name: "Synthetic", MakerCode: "SAVE10", Mode: "percentage", Scope: "all_products", PercentageOff: 10, StartAt: "2030-01-01T00:00:00Z"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SavePlan(p); e != nil {
		t.Fatal(e)
	}
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
			fmt.Fprint(w, `1:{"unexpected":true}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/create") {
			fmt.Fprintf(w, `(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"createDiscount")`, strings.Repeat("a", 40))
			return
		}
		fmt.Fprint(w, `{"discounts":[],"totalDiscounts":0,"pagination":{"pageCount":1,"selectedPage":1}}`)
	}))
	defer server.Close()
	api, _ := maker.New(maker.Config{BaseURL: server.URL, AllowLocalhost: true})
	op, e := s.Create(context.Background(), p.Digest, api)
	var uncertain *maker.AmbiguousDiscountError
	if !errors.As(e, &uncertain) || op.State != "uncertain" || posts != 1 {
		t.Fatal(op, e, posts)
	}
	if _, e = s.Create(context.Background(), p.Digest, api); e == nil || posts != 1 {
		t.Fatal("unknown outcome replayed", e, posts)
	}
}

func TestInvalidAcknowledgementDoesNotPoisonExactCodeReconciliation(t *testing.T) {
	for _, tc := range []struct{ name, ack string }{
		{"legacy", `1:{"success":false,"discountId":"wrong-id"}`},
		{"fixed", `1:{"success":true,"discountId":"invalid/id"}`},
		{"targeted", `1:{"discountId":"wrong-id"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := Store{Dir: t.TempDir()}
			const org, product = "synthetic-org", "synthetic-product"
			input := discountplan.Input{OrganizationID: org, Name: "Synthetic", MakerCode: "SAVE10", Mode: "percentage", Scope: "all_products", PercentageOff: 10, StartAt: "2030-01-01T00:00:00Z"}
			row := map[string]any{"id": "true-id", "name": "Synthetic", "discount_type": "makers", "mode": "percentage", "percentage_off": "10", "makers": []any{org}, "items": []any{}, "valid_from": input.StartAt, "expires_at": nil, "enabled": true, "state": "upcoming", "valid": false, "code": map[string]any{"maker_code": "SAVE10", "code": "synthetic-SAVE10", "max_unique_users": nil, "max_uses_per_user": nil}}
			switch tc.name {
			case "fixed":
				input.Mode, input.PercentageOff, input.AmountMinor, input.Currency = "fixed", 0, 500, "USD"
				row["mode"], row["percentage_off"], row["amount"], row["currency"] = "absolute", nil, 500, "USD"
			case "targeted":
				input.Scope, input.ProductIDs = "specific_products", []string{product}
				row["discount_type"], row["items"], row["amount"], row["currency"] = "products", []string{product}, nil, nil
			}
			plan, err := discountplan.Prepare(input)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.SavePlan(plan); err != nil {
				t.Fatal(err)
			}
			posted := false
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posted = true
					posts++
					fmt.Fprint(w, tc.ack)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/create") {
					fmt.Fprintf(w, `(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"createDiscount")`, strings.Repeat("a", 40))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/true-id") {
					json.NewEncoder(w).Encode(map[string]any{"discount": row})
					return
				}
				if r.URL.Path == "/"+org+"/products/"+product {
					fmt.Fprintf(w, `{"product":{"id":%q,"organization_id":%q,"name":"Synthetic Product","variants":[]}}`, product, org)
					return
				}
				rows := []any{}
				if posted {
					rows = append(rows, row)
				}
				json.NewEncoder(w).Encode(map[string]any{"discounts": rows, "totalDiscounts": len(rows), "pagination": map[string]int{"pageCount": 1, "selectedPage": 1}})
			}))
			defer server.Close()
			api, _ := maker.New(maker.Config{BaseURL: server.URL, AllowLocalhost: true})
			op, err := store.Create(context.Background(), plan.Digest, api)
			var ambiguous *maker.AmbiguousDiscountError
			if !errors.As(err, &ambiguous) || op.Receipt != nil || op.State != "uncertain" {
				t.Fatal("invalid acknowledgement stored as authoritative receipt", op, err)
			}
			saved, err := store.Load(plan.Digest)
			if err != nil || saved.Receipt != nil {
				t.Fatal("invalid receipt persisted", saved, err)
			}
			op, err = store.Status(context.Background(), plan.Digest, api)
			if err != nil || op.State != "verified" || op.Receipt == nil || op.Receipt.DiscountID != "true-id" || posts != 1 {
				t.Fatal("exact-code reconciliation blocked or mutation replayed", op, err, posts)
			}
		})
	}
}

func TestHistoricalStatusReconcilesExpiredAndDisabledTermsWithoutRetry(t *testing.T) {
	for _, state := range []string{"expired", "disabled"} {
		t.Run(state, func(t *testing.T) {
			store := Store{Dir: t.TempDir()}
			end := "2026-01-02T00:00:00Z"
			plan, err := discountplan.Prepare(discountplan.Input{OrganizationID: "synthetic-org", Name: "Synthetic", MakerCode: "SAVE10", Mode: "percentage", Scope: "all_products", PercentageOff: 10, StartAt: "2026-01-01T00:00:00Z", EndAt: &end})
			if err != nil {
				t.Fatal(err)
			}
			if err = store.SavePlan(plan); err != nil {
				t.Fatal(err)
			}
			enabled := state != "disabled"
			row := map[string]any{"id": "true-id", "name": "Synthetic", "discount_type": "makers", "mode": "percentage", "percentage_off": "10", "makers": []any{"synthetic-org"}, "items": []any{}, "valid_from": plan.Input.StartAt, "expires_at": end, "enabled": enabled, "state": state, "valid": false, "code": map[string]any{"maker_code": "SAVE10", "code": "synthetic-SAVE10", "max_unique_users": nil, "max_uses_per_user": nil}}
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					posts++
					t.Error("historical reconciliation mutated provider")
					return
				}
				if strings.HasSuffix(r.URL.Path, "/true-id") {
					json.NewEncoder(w).Encode(map[string]any{"discount": row})
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"discounts": []any{row}, "totalDiscounts": 1, "pagination": map[string]int{"pageCount": 1, "selectedPage": 1}})
			}))
			defer server.Close()
			api, _ := maker.New(maker.Config{BaseURL: server.URL, AllowLocalhost: true})
			op, err := store.Status(context.Background(), plan.Digest, api)
			if err != nil || op.State != "verified" || op.Receipt.DiscountID != "true-id" || op.RemoteState != state || op.Enabled == nil || *op.Enabled != enabled || posts != 0 {
				t.Fatal(op, err, posts)
			}
			if _, err = store.Create(context.Background(), plan.Digest, api); err == nil || posts != 0 {
				t.Fatal("historical status enabled retry", err, posts)
			}
			if err = maker.VerifyDiscount(plan, maker.Discount(row)); err == nil {
				t.Fatal("immediate create verifier accepted historical state")
			}
			op.Receipt.DiscountID = "wrong-id"
			if err = store.save(op); err != nil {
				t.Fatal(err)
			}
			if _, err = store.Status(context.Background(), plan.Digest, api); err == nil {
				t.Fatal("historical reconciliation ignored receipt ID mismatch")
			}
		})
	}
}
