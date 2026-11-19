package maker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreationReceiptScopedIdentity(t *testing.T) {
	target := model.Target{OrgID: "test-org", ProductID: "test-product", VariantID: "test-variant", UUID: "test-uuid"}
	receipt := CreationReceipt{Target: target, Name: "Synthetic Product", Description: "Synthetic description"}
	ctx, err := WithCreationReceipt(context.Background(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	identity := func(v any) any { return v }
	for _, test := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
		ctx    context.Context
		want   bool
	}{
		{"acknowledged provisional", func(p, v map[string]any) {}, ctx, true},
		{"lowercase draft provisional", func(p, v map[string]any) { p["status"] = "draft" }, ctx, true},
		{"missing receipt", func(p, v map[string]any) {}, context.Background(), false},
		{"conflicting UUID", func(p, v map[string]any) { v["streamdeckid"] = "other" }, ctx, false},
		{"non draft", func(p, v map[string]any) { p["status"] = "PUBLISHED" }, ctx, false},
		{"version exists", func(p, v map[string]any) { v["versions"] = []any{"version"} }, ctx, false},
		{"unknown versions", func(p, v map[string]any) { delete(v, "versions") }, ctx, false},
		{"name changed", func(p, v map[string]any) { p["name"] = "Other" }, ctx, false},
		{"description changed", func(p, v map[string]any) { p["description"] = "Other" }, ctx, false},
		{"exact UUID", func(p, v map[string]any) { v["streamdeckid"] = "test-uuid"; delete(v, "versions") }, context.Background(), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := map[string]any{"id": target.ProductID, "name": receipt.Name, "description": receipt.Description, "status": "DRAFT"}
			v := map[string]any{"id": target.VariantID, "streamdeckid": nil, "versions": []any{}}
			test.mutate(p, v)
			err := listingVariantIdentity(test.ctx, target, p, v, identity)
			if (err == nil) != test.want {
				t.Fatalf("err=%v want success %v", err, test.want)
			}
		})
	}
	other := target
	other.OrgID = "other-org"
	p := map[string]any{"id": target.ProductID, "name": receipt.Name, "description": receipt.Description, "status": "DRAFT"}
	v := map[string]any{"id": target.VariantID, "streamdeckid": nil, "versions": []any{}}
	if listingVariantIdentity(ctx, other, p, v, identity) == nil {
		t.Fatal("receipt crossed organization")
	}
}
func TestVerifyListingAuthoritativePricing(t *testing.T) {
	input := ListingDetailsInput{Target: model.Target{OrgID: "test-org", ProductID: "test-product", VariantID: "test-variant", UUID: "test-uuid"}, Price: "13.37", LanguageIDs: []string{"language-en"}, CategoryIDs: []string{"category-tools"}}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any, map[string]any, map[string]any)
		want   bool
	}{
		{"paid integer cents", func(p, v, price map[string]any) {}, true},
		{"paid string cents", func(p, v, price map[string]any) { price["unit_amount"] = "1337" }, true},
		{"wrong free flag", func(p, v, price map[string]any) { v["is_free"] = true }, false},
		{"missing free flag", func(p, v, price map[string]any) { delete(v, "is_free") }, false},
		{"wrong currency", func(p, v, price map[string]any) { price["currency"] = "EUR" }, false},
		{"missing currency", func(p, v, price map[string]any) { delete(price, "currency") }, false},
		{"wrong amount", func(p, v, price map[string]any) { price["unit_amount"] = 13.37 }, false},
		{"unknown amount", func(p, v, price map[string]any) { price["unit_amount"] = nil }, false},
		{"wrong plan", func(p, v, price map[string]any) { price["plan"] = "monthly" }, false},
		{"missing price", func(p, v, price map[string]any) { v["prices"] = []any{} }, false},
		{"duplicate price", func(p, v, price map[string]any) { v["prices"] = []any{price, price} }, false},
		{"missing tag", func(p, v, price map[string]any) { p["tags"] = []any{} }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			price := map[string]any{"unit_amount": 1337, "currency": "USD", "plan": "once"}
			v := map[string]any{"id": "test-variant", "streamdeckid": "test-uuid", "is_free": false, "prices": []any{price}}
			p := map[string]any{"id": "test-product", "tags": []string{"language-en", "category-tools"}, "variants": []any{v}}
			test.mutate(p, v, price)
			page, _ := json.Marshal(p)
			err := verifyListingPage(context.Background(), page, input)
			if (err == nil) != test.want {
				t.Fatalf("err=%v want success %v", err, test.want)
			}
		})
	}
}
func TestDecimalCentsLossless(t *testing.T) {
	for _, raw := range []any{"13.37", "0.01", "999999.99"} {
		if _, err := decimalCents(raw, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []any{"1e2", "13.371", "NaN", 13.37} {
		if _, err := decimalCents(raw, false); err == nil {
			t.Fatalf("accepted fractional/invalid cents: %v", raw)
		}
	}
}

func TestVerifyListingReadyExactReadOnlyLayout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
		want   bool
	}{
		{"exact", func(p, v map[string]any) {}, true},
		{"surplus gallery", func(p, v map[string]any) {
			v["previews"] = append(v["previews"].([]any), map[string]any{"id": "extra", "media_type": "preview", "order": 4, "video": nil})
		}, false},
		{"duplicate order", func(p, v map[string]any) { v["previews"].([]any)[1].(map[string]any)["order"] = 1 }, false},
		{"missing thumbnail", func(p, v map[string]any) { p["media"] = p["media"].([]any)[:1] }, false},
		{"wrong type", func(p, v map[string]any) {
			v["previews"].([]any)[0].(map[string]any)["video"] = map[string]any{"file_cdn": "https://example.test/video.mp4"}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := listingTestInput()
			input.Target.UUID = "test-uuid"
			var inputs []MediaInput
			for _, kind := range []string{"icon", "thumbnail", "gallery", "gallery", "gallery"} {
				order := 1
				if kind == "gallery" {
					order = len(inputs) - 1
				}
				inputs = append(inputs, MediaInput{Target: input.Target, Kind: kind, Order: order, Path: "synthetic.png", Size: 1, SHA256: strings.Repeat("a", 64)})
			}
			previews := []any{}
			for i := 1; i <= 3; i++ {
				previews = append(previews, map[string]any{"id": fmt.Sprintf("preview-%d", i), "media_type": "preview", "order": i, "video": nil})
			}
			p := syntheticProduct([]any{map[string]any{"id": "icon-id", "media_type": "app-icon"}, map[string]any{"id": "thumb-id", "media_type": "thumbnail"}}, previews)
			p["tags"] = []string{"language-en", "category-tools"}
			v := p["variants"].([]any)[0].(map[string]any)
			v["is_free"] = false
			v["prices"] = []any{map[string]any{"unit_amount": 1337, "currency": "USD", "plan": "once"}}
			tc.mutate(p, v)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("verification attempted %s", r.Method)
				}
				json.NewEncoder(w).Encode(p)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, AllowLocalhost: true, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			err = client.VerifyListingReady(context.Background(), input.Target, input, inputs)
			if (err == nil) != tc.want {
				t.Fatalf("error=%v want success %v", err, tc.want)
			}
		})
	}
}
