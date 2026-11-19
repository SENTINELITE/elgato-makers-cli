package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckVersionMutationScopesProductAndVariant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation %s", r.Method)
			http.Error(w, "unexpected mutation", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/org/products/product/versions", "/org/products/other-product/versions":
			fmt.Fprint(w, `[
				{"id":"product","name":"Plugin","variants":[
					{"id":"variant","versions":[{"id":"one","version_number":"1","status":"published"}]},
					{"id":"review-variant","versions":[{"id":"two","version_number":"2","status":"review"}]},
					{"id":"unknown-variant","versions":[{"id":"three","version_number":"2","status":"reviewed"}]}
				]},
				{"id":"other-product","name":"Other Plugin","variants":[
					{"id":"other-variant","versions":[{"id":"four","version_number":"3","status":"review"}]}
				]}
			]`)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(Credentials{}, Config{BaseURL: server.URL, AllowLocalhost: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		product, variant, requested string
		want                        error
	}{
		{"product", "variant", "2", nil},
		{"product", "variant", "1.0.0.0", ErrDuplicateVersion},
		{"product", "review-variant", "4", ErrVersionInReview},
		{"product", "unknown-variant", "4", ErrUnrecognizedVersionLifecycle},
		{"other-product", "other-variant", "4", ErrVersionInReview},
	} {
		t.Run(tc.product+"/"+tc.variant+"/"+tc.requested, func(t *testing.T) {
			err := client.CheckVersionMutation(context.Background(), "org", tc.product, tc.variant, "com.example.plugin", tc.requested)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if err := client.CheckVersionMutation(context.Background(), "org", "product", "other-variant", "com.example.plugin", "4"); err == nil {
		t.Fatal("accepted a variant owned by a different product")
	}
}
