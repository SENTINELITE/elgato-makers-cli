package v1

// The provider fixture mirrors the proven CLI direct-submit contract while
// exercising the independently imported public workflow adapter entry points.
import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/SENTINELITE/elgato-makers-cli/internal/listing"
	"github.com/SENTINELITE/elgato-makers-cli/pkg/connect"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const listingTestDescription = "Demo helps Stream Deck users trigger common actions from a clear set of buttons. Configure each action in the property inspector, choose a label, and organize related controls together. The plugin includes practical defaults and visible feedback so users can see which action is ready before pressing a key."

func fullBridgeListingFixture(t *testing.T) (string, ListingConfig) {
	t.Helper()
	dir := t.TempDir()
	pkg := filepath.Join(dir, "demo.streamDeckPlugin")
	f, e := os.Create(pkg)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	entry, e := z.Create("com.example.demo.sdPlugin/manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = io.WriteString(entry, `{"Name":"Demo","UUID":"com.example.demo","Version":"1.0.0.5","SDKVersion":3,"Software":{"MinimumVersion":"7.1"},"OS":[{"Platform":"mac","MinimumVersion":"12"}]}`); e != nil {
		t.Fatal(e)
	}
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	notes := filepath.Join(dir, "notes.md")
	description := filepath.Join(dir, "description.md")
	if e = os.WriteFile(notes, []byte("**Updates:**\n\n- Improved the dial."), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(description, []byte(listingTestDescription), 0600); e != nil {
		t.Fatal(e)
	}
	artwork := func(name string, width, height int, seed uint8) string {
		path := filepath.Join(dir, name)
		f, e := os.Create(path)
		if e != nil {
			t.Fatal(e)
		}
		img := image.NewGray(image.Rect(0, 0, width, height))
		img.SetGray(0, 0, color.Gray{Y: seed})
		if e = png.Encode(f, img); e != nil {
			t.Fatal(e)
		}
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
		return path
	}
	no := false
	return dir, ListingConfig{SchemaVersion: 1, Name: "Demo", PackagePath: pkg, DescriptionFile: description, ReleaseNotesFile: notes, Pricing: ListingPricing{Type: "paid", Amount: "6.99", Currency: "USD"}, Languages: []string{"English"}, Categories: []string{"Gaming"}, AutoPublish: &no, Assets: ListingAssets{Icon: artwork("icon.png", 288, 288, 1), Thumbnail: artwork("thumb.png", 1920, 960, 2), Gallery: []string{artwork("one.png", 1920, 960, 3), artwork("two.png", 1920, 960, 4), artwork("three.png", 1920, 960, 5)}}}
}
func TestFullPaidListingSubmitViaPublicBridge(t *testing.T) {
	const scriptPath = "/_next/static/listing-0123456789abcdef.js"
	var requestCount, scriptRequests atomic.Int64
	dir, config := fullBridgeListingFixture(t)
	pkg, err := os.ReadFile(config.PackagePath)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	manifestFile, err := archive.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := io.ReadAll(manifestFile)
	manifestFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	actionIDs := map[string]string{"createProduct": strings.Repeat("a", 40), "uploadAux": strings.Repeat("b", 40), "updateProductDetails": strings.Repeat("c", 40), "getIsSdVersionValid": strings.Repeat("d", 40), "createVersion": strings.Repeat("e", 40), "metadataDetails": strings.Repeat("f", 40)}
	created, submitted, detailsUpdated := false, false, false
	var writes []string
	var versions []any = []any{}
	var media []any = []any{}
	var previews []any = []any{}
	page := func(w http.ResponseWriter, data any) {
		b, _ := json.Marshal(data)
		frame, _ := json.Marshal([]any{1, "1:" + string(b) + "\n"})
		fmt.Fprintf(w, `<script src="%s"></script><script>self.__next_f.push(%s)</script>`, scriptPath, frame)
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if r.Method == "PUT" {
			writes = append(writes, "put")
			if r.Header.Get("Cookie") != "" {
				t.Error("Maker credential reached storage")
			}
			io.Copy(io.Discard, r.Body)
			return
		}
		if r.Method == "POST" {
			if r.Header.Get("Next-Action") == actionIDs["getIsSdVersionValid"] {
				writes = append(writes, "version-check")
				fmt.Fprint(w, "1:{\"is_valid\":true}\n")
				return
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				http.Error(w, "bad form", 400)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/organizations/") {
				kind := r.FormValue("media_type")
				if kind == "" {
					kind = "preview"
				}
				writes = append(writes, kind+":"+r.FormValue("order"))
				id := fmt.Sprintf("media-%d", len(media)+len(previews)+1)
				row := map[string]any{"id": id, "media_type": kind, "order": len(previews) + 1, "video": nil}
				if kind == "preview" {
					previews = append(previews, row)
				} else {
					media = append(media, row)
				}
				json.NewEncoder(w).Encode(map[string]string{"id": id})
				return
			}
			switch r.Header.Get("Next-Action") {
			case actionIDs["createProduct"]:
				writes = append(writes, "create")
				created = true
				if r.FormValue("_1_name") != "Demo" {
					t.Error("wrong product name")
				}
				fmt.Fprint(w, "1:{\"success\":true,\"productId\":\"product\",\"variantId\":\"variant\"}\n")
			case actionIDs["uploadAux"]:
				writes = append(writes, "uploadAux")
				fmt.Fprint(w, "1:{\"success\":true,\"id\":\"folder\"}\n")
			case actionIDs["updateProductDetails"]:
				if detailsUpdated {
					t.Error("attempted to initialize pricing twice")
				}
				writes = append(writes, "details")
				detailsUpdated = true
				if r.FormValue("_1_price[unit_amount]") != "6.99" || r.FormValue("_1_price[currency]") != "USD" || r.FormValue("_1_tags[0]") != "lang-en" || r.FormValue("_1_tags[1]") != "cat-gaming" {
					t.Error("listing taxonomy or price lost")
				}
				fmt.Fprint(w, "1:{\"success\":true}\n")
			case actionIDs["metadataDetails"]:
				writes = append(writes, "details")
				if r.URL.Path != "/org/products/product" || !detailsUpdated || r.FormValue("_1_id") != "product" || r.FormValue("_1_organization") != "org" || r.FormValue("_1_tags[0]") != "lang-en" || r.FormValue("_1_tags[1]") != "cat-gaming" {
					t.Error("metadata-only details contract changed")
				}
				for key := range r.MultipartForm.Value {
					if strings.HasPrefix(key, "_1_price[") || key == "_1_product" || key == "_1_variant" || key == "_1_organization_id" {
						t.Error("metadata-only update included pricing or wizard fields")
					}
				}
				fmt.Fprint(w, "1:{\"success\":true}\n")
			case actionIDs["getIsSdVersionValid"]:
				writes = append(writes, "version-check")
				fmt.Fprint(w, "1:{\"is_valid\":true}\n")
			case actionIDs["createVersion"]:
				writes = append(writes, "submit")
				if submitted {
					t.Error("duplicate submit")
				}
				submitted = true
				if r.FormValue("_1_version") != "1.0.0.5" || r.FormValue("_1_is_ready_for_publish") != "false" {
					t.Error("autopublish choice changed")
				}
				versions = []any{map[string]any{"id": "version", "version_number": "1.0.0.5", "status": "PENDING_REVIEW", "release_notes": r.FormValue("_1_release_notes"), "is_ready_for_publish": false}}
				fmt.Fprint(w, "1:{\"success\":true,\"id\":\"version\"}\n")
			default:
				t.Errorf("unknown action: %s", r.Header.Get("Next-Action"))
				http.Error(w, "unknown", 400)
			}
			return
		}
		switch r.URL.Path {
		case scriptPath:
			scriptRequests.Add(1)
			// Mirror the route and form-field capabilities exposed by the
			// observed Maker client bundle; production preflight remains strict.
			fmt.Fprint(w, `async function uploadMedia(org,product,variant,file,kind){const form=new FormData();form.set("s3_file_id",file);form.set("media_type",kind);const signed=await fetch("/api/presigned-url?file="+file);const path="/api/organizations/"+org+"/products/"+product+(variant?"/variants/"+variant:"")+"/media";return fetch(path,{method:"POST",body:form})}const mediaKinds=["app-icon","thumbnail"];`)
			for name, id := range actionIDs {
				if name == "metadataDetails" {
					continue
				}
				fmt.Fprintf(w, `(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"%s");`, id, name)
			}
			fmt.Fprintf(w, `({910:(e,t,r)=>{let a=(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"updateProduct");function form(e){let t=e.product;return {defaults:{additional_links:t.additional_links,tags:[...t.tags]},title:"Details",submit:{...e,id:t.id,organization:r}}}}});`, actionIDs["metadataDetails"])
		case "/org/create/plugins":
			page(w, map[string]any{"extension": map[string]string{"id": "extension", "slug": "plugins"}, "tagsets": []any{map[string]any{"id": "languages", "name": "Languages", "slug": "language", "tags": []any{map[string]string{"id": "lang-en", "name": "English"}}}, map[string]any{"id": "categories", "name": "Categories", "slug": "type", "tags": []any{map[string]string{"id": "cat-gaming", "name": "Gaming"}}}}})
		case "/api/presigned-url":
			json.NewEncoder(w).Encode(map[string]string{"presignedUrl": server.URL + "/storage/object"})
		case "/api/upload-aux/manifest/folder":
			json.NewEncoder(w).Encode(map[string]any{"manifest": json.RawMessage(manifest)})
		case "/api/upload-aux/tags/folder":
			fmt.Fprint(w, `{"tags":{}}`)
		case "/org/products", "/org/products/product/versions", "/org/products/product":
			product := map[string]any{"id": "product", "name": "Demo", "description": listingTestDescription, "status": "DRAFT", "streamdeckid": nil, "tags": []any{}, "media": media, "variants": []any{map[string]any{"id": "variant", "streamdeckid": nil, "is_free": true, "prices": []any{}, "versions": versions, "previews": previews}}}
			if detailsUpdated {
				product["streamdeckid"] = "com.example.demo"
				product["tags"] = []any{"lang-en", "cat-gaming"}
				product["additional_links"] = []any{}
				variant := product["variants"].([]any)[0].(map[string]any)
				variant["streamdeckid"] = "com.example.demo"
				variant["is_free"] = false
				variant["prices"] = []any{map[string]any{"plan": "once", "unit_amount": 699, "currency": "USD"}}
			}
			if r.URL.Path == "/org/products" {
				products := []any{}
				if created {
					products = append(products, product)
				}
				page(w, map[string]any{"products": products})
			} else {
				page(w, map[string]any{"product": product})
			}
		default:
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := connect.NewWorkflowClient(connect.Credentials{}, connect.Config{BaseURL: server.URL, AllowLocalhost: true, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(filepath.Join(dir, "state"), "org", client)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := service.PrepareListing(config)
	if err != nil {
		t.Fatal(err)
	}
	if created || submitted || len(writes) != 0 {
		t.Fatal("preparation wrote to provider")
	}
	ctx := context.Background()
	completed, err := service.Apply(ctx, Submit, prepared.Ref, false)
	if err != nil {
		t.Fatalf("direct submission through bridge: %v; writes=%v", err, writes)
	}
	var plan listing.Plan
	if err = json.Unmarshal(completed.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	if !created || !submitted || plan.State != listing.Submitted || len(plan.Media) != 5 || plan.Prepared.Package.Manifest.Version != "1.0.0.5" {
		t.Fatalf("incomplete bridge submission: %s", completed.Plan)
	}
	for i, m := range plan.Media {
		if m.State != listing.Done {
			t.Fatalf("asset %d not acknowledged: %s", i, m.State)
		}
	}
	want := []string{"create", "details", "put", "uploadAux", "details", "put", "app-icon:1", "put", "thumbnail:1", "put", "preview:1", "put", "preview:2", "put", "preview:3", "version-check", "submit"}
	if strings.Join(writes, ",") != strings.Join(want, ",") {
		t.Fatalf("wrong sequential write order: %v", writes)
	}
	const requestBudget = 71 // Includes a fresh version check before the first details write.
	if scriptRequests.Load() != 1 || requestCount.Load() > requestBudget {
		t.Fatalf("unexpected request budget: total=%d scripts=%d", requestCount.Load(), scriptRequests.Load())
	}
	before := len(writes)
	status, err := service.Status(ctx, Listing, prepared.Ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(status.Plan, &plan); err != nil || plan.State != listing.Submitted {
		t.Fatalf("status missing submission: %v %s", err, status.Plan)
	}
	// Read the child release through a host-owned path solely to verify its exact
	// durable receipt; the public listing plan already carries its child plan ID.
	child, err := os.ReadFile(filepath.Join(dir, "state", "listings", "releases", plan.ReleasePlanID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		SubmissionID        string `json:"submission_id"`
		SubmissionReceiptID string `json:"submission_receipt_id"`
		AutoPublish         bool   `json:"auto_publish"`
	}
	if err = json.Unmarshal(child, &receipt); err != nil || receipt.SubmissionID != "version" || receipt.AutoPublish {
		t.Fatalf("release receipt not preserved: %v %s", err, child)
	}
	// Both stale and current reviewed refs must never dispatch a second submit.
	_, _ = service.Apply(ctx, Submit, prepared.Ref, false)
	_, _ = service.Apply(ctx, Submit, status.Ref, false)
	if len(writes) != before {
		t.Fatalf("status/reapply repeated writes: %v", writes[before:])
	}
}
