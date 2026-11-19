package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/auth"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
)

func fixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	target := model.Target{Alias: "demo", OrgID: "org", ProductID: "product", VariantID: "variant", UUID: "com.example.demo", Name: "Demo"}
	if err := saveConfig(config, configuration{OrgID: "org", Products: map[string]model.Target{"demo": target}}); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(dir, "demo.streamDeckPlugin")
	f, err := os.Create(pkg)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	entry, _ := w.Create("com.example.demo.sdPlugin/manifest.json")
	io.WriteString(entry, `{"Name":"Demo","UUID":"com.example.demo","Version":"1.0.0.5","SDKVersion":3,"Software":{"MinimumVersion":"7.1"},"OS":[{"Platform":"mac","MinimumVersion":"12"}]}`)
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	notes := filepath.Join(dir, "notes.md")
	os.WriteFile(notes, []byte("**Updates:**\n\n- Improved the dial."), 0600)
	return dir, config, pkg, notes
}

func command(t *testing.T, o Options, args ...string) (string, string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	o.Out = &out
	o.Err = &stderr
	if o.IsTerminal == nil {
		o.IsTerminal = func() bool { return false }
	}
	r := New(o)
	r.SetArgs(args)
	err := r.ExecuteContext(context.Background())
	return out.String(), stderr.String(), err
}

func TestPreparationIsLocalAndPublishChoiceIsExplicit(t *testing.T) {
	dir, cfg, pkg, notes := fixture(t)
	factory := func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Fatal("offline preparation must not create a client")
		return nil, nil
	}
	base := []string{"--config", cfg, "--state-dir", filepath.Join(dir, "plans"), "release", "prepare", "--product", "demo", "--file", pkg, "--notes", notes, "--offline"}
	for _, choice := range [][]string{nil, {"--auto-publish", "--no-auto-publish"}, {"--auto-publish=false"}, {"--no-auto-publish=false"}} {
		_, _, err := command(t, Options{ClientFactory: factory}, append(append([]string{}, base...), choice...)...)
		if err == nil {
			t.Fatalf("choice %v should fail", choice)
		}
	}
	out, _, err := command(t, Options{ClientFactory: factory}, append(append([]string{"--json"}, base...), "--no-auto-publish")...)
	if err != nil {
		t.Fatal(err)
	}
	var p release.Plan
	if err = json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatal(err)
	}
	if p.State != release.Prepared || p.AutoPublish || p.Package.Manifest.Version != "1.0.0.5" {
		t.Fatalf("bad preview: %+v", p)
	}
}

func TestSubmissionCannotBeEnabledGloballyOrByPipe(t *testing.T) {
	for _, args := range [][]string{{"--yes", "products"}, {"release", "upload", "id", "--yes"}, {"release", "submit", "id"}} {
		_, _, err := command(t, Options{}, args...)
		if err == nil {
			t.Fatalf("unsafe argument sequence accepted: %v", args)
		}
	}
}

func TestAuthImportOutputsNoSecret(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "private")
	os.Mkdir(private, 0700)
	secret := "dummy-session-secret"
	file := filepath.Join(private, "cookies.json")
	out, stderr, err := command(t, Options{In: strings.NewReader("Cookie: __Host-kc_at=" + secret + "; analytics=ignore")}, "--credentials-file", file, "auth", "import", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out+stderr, secret) {
		t.Fatal("credentials leaked")
	}
	c, err := auth.NewFileStore(file).Load()
	if err != nil || len(c.Cookies()) != 1 {
		t.Fatal("import did not persist filtered credentials", err)
	}
}

func TestCookieRotationPersistsAfterSuccessAndFailure(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			dir, cfg, _, _ := fixture(t)
			private := filepath.Join(dir, "private")
			if err := os.Mkdir(private, 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(private, "cookies.json")
			store := auth.NewFileStore(file)
			creds, err := auth.Parse(strings.NewReader("__Host-kc_at=dummy-access; __Secure-kc_rt=dummy-before"))
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Save(creds); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/org/products" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				http.SetCookie(w, &http.Cookie{Name: "__Secure-kc_rt", Value: "dummy-after", Path: "/api", Secure: true, HttpOnly: true})
				w.WriteHeader(status)
				io.WriteString(w, `{"products":[]}`)
			}))
			defer server.Close()
			factory := func(c []*http.Cookie, readOnly bool) (*maker.Client, error) {
				return maker.New(maker.Config{BaseURL: server.URL, HTTPClient: server.Client(), Cookies: c, ReadOnly: readOnly, AllowLocalhost: true})
			}
			out, stderr, err := command(t, Options{ClientFactory: factory}, "--config", cfg, "--credentials-file", file, "--json", "products")
			if (err == nil) != (status == http.StatusOK) {
				t.Fatalf("wrong command outcome: %v", err)
			}
			if strings.Contains(out+stderr, "dummy-after") || strings.Contains(out+stderr, "dummy-before") {
				t.Fatal("cookie rotation exposed credentials")
			}
			rotated, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			cookies := rotated.Cookies()
			if len(cookies) != 2 || cookies[1].Value != "dummy-after" || cookies[1].Path != "/api" {
				t.Fatal("rotated cookie did not persist with its scope")
			}
		})
	}
}

func TestCLIReleaseFlowUsesOnlySyntheticServer(t *testing.T) {
	dir, cfg, pkg, notes := fixture(t)
	credDir := filepath.Join(dir, "private")
	os.Mkdir(credDir, 0700)
	cred := filepath.Join(credDir, "cookies.json")
	c, _ := auth.Parse(strings.NewReader("__Host-kc_at=dummy"))
	if err := auth.NewFileStore(cred).Save(c); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	mutations := 0
	remote := []any{}
	ids := map[string]string{"uploadAux": strings.Repeat("a", 40), "getIsSdVersionValid": strings.Repeat("b", 40), "createVersion": strings.Repeat("c", 40)}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/storage/package" {
			if r.Header.Get("Cookie") != "" {
				t.Error("cookies reached storage")
			}
			b, _ := io.ReadAll(r.Body)
			expected, _ := os.ReadFile(pkg)
			if !bytes.Equal(b, expected) {
				t.Error("wrong package uploaded")
			}
			mu.Lock()
			mutations++
			mu.Unlock()
			return
		}
		if r.Header.Get("Cookie") == "" {
			t.Error("Maker cookie missing")
		}
		if r.Method == "POST" {
			mu.Lock()
			mutations++
			mu.Unlock()
			switch r.Header.Get("Next-Action") {
			case ids["uploadAux"]:
				r.ParseMultipartForm(1024 * 1024)
				if r.FormValue("0") != `[null,"$K1"]` {
					t.Error("wrong upload encoding")
				}
				io.WriteString(w, "1:{\"success\":true,\"id\":\"folder\"}\n")
			case ids["getIsSdVersionValid"]:
				io.WriteString(w, "1:{\"is_valid\":true}\n")
			case ids["createVersion"]:
				r.ParseMultipartForm(1024 * 1024)
				if r.FormValue("_1_is_ready_for_publish") != "false" {
					t.Error("publication choice changed")
				}
				mu.Lock()
				remote = []any{map[string]any{"id": "remote-version", "version_number": "1.0.0.5", "status": "PENDING_REVIEW", "release_notes": r.FormValue("_1_release_notes"), "is_ready_for_publish": false}}
				mu.Unlock()
				io.WriteString(w, "1:{\"success\":true,\"id\":\"remote-version\"}\n")
			default:
				t.Error("unknown action")
				http.Error(w, "unknown action", 400)
			}
			return
		}
		switch r.URL.Path {
		case "/api/presigned-url":
			json.NewEncoder(w).Encode(map[string]string{"presignedUrl": server.URL + "/storage/package"})
		case "/api/upload-aux/manifest/folder":
			b, _ := os.ReadFile(pkg)
			z, _ := zip.NewReader(bytes.NewReader(b), int64(len(b)))
			f, _ := z.File[0].Open()
			defer f.Close()
			io.Copy(w, f)
		case "/api/upload-aux/tags/folder":
			io.WriteString(w, `{}`)
		case "/_next/static/current.js":
			for name, id := range ids {
				fmt.Fprintf(w, `(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"%s");`, id, name)
			}
		case "/org/products", "/org/products/product/versions":
			mu.Lock()
			vs := append([]any{}, remote...)
			mu.Unlock()
			p := map[string]any{"id": "product", "name": "Demo", "status": "PUBLISHED", "variants": []any{map[string]any{"id": "variant", "streamdeckid": "com.example.demo", "versions": vs}}}
			var data any = map[string]any{"product": p}
			if r.URL.Path == "/org/products" {
				data = map[string]any{"products": []any{p}}
			}
			b, _ := json.Marshal(data)
			frame, _ := json.Marshal([]any{1, "1:" + string(b) + "\n"})
			fmt.Fprintf(w, `<script src="/_next/static/current.js"></script><script>self.__next_f.push(%s)</script>`, frame)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	factory := func(cookies []*http.Cookie, readOnly bool) (*maker.Client, error) {
		for _, c := range cookies {
			c.Secure = false
		}
		return maker.New(maker.Config{BaseURL: server.URL, AllowLocalhost: true, Cookies: cookies, ReadOnly: readOnly, HTTPClient: server.Client()})
	}
	o := Options{ClientFactory: factory}
	common := []string{"--json", "--config", cfg, "--state-dir", filepath.Join(dir, "plans"), "--credentials-file", cred}
	run := func(args ...string) string {
		t.Helper()
		out, _, err := command(t, o, append(append([]string{}, common...), args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	var ps []model.Product
	if err := json.Unmarshal([]byte(run("products")), &ps); err != nil || len(ps) != 1 {
		t.Fatal("product read failed", err)
	}
	var p release.Plan
	json.Unmarshal([]byte(run("release", "prepare", "--product", "demo", "--file", pkg, "--notes", notes, "--no-auto-publish")), &p)
	mu.Lock()
	n := mutations
	mu.Unlock()
	if n != 0 {
		t.Fatal("prepare mutated remote")
	}
	json.Unmarshal([]byte(run("release", "upload", p.ID)), &p)
	if p.State != release.Uploaded {
		t.Fatal("upload did not stage")
	}
	mu.Lock()
	n = mutations
	mu.Unlock()
	if n != 2 {
		t.Fatal("upload submitted or missed staging")
	}
	json.Unmarshal([]byte(run("release", "submit", p.ID, "--yes")), &p)
	if p.State != release.Submitted || p.RemoteStatus != "PENDING_REVIEW" {
		t.Fatal("submit verification failed")
	}
	if _, _, err := command(t, o, append(append([]string{}, common...), "release", "submit", p.ID, "-y")...); err == nil {
		t.Fatal("duplicate submission allowed")
	}
}
