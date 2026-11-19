package maker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func detailsFixture() map[string]any {
	return map[string]any{"tagsets": []any{
		map[string]any{"slug": "language", "tags": []any{map[string]any{"id": "lang-en", "name": "English"}}},
		map[string]any{"slug": "os", "tags": []any{map[string]any{"id": "os-mac", "name": "Mac"}}},
	}, "product": map[string]any{"id": "synthetic-product", "organization_id": "synthetic-org", "name": "Synthetic", "description": "A private draft description", "status": "DRAFT", "tags": []string{"lang-en", "os-mac"}, "additional_links": []any{map[string]any{"display_text": "Support", "url": "https://example.test/support"}}, "variants": []any{map[string]any{"id": "synthetic-variant", "streamdeckid": "com.example.plugin", "is_free": false, "prices": []any{map[string]any{"plan": "once", "currency": "USD", "unit_amount": 1337}, map[string]any{"plan": "monthly", "currency": "EUR", "unit_amount": "500"}}}}, "secret_unselected": "not-for-output"}}
}
func TestOwnedProductDetailsTypedReadPreservesCurrenciesAndIdentity(t *testing.T) {
	input := detailsFixture()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" || r.URL.Path != "/synthetic-org/products/synthetic-product" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(input)
	}))
	defer server.Close()
	c, err := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.ProductDetails(context.Background(), "synthetic-org", "synthetic-product")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if requests != 1 || out.Description == nil || *out.Description != "A private draft description" || out.Languages == nil || (*out.Languages)[0].Name != "English" || out.Platforms == nil || (*out.Platforms)[0].Name != "Mac" || out.Links == nil || (*out.Links)[0].URL == nil || len(*out.Variants[0].Prices) != 2 || *(*out.Variants[0].Prices)[1].Currency != "EUR" || *out.Variants[0].UUID != "com.example.plugin" || strings.Contains(string(raw), "not-for-output") {
		t.Fatal(string(raw))
	}
	if len(out.UnknownFields) != 0 {
		t.Fatal(out.UnknownFields)
	}
}
func TestOwnedProductDetailsMissingUnsafeAndConflictingEvidence(t *testing.T) {
	now := time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)
	for _, kind := range []string{"missing", "unsafe_link", "paid_empty", "conflict", "ownership"} {
		t.Run(kind, func(t *testing.T) {
			root := detailsFixture()
			product := root["product"].(map[string]any)
			switch kind {
			case "missing":
				delete(root, "tagsets")
				delete(product, "description")
				delete(product, "additional_links")
				delete(product["variants"].([]any)[0].(map[string]any), "is_free")
			case "unsafe_link":
				product["additional_links"].([]any)[0].(map[string]any)["url"] = "https://storage.example.test/file?X-Amz-Signature=secret-marker"
			case "paid_empty":
				product["variants"].([]any)[0].(map[string]any)["prices"] = []any{}
			case "conflict":
				other := map[string]any{}
				for k, v := range product {
					other[k] = v
				}
				other["description"] = "Conflicting description"
				root["other"] = other
			case "ownership":
				product["organization_id"] = "other-org"
			}
			raw, _ := json.Marshal(root)
			out, err := parseProductDetails(raw, "synthetic-org", "synthetic-product", now)
			if kind == "conflict" || kind == "ownership" {
				if err == nil {
					t.Fatal("conflict accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(out)
			if strings.Contains(string(raw), "secret-marker") || strings.Contains(string(raw), "not-for-output") {
				t.Fatal("unsafe/raw provider data exposed")
			}
			switch kind {
			case "missing":
				if out.Description != nil || out.Languages != nil || out.Platforms != nil || out.Links != nil || out.Coverage["pricing"] == "returned_by_authenticated_product_page" {
					t.Fatal(string(raw))
				}
			case "unsafe_link":
				if (*out.Links)[0].URL != nil || out.Coverage["links"] == "returned_by_authenticated_product_page" {
					t.Fatal(string(raw))
				}
			case "paid_empty":
				if out.Coverage["pricing"] == "returned_by_authenticated_product_page" {
					t.Fatal(string(raw))
				}
			}
		})
	}
}
