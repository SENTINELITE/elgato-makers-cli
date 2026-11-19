package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/auth"
	"github.com/SENTINELITE/elgato-makers-cli/internal/draft"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

func testToken(exp time.Time) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".synthetic"
}

func TestCLIRenewalPersistsBeforeReadAndSupportsExplicitRefresh(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprint(manual), func(t *testing.T) {
			dir, cfg, _, _ := fixture(t)
			private := filepath.Join(dir, "private")
			os.Mkdir(private, 0700)
			file := filepath.Join(private, "session.json")
			store := auth.NewFileStore(file)
			creds, _ := auth.Parse(strings.NewReader("__Host-kc_at=" + testToken(time.Now().Add(-time.Minute)) + "; __Secure-kc_rt=dummy-refresh"))
			if err := store.Save(creds); err != nil {
				t.Fatal(err)
			}
			rotated := testToken(time.Now().Add(15 * time.Minute))
			refreshes, reads := 0, 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("unexpected mutation")
					http.Error(w, "no", 400)
					return
				}
				switch r.URL.Path {
				case "/api/auth/session":
					refreshes++
					http.SetCookie(w, &http.Cookie{Name: "__Host-kc_at", Value: rotated, Path: "/", Secure: true, HttpOnly: true, MaxAge: 900})
					fmt.Fprint(w, `{"session":true}`)
				case "/org/products":
					reads++
					stored, err := store.Load()
					if err != nil {
						t.Error(err)
					}
					if len(stored.Cookies()) < 1 || stored.Cookies()[0].Value != rotated {
						t.Error("rotated cookie not persisted before follow-up read")
					}
					fmt.Fprint(w, `{"products":[]}`)
				default:
					t.Error("unexpected route")
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			factory := func(c []*http.Cookie, ro bool) (*maker.Client, error) {
				return maker.New(maker.Config{BaseURL: server.URL, HTTPClient: server.Client(), Cookies: c, ReadOnly: ro, AllowLocalhost: true})
			}
			args := []string{"--config", cfg, "--credentials-file", file, "--json"}
			if manual {
				args = append(args, "auth", "refresh")
			} else {
				args = append(args, "products")
			}
			out, stderr, err := command(t, Options{ClientFactory: factory}, args...)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out+stderr, rotated) || strings.Contains(out+stderr, "dummy-refresh") {
				t.Fatal("secret leak")
			}
			if refreshes != 1 || (!manual && reads != 1) || (manual && reads != 0) {
				t.Fatal("unexpected request count", refreshes, reads)
			}
			stored, err := store.Load()
			if err != nil || stored.Cookies()[0].Value != rotated {
				t.Fatal("renewal not retained", err)
			}
			if _, err = os.Stat(file + ".lock"); !os.IsNotExist(err) {
				t.Fatal("session lock not released")
			}
		})
	}
}

func TestCLIDraftFlowNeverUploadsOrSubmits(t *testing.T) {
	dir, cfg, pkg, notes := fixture(t)
	private := filepath.Join(dir, "private")
	os.Mkdir(private, 0700)
	file := filepath.Join(private, "session.json")
	creds, _ := auth.Parse(strings.NewReader("__Host-kc_at=synthetic-access"))
	if err := auth.NewFileStore(file).Save(creds); err != nil {
		t.Fatal(err)
	}
	posts := 0
	created := false
	description, err := os.ReadFile(notes)
	if err != nil {
		t.Fatal(err)
	}
	product := map[string]any{"id": "new-product", "name": "Demo", "description": string(description), "status": "DRAFT", "variants": []any{map[string]any{"id": "new-variant", "streamdeckid": "com.example.demo", "versions": []any{}}}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/org/products":
			items := []any{}
			if created {
				items = append(items, product)
			}
			json.NewEncoder(w).Encode(map[string]any{"products": items})
		case "/org/create/plugins":
			if r.Method == "GET" {
				fmt.Fprintf(w, `<script src="/_next/static/draft.js"></script><script>self.__next_f.push([1,"1:{\"extension\":{\"id\":\"plugin-extension\",\"slug\":\"plugins\"}}\n"])</script>`)
				return
			}
			posts++
			if r.Method != "POST" || r.Header.Get("Next-Action") != strings.Repeat("a", 40) {
				t.Error("unexpected draft action")
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			if r.FormValue("_1_streamDeckId") != "com.example.demo" || len(r.MultipartForm.File) != 0 {
				t.Error("wrong draft form or unexpected upload")
			}
			created = true
			fmt.Fprint(w, "1:{\"success\":true,\"productId\":\"new-product\",\"variantId\":\"new-variant\"}\n")
		case "/org/products/new-product/versions":
			if r.Method != http.MethodGet {
				t.Error("versions reconciliation must remain read-only")
				http.Error(w, "unexpected mutation", http.StatusMethodNotAllowed)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"product": product})
		case "/_next/static/draft.js":
			fmt.Fprintf(w, `(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"createProduct");`, strings.Repeat("a", 40))
		default:
			t.Error("unexpected route: no upload or submission is allowed", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	factory := func(c []*http.Cookie, ro bool) (*maker.Client, error) {
		return maker.New(maker.Config{BaseURL: server.URL, HTTPClient: server.Client(), Cookies: c, ReadOnly: ro, AllowLocalhost: true})
	}
	common := []string{"--config", cfg, "--state-dir", filepath.Join(dir, "plans"), "--credentials-file", file, "--json", "product", "draft"}
	run := func(args ...string) string {
		t.Helper()
		out, _, err := command(t, Options{ClientFactory: factory}, append(append([]string{}, common...), args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	var p draft.Plan
	if err := json.Unmarshal([]byte(run("prepare", "--file", pkg, "--description-file", notes)), &p); err != nil {
		t.Fatal(err)
	}
	if posts != 0 || p.State != draft.Prepared {
		t.Fatal("prepare mutated")
	}
	if err := json.Unmarshal([]byte(run("create", p.ID)), &p); err != nil {
		t.Fatal(err)
	}
	if p.State != draft.Created || p.Target.ProductID != "new-product" || posts != 1 {
		t.Fatal("draft creation failed")
	}
	run("create", p.ID)
	run("status", p.ID)
	if posts != 1 {
		t.Fatal("repeat created duplicate")
	}
}

func TestCredentialLockPreventsOverlappingImport(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "private")
	os.Mkdir(private, 0700)
	file := filepath.Join(private, "session.json")
	if err := os.WriteFile(file+".lock", []byte("pid=synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := command(t, Options{In: strings.NewReader("__Host-kc_at=synthetic")}, "--credentials-file", file, "auth", "import", "--stdin")
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatal("lock did not prevent import")
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("locked import wrote credentials")
	}
}
