package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/auth"
	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/listing"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

const listingTestDescription = "Demo helps Stream Deck users trigger common actions from a clear set of buttons. Configure each action in the property inspector, choose a label, and organize related controls together. The plugin includes practical defaults and visible feedback so users can see which action is ready before pressing a key."

func listingFixture(t *testing.T) (dir, cfg, file string) {
	t.Helper()
	dir, cfg, pkg, notes := fixture(t)
	imageFile := func(name string, width, height int, seed uint8) string {
		t.Helper()
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		// Keep full dimensions and distinct content without expensive alpha
		// decoding in repeated workflow race checks. Media tests cover RGBA.
		img := image.NewGray(image.Rect(0, 0, width, height))
		img.SetGray(0, 0, color.Gray{Y: seed})
		if err = png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	description := filepath.Join(dir, "description.md")
	if err := os.WriteFile(description, []byte(listingTestDescription), 0600); err != nil {
		t.Fatal(err)
	}
	no := false
	c := listing.Config{SchemaVersion: 1, OrganizationID: "org", Name: "Demo", PackagePath: pkg, DescriptionFile: description, ReleaseNotesFile: notes, Pricing: listing.Pricing{Type: "free", Amount: "0", Currency: "USD"}, Languages: []string{"English"}, Categories: []string{"Gaming"}, AutoPublish: &no, Assets: listing.Assets{Icon: imageFile("icon.png", 288, 288, 1), Thumbnail: imageFile("thumbnail.png", 1920, 960, 2), Gallery: []string{imageFile("one.png", 1920, 960, 3), imageFile("two.png", 1920, 960, 4), imageFile("three.png", 1920, 960, 5)}}}
	file = filepath.Join(dir, "listing.json")
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, cfg, file
}

func TestListingOfflineValidatePrepareShowAndStatus(t *testing.T) {
	dir, cfg, file := listingFixture(t)
	state := filepath.Join(dir, "state")
	options := Options{ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Fatal("offline command created a client")
		return nil, nil
	}}
	common := []string{"--config", cfg, "--state-dir", state, "--minify"}
	run := func(args ...string) string {
		t.Helper()
		out, stderr, err := command(t, options, append(append([]string{}, common...), args...)...)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid([]byte(out)) || strings.Count(out, "\n") != 1 {
			t.Fatalf("stdout is not one compact JSON result: %s", out)
		}
		if strings.Contains(out, "Success:") {
			t.Fatal("progress poisoned JSON")
		}
		if args[1] == "validate" && !strings.Contains(stderr, "validated locally") {
			t.Fatal("missing validation completion")
		}
		return out
	}
	var prepared listing.Prepared
	if err := json.Unmarshal([]byte(run("listing", "validate", "--file", file)), &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Config.GalleryUploadMode != "sequential" {
		t.Fatal("default mode missing")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("validate wrote state: %v", err)
	}
	var plan listing.Plan
	if err := json.Unmarshal([]byte(run("listing", "prepare", "--file", file)), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.ID == "" || plan.Target.OrgID != "org" || plan.State != listing.Ready {
		t.Fatalf("bad plan: %+v", plan)
	}
	if _, err := os.Stat(filepath.Join(state, "listings", plan.ID+".json")); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"show", "status"} {
		var got listing.Plan
		if err := json.Unmarshal([]byte(run("listing", sub, plan.ID)), &got); err != nil {
			t.Fatal(err)
		}
		if got.ID != plan.ID {
			t.Fatal("wrong plan")
		}
	}
}

func TestListingSubmitRejectsUnsafeArgumentsBeforeAuthOrState(t *testing.T) {
	dir, cfg, file := listingFixture(t)
	state := filepath.Join(dir, "state")
	options := Options{ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Fatal("invalid command created client")
		return nil, nil
	}}
	for _, args := range [][]string{
		{"listing", "submit", "--file", file},
		{"listing", "submit", "--yes"},
		{"listing", "submit", "id", "--file", file, "-y"},
		{"listing", "submit", "id", "--gallery-upload-mode", "sequential", "-y"},
		{"listing", "prepare", "--file", file, "--yes"},
		{"listing", "upload", "id", "--yes"},
		{"--yes", "listing", "show", "id"},
	} {
		out, stderr, err := command(t, options, append([]string{"--config", cfg, "--state-dir", state}, args...)...)
		if err == nil {
			t.Fatalf("accepted %v", args)
		}
		if out != "" || strings.Contains(stderr, "Success:") {
			t.Fatalf("invalid command reported success: %s %s", out, stderr)
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("rejected submission wrote state", err)
	}
}

func TestListingDirectSubmitValidatesFilesBeforeAuthentication(t *testing.T) {
	dir, cfg, file := listingFixture(t)
	var c listing.Config
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(c.Assets.Gallery[2]); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := command(t, Options{ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Fatal("invalid artifacts reached auth")
		return nil, nil
	}}, "--config", cfg, "--state-dir", filepath.Join(dir, "state"), "--credentials-file", filepath.Join(dir, "missing-credentials"), "listing", "submit", "--file", file, "-y")
	if err == nil || strings.Contains(err.Error(), "session") {
		t.Fatalf("expected artifact error before auth: %v", err)
	}
	if out != "" || strings.Contains(stderr, "Success:") {
		t.Fatal("invalid files reported success")
	}
}

func TestListingGalleryModeOverrideIsValidatedOffline(t *testing.T) {
	_, cfg, file := listingFixture(t)
	_, _, err := command(t, Options{}, "--config", cfg, "listing", "validate", "--file", file, "--gallery-upload-mode", "parallel")
	if err == nil || !strings.Contains(err.Error(), "sequential") {
		t.Fatalf("parallel mode accepted: %v", err)
	}
}

func listingCredentials(t *testing.T, dir string) string {
	t.Helper()
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, "cookies.json")
	creds, err := auth.Parse(strings.NewReader("__Host-kc_at=synthetic-session"))
	if err != nil {
		t.Fatal(err)
	}
	if err = auth.NewFileStore(path).Save(creds); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestListingDirectSubmitTwoDefaultNoPromptsBeforeAnyRequest(t *testing.T) {
	for _, input := range []string{"\n", "yes\n\n", "yes\n"} {
		t.Run(strings.ReplaceAll(input, "\n", "enter"), func(t *testing.T) {
			dir, cfg, file := listingFixture(t)
			credentials := listingCredentials(t, dir)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				t.Errorf("cancelled submission requested %s %s", r.Method, r.URL.Path)
				http.Error(w, "unexpected", 500)
			}))
			defer server.Close()
			options := Options{In: strings.NewReader(input), IsTerminal: func() bool { return true }, ClientFactory: func(cookies []*http.Cookie, readOnly bool) (*maker.Client, error) {
				if readOnly {
					t.Error("submission needs mutable client")
				}
				for _, cookie := range cookies {
					cookie.Secure = false
				}
				return maker.New(maker.Config{BaseURL: server.URL, HTTPClient: server.Client(), Cookies: cookies, AllowLocalhost: true, ReadOnly: readOnly})
			}}
			out, stderr, err := command(t, options, "--config", cfg, "--state-dir", filepath.Join(dir, "state"), "--credentials-file", credentials, "--json", "listing", "submit", "--file", file)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Cancelled bool         `json:"cancelled"`
				Submitted bool         `json:"submitted"`
				Plan      listing.Plan `json:"plan"`
			}
			if err = json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
			if !result.Cancelled || result.Submitted || result.Plan.ID == "" || requests != 0 {
				t.Fatalf("bad cancellation: %s", out)
			}
			if strings.Contains(stderr, "Success:") || !strings.Contains(stderr, "[y/N]") || !strings.Contains(stderr, "Listing plan:") {
				t.Fatalf("bad cancellation progress: %s", stderr)
			}
			if strings.HasPrefix(input, "yes\n") && strings.Count(stderr, "[y/N]") != 2 {
				t.Fatal("missing second default-No prompt")
			}
		})
	}
}

func TestListingDirectSubmitAuthFailureKeepsResumablePlanAndNoSuccess(t *testing.T) {
	for _, mode := range []string{"", "--json", "--minify"} {
		t.Run(mode, func(t *testing.T) {
			dir, cfg, file := listingFixture(t)
			credentials := listingCredentials(t, dir)
			args := []string{"--config", cfg, "--state-dir", filepath.Join(dir, "state"), "--credentials-file", credentials}
			if mode != "" {
				args = append(args, mode)
			}
			args = append(args, "listing", "submit", "--file", file, "--yes")
			out, stderr, err := command(t, Options{ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
				return nil, errors.New("synthetic authentication failure")
			}}, args...)
			if err == nil {
				t.Fatal("expected auth error")
			}
			files, err := filepath.Glob(filepath.Join(dir, "state", "listings", "*.json"))
			if err != nil || len(files) != 1 {
				t.Fatalf("missing saved plan: %v %v", files, err)
			}
			raw, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var plan listing.Plan
			if err = json.Unmarshal(raw, &plan); err != nil {
				t.Fatal(err)
			}
			if plan.ID == "" || plan.State != listing.Ready {
				t.Fatal("partial plan missing")
			}
			if mode == "" {
				if out != "" {
					t.Fatalf("default failure dumped JSON: %s", out)
				}
			} else {
				var emitted listing.Plan
				if err = json.Unmarshal([]byte(out), &emitted); err != nil || emitted.ID != plan.ID {
					t.Fatalf("missing JSON failure plan: %v %s", err, out)
				}
			}
			if strings.Contains(stderr, "Success:") || !strings.Contains(stderr, "maker listing status "+plan.ID) || !strings.Contains(stderr, "Remaining checkpoints:") {
				t.Fatalf("missing recovery guidance: %s", stderr)
			}
		})
	}
}

func TestListingDirectSubmitFullSyntheticHTTPFlow(t *testing.T) {
	const scriptPath = "/_next/static/listing-0123456789abcdef.js"
	var requestCount, scriptRequests atomic.Int64
	dir, cfg, file := listingFixture(t)
	credentials := listingCredentials(t, dir)
	var config listing.Config
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	config.Pricing = listing.Pricing{Type: "paid", Amount: "6.99", Currency: "USD"}
	raw, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
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
				if r.FormValue("_1_price[currency]") != "USD" || r.FormValue("_1_tags[0]") != "lang-en" || r.FormValue("_1_tags[1]") != "cat-gaming" {
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
				if r.FormValue("_1_is_ready_for_publish") != "false" {
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
	options := Options{ClientFactory: func(cookies []*http.Cookie, readOnly bool) (*maker.Client, error) {
		for _, c := range cookies {
			c.Secure = false
		}
		return maker.New(maker.Config{BaseURL: server.URL, AllowLocalhost: true, HTTPClient: server.Client(), Cookies: cookies, ReadOnly: readOnly})
	}}
	common := []string{"--config", cfg, "--state-dir", filepath.Join(dir, "state"), "--credentials-file", credentials, "--minify", "--verbose"}
	out, stderr, err := command(t, options, append(append([]string{}, common...), "listing", "submit", "--file", file, "--yes")...)
	if err != nil {
		t.Fatalf("direct submission failed: %v\n%s\n%s", err, stderr, out)
	}
	var plan listing.Plan
	if err = json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatal(err)
	}
	if !created || !submitted || plan.State != listing.Submitted || len(plan.Media) != 5 {
		t.Fatalf("incomplete workflow: %s", out)
	}
	if !strings.Contains(stderr, "Success:") || strings.Contains(out, "Success:") || strings.Count(out, "\n") != 1 {
		t.Fatal("invalid progress or JSON output")
	}
	want := []string{"create", "details", "put", "uploadAux", "details", "put", "app-icon:1", "put", "thumbnail:1", "put", "preview:1", "put", "preview:2", "put", "preview:3", "version-check", "submit"}
	if strings.Join(writes, ",") != strings.Join(want, ",") {
		t.Fatalf("wrong sequential mutation order: %v", writes)
	}
	if scriptRequests.Load() != 1 || !strings.Contains(stderr, "HTTP:") || strings.Contains(out, "HTTP:") {
		t.Fatalf("hashed scripts were fetched repeatedly or statistics contaminated JSON: script requests=%d stderr=%s", scriptRequests.Load(), stderr)
	}
	const requestBudget = 71 // Includes a fresh version check before the first details write.
	if requestCount.Load() > requestBudget {
		t.Fatalf("complete synthetic submission exceeded its %d-request budget: %d", requestBudget, requestCount.Load())
	}
	t.Logf("complete synthetic submission: %d HTTP requests, %d script download", requestCount.Load(), scriptRequests.Load())
	before := len(writes)
	out, _, err = command(t, options, append(append([]string{}, common...), "listing", "status", plan.ID)...)
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if len(writes) != before {
		t.Fatal("status repeated a write")
	}
	if !json.Valid([]byte(out)) {
		t.Fatal("status output is not JSON")
	}
}

func TestListingDiscardIsOfflineAndAllowsNewPreparation(t *testing.T) {
	dir, cfg, file := listingFixture(t)
	options := Options{ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Fatal("local discard reached authentication")
		return nil, nil
	}}
	common := []string{"--config", cfg, "--state-dir", filepath.Join(dir, "state"), "--minify"}
	run := func(args ...string) (string, string, error) {
		t.Helper()
		return command(t, options, append(append([]string{}, common...), args...)...)
	}
	out, _, err := run("listing", "prepare", "--file", file)
	if err != nil {
		t.Fatal(err)
	}
	var initial listing.Plan
	if err = json.Unmarshal([]byte(out), &initial); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := run("listing", "discard", initial.ID)
	if err != nil {
		t.Fatal(err)
	}
	var discarded listing.Plan
	if err = json.Unmarshal([]byte(out), &discarded); err != nil {
		t.Fatal(err)
	}
	if discarded.ID != initial.ID || discarded.State != listing.Discarded {
		t.Fatalf("wrong discard result: %s", out)
	}
	if !strings.Contains(stderr, "discarded locally; no remote deletion or submission") {
		t.Fatalf("missing discard completion: %s", stderr)
	}
	out, _, err = run("listing", "prepare", "--file", file)
	if err != nil {
		t.Fatal(err)
	}
	var fresh listing.Plan
	if err = json.Unmarshal([]byte(out), &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.ID == initial.ID || fresh.State != listing.Ready {
		t.Fatal("new preparation did not create a fresh plan")
	}
	out, stderr, err = run("listing", "discard", "invalid-id")
	if err == nil || out != "" || strings.Contains(stderr, "Success:") {
		t.Fatalf("invalid discard reported success: %v %s %s", err, out, stderr)
	}
}

func TestListingRequiredArtworkFailsBeforeAuthentication(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*listing.Config)
	}{
		{"missing icon", func(c *listing.Config) { c.Assets.Icon = "" }},
		{"missing thumbnail", func(c *listing.Config) { c.Assets.Thumbnail = "" }},
		{"no gallery", func(c *listing.Config) { c.Assets.Gallery = nil }},
		{"two gallery images", func(c *listing.Config) { c.Assets.Gallery = c.Assets.Gallery[:2] }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, cfg, file := listingFixture(t)
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var c listing.Config
			if err = json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			tt.edit(&c)
			raw, err = json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(dir, "state")
			options := Options{ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
				t.Fatal("missing required artwork reached authentication")
				return nil, nil
			}}
			out, stderr, err := command(t, options, "--config", cfg, "--state-dir", state, "--credentials-file", filepath.Join(dir, "missing-credentials"), "listing", "submit", "--file", file, "--yes")
			if err == nil || strings.Contains(err.Error(), "session") {
				t.Fatalf("expected artwork validation before auth: %v", err)
			}
			if out != "" || strings.Contains(stderr, "Success:") || strings.Contains(stderr, "Connecting") {
				t.Fatalf("invalid artwork reached submission: %s %s", out, stderr)
			}
			if _, err = os.Stat(state); !os.IsNotExist(err) {
				t.Fatalf("invalid artwork created persistent state: %v", err)
			}
		})
	}
}

func TestListingReorderIsOfflinePreservesFieldsAndResolvesPaths(t *testing.T) {
	dir, cfg, file := listingFixture(t)
	original, err := listing.LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	// Reusing the thumbnail in the gallery is allowed. The JSON uses relative
	// paths so moving the emitted configuration must keep the same artifacts.
	original.Assets.Gallery[0] = original.Assets.Thumbnail
	relativize := func(path string) string {
		t.Helper()
		p, e := filepath.Rel(dir, path)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	original.PackagePath = relativize(original.PackagePath)
	original.DescriptionFile = relativize(original.DescriptionFile)
	original.ReleaseNotesFile = relativize(original.ReleaseNotesFile)
	original.Assets.Icon = relativize(original.Assets.Icon)
	original.Assets.Thumbnail = relativize(original.Assets.Thumbnail)
	for i := range original.Assets.Gallery {
		original.Assets.Gallery[i] = relativize(original.Assets.Gallery[i])
	}
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := listing.LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := listing.Reorder(resolved, []int{3, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	options := Options{ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Fatal("local reorder reached authentication")
		return nil, nil
	}}
	state := filepath.Join(dir, "state")
	common := []string{"--config", cfg, "--state-dir", state, "--credentials-file", filepath.Join(dir, "missing-credentials"), "--minify", "listing", "reorder", "--file", file, "--order", "3,1,2"}
	for _, output := range []string{"", filepath.Join(t.TempDir(), "reordered.json")} {
		args := append([]string{}, common...)
		if output != "" {
			args = append(args, "--output", output)
		}
		out, stderr, err := command(t, options, args...)
		if err != nil {
			t.Fatal(err)
		}
		var got listing.Config
		if err = json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("reordered config lost fields: %+v", got)
		}
		if strings.Count(out, "\n") != 1 || strings.Contains(out, "Success:") || !strings.Contains(stderr, "local listing configuration only; no remote changes") {
			t.Fatalf("bad reorder output: %s %s", out, stderr)
		}
		prepared, err := listing.PrepareConfig(got)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Gallery[1].Path != prepared.Thumbnail.Path {
			t.Fatal("thumbnail gallery reuse lost")
		}
		if output != "" {
			saved, err := listing.LoadConfig(output)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(saved, expected) {
				t.Fatal("output moved artifact paths")
			}
			if !filesec.IsPrivateFile(output) {
				t.Fatal("output is not private")
			}
		}
	}
	if _, err = os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("local reorder wrote state", err)
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(after, b) {
		t.Fatal("reorder changed source config", err)
	}
}

func TestListingReorderRejectsInvalidPermutationWithoutWrites(t *testing.T) {
	dir, cfg, file := listingFixture(t)
	for _, order := range []string{"", "1,2", "1,2,2", "0,1,2", "1,2,4", "-1,2,3", "three,1,2", "1,,2", "1,2,3,4"} {
		t.Run(order, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "reordered.json")
			out, stderr, err := command(t, Options{ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
				t.Fatal("invalid reorder reached authentication")
				return nil, nil
			}}, "--config", cfg, "--state-dir", filepath.Join(dir, "state"), "listing", "reorder", "--file", file, "--order", order, "--output", output)
			if err == nil || out != "" || strings.Contains(stderr, "Success:") {
				t.Fatalf("accepted invalid order: %v %s %s", err, out, stderr)
			}
			if _, err = os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("invalid reorder created output", err)
			}
		})
	}
}

func TestListingReorderNeverOverwritesOutput(t *testing.T) {
	_, cfg, file := listingFixture(t)
	existing := filepath.Join(t.TempDir(), "existing.json")
	sentinel := []byte("preserve existing bytes")
	if err := os.WriteFile(existing, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(existing, link); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{existing, link, file} {
		out, stderr, err := command(t, Options{}, "--config", cfg, "listing", "reorder", "--file", file, "--order", "3,1,2", "--output", output)
		if err == nil || out != "" || strings.Contains(stderr, "Success:") {
			t.Fatalf("overwrote output %s: %v", output, err)
		}
	}
	got, err := os.ReadFile(existing)
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatal("existing output changed", err)
	}
	got, err = os.ReadFile(file)
	if err != nil || !bytes.Equal(got, source) {
		t.Fatal("source config changed", err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("output symlink changed", err)
	}
}

func TestListingFailureSummaryKeepsRemoteAndChildIDs(t *testing.T) {
	var out, stderr bytes.Buffer
	a := &app{o: Options{Out: &out, Err: &stderr}}
	plan := listing.Plan{ID: "listing-local", State: listing.Ready, Target: model.Target{ProductID: "created-product", VariantID: "created-variant"}, DraftPlanID: "draft-child", ReleasePlanID: "release-child", DetailsState: listing.Ready, PackageState: listing.Done, Media: []listing.MediaCheckpoint{{State: listing.Done}, {State: listing.Unknown}}}
	cause := errors.New("UUID reconciliation failed")
	if err := a.listingResult(plan, cause, ""); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("failure polluted stdout")
	}
	for _, want := range []string{"created-product", "created-variant", "draft-child", "release-child", "package upload", "media 1/2", "listing details [prepared]", "maker listing status listing-local"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("missing %q in %s", want, stderr.String())
		}
	}
}

func TestListingProgressStopsWhenOperationReturnsOrContextCancels(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		var out, stderr bytes.Buffer
		a := &app{o: Options{Out: &out, Err: &stderr}}
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		_, err := a.runListing(ctx, nil, "Reading product", func(_ *listing.Manager) (listing.Plan, error) {
			return listing.Plan{}, errors.New("synthetic failure")
		})
		cancel()
		if err == nil || out.Len() != 0 || !strings.Contains(stderr.String(), "Stopped during: Reading product [elapsed") {
			t.Fatalf("bad progress: %v %s", err, stderr.String())
		}
	}
}

func TestListingRetireUsesReadOnlyClientAndKeepsOfflineStatus(t *testing.T) {
	dir, cfg, file := listingFixture(t)
	credentials := listingCredentials(t, dir)
	config, err := listing.LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	config.ProductID, config.VariantID = "deleted-product", "deleted-variant"
	prepared, err := listing.PrepareConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	plan, err := listing.NewManager(filepath.Join(state, "listings"), nil).Prepare(prepared, "org")
	if err != nil {
		t.Fatal(err)
	}
	reads, clients := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if r.Method != http.MethodGet || r.URL.Path != "/org/products" {
			t.Errorf("retire attempted %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"products":[]}`)
	}))
	defer server.Close()
	options := Options{ClientFactory: func(cookies []*http.Cookie, readOnly bool) (*maker.Client, error) {
		clients++
		if !readOnly {
			t.Error("retirement requires read-only client")
		}
		for _, c := range cookies {
			c.Secure = false
		}
		return maker.New(maker.Config{BaseURL: server.URL, AllowLocalhost: true, HTTPClient: server.Client(), Cookies: cookies, ReadOnly: readOnly})
	}}
	common := []string{"--config", cfg, "--state-dir", state, "--credentials-file", credentials}
	out, stderr, err := command(t, options, append(append([]string{}, common...), "listing", "retire", plan.ID)...)
	if err != nil {
		t.Fatalf("retire failed: %v %s", err, stderr)
	}
	var result listing.Plan
	if err = json.Unmarshal([]byte(out), &result); err != nil || result.State != listing.Retired || reads != 1 || clients != 1 {
		t.Fatalf("bad retirement: %v %s reads=%d clients=%d", err, out, reads, clients)
	}
	if !strings.Contains(stderr, "retired locally") || !strings.Contains(stderr, "no remote deletion") {
		t.Fatalf("unclear retirement result: %s", stderr)
	}
	for _, cmd := range []string{"status", "show", "retire"} {
		if _, _, err = command(t, options, append(append([]string{}, common...), "listing", cmd, plan.ID)...); err != nil {
			t.Fatal(err)
		}
	}
	if clients != 1 || reads != 1 {
		t.Fatal("retired terminal plan caused remote access")
	}
}

func TestListingFailureSummaryIncludesInitialPricingCheckpoint(t *testing.T) {
	for _, state := range []string{"", listing.Done, listing.Unknown, listing.Ready} {
		t.Run(state, func(t *testing.T) {
			var out, stderr bytes.Buffer
			a := &app{o: Options{Out: &out, Err: &stderr}}
			plan := listing.Plan{ID: "listing-local", State: listing.Unknown, EarlyDetailsState: state, DetailsState: listing.Ready, PackageState: listing.Unknown}
			if err := a.listingResult(plan, errors.New("package failure"), ""); err == nil {
				t.Fatal("missing failure")
			}
			message := stderr.String()
			if out.Len() != 0 {
				t.Fatal("failure polluted stdout")
			}
			switch state {
			case "":
				if strings.Contains(message, "initial pricing and details") {
					t.Fatal("legacy plan invented an initial pricing checkpoint")
				}
			case listing.Done:
				if !strings.Contains(message, "Completed checkpoints: initial pricing and details;") {
					t.Fatalf("completed pricing omitted: %s", message)
				}
			default:
				if !strings.Contains(message, "Remaining checkpoints: initial pricing and details ["+state+"]") {
					t.Fatalf("pending pricing omitted: %s", message)
				}
			}
		})
	}
}
