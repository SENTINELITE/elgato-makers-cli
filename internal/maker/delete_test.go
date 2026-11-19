package maker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

func deleteTarget() model.Target {
	return model.Target{OrgID: "org", ProductID: "draft-product", Name: "Synthetic", UUID: "com.example.synthetic"}
}
func deleteProductPage(name, status string) string {
	return fmt.Sprintf(`{"products":[{"id":"draft-product","name":%q,"status":%q,"variants":[{"id":"variant","streamdeckid":"com.example.synthetic","versions":[]}]}]}`, name, status)
}
func deleteAction(id string) string {
	return `(0,n.createServerReference)("` + strings.Repeat(id, 40) + `",n.callServer,void 0,n.findSourceMapURL,"deleteProductAction")`
}

type deleteFixture struct {
	status                                      int
	response, location                          string
	productStatus, productName, action, catalog string
	beforeCatalog                               string
	readOnly, lost                              bool
	changeDuringDiscovery                       string
}

func runDeleteFixture(t *testing.T, f deleteFixture) (DeleteResult, error, int, int) {
	t.Helper()
	var posts, redirects atomic.Int32
	var discovered atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			if r.URL.Path != "/org/products/draft-product" {
				t.Error("wrong deletion path", r.URL.Path)
			}
			if r.Header.Get("Next-Action") != strings.Repeat("a", 40) {
				t.Error("wrong current action")
			}
			if r.Header.Get("Accept") != "text/x-component" {
				t.Error("missing action accept header")
			}
			if e := r.ParseMultipartForm(1 << 20); e != nil {
				t.Error(e)
				return
			}
			want := map[string]string{"_1_organization_id": "org", "_1_product_id": "draft-product", "0": `[{"success":false},"$K1"]`}
			if len(r.MultipartForm.Value) != len(want) || len(r.MultipartForm.File) != 0 {
				t.Error("unexpected deletion form fields")
			}
			for k, v := range want {
				if r.FormValue(k) != v {
					t.Error("wrong deletion field", k)
				}
			}
			if f.lost {
				conn, _, e := w.(http.Hijacker).Hijack()
				if e != nil {
					t.Error(e)
				} else {
					conn.Close()
				}
				return
			}
			if f.location != "" {
				w.Header().Set("Location", f.location)
			}
			w.Header().Set("Content-Type", "text/x-component")
			status := f.status
			if status == 0 {
				status = 303
			}
			w.WriteHeader(status)
			fmt.Fprint(w, f.response)
			return
		}
		switch r.URL.Path {
		case "/redirect":
			redirects.Add(1)
			fmt.Fprint(w, `{"products":[]}`)
		case "/_next/static/chunks/3315-current.js":
			discovered.Store(true)
			action := f.action
			if action == "" {
				action = deleteAction("a")
			}
			fmt.Fprint(w, action)
		case "/org/products":
			if posts.Load() > 0 {
				catalog := f.catalog
				if catalog == "" {
					catalog = `{"products":[]}`
				}
				fmt.Fprint(w, catalog)
			} else {
				catalog := f.beforeCatalog
				if catalog == "" {
					catalog = deleteProductPage("Synthetic", "DRAFT")
				}
				frame, _ := json.Marshal([]any{1, "a:" + catalog + "\n"})
				fmt.Fprint(w, `<script src="/_next/static/chunks/3315-current.js"></script><script>self.__next_f.push(`+string(frame)+`)</script>`)
			}
		case "/org/products/draft-product":
			fmt.Fprint(w, `<html>General</html>`)
		case "/org/products/draft-product/versions":
			name, status := f.productName, f.productStatus
			if name == "" {
				name = "Synthetic"
			}
			if status == "" {
				status = "DRAFT"
			}
			if discovered.Load() {
				switch f.changeDuringDiscovery {
				case "rename":
					name = "Changed during discovery"
				case "publish":
					status = "PUBLISHED"
				}
			}
			fmt.Fprint(w, deleteProductPage(name, status))
		default:
			t.Error("unexpected deletion route", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, e := New(Config{BaseURL: server.URL, AllowLocalhost: true, ReadOnly: f.readOnly})
	if e != nil {
		t.Fatal(e)
	}
	result, err := client.DeleteProduct(context.Background(), deleteTarget())
	return result, err, int(posts.Load()), int(redirects.Load())
}
func TestDeleteProductAcknowledgementRequiresCatalogAbsence(t *testing.T) {
	for _, f := range []deleteFixture{{status: 303, location: "/redirect"}, {status: 200, response: "1:{\"success\":true}\n"}} {
		result, err, posts, redirects := runDeleteFixture(t, f)
		if err != nil || !result.Deleted || result.ProductID != "draft-product" || posts != 1 || redirects != 0 {
			t.Fatal(result, err, posts, redirects)
		}
	}
}
func TestDeleteProductPreflightNeverDispatches(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    deleteFixture
	}{
		{"read only", deleteFixture{readOnly: true}},
		{"wrong name", deleteFixture{productName: "Wrong"}},
		{"published", deleteFixture{productStatus: "PUBLISHED"}},
		{"renamed during discovery", deleteFixture{changeDuringDiscovery: "rename"}},
		{"published during discovery", deleteFixture{changeDuringDiscovery: "publish"}},
		{"target missing from catalog", deleteFixture{beforeCatalog: `{"products":[]}`}},
		{"target renamed in catalog", deleteFixture{beforeCatalog: deleteProductPage("Changed", "DRAFT")}},
		{"malformed preflight catalog", deleteFixture{beforeCatalog: `{"products":[{}]}`}},
		{"missing action", deleteFixture{action: "unknown"}},
		{"conflicting action", deleteFixture{action: deleteAction("a") + deleteAction("b")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err, posts, _ := runDeleteFixture(t, tc.f)
			var known *NotDeletedError
			if !errors.As(err, &known) || !known.NotDeleted() || result.Deleted || posts != 0 {
				t.Fatal(result, err, posts)
			}
			if tc.f.readOnly && !errors.Is(err, ErrReadOnly) {
				t.Fatal("lost read-only cause")
			}
		})
	}
}
func TestDeleteProductFailuresAreAmbiguousAndNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    deleteFixture
	}{
		{"lost response", deleteFixture{lost: true}},
		{"cross origin", deleteFixture{status: 303, location: "https://example.invalid/secret"}},
		{"other redirect", deleteFixture{status: 307, location: "/redirect"}},
		{"unauthorized", deleteFixture{status: 401}},
		{"forbidden", deleteFixture{status: 403}},
		{"server error", deleteFixture{status: 500}},
		{"application error", deleteFixture{status: 200, response: "1:{\"success\":false}\n"}},
		{"missing result", deleteFixture{status: 200, response: "0:{}\n"}},
		{"still present", deleteFixture{catalog: deleteProductPage("Synthetic", "DRAFT")}},
		{"missing catalog", deleteFixture{catalog: `{"unrelated":[]}`}},
		{"malformed catalog entry", deleteFixture{catalog: `{"products":[{"id":"draft-product","name":"Synthetic"}]}`}},
		{"wrong catalog type", deleteFixture{catalog: `{"products":{}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err, posts, redirects := runDeleteFixture(t, tc.f)
			var ambiguous *AmbiguousDeleteError
			var known *NotDeletedError
			if !errors.As(err, &ambiguous) || errors.As(err, &known) || result.Deleted || posts != 1 || redirects != 0 {
				t.Fatal(result, err, posts, redirects)
			}
		})
	}
}
func TestDeleteProductCanceledBeforeDispatch(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.DeleteProduct(ctx, deleteTarget())
	var known *NotDeletedError
	if !errors.As(err, &known) || calls != 0 {
		t.Fatal(err, calls)
	}
}
func TestDeleteCatalogRequiresExplicitValidCollection(t *testing.T) {
	for _, input := range []string{
		`{"products":[]}`,
		`a:{"products":"$b"}` + "\n" + `b:[]`,
	} {
		if _, e := parseDeleteCatalog([]byte(input)); e != nil {
			t.Fatal(e)
		}
	}
	for _, input := range []string{deleteProductPage("Synthetic", "DRAFT")[12:], `{"products":[null]}`, `{"products":[{"id":"x","name":"x","variants":[]}],"other":{"products":"$missing"}}`} {
		if _, e := parseDeleteCatalog([]byte(input)); e == nil {
			t.Fatal("accepted unrecognized catalog", input)
		}
	}
}

func syntheticRenderedCatalog(mutate func([]any, map[string]any)) []byte {
	rows := []any{}
	records := map[string]any{}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("synthetic-product-%d", i)
		item := map[string]any{"id": id, "name": fmt.Sprintf("Synthetic %d", i), "status": "draft", "extension_name": "Stream Deck Plugin", "default_variant_id": "synthetic-variant", "variants": []any{map[string]any{"id": "synthetic-variant", "streamdeckid": nil}}}
		card := []any{"$", "$Lcard", id, map[string]any{"item": item, "orgId": "org", "stripeStatus": nil}}
		link := []any{"$", "$Llink", id, map[string]any{"href": "/org/products/" + id, "prefetch": false, "children": card}}
		if i == 0 {
			rows = append(rows, link)
		} else {
			ref := fmt.Sprintf("row%d", i)
			records[ref] = link
			rows = append(rows, "$L"+ref)
		}
	}
	paging := map[string]any{"pageCount": 1, "selectedPage": 1}
	if mutate != nil {
		mutate(rows, paging)
	}
	grid := []any{"$", "div", nil, map[string]any{"className": deletionCatalogGridClass, "children": rows}}
	root := []any{grid, "$Lpagination"}
	records["pagination"] = []any{"$", "$Lpager", nil, paging}
	var stream strings.Builder
	rootJSON, _ := json.Marshal(root)
	fmt.Fprintf(&stream, "root:%s\n", rootJSON)
	for key, value := range records {
		body, _ := json.Marshal(value)
		fmt.Fprintf(&stream, "%s:%s\n", key, body)
	}
	// Flight fragments may split through records and values.
	body := stream.String()
	split := len(body) / 2
	var page strings.Builder
	for _, fragment := range []string{body[:split], body[split:]} {
		frame, _ := json.Marshal([]any{1, fragment})
		fmt.Fprintf(&page, "<script>self.__next_f.push(%s)</script>", frame)
	}
	return []byte(page.String())
}
func TestDeleteRenderedCatalogStrictCompleteGrid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]any, map[string]any)
		org    string
		want   bool
	}{
		{"complete single page", nil, "org", true},
		{"wrong organization", nil, "other-org", false},
		{"multiple pages", func(rows []any, p map[string]any) { p["pageCount"] = 2 }, "org", false},
		{"second page", func(rows []any, p map[string]any) { p["selectedPage"] = 2 }, "org", false},
		{"missing pagination count", func(rows []any, p map[string]any) { delete(p, "pageCount") }, "org", false},
		{"unknown row", func(rows []any, p map[string]any) { rows[1] = "$Lmissing" }, "org", false},
		{"duplicate row", func(rows []any, p map[string]any) { rows[1] = rows[0] }, "org", false},
		{"wrong href", func(rows []any, p map[string]any) {
			rows[0].([]any)[3].(map[string]any)["href"] = "/org/products/other"
		}, "org", false},
		{"malformed variants", func(rows []any, p map[string]any) {
			card := rows[0].([]any)[3].(map[string]any)["children"].([]any)
			card[3].(map[string]any)["item"].(map[string]any)["variants"] = nil
		}, "org", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			products, err := parseDeleteCatalogForOrg(syntheticRenderedCatalog(tc.mutate), tc.org)
			if (err == nil) != tc.want {
				t.Fatalf("error=%v want success %v", err, tc.want)
			}
			if tc.want && len(products) != 6 {
				t.Fatalf("products=%d", len(products))
			}
		})
	}
}
func TestDeleteRenderedCatalogEmptyAndMissingEnvelope(t *testing.T) {
	empty := []any{[]any{"$", "div", nil, map[string]any{"className": deletionCatalogGridClass, "children": []any{}}}, []any{"$", "$Lpager", nil, map[string]any{"pageCount": 1, "selectedPage": 1}}}
	body, _ := json.Marshal(empty)
	products, err := parseDeleteCatalogForOrg(body, "org")
	if err != nil || len(products) != 0 {
		t.Fatalf("empty complete grid: %v", err)
	}
	body, _ = json.Marshal(empty[0])
	if _, err := parseDeleteCatalogForOrg(body, "org"); err == nil {
		t.Fatal("accepted grid without adjacent pagination")
	}
	if _, err := parseDeleteCatalogForOrg([]byte(`{"id":"standalone","name":"Standalone","variants":[]}`), "org"); err == nil {
		t.Fatal("accepted loose detail as catalog")
	}
}
func TestDeleteProductLowercaseDraftEnum(t *testing.T) {
	result, err, posts, _ := runDeleteFixture(t, deleteFixture{productStatus: "draft"})
	if err != nil || !result.Deleted || posts != 1 {
		t.Fatalf("result=%+v error=%v posts=%d", result, err, posts)
	}
}

func TestDeleteExplicitCatalogPaginationMustProveCompleteness(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        bool
	}{
		{"legacy unpaged", `{"products":[]}`, true},
		{"known complete page", `{"products":[],"pageCount":1,"selectedPage":1}`, true},
		{"multiple pages", `{"products":[],"pageCount":2,"selectedPage":1}`, false},
		{"later page", `{"products":[],"pageCount":1,"selectedPage":2}`, false},
		{"count wrong type", `{"products":[],"pageCount":"1","selectedPage":1}`, false},
		{"selected wrong type", `{"products":[],"pageCount":1,"selectedPage":true}`, false},
		{"missing selected", `{"products":[],"pageCount":1}`, false},
		{"missing count", `{"products":[],"selectedPage":1}`, false},
		{"has more", `{"products":[],"hasMore":true}`, false},
		{"unknown has more false", `{"products":[],"hasMore":false}`, false},
		{"next cursor", `{"products":[],"nextCursor":"synthetic-cursor"}`, false},
		{"null cursor", `{"products":[],"next_cursor":null}`, false},
		{"total count", `{"products":[],"totalCount":10}`, false},
		{"pageInfo", `{"products":[],"pageInfo":{"hasNextPage":false}}`, false},
		{"unsupported pageInfo pair", `{"products":[],"pageInfo":{"pageCount":1,"selectedPage":1}}`, false},
		{"nested incomplete pagination", `{"products":[],"pagination":{"pageCount":2,"selectedPage":1}}`, false},
		{"nested complete pagination", `{"products":[],"pagination":{"pageCount":1,"selectedPage":1}}`, true},
		{"nested unknown pagination field", `{"products":[],"pagination":{"pageCount":1,"selectedPage":1,"unknown":false}}`, false},
		{"empty pagination object", `{"products":[],"pagination":{}}`, false},
		{"malformed pagination object", `{"products":[],"pagination":"unknown"}`, false},
		{"nested metadata pagination", `{"products":[],"meta":{"pagination":{"pageCount":2,"selectedPage":1}}}`, false},
		{"ancestor paging", `{"pageCount":2,"selectedPage":1,"data":{"products":[]}}`, false},
		{"ancestor complete paging", `{"pageCount":1,"selectedPage":1,"data":{"products":[]}}`, true},
		{"sibling metadata paging", `{"data":{"products":[]},"metadata":{"pageCount":2,"selectedPage":1}}`, false},
		{"sibling nested metadata cursor", `{"data":{"products":[]},"meta":{"pagination":{"nextCursor":null}}}`, false},
		{"unrelated UI paging", `{"data":{"products":[]},"sidebar":{"pageCount":2,"selectedPage":1}}`, true},
		{"nonpaging metadata", `{"products":[],"meta":{"requestId":"synthetic"}}`, true},
		{"null nonpaging metadata", `{"products":[],"meta":null}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseDeleteCatalog([]byte(tc.input))
			if (err == nil) != tc.want {
				t.Fatalf("error=%v want success=%v", err, tc.want)
			}
		})
	}
}
func TestDeleteExplicitCatalogFlightPagination(t *testing.T) {
	for _, tc := range []struct {
		name, stream string
		want         bool
	}{
		{"adjacent incomplete component", `a:[{"products":"$b"},"$Lc"]` + "\n" + `b:[]` + "\n" + `c:["$","$Lpager",null,{"pageCount":2,"selectedPage":1}]`, false},
		{"adjacent complete component", `a:[{"products":"$b"},"$Lc"]` + "\n" + `b:[]` + "\n" + `c:["$","$Lpager",null,{"pageCount":1,"selectedPage":1}]`, true},
		{"separate incomplete component", `a:{"products":"$b"}` + "\n" + `b:[]` + "\n" + `c:["$","$Lpager",null,{"pageCount":2,"selectedPage":1}]`, false},
		{"separate missing partner", `a:{"products":"$b"}` + "\n" + `b:[]` + "\n" + `c:["$","$Lpager",null,{"selectedPage":1}]`, false},
		{"sibling plain paging", `a:[{"products":[]},{"pageCount":2,"selectedPage":1}]`, false},
		{"separate nested component pagination", `a:{"products":[]}` + "\n" + `b:["$","$Lpager",null,{"pagination":{"pageCount":2,"selectedPage":1}}]`, false},
		{"separate cursor component pagination", `a:{"products":[]}` + "\n" + `b:["$","$Lpager",null,{"nextCursor":null}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, _ := json.Marshal([]any{1, tc.stream})
			page := []byte(`<script>self.__next_f.push(` + string(frame) + `)</script>`)
			_, err := parseDeleteCatalog(page)
			if (err == nil) != tc.want {
				t.Fatalf("error=%v want success=%v", err, tc.want)
			}
		})
	}
}
