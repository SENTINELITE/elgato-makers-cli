package maker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

func TestProvisionalReceiptDescriptionReadback(t *testing.T) {
	target := model.Target{OrgID: "synthetic-org", ProductID: "synthetic-product", VariantID: "synthetic-variant", UUID: "com.example.synthetic"}
	receipt := CreationReceipt{Target: target, Name: "Synthetic Product", Description: "<p>Synthetic description</p>\n"}
	ctx, err := WithCreationReceipt(context.Background(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
		want   bool
	}{
		{"provider trims LF", func(p, v map[string]any) {}, true},
		{"provider preserves CRLF", func(p, v map[string]any) { p["description"] = "<p>Synthetic description</p>\r\n" }, true},
		{"internal spacing changed", func(p, v map[string]any) { p["description"] = "<p>Synthetic  description</p>" }, false},
		{"leading space", func(p, v map[string]any) { p["description"] = " <p>Synthetic description</p>" }, false},
		{"trailing space", func(p, v map[string]any) { p["description"] = "<p>Synthetic description</p> \n" }, false},
		{"markup changed", func(p, v map[string]any) { p["description"] = "<div>Synthetic description</div>" }, false},
		{"product ID mismatch", func(p, v map[string]any) { p["id"] = "other-product" }, false},
		{"variant ID mismatch", func(p, v map[string]any) { v["id"] = "other-variant" }, false},
		{"name mismatch", func(p, v map[string]any) { p["name"] = "Other" }, false},
		{"status mismatch", func(p, v map[string]any) { p["status"] = "published" }, false},
		{"UUID conflict", func(p, v map[string]any) { v["streamdeckid"] = "com.example.other" }, false},
		{"version exists", func(p, v map[string]any) { v["versions"] = []any{"synthetic-version"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := map[string]any{"id": target.ProductID, "name": receipt.Name, "description": "<p>Synthetic description</p>", "status": "draft"}
			v := map[string]any{"id": target.VariantID, "streamdeckid": nil, "versions": []any{}}
			tc.mutate(p, v)
			err := listingVariantIdentity(ctx, target, p, v, func(value any) any { return value })
			if (err == nil) != tc.want {
				t.Fatalf("error=%v want success %v", err, tc.want)
			}
		})
	}
}

func TestReadyReceiptDescriptionReadback(t *testing.T) {
	for _, tc := range []struct {
		name, remoteDescription string
		want                    bool
	}{
		{"provider trims LF", "<p>Synthetic description</p>", true},
		{"provider preserves CRLF", "<p>Synthetic description</p>\r\n", true},
		{"internal spacing changed", "<p>Synthetic  description</p>", false},
		{"leading space", " <p>Synthetic description</p>", false},
		{"trailing space", "<p>Synthetic description</p> \n", false},
		{"internal newline changed", "<p>Synthetic\ndescription</p>", false},
		{"markup changed", "<div>Synthetic description</div>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := model.Target{OrgID: "synthetic-org", ProductID: "synthetic-product", VariantID: "synthetic-variant", UUID: "com.example.synthetic"}
			receipt := CreationReceipt{Target: target, Name: "Synthetic Product", Description: "<p>Synthetic description</p>\n"}
			ctx, err := WithCreationReceipt(context.Background(), receipt)
			if err != nil {
				t.Fatal(err)
			}
			details := ListingDetailsInput{Target: target, LanguageIDs: []string{"synthetic-language"}, CategoryIDs: []string{"synthetic-category"}, Price: "13.37"}
			inputs := []MediaInput{}
			for _, kind := range []string{"icon", "thumbnail", "gallery", "gallery", "gallery"} {
				order := 1
				if kind == "gallery" {
					order = len(inputs) - 1
				}
				inputs = append(inputs, MediaInput{Target: target, Kind: kind, Order: order, Path: "synthetic.png", SHA256: strings.Repeat("a", 64), Size: 1})
			}
			previews := []any{}
			for i := 1; i <= 3; i++ {
				previews = append(previews, map[string]any{"id": fmt.Sprintf("synthetic-preview-%d", i), "media_type": "preview", "order": i, "video": nil})
			}
			// The UUID already matches, exercising the independent final metadata check.
			product := map[string]any{"id": target.ProductID, "name": receipt.Name, "description": tc.remoteDescription, "status": "draft", "tags": []string{"synthetic-language", "synthetic-category"}, "media": []any{map[string]any{"id": "synthetic-icon", "media_type": "app-icon"}, map[string]any{"id": "synthetic-thumbnail", "media_type": "thumbnail"}}, "variants": []any{map[string]any{"id": target.VariantID, "streamdeckid": target.UUID, "is_free": false, "prices": []any{map[string]any{"plan": "once", "currency": "USD", "unit_amount": 1337}}, "previews": previews}}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s", r.Method)
				}
				json.NewEncoder(w).Encode(product)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, AllowLocalhost: true, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			err = client.VerifyListingReady(ctx, target, details, inputs)
			if (err == nil) != tc.want {
				t.Fatalf("error=%v want success %v", err, tc.want)
			}
		})
	}
}
