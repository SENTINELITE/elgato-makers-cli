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
)

func syntheticDraft() DraftInput {
	return DraftInput{OrgID: "org", Name: "Synthetic", Description: "Synthetic plugin", DescriptionPlainTextLength: 16, UUID: "com.example.synthetic"}
}
func draftPage(extension string) string {
	payload := `a:{"extension":"$b"}` + "\n" + `b:{"id":"` + extension + `","slug":"plugins"}` + "\n"
	frame, _ := json.Marshal([]any{1, payload})
	return `<html><script src="/_next/static/draft.js"></script><script>self.__next_f.push(` + string(frame) + `)</script></html>`
}
func draftAction(rotation string) string {
	return `(0,c.createServerReference)("` + strings.Repeat(rotation, 40) + `",c.callServer,void 0,c.findSourceMapURL,"createProduct");`
}

func TestDraftCreationRotatesDiscoveryAndSendsOnlyCreateFields(t *testing.T) {
	rotation, extension := "a", "extension-one"
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_next/static/draft.js" {
			fmt.Fprint(w, draftAction(rotation))
			return
		}
		if r.URL.Path != "/org/create/plugins" {
			t.Error("unexpected route", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method == "GET" {
			fmt.Fprint(w, draftPage(extension))
			return
		}
		posts++
		if r.Header.Get("Next-Action") != strings.Repeat(rotation, 40) {
			t.Error("stale action")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		want := map[string]string{"_1_name": "Synthetic", "_1_description": "Synthetic plugin", "_1_descriptionPlainTextLength": "16", "_1_organization_id": "org", "_1_extension": extension, "_1_extension_slug": "plugins", "_1_streamDeckId": "com.example.synthetic", "0": `[null,"$K1"]`}
		if len(r.MultipartForm.Value) != len(want) {
			t.Error("extra fields", r.MultipartForm.Value)
		}
		for key, value := range want {
			if r.FormValue(key) != value {
				t.Error("wrong form field", key)
			}
		}
		if len(r.MultipartForm.File) != 0 {
			t.Error("unexpected upload")
		}
		fmt.Fprint(w, "1:{\"success\":true,\"productId\":\"synthetic-product\",\"variantId\":\"synthetic-variant\"}\n")
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	for i := 0; i < 2; i++ {
		result, err := client.CreateDraft(context.Background(), syntheticDraft())
		if err != nil || result.ProductID != "synthetic-product" || result.VariantID != "synthetic-variant" {
			t.Fatal(result, err)
		}
		rotation, extension = "b", "extension-two"
	}
	if posts != 2 {
		t.Fatal("incorrect request count", posts)
	}
}

func TestDraftDiscoveryFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, page, action string }{
		{"missing extension", `{"extension":{"id":"unknown","slug":"profiles"}}`, draftAction("a")},
		{"malformed extension", `{"extension":{"id":42,"slug":"plugins"}}`, draftAction("a")},
		{"conflicting extensions", `{"extensions":[{"id":"one","slug":"plugins"},{"id":"two","slug":"plugins"}]}`, draftAction("a")},
		{"missing action", draftPage("extension"), `unrecognized`},
		{"conflicting action", draftPage("extension"), draftAction("a") + draftAction("b")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts++
					return
				}
				if r.URL.Path == "/_next/static/draft.js" {
					fmt.Fprint(w, tc.action)
					return
				}
				fmt.Fprint(w, tc.page)
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			_, err := client.CreateDraft(context.Background(), syntheticDraft())
			var notCreated *DraftNotCreatedError
			if !errors.As(err, &notCreated) || !notCreated.NotCreated() || posts != 0 {
				t.Fatal("preflight did not block", err, posts)
			}
		})
	}
}

func TestDraftReadOnlyAndInputValidation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true, ReadOnly: true})
	_, err := client.CreateDraft(context.Background(), syntheticDraft())
	var notCreated *DraftNotCreatedError
	if !errors.Is(err, ErrReadOnly) || !errors.As(err, &notCreated) || calls != 0 {
		t.Fatal("read-only failed", err, calls)
	}
	client.readOnly = false
	input := syntheticDraft()
	input.OrgID = "../secret"
	_, err = client.CreateDraft(context.Background(), input)
	if !errors.As(err, &notCreated) || calls != 0 {
		t.Fatal("invalid identity dispatched", err)
	}
}

func TestDraftActionFailureIsAmbiguousAndNeverRetried(t *testing.T) {
	for _, body := range []string{`1:E{"message":"synthetic-secret"}`, `1:{"success":false,"message":"synthetic-secret"}`, `1:{"success":true,"productId":"product"}`, `1:{"success":true,"productId":"../bad","variantId":"variant"}`, `0:{}`} {
		t.Run(body, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts++
					fmt.Fprint(w, body)
					return
				}
				if r.URL.Path == "/_next/static/draft.js" {
					fmt.Fprint(w, draftAction("a"))
					return
				}
				fmt.Fprint(w, draftPage("extension"))
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			result, err := client.CreateDraft(context.Background(), syntheticDraft())
			var ambiguous *AmbiguousDraftError
			var notCreated *DraftNotCreatedError
			if !errors.As(err, &ambiguous) || errors.As(err, &notCreated) || posts != 1 || result != (DraftResult{}) || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("ambiguity failed", result, err, posts)
			}
		})
	}
}

func TestDraftTransportFailureIsAmbiguous(t *testing.T) {
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		if r.URL.Path == "/_next/static/draft.js" {
			fmt.Fprint(w, draftAction("a"))
			return
		}
		fmt.Fprint(w, draftPage("extension"))
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	_, err := client.CreateDraft(context.Background(), syntheticDraft())
	var ambiguous *AmbiguousDraftError
	if !errors.As(err, &ambiguous) || posts.Load() != 1 {
		t.Fatal("transport ambiguity failed", err, posts.Load())
	}
}

func TestDraftExtensionSplitFlightAndReferences(t *testing.T) {
	payload := `a:{"extension":{"slug":"$b","id":"$c"}}` + "\n" + `b:"plugins"` + "\n" + `c:"rotated-extension"` + "\n"
	a, _ := json.Marshal([]any{1, payload[:15]})
	b, _ := json.Marshal([]any{1, payload[15:]})
	page := `<script>self.__next_f.push(` + string(a) + `)</script><script>self.__next_f.push(` + string(b) + `)</script>`
	extension, err := draftExtension([]byte(page))
	if err != nil || extension != "rotated-extension" {
		t.Fatal(extension, err)
	}
}

func TestDraftExplicitExtensionFallbackAndConflicts(t *testing.T) {
	for _, tc := range []struct {
		name, page, explicit string
		wantPost             bool
	}{
		{"missing with explicit", `{"page":"create"}`, "explicit-extension", true},
		{"matching explicit", `{"extension":{"id":"explicit-extension","slug":"plugins"}}`, "explicit-extension", true},
		{"conflicting explicit", `{"extension":{"id":"discovered-extension","slug":"plugins"}}`, "explicit-extension", false},
		{"malformed despite explicit", `{"extension":{"id":null,"slug":"plugins"}}`, "explicit-extension", false},
		{"conflicting discovery despite explicit", `{"extensions":[{"id":"one","slug":"plugins"},{"id":"two","slug":"plugins"}]}`, "explicit-extension", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts++
					r.ParseMultipartForm(1 << 20)
					if r.FormValue("_1_extension") != tc.explicit {
						t.Error("incorrect fallback")
					}
					fmt.Fprint(w, `1:{"success":true,"productId":"product","variantId":"variant"}`)
					return
				}
				fmt.Fprint(w, `<script src="/_next/static/draft.js"></script>`)
				if r.URL.Path == "/_next/static/draft.js" {
					fmt.Fprint(w, draftAction("a"))
					return
				}
				frame, _ := json.Marshal([]any{1, "a:" + tc.page + "\n"})
				fmt.Fprint(w, `<script>self.__next_f.push(`+string(frame)+`)</script>`)
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			input := syntheticDraft()
			input.ExtensionID = tc.explicit
			_, err := client.CreateDraft(context.Background(), input)
			if tc.wantPost {
				if err != nil || posts != 1 {
					t.Fatal("fallback failed", err, posts)
				}
			} else {
				var notCreated *DraftNotCreatedError
				if !errors.As(err, &notCreated) || posts != 0 {
					t.Fatal("conflict did not block", err, posts)
				}
			}
		})
	}
}

func TestDraftJavaScriptLengthLimitsBlockBeforeNetwork(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	for _, tc := range []struct {
		name  string
		input DraftInput
	}{
		{"emoji name exceeds 50 UTF-16 units", func() DraftInput { input := syntheticDraft(); input.Name = strings.Repeat("😀", 26); return input }()},
		{"description exceeds 4000 units", func() DraftInput {
			input := syntheticDraft()
			input.Description = strings.Repeat("😀", 2001)
			input.DescriptionPlainTextLength = 4002
			return input
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.CreateDraft(context.Background(), tc.input)
			var notCreated *DraftNotCreatedError
			if !errors.As(err, &notCreated) || !notCreated.NotCreated() || calls != 0 {
				t.Fatal("length limit dispatched network request", err, calls)
			}
		})
	}
}

func TestAmbiguousDraftFailureCategoriesAreAllowlisted(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{errors.New("Maker action rejected"), "action rejected"},
		{errors.New("unrecognized Maker action response"), "unrecognized action response"},
		{errors.New("missing Maker action result"), "action acknowledgement missing"},
		{errors.New("Maker draft result did not confirm complete identifiers"), "creation acknowledgement missing or incomplete"},
		{errors.New("Maker draft result reported unsuccessful creation"), "server reported unsuccessful creation"},
		{errors.New("Maker transport failed"), "transport failure"},
		{errors.New("Maker authentication or access rejected"), "authentication or access rejected"},
		{errors.New("Maker response read failed"), "response read failure"},
		{errors.New("Maker response exceeds limit"), "response exceeded size limit"},
		{errors.New("Maker HTTP status 500"), "HTTP status 500"},
		{errors.New("Maker HTTP status 422"), "HTTP status 422"},
		{context.DeadlineExceeded, "request deadline exceeded"},
		{context.Canceled, "request canceled"},
		{fmt.Errorf("synthetic-secret: %w", errors.New("Maker action rejected")), "action rejected"},
		{errors.New("Maker action rejected synthetic-secret"), "unknown failure"},
		{errors.New("Maker HTTP status 500 synthetic-secret"), "unknown failure"},
		{errors.New("Maker HTTP status 999"), "unknown failure"},
		{errors.New("https://example.test/?token=synthetic-secret"), "unknown failure"},
		{nil, "unknown failure"},
	} {
		err := &AmbiguousDraftError{Cause: tc.cause}
		if category := err.FailureCategory(); category != tc.want {
			t.Errorf("category=%q want %q", category, tc.want)
		}
		if !strings.Contains(err.Error(), "("+tc.want+")") || strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "example.test") {
			t.Errorf("unsafe or absent diagnostic: %q", err.Error())
		}
	}
}

func TestDraftHTTPAndSchemaDiagnosticsStayAmbiguous(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"HTTP rejected", 500, "synthetic-secret", "HTTP status 500"},
		{"access rejected", 403, "synthetic-secret", "authentication or access rejected"},
		{"action rejected", 200, `1:E{"message":"synthetic-secret"}`, "action rejected"},
		{"schema rejected", 200, `1:[]`, "unrecognized action response"},
		{"acknowledgement missing", 200, `0:{}`, "action acknowledgement missing"},
		{"identifiers incomplete", 200, `1:{"success":true,"productId":"synthetic-product","message":"synthetic-secret"}`, "creation acknowledgement missing or incomplete"},
		{"server unsuccessful", 200, `1:{"success":false,"message":"synthetic-secret"}`, "server reported unsuccessful creation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
					return
				}
				if r.URL.Path == "/_next/static/draft.js" {
					fmt.Fprint(w, draftAction("a"))
					return
				}
				fmt.Fprint(w, draftPage("extension"))
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			result, err := client.CreateDraft(context.Background(), syntheticDraft())
			var ambiguous *AmbiguousDraftError
			var notCreated *DraftNotCreatedError
			if !errors.As(err, &ambiguous) || errors.As(err, &notCreated) || posts != 1 || result != (DraftResult{}) {
				t.Fatalf("ambiguity changed: result=%+v error=%v posts=%d", result, err, posts)
			}
			if ambiguous.FailureCategory() != tc.want || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatalf("diagnostic=%v want %q", err, tc.want)
			}
		})
	}
}
