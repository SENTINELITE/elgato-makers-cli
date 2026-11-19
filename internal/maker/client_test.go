package maker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

func fixturePage() string {
	return `<html><script src="/_next/static/chunk.js"></script><script>self.__next_f.push([1,"a:{\"product\":\"$b\"}\nb:{\"id\":\"product\",\"name\":\"Synthetic\",\"variants\":[{\"id\":\"variant\",\"streamdeckid\":\"com.example.plugin\",\"versions\":[{\"id\":\"version-id\",\"version_number\":\"1.0.0\",\"status\":\"published\"}]}]}\n"])</script></html>`
}
func fixtureActions(rotation string) string {
	var b strings.Builder
	for i, n := range []string{"uploadAux", "getIsSdVersionValid", "createVersion"} {
		fmt.Fprintf(&b, `(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"%s");`, strings.Repeat(rotation, 39)+fmt.Sprint(i), n)
	}
	return b.String()
}
func TestRSCReferences(t *testing.T) {
	p, v, e := parsePage([]byte(fixturePage()))
	if e != nil || len(p) != 1 || p[0].UUID != "com.example.plugin" || len(v) != 1 || v[0].VariantID != "variant" {
		t.Fatalf("parse failed %v %v %v", p, v, e)
	}
	for _, b := range []string{`<html>Login</html>`, `{"secret":"synthetic"}`} {
		if _, _, e = parsePage([]byte(b)); e == nil {
			t.Fatal("unknown schema accepted")
		}
	}
}
func TestReadOnlyAndOrigins(t *testing.T) {
	c, _ := New(Config{ReadOnly: true})
	if _, e := c.Upload(context.Background(), model.Target{}, model.Package{}); !errors.Is(e, ErrReadOnly) {
		t.Fatal(e)
	}
	if _, e := c.Submit(context.Background(), model.Target{}, model.Submission{}); !errors.Is(e, ErrReadOnly) {
		t.Fatal(e)
	}
	for _, u := range []string{"http://maker.elgato.com", "https://evil.example", "https://maker.elgato.com@evil.example", "https://maker.elgato.com?token=secret"} {
		if _, e := New(Config{BaseURL: u}); e == nil {
			t.Fatal("accepted", u)
		}
	}
}
func TestActionRotation(t *testing.T) {
	rotation := "a"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_next/static/chunk.js" {
			fmt.Fprint(w, fixtureActions(rotation))
		} else {
			fmt.Fprint(w, fixturePage())
		}
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, AllowLocalhost: true})
	target := model.Target{OrgID: "org", ProductID: "product"}
	a, e := c.DiscoverActions(context.Background(), target)
	if e != nil {
		t.Fatal(e)
	}
	rotation = "b"
	b, e := c.DiscoverActions(context.Background(), target)
	if e != nil || a.CreateVersion == b.CreateVersion {
		t.Fatal("action rotation not observed", e)
	}
}
func TestSyntheticUploadSubmit(t *testing.T) {
	var server *httptest.Server
	stage := 0
	bytesReceived := []byte{}
	manifest := model.Manifest{Name: "Synthetic", UUID: "com.example.plugin", Version: "1.0.1", SDKVersion: 3}
	manifest.Software.MinimumVersion = "7.1"
	manifest.OS = []model.OS{{Platform: "mac", MinimumVersion: "12"}}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_next/static/chunk.js":
			fmt.Fprint(w, fixtureActions("a"))
		case r.URL.Path == "/api/presigned-url":
			if stage != 0 {
				t.Error("wrong stage")
			}
			stage = 1
			json.NewEncoder(w).Encode(map[string]string{"presignedUrl": server.URL + "/bucket?signature=synthetic-secret"})
		case r.URL.Path == "/bucket":
			if r.Method != "PUT" || r.Header.Get("Cookie") != "" {
				t.Error("signed upload method/cookie")
			}
			bytesReceived, _ = io.ReadAll(r.Body)
			stage = 2
		case strings.HasPrefix(r.URL.Path, "/api/upload-aux/manifest/"):
			json.NewEncoder(w).Encode(manifest)
		case strings.HasPrefix(r.URL.Path, "/api/upload-aux/tags/"):
			fmt.Fprint(w, `{"Dial support":"synthetic"}`)
		case r.Method == "POST":
			a := r.Header.Get("Next-Action")
			switch {
			case strings.HasSuffix(a, "0"):
				if stage != 2 {
					t.Error("upload before PUT")
				}
				r.ParseMultipartForm(1024 * 1024)
				if r.FormValue("0") != `[null,"$K1"]` || r.FormValue("_1_s3_link_id") == "" {
					t.Error("upload fields")
				}
				fmt.Fprint(w, "1:{\"success\":true,\"id\":\"folder\"}\n")
				stage = 3
			case strings.HasSuffix(a, "1"):
				b, _ := io.ReadAll(r.Body)
				if string(b) != `[{"sdVersion":"7.1"}]` {
					t.Error("minimum version args")
				}
				fmt.Fprint(w, "1:{\"is_valid\":true}\n")
			case strings.HasSuffix(a, "2"):
				r.ParseMultipartForm(1024 * 1024)
				for k, v := range map[string]string{"0": `[{},"$K1"]`, "_1_version": "1.0.1", "_1_variant_id": "variant", "_1_org_id": "org", "_1_streamdeck_id": "com.example.plugin", "_1_os_versions[0][name]": "mac", "_1_is_ready_for_publish": "false", "_1_release_notes": "Synthetic notes", "_1_release_notesPlainTextLength": "15"} {
					if r.FormValue(k) != v {
						t.Errorf("wrong %s", k)
					}
				}
				fmt.Fprint(w, "1:{\"success\":true,\"id\":\"submitted-version\"}\n")
				stage = 4
			default:
				t.Error("unknown action")
			}
		default:
			fmt.Fprint(w, fixturePage())
		}
	}))
	defer server.Close()
	file := t.TempDir() + "/test.streamDeckPlugin"
	os.WriteFile(file, []byte("exact synthetic binary"), 0600)
	p := model.Package{Path: file, Size: 22, Manifest: manifest}
	c, e := New(Config{BaseURL: server.URL, AllowLocalhost: true, Cookies: []*http.Cookie{{Name: "session", Value: "synthetic"}}})
	if e != nil {
		t.Fatal(e)
	}
	target := model.Target{OrgID: "org", ProductID: "product", VariantID: "variant", UUID: manifest.UUID}
	u, e := c.Upload(context.Background(), target, p)
	if e != nil {
		t.Fatal(e)
	}
	if string(bytesReceived) != "exact synthetic binary" || stage != 3 {
		t.Fatal("upload wrong")
	}
	r, e := c.Submit(context.Background(), target, model.Submission{Package: p, Upload: u, Notes: model.Notes{Markdown: "Synthetic notes", PlainTextLength: 15}})
	if e != nil || r.ID != "submitted-version" || stage != 4 {
		t.Fatal("submission", r, e)
	}
}
func TestHTTP200ActionErrorAndAmbiguity(t *testing.T) {
	for _, b := range []string{"1:{\"error\":\"synthetic-secret\"}", "1:E{\"message\":\"synthetic-secret\"}", "0:{}"} {
		_, e := actionResult([]byte(b))
		if e == nil || strings.Contains(e.Error(), "synthetic-secret") {
			t.Fatal("action error failed", e)
		}
	}
	e := &AmbiguousSubmissionError{errors.New("synthetic-secret")}
	if strings.Contains(e.Error(), "synthetic-secret") {
		t.Fatal("ambiguity leaked")
	}
}
func TestRedirectNoCookieLeak(t *testing.T) {
	hit := false
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer dest.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL+"?token=synthetic-secret", 302)
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, AllowLocalhost: true, Cookies: []*http.Cookie{{Name: "token", Value: "synthetic-secret"}}})
	_, e := c.Products(context.Background(), "org")
	if e == nil || hit || strings.Contains(e.Error(), "synthetic-secret") {
		t.Fatal("redirect or redaction failed")
	}
}

func TestSubmitAmbiguousDoesNotRetry(t *testing.T) {
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/upload-aux/manifest/folder" {
			_, submission := syntheticSubmission()
			json.NewEncoder(w).Encode(submission.Upload.Manifest)
			return
		}
		if r.URL.Path == "/_next/static/chunk.js" {
			fmt.Fprint(w, fixtureActions("a"))
			return
		}
		if r.Method == "GET" {
			fmt.Fprint(w, fixturePage())
			return
		}
		if strings.HasSuffix(r.Header.Get("Next-Action"), "1") {
			fmt.Fprint(w, "1:{\"is_valid\":true}\n")
			return
		}
		creates++
		fmt.Fprint(w, "1:E{\"message\":\"synthetic-private-token\"}\n")
	}))
	defer server.Close()
	c, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	m := model.Manifest{UUID: "com.example.plugin", Version: "1.0.1"}
	m.Software.MinimumVersion = "7.1"
	_, err := c.Submit(context.Background(), model.Target{OrgID: "org", ProductID: "product", VariantID: "variant", UUID: m.UUID}, model.Submission{Package: model.Package{Manifest: m}, Upload: model.UploadResult{FileID: "file", FolderID: "folder", Manifest: m}})
	var ambiguous *AmbiguousSubmissionError
	if !errors.As(err, &ambiguous) || creates != 1 || strings.Contains(err.Error(), "synthetic-private-token") {
		t.Fatal("submit ambiguity/retry policy failed")
	}
}

func TestVersionSchemaFailsClosed(t *testing.T) {
	for _, input := range []string{`{"id":"product","name":"Synthetic","variants":[{"id":"variant"}]}`, `{"id":"product","name":"Synthetic","variants":[{"id":"variant","versions":[{"id":"version"}]}]}`, `{"id":"product","name":"Synthetic","variants":[{"id":"variant","versions":["unknown"]}]}`} {
		if _, _, err := parsePageMode([]byte(input), true); err == nil {
			t.Fatal("unknown version schema accepted")
		}
	}
	_, versions, err := parsePageMode([]byte(`{"id":"product","name":"Synthetic","variants":[{"id":"variant","versions":[]}]}`), true)
	if err != nil || len(versions) != 0 {
		t.Fatal("recognized empty versions rejected")
	}
}

func TestMakerCookieRotationAndSignedUploadIsolation(t *testing.T) {
	makerRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/signed" {
			if r.Header.Get("Cookie") != "" {
				t.Error("Maker cookies reached signed upload")
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "signed-cookie", Path: "/"})
			return
		}
		makerRequests++
		session, err := r.Cookie("session")
		if err != nil {
			t.Error("Maker session missing")
			return
		}
		if makerRequests == 1 {
			if session.Value != "original" {
				t.Error("seed cookie wrong")
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "rotated", Path: "/", MaxAge: 600})
		} else if session.Value != "rotated" {
			t.Error("rotated cookie not sent")
		}
		http.SetCookie(w, &http.Cookie{Name: "outside", Value: "synthetic", Domain: "evil.example", Path: "/"})
		fmt.Fprint(w, fixturePage())
	}))
	defer server.Close()
	c, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true, Cookies: []*http.Cookie{{Name: "session", Value: "original", Path: "/"}}})
	if _, err := c.Products(context.Background(), "org"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.request(context.Background(), "PUT", server.URL+"/signed", "", "", strings.NewReader("synthetic"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Products(context.Background(), "org"); err != nil {
		t.Fatal(err)
	}
	cookies := c.CookieSnapshot()
	if len(cookies) != 1 || cookies[0].Value != "rotated" || cookies[0].Expires.IsZero() {
		t.Fatal("rotation snapshot failed")
	}
	cookies[0].Value = "modified"
	if c.CookieSnapshot()[0].Value != "rotated" {
		t.Fatal("snapshot aliases secret state")
	}
}

func TestRSCTextRecordNotes(t *testing.T) {
	notes := "Synthetic notes\nwith another line"
	payload := fmt.Sprintf("a:{\"id\":\"product\",\"name\":\"Synthetic\",\"variants\":[{\"id\":\"variant\",\"versions\":[{\"id\":\"version\",\"version_number\":\"1.0.0\",\"status\":\"published\",\"release_notes\":\"$b\"}]}]}\nb:T%x,%s", len(notes), notes)
	_, versions, err := parsePageMode([]byte(payload), true)
	if err != nil || len(versions) != 1 || versions[0].ReleaseNotes != notes {
		t.Fatal("RSC text reference failed", err)
	}
}

func syntheticSubmission() (model.Target, model.Submission) {
	m := model.Manifest{UUID: "com.example.plugin", Version: "1.0.1"}
	m.Software.MinimumVersion = "7.1"
	return model.Target{OrgID: "org", ProductID: "product", VariantID: "variant", UUID: m.UUID}, model.Submission{Package: model.Package{Manifest: m}, Upload: model.UploadResult{FileID: "file", FolderID: "folder", Manifest: m}}
}
func TestAuthPreflightProvesNotSubmitted(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
		}
		http.Error(w, "synthetic-token", http.StatusUnauthorized)
	}))
	defer server.Close()
	c, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	target, submission := syntheticSubmission()
	_, err := c.Submit(context.Background(), target, submission)
	var marker interface{ NotSubmitted() bool }
	if !errors.As(err, &marker) || !marker.NotSubmitted() || posts != 0 || strings.Contains(err.Error(), "synthetic-token") {
		t.Fatal("auth preflight marker failed")
	}
	ro, _ := New(Config{ReadOnly: true})
	_, err = ro.Submit(context.Background(), target, submission)
	if !errors.Is(err, ErrReadOnly) || !errors.As(err, &marker) {
		t.Fatal("read-only wrapping failed")
	}
}
func TestCreateTransportFailureRemainsAmbiguous(t *testing.T) {
	var creates atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/upload-aux/manifest/folder" {
			_, submission := syntheticSubmission()
			json.NewEncoder(w).Encode(submission.Upload.Manifest)
			return
		}
		if r.URL.Path == "/_next/static/chunk.js" {
			fmt.Fprint(w, fixtureActions("a"))
			return
		}
		if r.Method == "GET" {
			fmt.Fprint(w, fixturePage())
			return
		}
		if strings.HasSuffix(r.Header.Get("Next-Action"), "1") {
			fmt.Fprint(w, "1:{\"is_valid\":true}\n")
			return
		}
		creates.Add(1)
		hijacker := w.(http.Hijacker)
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Error("synthetic hijack failed")
			return
		}
		connection.Close()
	}))
	defer server.Close()
	c, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
	target, submission := syntheticSubmission()
	_, err := c.Submit(context.Background(), target, submission)
	var ambiguous *AmbiguousSubmissionError
	var marker interface{ NotSubmitted() bool }
	if !errors.As(err, &ambiguous) || errors.As(err, &marker) || creates.Load() != 1 {
		t.Fatal("dispatch ambiguity boundary failed")
	}
}

func TestProductExtensionNameTypeFallback(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"id":"product","name":"Synthetic","extension_name":"Stream Deck Plugin","variants":[]}`, "Stream Deck Plugin"},
		{`{"id":"product","name":"Synthetic","type":"explicit","extension_name":"Stream Deck Plugin","variants":[]}`, "explicit"},
		{`{"id":"product","name":"Synthetic","extension_name":{"name":"unsupported"},"variants":[]}`, ""},
	} {
		products, _, err := parsePage([]byte(tc.body))
		if err != nil || len(products) != 1 || products[0].Type != tc.want {
			t.Fatal("product type fallback failed", err)
		}
	}
}

func TestAuthCookieRotationReplacesImportedRootPath(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/org/products":
			cookies := r.Cookies()
			if len(cookies) != 1 || cookies[0].Name != "__Secure-kc_rt" || cookies[0].Value != "imported-root" {
				t.Error("imported root cookie missing")
			}
			http.SetCookie(w, &http.Cookie{Name: "__Secure-kc_rt", Value: "rotated-api", Path: "/api", Secure: true, HttpOnly: true, MaxAge: 600})
			fmt.Fprint(w, fixturePage())
		case "/api/probe":
			cookies := r.Cookies()
			if len(cookies) != 1 || cookies[0].Name != "__Secure-kc_rt" || cookies[0].Value != "rotated-api" {
				t.Error("API scope received duplicate or stale auth cookie")
			}
		case "/outside":
			if r.Header.Get("Cookie") != "" {
				t.Error("scoped auth cookie reached outside path")
			}
		case "/api/signed":
			if r.Header.Get("Cookie") != "" {
				t.Error("Maker auth cookie reached signed upload")
			}
		}
	}))
	defer server.Close()
	c, _ := New(Config{BaseURL: server.URL, HTTPClient: server.Client(), AllowLocalhost: true, Cookies: []*http.Cookie{{Name: "__Secure-kc_rt", Value: "imported-root", Path: "/", Secure: true, HttpOnly: true}}})
	if _, err := c.Products(context.Background(), "org"); err != nil {
		t.Fatal(err)
	}
	snapshot := c.CookieSnapshot()
	if len(snapshot) != 1 || snapshot[0].Value != "rotated-api" || snapshot[0].Path != "/api" || snapshot[0].Expires.IsZero() {
		t.Fatal("authoritative scoped cookie snapshot failed")
	}
	if _, err := c.get(context.Background(), "/api/probe"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.get(context.Background(), "/outside"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.request(context.Background(), "PUT", server.URL+"/api/signed", "", "", strings.NewReader("synthetic"), false); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitFreshManifestGateBeforeAnyAction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*model.Manifest)
		want   bool
	}{
		{"matching", func(m *model.Manifest) {}, true},
		{"UUID drift", func(m *model.Manifest) { m.UUID = "com.example.other" }, false},
		{"version drift", func(m *model.Manifest) { m.Version = "2.0.0" }, false},
		{"minimum Stream Deck drift", func(m *model.Manifest) { m.Software.MinimumVersion = "8.0" }, false},
		{"OS version drift", func(m *model.Manifest) { m.OS[0].MinimumVersion = "15" }, false},
		{"OS platform drift", func(m *model.Manifest) { m.OS[0].Platform = "windows" }, false},
		{"OS omitted", func(m *model.Manifest) { m.OS = nil }, false},
		{"name drift", func(m *model.Manifest) { m.Name = "Changed" }, false},
		{"SDK drift", func(m *model.Manifest) { m.SDKVersion = 4 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, submission := syntheticSubmission()
			submission.Package.Manifest.Name = "Synthetic"
			submission.Package.Manifest.SDKVersion = 3
			submission.Package.Manifest.OS = []model.OS{{Platform: "mac", MinimumVersion: "12"}}
			submission.Upload.Manifest = submission.Package.Manifest
			fresh := submission.Upload.Manifest
			fresh.OS = append([]model.OS(nil), fresh.OS...)
			tc.mutate(&fresh)
			reads, posts, creates := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts++
					if reads != 1 {
						t.Error("server action before fresh manifest read")
					}
				}
				if r.URL.Path == "/api/upload-aux/manifest/folder" {
					reads++
					json.NewEncoder(w).Encode(fresh)
					return
				}
				if r.URL.Path == "/_next/static/chunk.js" {
					fmt.Fprint(w, fixtureActions("a"))
					return
				}
				if r.Method == "GET" {
					fmt.Fprint(w, fixturePage())
					return
				}
				if strings.HasSuffix(r.Header.Get("Next-Action"), "1") {
					fmt.Fprint(w, "1:{\"is_valid\":true}\n")
					return
				}
				creates++
				fmt.Fprint(w, "1:{\"success\":true,\"id\":\"synthetic-version\"}\n")
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Submit(context.Background(), target, submission)
			if tc.want {
				if err != nil || reads != 1 || posts != 2 || creates != 1 {
					t.Fatalf("matching: reads=%d posts=%d creates=%d error=%v", reads, posts, creates, err)
				}
			} else {
				var marker interface{ NotSubmitted() bool }
				if err == nil || !errors.As(err, &marker) || !marker.NotSubmitted() || reads != 1 || posts != 0 || creates != 0 {
					t.Fatalf("drift gate: reads=%d posts=%d creates=%d error=%v", reads, posts, creates, err)
				}
			}
		})
	}
}
