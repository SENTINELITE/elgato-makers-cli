package maker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Reduced from the current public editor contracts. Both actions have the same
// exported name; hashes and module identifiers here are wholly synthetic.
func syntheticEditorActions(detailsHash string) string {
	return fmt.Sprintf(`(self.webpackChunk_N_E=self.webpackChunk_N_E||[]).push([[901],{
  810:(e,t,r)=>{"use strict";let d=(0,u.createServerReference)("%s",u.callServer,void 0,u.findSourceMapURL,"updateProduct");function h(e){let{product:t,organization:r}=e,u=()=>({name:t.name,description:t.description});return {title:"About",submit:{...e,id:t.id,organization:r}}}},
  911:(e,t,r)=>{"use strict";let m=(0,d.createServerReference)("%s",d.callServer,void 0,d.findSourceMapURL,"updateProduct");function h(e){let{product:t,organization:r,defaultTags:i,tagsets:d,additionalLinkCategories:v}=e,h=()=>({additional_links:t.additional_links,tags:[...t.tags]});return {title:"Details",submit:{...e,id:t.id,organization:r}}}}
 }]);`, strings.Repeat("a", 40), detailsHash)
}
func syntheticMetadataProductPage(product map[string]any) string {
	body, _ := json.Marshal(product)
	frame, _ := json.Marshal([]any{1, "p:" + string(body) + "\n"})
	return `<script>self.__next_f.push(` + string(frame) + `)</script><script src="/_next/static/editor.js"></script>`
}
func TestMetadataListingActionDiscoverySeparatesAboutAndDetails(t *testing.T) {
	got, err := metadataListingActions([]byte(syntheticEditorActions(strings.Repeat("b", 40))))
	if err != nil || len(got) != 1 || got[0] != strings.Repeat("b", 40) {
		t.Fatalf("actions=%v error=%v", got, err)
	}
	for _, source := range []string{
		strings.ReplaceAll(syntheticEditorActions(strings.Repeat("b", 40)), `title:"Details"`, `title:"About"`),
		strings.ReplaceAll(syntheticEditorActions(strings.Repeat("b", 40)), `additional_links:t.additional_links,tags:[...t.tags]`, `name:t.name,description:t.description`),
		strings.ReplaceAll(syntheticEditorActions(strings.Repeat("b", 40)), `{...e,id:t.id,organization:r}`, `{...e,product:t.id,organization_id:r}`),
	} {
		actions, err := metadataListingActions([]byte(source))
		if err == nil && len(actions) != 0 {
			t.Fatalf("accepted unrecognized Details contract: %v", actions)
		}
	}
}
func TestUpdateListingExistingPriceUsesOnlyMetadataContract(t *testing.T) {
	for _, tc := range []struct {
		name            string
		isFree, initial bool
	}{
		{"matching paid price", false, false},
		{"initial Free to paid", false, true},
		{"matching Free zero price", true, false},
		{"initial Free plan", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := listingTestInput()
			input.Target.UUID = "test-uuid"
			input.Links = map[string]string{"Support": "https://example.test/support"}
			input.IsFree = tc.isFree
			if tc.isFree {
				input.Price = "0"
			}
			product := syntheticProduct([]any{}, []any{})
			variant := product["variants"].([]any)[0].(map[string]any)
			amount := 1337
			if tc.isFree {
				amount = 0
			}
			if !tc.initial {
				variant["is_free"] = tc.isFree
				variant["prices"] = []any{map[string]any{"plan": "once", "currency": "USD", "unit_amount": amount}}
			}
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Fatal(err)
					}
					if tc.initial {
						if r.URL.Path != "/test-org/create/plugins" || r.Header.Get("Next-Action") != strings.Repeat("c", 40) {
							t.Error("initial price used wrong action")
						}
						if r.FormValue("_1_price[unit_amount]") != input.Price || r.FormValue("_1_variant") != "test-variant" {
							t.Error("initial price payload incorrect")
						}
					} else {
						if r.URL.Path != "/test-org/products/test-product" || r.Header.Get("Next-Action") != strings.Repeat("b", 40) {
							t.Error("existing price used wrong metadata action")
						}
						if r.FormValue("_1_id") != "test-product" || r.FormValue("_1_organization") != "test-org" {
							t.Error("metadata identity payload incorrect")
						}
						for key := range r.MultipartForm.Value {
							if strings.HasPrefix(key, "_1_price") || key == "_1_variant" || key == "_1_product" || key == "_1_organization_id" {
								t.Errorf("metadata save contains pricing/creation field %s", key)
							}
						}
					}
					if r.FormValue("_1_tags[2]") != "os-mac" || r.FormValue("_1_additional_links[0][url]") != "https://example.test/support" {
						t.Error("tags or links lost")
					}
					fmt.Fprint(w, "1:{\"success\":true}\n")
					return
				}
				switch r.URL.Path {
				case "/test-org/create/plugins":
					fmt.Fprint(w, syntheticListingPage(t))
				case "/test-org/products/test-product":
					fmt.Fprint(w, syntheticMetadataProductPage(product))
				case "/_next/static/listing.js":
					fmt.Fprintf(w, `createServerReference)("%s",x,"updateProductDetails")`, strings.Repeat("c", 40))
				case "/_next/static/editor.js":
					fmt.Fprint(w, syntheticEditorActions(strings.Repeat("b", 40)))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			if err := client.UpdateListing(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			if posts != 1 {
				t.Fatalf("posts=%d", posts)
			}
		})
	}
}
func TestUpdateListingExistingPricingMismatchBlocksWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong amount", func(v map[string]any) { v["prices"].([]any)[0].(map[string]any)["unit_amount"] = 699 }},
		{"wrong currency", func(v map[string]any) { v["prices"].([]any)[0].(map[string]any)["currency"] = "EUR" }},
		{"wrong plan", func(v map[string]any) { v["prices"].([]any)[0].(map[string]any)["plan"] = "monthly" }},
		{"missing price", func(v map[string]any) { delete(v, "prices") }},
		{"empty paid prices", func(v map[string]any) { v["prices"] = []any{} }},
		{"duplicate price", func(v map[string]any) { v["prices"] = append(v["prices"].([]any), v["prices"].([]any)[0]) }},
		{"unknown Free flag", func(v map[string]any) { delete(v, "is_free") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			product := syntheticProduct([]any{}, []any{})
			variant := product["variants"].([]any)[0].(map[string]any)
			variant["is_free"] = false
			variant["prices"] = []any{map[string]any{"plan": "once", "currency": "USD", "unit_amount": 1337}}
			tc.mutate(variant)
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					posts++
				}
				if r.URL.Path == "/test-org/create/plugins" {
					fmt.Fprint(w, syntheticListingPage(t))
				} else {
					fmt.Fprint(w, syntheticMetadataProductPage(product))
				}
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			err := client.UpdateListing(context.Background(), listingTestInput())
			var marker *ListingNotUpdatedError
			if !errors.As(err, &marker) || posts != 0 {
				t.Fatalf("error=%v posts=%d", err, posts)
			}
		})
	}
}
func TestAmbiguousListingPriceFailureIsFixedAndSafe(t *testing.T) {
	err := &AmbiguousListingError{Cause: errListingPriceCreation}
	if !strings.Contains(err.Error(), "details may already be saved") || !strings.Contains(err.Error(), "listing status") {
		t.Fatal(err)
	}
	unknown := &AmbiguousListingError{Cause: errors.New("synthetic-secret provider message")}
	if strings.Contains(unknown.Error(), "synthetic-secret") {
		t.Fatal("arbitrary cause leaked")
	}
}

func TestUpdateListingRechecksPricingAfterDiscovery(t *testing.T) {
	product := syntheticProduct([]any{}, []any{})
	variant := product["variants"].([]any)[0].(map[string]any)
	variant["is_free"] = false
	variant["prices"] = []any{map[string]any{"plan": "once", "currency": "USD", "unit_amount": 1337}}
	reads, posts := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			fmt.Fprint(w, "1:{\"success\":true}\n")
			return
		}
		switch r.URL.Path {
		case "/test-org/create/plugins":
			fmt.Fprint(w, syntheticListingPage(t))
		case "/test-org/products/test-product":
			reads++
			if reads >= 3 {
				variant["prices"].([]any)[0].(map[string]any)["unit_amount"] = 699
			}
			fmt.Fprint(w, syntheticMetadataProductPage(product))
		case "/_next/static/editor.js":
			fmt.Fprint(w, syntheticEditorActions(strings.Repeat("b", 40)))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	err := client.UpdateListing(context.Background(), listingTestInput())
	var marker *ListingNotUpdatedError
	if !errors.As(err, &marker) || posts != 0 || reads != 3 {
		t.Fatalf("error=%v posts=%d reads=%d", err, posts, reads)
	}
}
func TestMetadataListingDiscoveryRejectsConflictingContracts(t *testing.T) {
	source := syntheticEditorActions(strings.Repeat("b", 40)) + syntheticEditorActions(strings.Repeat("c", 40))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("discovery mutation: %s", r.Method)
		}
		fmt.Fprint(w, source)
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	if _, err := client.discoverMetadataListingAction(context.Background(), "/synthetic/products/synthetic-product"); err == nil {
		t.Fatal("accepted conflicting metadata action references")
	}
}
func TestUpdateListingPriceFailureReportsFixedCategoryOnly(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		known         bool
	}{
		{"known price failure", "There was an error creating a price for this product.", true},
		{"unknown private failure", "synthetic-secret provider detail", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
					body, _ := json.Marshal(map[string]any{"success": false, "message": tc.message})
					fmt.Fprintf(w, "1:%s\n", body)
					return
				}
				switch r.URL.Path {
				case "/test-org/create/plugins":
					fmt.Fprint(w, syntheticListingPage(t))
				case "/test-org/products/test-product":
					json.NewEncoder(w).Encode(syntheticProduct([]any{}, []any{}))
				case "/_next/static/listing.js":
					fmt.Fprintf(w, `createServerReference)("%s",x,"updateProductDetails")`, strings.Repeat("c", 40))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			err := client.UpdateListing(context.Background(), listingTestInput())
			var ambiguous *AmbiguousListingError
			var notUpdated *ListingNotUpdatedError
			if !errors.As(err, &ambiguous) || errors.As(err, &notUpdated) || posts != 1 {
				t.Fatalf("ambiguity changed: %v posts=%d", err, posts)
			}
			if strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "details may already be saved") != tc.known {
				t.Fatalf("unsafe/incorrect diagnostic: %v", err)
			}
		})
	}
}

func TestMetadataListingActionRecognizesFirstModule(t *testing.T) {
	source := fmt.Sprintf(`({910:(e,t,r)=>{let m=(0,d.createServerReference)("%s",d.callServer,void 0,d.findSourceMapURL,"updateProduct");function h(e){let{product:t,organization:r}=e,h=()=>({additional_links:t.additional_links,tags:[...t.tags]});return {title:"Details",submit:{...e,id:t.id,organization:r}}}}})`, strings.Repeat("b", 40))
	actions, err := metadataListingActions([]byte(source))
	if err != nil || len(actions) != 1 || actions[0] != strings.Repeat("b", 40) {
		t.Fatalf("first module actions=%v error=%v", actions, err)
	}
}
