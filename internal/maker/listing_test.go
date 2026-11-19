package maker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SENTINELITE/elgato-makers-cli/internal/mediatest"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func listingTestInput() ListingDetailsInput {
	return ListingDetailsInput{Target: model.Target{OrgID: "test-org", ProductID: "test-product", VariantID: "test-variant"}, LanguageIDs: []string{"language-en"}, CategoryIDs: []string{"category-tools"}, Price: "13.37"}
}
func TestListingFieldsDecimalUSD(t *testing.T) {
	f, err := listingFields(listingTestInput())
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"_1_price[unit_amount]": "13.37", "_1_price[currency]": "USD", "_1_price[plan]": "once", "_1_tags[0]": "language-en", "_1_tags[1]": "category-tools", "_1_product": "test-product", "_1_variant": "test-variant"} {
		if f[k] != want {
			t.Errorf("%s = %q, want %q", k, f[k], want)
		}
	}
}
func TestListingFieldsRejectInvalidInput(t *testing.T) {
	for _, price := range []string{"", "-1", "NaN", "13.370", "1e2", "1,000", "01.00"} {
		in := listingTestInput()
		in.Price = price
		if _, err := listingFields(in); err == nil {
			t.Errorf("accepted invalid price %q", price)
		}
	}
	in := listingTestInput()
	in.CategoryIDs = []string{"language-en"}
	if _, err := listingFields(in); err == nil {
		t.Error("accepted duplicate taxonomy")
	}
	in = listingTestInput()
	in.IsFree = true
	if _, err := listingFields(in); err == nil {
		t.Error("accepted conflicting free price")
	}
	in = listingTestInput()
	in.Links = map[string]string{"website": "http://example.test"}
	if _, err := listingFields(in); err == nil {
		t.Error("accepted insecure link")
	}
}
func TestListingResolve(t *testing.T) {
	m := ListingMetadata{Languages: []ListingOption{{"lang-en", "English"}}, Categories: []ListingOption{{"cat-a", "Tools"}, {"cat-b", "Tools"}}}
	if id, err := m.Resolve("language", " english "); err != nil || id != "lang-en" {
		t.Fatalf("resolve = %q, %v", id, err)
	}
	if _, err := m.Resolve("category", "Tools"); err == nil {
		t.Error("accepted ambiguous category")
	}
	if _, err := m.Resolve("language", ""); err == nil {
		t.Error("accepted absent language")
	}
}

func TestListingActionDiscoveryStaysSameOrigin(t *testing.T) {
	hash := strings.Repeat("a", 40)
	var foreign int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign++; w.WriteHeader(500) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/test-org/create/plugins":
			fmt.Fprintf(w, `<script src="%s/_next/static/evil.js"></script><script src="/_next/static/listing.js"></script>`, other.URL)
		case "/_next/static/listing.js":
			fmt.Fprintf(w, `createServerReference)("%s",x,"syntheticListingAction")`, hash)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.discoverListingAction(context.Background(), "/test-org/create/plugins", "syntheticListingAction")
	if err != nil || got != hash {
		t.Fatalf("action=%q, err=%v", got, err)
	}
	if foreign != 0 {
		t.Fatal("fetched foreign script")
	}
}
func TestListingActionDiscoveryConflicts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `createServerReference)("%s",x,"syntheticListingAction");createServerReference)("%s",x,"syntheticListingAction")`, strings.Repeat("a", 40), strings.Repeat("b", 40))
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	if _, err := client.discoverListingAction(context.Background(), "/test-org/create/plugins", "syntheticListingAction"); err == nil {
		t.Fatal("accepted conflicting action hashes")
	}
}

func syntheticListingPage(t *testing.T) string {
	t.Helper()
	root := map[string]any{"tagsets": []any{
		map[string]any{"id": "group-language", "name": "Language", "label": "Language", "slug": "language", "selection": "multiple", "tags": []any{map[string]any{"id": "language-en", "name": "English"}}},
		map[string]any{"id": "group-type", "name": "Type", "label": "Type", "slug": "type", "selection": "multiple", "tags": []any{map[string]any{"id": "category-tools", "name": "Tools"}}},
		map[string]any{"id": "group-os", "name": "OS", "label": "OS", "slug": "os", "selection": "multiple", "tags": []any{map[string]any{"id": "os-mac", "name": "Mac"}}},
	}, "additionalLinkCategories": []string{"Support"}}
	b, _ := json.Marshal(root)
	frame, _ := json.Marshal([]any{1, "1:" + string(b) + "\n"})
	return "<script>self.__next_f.push(" + string(frame) + ")</script><script src=\"/_next/static/listing.js\"></script>"
}
func syntheticProduct(media, previews []any) map[string]any {
	return map[string]any{"id": "test-product", "name": "Synthetic Product", "status": "DRAFT", "tags": []string{"os-mac"}, "media": media, "variants": []any{map[string]any{"id": "test-variant", "streamdeckid": "test-uuid", "is_free": true, "prices": []any{}, "versions": []any{}, "previews": previews}}}
}
func TestListingUpdatePreservesCompatibilityTags(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/test-org/create/plugins":
			if r.Method == "GET" {
				fmt.Fprint(w, syntheticListingPage(t))
				return
			}
			posts++
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			if r.FormValue("_1_price[unit_amount]") != "13.37" {
				t.Error("decimal USD changed")
			}
			if r.FormValue("_1_tags[2]") != "os-mac" {
				t.Error("compatibility tag removed")
			}
			fmt.Fprint(w, "1:{\"success\":true}\n")
		case "/_next/static/listing.js":
			fmt.Fprintf(w, `createServerReference)("%s",x,"updateProductDetails")`, strings.Repeat("a", 40))
		case "/test-org/products/test-product":
			json.NewEncoder(w).Encode(syntheticProduct([]any{}, []any{}))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	if err := client.UpdateListing(context.Background(), listingTestInput()); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatal("listing mutation count", posts)
	}
}
func TestMediaUploadReplacesExistingSlotWithoutCookies(t *testing.T) {
	image := syntheticPNG(t, 288, 288)
	digest := sha256.Sum256(image)
	imagePath := filepath.Join(t.TempDir(), "icon.png")
	if err := os.WriteFile(imagePath, image, 0600); err != nil {
		t.Fatal(err)
	}
	in := MediaInput{Target: model.Target{OrgID: "test-org", ProductID: "test-product", VariantID: "test-variant", UUID: "test-uuid"}, Kind: "icon", Order: 1, Path: imagePath, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(image))}
	var server *httptest.Server
	puts, patches := 0, 0
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/test-org/products/test-product":
			json.NewEncoder(w).Encode(syntheticProduct([]any{map[string]any{"id": "existing-icon", "media_type": "app-icon", "file": "https://cdn.example.test/transformed.png"}}, []any{}))
		case "/api/presigned-url":
			if !strings.HasPrefix(r.URL.Query().Get("file"), "media/test-org/test-product/") {
				t.Error("invalid media key")
			}
			json.NewEncoder(w).Encode(map[string]string{"presignedUrl": server.URL + "/fake-s3?signature=synthetic"})
		case "/fake-s3":
			puts++
			if r.Method != "PUT" || r.Header.Get("Cookie") != "" {
				t.Error("storage received wrong method or Maker cookie")
			}
			got, _ := io.ReadAll(r.Body)
			if !bytes.Equal(got, image) {
				t.Error("image bytes changed")
			}
		case "/api/organizations/test-org/products/test-product/media/existing-icon":
			patches++
			if r.Method != "PATCH" {
				t.Error("existing image was appended")
			}
			r.ParseMultipartForm(1 << 20)
			if r.FormValue("media_type") != "app-icon" || r.FormValue("order") != "1" || strings.HasPrefix(r.FormValue("s3_file_id"), "media/") {
				t.Error("invalid association fields")
			}
			fmt.Fprint(w, `{"id":"existing-icon"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true, Cookies: []*http.Cookie{{Name: "fake_session", Value: "synthetic"}}})
	result, err := client.UploadMedia(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Acknowledged || result.MediaID != "existing-icon" || puts != 1 || patches != 1 {
		t.Fatalf("result=%+v puts=%d patches=%d", result, puts, patches)
	}
	reconciled, err := client.ReconcileMedia(context.Background(), in, result)
	if err != nil || !reconciled.Reconciled {
		t.Fatalf("reconcile %+v %v", reconciled, err)
	}
	result.Acknowledged = false
	reconciled, err = client.ReconcileMedia(context.Background(), in, result)
	if err != nil || reconciled.Reconciled {
		t.Fatal("unacknowledged PATCH treated as proven")
	}
}
func TestMediaDuplicateGalleryOrderBlocksBeforeUpload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/test-org/products/test-product" {
			t.Error("mutation or upload occurred before conflict detection")
		}
		json.NewEncoder(w).Encode(syntheticProduct([]any{}, []any{map[string]any{"id": "first", "media_type": "preview", "video": nil, "order": 1}, map[string]any{"id": "second", "media_type": "preview", "video": nil, "order": 1}}))
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	in := MediaInput{Target: model.Target{OrgID: "test-org", ProductID: "test-product", VariantID: "test-variant"}, Kind: "gallery", Order: 1, Path: "gallery.png", SHA256: strings.Repeat("a", 64), Size: 1}
	if _, err := client.mediaSlots(context.Background(), in); err == nil {
		t.Fatal("accepted conflicting gallery order")
	}
}

func TestExistingListingPlanRejectsSurplusAndPricingTransition(t *testing.T) {
	for _, test := range []struct {
		name   string
		free   bool
		orders []int
	}{{"surplus", false, []int{1, 2, 3, 4}}, {"duplicate", false, []int{1, 2, 2}}, {"pricing", true, []int{1, 2, 3}}} {
		t.Run(test.name, func(t *testing.T) {
			var previews []any
			for i, order := range test.orders {
				previews = append(previews, map[string]any{"id": fmt.Sprintf("preview-%d", i), "order": order, "media_type": "preview", "video": nil})
			}
			product := syntheticProduct([]any{}, previews)
			variant := product["variants"].([]any)[0].(map[string]any)
			variant["is_free"] = test.free
			variant["versions"] = []any{map[string]any{"id": "submitted-version"}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("plan validation mutated remote")
				}
				json.NewEncoder(w).Encode(product)
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			target := model.Target{OrgID: "test-org", ProductID: "test-product", VariantID: "test-variant", UUID: "test-uuid"}
			if err := client.ValidateExistingListingPlan(context.Background(), target, false, 3); err == nil {
				t.Fatal("accepted incompatible existing listing plan")
			}
		})
	}
}
func TestResolveUploadTagsExcludesUserTaxonomy(t *testing.T) {
	m := ListingMetadata{Tagsets: []ListingTagset{{ID: "group-language", Label: "Languages", Slug: "language", Tags: []ListingOption{{ID: "lang-en", Name: "English"}}}, {ID: "group-os", Label: "OS", Slug: "os", Tags: []ListingOption{{ID: "os-mac", Name: "Mac"}}}}}
	got, err := m.ResolveUploadTags(map[string][]string{"Languages": {"English"}, "OS": {"Mac"}})
	if err != nil || len(got) != 1 || got[0] != "os-mac" {
		t.Fatalf("tags=%v err=%v", got, err)
	}
	if _, err := m.ResolveUploadTags(map[string][]string{"Unknown": {"Something"}}); err == nil {
		t.Fatal("accepted unknown package tag group")
	}
}

func TestListingLinksIntentSurvivesJSONRoundTrip(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		input := listingTestInput()
		if explicit {
			input.Links = map[string]string{}
		}
		b, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var restored ListingDetailsInput
		if err = json.Unmarshal(b, &restored); err != nil {
			t.Fatal(err)
		}
		if (restored.Links != nil) != explicit {
			t.Fatalf("explicit=%v links=%#v JSON=%s", explicit, restored.Links, b)
		}
		fields, err := listingFields(restored)
		if err != nil {
			t.Fatal(err)
		}
		for field := range fields {
			if strings.HasPrefix(field, "_1_additional_links[") {
				t.Fatal("empty or omitted links emitted an invented link")
			}
		}
	}
}
func TestMediaIdentityRejectsUnobservedFormats(t *testing.T) {
	for _, ext := range []string{"gif", "webp"} {
		input := MediaInput{Target: model.Target{OrgID: "test-org", ProductID: "test-product", VariantID: "test-variant"}, Kind: "icon", Order: 1, Path: "icon." + ext, SHA256: strings.Repeat("a", 64), Size: 1}
		if err := ValidateMediaInput(input); err == nil {
			t.Errorf("accepted %s", ext)
		}
	}
}
func TestListingPricingCannotChangeOutsideRecognizedEmptyDraft(t *testing.T) {
	for _, status := range []string{"PENDING", "PUBLISHED", ""} {
		t.Run(status, func(t *testing.T) {
			product := syntheticProduct([]any{}, []any{})
			product["status"] = status
			variant := product["variants"].([]any)[0].(map[string]any)
			variant["is_free"] = true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/test-org/create/plugins":
					if r.Method != "GET" {
						t.Error("pricing mutation dispatched")
					}
					fmt.Fprint(w, syntheticListingPage(t))
				case "/test-org/products/test-product":
					json.NewEncoder(w).Encode(product)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			if err := client.ValidateExistingListingPlan(context.Background(), listingTestInput().Target, false, 3); err == nil {
				t.Error("plan accepted pricing transition outside draft")
			}
			if err := client.UpdateListing(context.Background(), listingTestInput()); err == nil {
				t.Error("update accepted pricing transition outside draft")
			}
		})
	}
}
func TestListingMissingPricingFailsBeforeMutation(t *testing.T) {
	product := syntheticProduct([]any{}, []any{})
	delete(product["variants"].([]any)[0].(map[string]any), "is_free")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/test-org/create/plugins":
			if r.Method != "GET" {
				t.Error("pricing mutation dispatched")
			}
			fmt.Fprint(w, syntheticListingPage(t))
		case "/test-org/products/test-product":
			json.NewEncoder(w).Encode(product)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	if err := client.UpdateListing(context.Background(), listingTestInput()); err == nil {
		t.Error("accepted missing is_free")
	}
}

func TestGalleryUnsupportedMediaBlocksPlanAndUpload(t *testing.T) {
	for _, test := range []struct {
		name, kind string
		video      any
		omitVideo  bool
	}{{"video-object", "preview", map[string]any{"id": "video-record"}, false}, {"video-false", "preview", false, false}, {"missing-video", "preview", nil, true}, {"non-preview", "video", nil, false}} {
		t.Run(test.name, func(t *testing.T) {
			preview := map[string]any{"id": "existing-artwork", "order": 1, "media_type": test.kind}
			if !test.omitVideo {
				preview["video"] = test.video
			}
			product := syntheticProduct([]any{}, []any{preview})
			mutations := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/test-org/products/test-product" {
					mutations++
					w.WriteHeader(500)
					return
				}
				json.NewEncoder(w).Encode(product)
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			target := model.Target{OrgID: "test-org", ProductID: "test-product", VariantID: "test-variant", UUID: "test-uuid"}
			if err := client.ValidateExistingListingPlan(context.Background(), target, false, 3); err == nil {
				t.Error("plan accepted unsupported media")
			}
			image := syntheticPNG(t, 1920, 960)
			sum := sha256.Sum256(image)
			path := filepath.Join(t.TempDir(), "gallery.png")
			if err := os.WriteFile(path, image, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := client.UploadMedia(context.Background(), MediaInput{Target: target, Kind: "gallery", Order: 1, Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(image))})
			if err == nil {
				t.Error("upload accepted unsupported media")
			}
			var preflight *MediaNotUploadedError
			if !errors.As(err, &preflight) {
				t.Fatalf("expected not-dispatched error, got %v", err)
			}
			if mutations != 0 {
				t.Fatal("upload dispatched before unsupported-media rejection")
			}
		})
	}
}

func TestGalleryRecognizedImageRemainsReplaceable(t *testing.T) {
	product := syntheticProduct([]any{}, []any{map[string]any{"id": "existing-image", "media_type": "preview", "video": nil, "order": 1}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(product) }))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	target := model.Target{OrgID: "test-org", ProductID: "test-product", VariantID: "test-variant", UUID: "test-uuid"}
	if err := client.ValidateExistingListingPlan(context.Background(), target, false, 3); err != nil {
		t.Fatal(err)
	}
	slots, err := client.mediaSlots(context.Background(), MediaInput{Target: target, Kind: "gallery", Order: 1})
	if err != nil || len(slots) != 1 || slots[0] != "existing-image" {
		t.Fatalf("slots=%v err=%v", slots, err)
	}
}

func syntheticPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestMediaPNGValidationPrecedesRemoteAccess(t *testing.T) {
	valid := syntheticPNG(t, 288, 288)
	for _, test := range []struct {
		name string
		data []byte
	}{{"not-png", []byte("invalid image")}, {"wrong-dimensions", syntheticPNG(t, 32, 32)}, {"truncated-png", valid[:len(valid)/2]}} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(500) }))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			path := filepath.Join(t.TempDir(), "icon.png")
			if err := os.WriteFile(path, test.data, 0600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(test.data)
			_, err := client.UploadMedia(context.Background(), MediaInput{Target: listingTestInput().Target, Kind: "icon", Order: 1, Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(test.data))})
			if err == nil {
				t.Fatal("accepted invalid marketplace PNG")
			}
			var preflight *MediaNotUploadedError
			if !errors.As(err, &preflight) {
				t.Fatalf("expected not-dispatched error, got %v", err)
			}
			if requests != 0 {
				t.Fatal("accessed remote API before local PNG validation")
			}
		})
	}
}
func TestMarketplaceGalleryLimits(t *testing.T) {
	for _, order := range []int{0, 11} {
		in := MediaInput{Target: listingTestInput().Target, Kind: "gallery", Order: order, Path: "gallery.png", SHA256: strings.Repeat("a", 64), Size: 1}
		if err := ValidateMediaInput(in); err == nil {
			t.Errorf("accepted gallery order %d", order)
		}
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(500) }))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	for _, count := range []int{0, 1, 2, 11} {
		if err := client.ValidateExistingListingPlan(context.Background(), listingTestInput().Target, false, count); err == nil {
			t.Errorf("accepted gallery count %d", count)
		}
	}
	if requests != 0 {
		t.Fatal("requested remote state for invalid gallery count")
	}
}

func TestVideoGalleryUploadUsesVideoWireContract(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint("replace=", replace), func(t *testing.T) {
			data := mediatest.MP4()
			sum := sha256.Sum256(data)
			path := filepath.Join(t.TempDir(), "gallery.mp4")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			in := MediaInput{Target: model.Target{OrgID: "test-org", ProductID: "test-product", VariantID: "test-variant", UUID: "test-uuid"}, Kind: "gallery", Order: 1, Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
			previews := []any{}
			if replace {
				previews = []any{map[string]any{"id": "existing-video", "media_type": "preview", "order": 1, "video": map[string]any{"file_cdn": "https://video-cdn.example.test/rendered.mp4"}}}
			}
			var server *httptest.Server
			puts, associations := 0, 0
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/test-org/products/test-product":
					json.NewEncoder(w).Encode(syntheticProduct([]any{}, previews))
				case "/api/presigned-url":
					if !strings.HasPrefix(r.URL.Query().Get("file"), "media/test-org/test-product/test-variant/") || !strings.HasSuffix(r.URL.Query().Get("file"), ".mp4") {
						t.Error("wrong video storage key")
					}
					json.NewEncoder(w).Encode(map[string]string{"presignedUrl": server.URL + "/fake-s3?signature=synthetic"})
				case "/fake-s3":
					puts++
					if r.Method != "PUT" || r.Header.Get("Cookie") != "" {
						t.Error("video PUT leaked Maker cookie or used wrong method")
					}
					if r.Header.Get("Content-Type") != "video/mp4" {
						t.Error("video PUT omitted inspected MIME type")
					}
					got, _ := io.ReadAll(r.Body)
					if !bytes.Equal(got, data) {
						t.Error("video payload changed")
					}
				case "/api/organizations/test-org/products/test-product/variants/test-variant/media", "/api/organizations/test-org/products/test-product/variants/test-variant/media/existing-video":
					associations++
					want := "POST"
					if replace {
						want = "PATCH"
					}
					if r.Method != want {
						t.Errorf("method %s want %s", r.Method, want)
					}
					r.ParseMultipartForm(1 << 20)
					if r.FormValue("file_type") != "video" || r.FormValue("order") != "1" || r.FormValue("media_type") != "" {
						t.Error("wrong video association fields")
					}
					fmt.Fprint(w, `{"media_id":"acknowledged-video"}`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true, Cookies: []*http.Cookie{{Name: "fake_session", Value: "synthetic"}}})
			if err := client.ValidateExistingListingPlan(context.Background(), in.Target, false, 3); err != nil {
				t.Fatal(err)
			}
			out, err := client.UploadMedia(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if !out.Acknowledged || out.MediaID != "acknowledged-video" || puts != 1 || associations != 1 {
				t.Fatalf("result=%+v puts=%d associations=%d", out, puts, associations)
			}
		})
	}
}
func TestGalleryCrossTypePlanFailsBeforeWrites(t *testing.T) {
	for _, desiredVideo := range []bool{false, true} {
		t.Run(fmt.Sprint("desired-video=", desiredVideo), func(t *testing.T) {
			target := model.Target{OrgID: "test-org", ProductID: "test-product", VariantID: "test-variant", UUID: "test-uuid"}
			imageData := syntheticPNG(t, 1920, 960)
			videoData := mediatest.MP4()
			var inputs []MediaInput
			for i := 1; i <= 3; i++ {
				data, ext := imageData, ".png"
				if i == 2 && desiredVideo {
					data, ext = videoData, ".mp4"
				}
				path := filepath.Join(t.TempDir(), fmt.Sprintf("gallery-%d%s", i, ext))
				os.WriteFile(path, data, 0600)
				sum := sha256.Sum256(data)
				inputs = append(inputs, MediaInput{Target: target, Kind: "gallery", Order: i, Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))})
			}
			var remoteVideo any
			if !desiredVideo {
				remoteVideo = map[string]any{"file_cdn": "https://video-cdn.example.test/existing.mp4"}
			}
			previews := []any{map[string]any{"id": "slot-two", "media_type": "preview", "order": 2, "video": remoteVideo}}
			badRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/test-org/products/test-product" {
					badRequests++
					w.WriteHeader(500)
					return
				}
				json.NewEncoder(w).Encode(syntheticProduct([]any{}, previews))
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			if err := client.ValidateGallery(context.Background(), target, inputs); err == nil {
				t.Error("accepted cross-type whole gallery plan")
			}
			if _, err := client.UploadMedia(context.Background(), inputs[1]); err == nil {
				t.Error("accepted cross-type individual upload")
			}
			if badRequests != 0 {
				t.Error("cross-type plan dispatched upload or mutation")
			}
			row := previews[0].(map[string]any)
			row["video"] = nil
			if desiredVideo {
				row["video"] = map[string]any{"file_cdn": "https://video-cdn.example.test/existing.mp4"}
			}
			if err := client.ValidateGallery(context.Background(), target, inputs); err != nil {
				t.Fatalf("matching gallery plan failed: %v", err)
			}
		})
	}
}
func TestJPEGMarketplaceImageSupported(t *testing.T) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 288, 288)), nil); err != nil {
		t.Fatal(err)
	}
	data := b.Bytes()
	path := filepath.Join(t.TempDir(), "icon.jpg")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	input := MediaInput{Target: listingTestInput().Target, Kind: "icon", Order: 1, Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	if err := ValidateMediaInput(input); err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/test-org/products/test-product":
			json.NewEncoder(w).Encode(syntheticProduct([]any{}, []any{}))
		case "/api/presigned-url":
			json.NewEncoder(w).Encode(map[string]string{"presignedUrl": server.URL + "/fake-s3"})
		case "/fake-s3":
			w.WriteHeader(200)
		case "/api/organizations/test-org/products/test-product/media":
			r.ParseMultipartForm(1 << 20)
			if r.FormValue("file_type") != "media" {
				t.Error("JPEG used video file type")
			}
			fmt.Fprint(w, `{"id":"jpeg-icon"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	if _, err := client.UploadMedia(context.Background(), input); err != nil {
		t.Fatal(err)
	}
}

func TestListingLowercaseDraftAllowsInitialPaidPlan(t *testing.T) {
	product := syntheticProduct([]any{}, []any{})
	product["status"] = "draft"
	variant := product["variants"].([]any)[0].(map[string]any)
	variant["is_free"] = true
	variant["streamdeckid"] = nil
	product["description"] = "Synthetic description"
	input := listingTestInput()
	input.Target.UUID = "test-uuid"
	ctx, err := WithCreationReceipt(context.Background(), CreationReceipt{Target: input.Target, Name: "Synthetic Product", Description: "Synthetic description"})
	if err != nil {
		t.Fatal(err)
	}
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/test-org/products/test-product":
			json.NewEncoder(w).Encode(product)
		case "/test-org/create/plugins":
			if r.Method == "GET" {
				fmt.Fprint(w, syntheticListingPage(t))
				return
			}
			posts++
			fmt.Fprint(w, "1:{\"success\":true}\n")
		case "/_next/static/listing.js":
			fmt.Fprintf(w, `createServerReference)("%s",x,"updateProductDetails")`, strings.Repeat("a", 40))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	if err := client.ValidateExistingListingPlan(ctx, input.Target, false, 3); err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateListing(ctx, input); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("updates=%d", posts)
	}
}
