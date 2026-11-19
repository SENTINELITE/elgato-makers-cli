package maker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

func reconciliationInput() ListingDetailsInput {
	return ListingDetailsInput{Target: model.Target{OrgID: "synthetic-org", ProductID: "synthetic-product", VariantID: "synthetic-variant", UUID: "com.example.synthetic"}, LanguageIDs: []string{"language-en"}, CategoryIDs: []string{"category-tools"}, ExtraTagIDs: []string{"tag-one", "tag-two", "tag-three", "tag-four"}, Price: "6.99", Links: map[string]string{"Support": "https://example.test/support"}}
}
func reconciliationPage(input ListingDetailsInput) (map[string]any, map[string]any, map[string]any) {
	price := map[string]any{"unit_amount": 699, "currency": "USD", "plan": "once"}
	variant := map[string]any{"id": input.Target.VariantID, "streamdeckid": input.Target.UUID, "is_free": false, "prices": []any{price}, "versions": []any{}}
	tags := []any{}
	for _, ids := range [][]string{input.LanguageIDs, input.CategoryIDs, input.ExtraTagIDs} {
		for _, id := range ids {
			tags = append(tags, id)
		}
	}
	product := map[string]any{"id": input.Target.ProductID, "name": "Synthetic", "description": "Synthetic description", "status": "DRAFT", "variants": []any{variant}, "tags": tags, "additional_links": []any{map[string]any{"display_text": "Support", "url": "https://example.test/support"}}}
	return product, variant, price
}
func TestReconcileListingUsesOnlyGETAndRequiresCompleteDesiredEvidence(t *testing.T) {
	for _, kind := range []string{"exact", "wrong price", "wrong free", "wrong currency", "wrong plan", "duplicate price", "missing tag", "extra tag", "wrong link", "extra link", "missing links", "wrong product", "wrong variant", "wrong UUID", "malformed prices", "malformed response", "HTTP failure"} {
		t.Run(kind, func(t *testing.T) {
			input := reconciliationInput()
			product, variant, price := reconciliationPage(input)
			status := http.StatusOK
			switch kind {
			case "wrong price":
				price["unit_amount"] = 700
			case "wrong free":
				variant["is_free"] = true
			case "wrong currency":
				price["currency"] = "EUR"
			case "wrong plan":
				price["plan"] = "monthly"
			case "duplicate price":
				variant["prices"] = []any{price, price}
			case "missing tag":
				product["tags"] = []any{"language-en", "category-tools"}
			case "extra tag":
				product["tags"] = append(product["tags"].([]any), "unexpected")
			case "wrong link":
				product["additional_links"] = []any{map[string]any{"display_text": "Support", "url": "https://example.test/other"}}
			case "extra link":
				product["additional_links"] = append(product["additional_links"].([]any), map[string]any{"display_text": "Website", "url": "https://example.test"})
			case "missing links":
				delete(product, "additional_links")
			case "wrong product":
				product["id"] = "other"
			case "wrong variant":
				variant["id"] = "other"
			case "wrong UUID":
				variant["streamdeckid"] = "other"
			case "malformed prices":
				variant["prices"] = nil
			case "HTTP failure":
				status = http.StatusInternalServerError
			}
			reads, writes := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				reads++
				if r.URL.Path != "/synthetic-org/products/synthetic-product" {
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				w.WriteHeader(status)
				if kind == "malformed response" {
					_, _ = w.Write([]byte(`{"unrecognized":true}`))
					return
				}
				_ = json.NewEncoder(w).Encode(product)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			if err != nil {
				t.Fatal(err)
			}
			confirmed, err := client.ReconcileListing(context.Background(), input)
			if kind == "exact" || kind == "extra tag" {
				if !confirmed || err != nil {
					t.Fatalf("matching evidence unresolved: %v %v", confirmed, err)
				}
			} else if confirmed || err == nil {
				t.Fatalf("unsafe evidence confirmed: %v %v", confirmed, err)
			}
			if reads != 1 || writes != 0 {
				t.Fatalf("requests: reads=%d writes=%d", reads, writes)
			}
		})
	}
}

func TestReconcileListingNilLinksPreserveExisting(t *testing.T) {
	input := reconciliationInput()
	input.Links = nil
	product, _, _ := reconciliationPage(input)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(product)
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	confirmed, err := client.ReconcileListing(context.Background(), input)
	if !confirmed || err != nil {
		t.Fatalf("nil links did not preserve remote links: %v", err)
	}
	input.Links = map[string]string{}
	if confirmed, err = client.ReconcileListing(context.Background(), input); confirmed || err == nil {
		t.Fatal("explicit empty links accepted nonempty remote links")
	}
}

func TestReconcileListingProvisionalIdentityRequiresScopedReceipt(t *testing.T) {
	for _, receipt := range []bool{true, false} {
		input := reconciliationInput()
		product, variant, _ := reconciliationPage(input)
		variant["streamdeckid"] = nil
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("unexpected mutation %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(product)
		}))
		client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
		ctx := context.Background()
		if receipt {
			var err error
			ctx, err = WithCreationReceipt(ctx, CreationReceipt{Target: input.Target, Name: "Synthetic", Description: "Synthetic description"})
			if err != nil {
				t.Fatal(err)
			}
		}
		confirmed, err := client.ReconcileListing(ctx, input)
		server.Close()
		if receipt && (!confirmed || err != nil) {
			t.Fatalf("scoped provisional identity failed: %v", err)
		}
		if !receipt && (confirmed || err == nil) {
			t.Fatal("missing UUID confirmed without receipt")
		}
	}
}

func TestReconcileListingInvalidInputNeverReads(t *testing.T) {
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reads++; w.WriteHeader(500) }))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	for _, kind := range []string{"invalid price", "invalid link"} {
		input := reconciliationInput()
		switch kind {
		case "invalid price":
			input.Price = "bad"
		case "invalid link":
			input.Links = map[string]string{"Support": "http://example.test"}
		}
		if confirmed, err := client.ReconcileListing(context.Background(), input); confirmed || err == nil {
			t.Fatalf("invalid input confirmed: %s", kind)
		}
	}
	if reads != 0 {
		t.Fatal("invalid input read remote state")
	}
}
