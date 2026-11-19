package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/testutil"
)

func fakeMCP(t *testing.T, extra func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	var origin string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			json.NewEncoder(w).Encode(map[string]any{"resource": origin + "/mcp", "authorization_servers": []string{origin}})
		case "/.well-known/oauth-authorization-server":
			json.NewEncoder(w).Encode(map[string]any{"issuer": origin, "authorization_endpoint": origin + "/oauth/authorize", "token_endpoint": origin + "/oauth/token", "registration_endpoint": origin + "/oauth/register", "revocation_endpoint": origin + "/oauth/revoke", "code_challenge_methods_supported": []string{"S256"}})
		default:
			extra(w, r)
		}
	}))
	origin = s.URL
	t.Cleanup(s.Close)
	return s
}
func mcpTestApp(t *testing.T) *app {
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	return &app{configPath: filepath.Join(dir, "config.json"), o: Options{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, Now: time.Now}}
}
func TestMCPLoginPKCEAndState(t *testing.T) {
	var redirect, challenge string
	s := fakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/register":
			var body struct {
				Redirects []string `json:"redirect_uris"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Redirects) != 1 {
				t.Error("redirect registration")
			}
			redirect = body.Redirects[0]
			fmt.Fprint(w, `{"client_id":"synthetic-cli"}`)
		case "/oauth/token":
			r.ParseForm()
			if r.Form.Get("redirect_uri") != redirect || r.Form.Get("code") != "synthetic-code" || mcpChallenge(r.Form.Get("code_verifier")) != challenge || r.Form.Get("resource") == "" {
				t.Error("PKCE/token binding failed")
			}
			fmt.Fprint(w, `{"access_token":"SYNTHETIC-ACCESS","refresh_token":"SYNTHETIC-REFRESH","token_type":"Bearer","expires_in":900}`)
		default:
			http.NotFound(w, r)
		}
	})
	a := mcpTestApp(t)
	pr, pw := io.Pipe()
	a.o.Err = pw
	done := make(chan error, 1)
	go func() { done <- a.mcpLogin(context.Background(), s.URL, true); pw.Close() }()
	scanner := bufio.NewScanner(pr)
	if !scanner.Scan() {
		t.Fatal("missing hint")
	}
	if !scanner.Scan() {
		t.Fatal("missing URL")
	}
	u, e := url.Parse(scanner.Text())
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	challenge = q.Get("code_challenge")
	if q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("resource") != s.URL+"/mcp" {
		t.Fatal("authorization binding")
	}
	callback := q.Get("redirect_uri")
	bad := callback + "?state=wrong&code=attacker"
	resp, e := http.Get(bad)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatal("bad state accepted")
	}
	resp, e = http.Get(callback + "?" + url.Values{"state": {q.Get("state")}, "code": {"synthetic-code"}}.Encode())
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("callback rejected")
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	auth, e := a.loadMCP(s.URL)
	if e != nil || auth.AccessToken != "SYNTHETIC-ACCESS" {
		t.Fatal("login not saved")
	}
	if !filesec.IsPrivateFile(a.mcpAuthPath(s.URL)) {
		t.Fatal("credentials not private")
	}
	if strings.Contains(a.o.Out.(*bytes.Buffer).String(), "SYNTHETIC-") {
		t.Fatal("output leaked credential")
	}
}
func TestMCPRefreshStatusIsolationAndLogout(t *testing.T) {
	refreshed, revoked := false, false
	s := fakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			r.ParseForm()
			if r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("resource") == "" {
				t.Error("refresh binding")
			}
			refreshed = true
			fmt.Fprint(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":900}`)
		case "/mcp":
			if r.Header.Get("Authorization") != "Bearer rotated-access" {
				t.Error("rotated access not used")
			}
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{"data":{"state":"connected","org_id":"synthetic-org","cookie":"DO-NOT-PRINT"}},"content":[{"type":"text","text":"DO-NOT-PRINT"}]}}`)
		case "/oauth/revoke":
			r.ParseForm()
			if r.Form.Get("token") != "rotated-refresh" || r.Form.Get("client_id") != "synthetic-cli" {
				t.Error("wrong grant revoked")
			}
			revoked = true
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	})
	a := mcpTestApp(t)
	a.json = true
	os.WriteFile(filepath.Join(filepath.Dir(a.configPath), "maker-cookies"), []byte("unchanged"), 0600)
	auth := mcpAuth{Server: s.URL, ClientID: "synthetic-cli", AccessToken: "expired", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	if e := a.saveMCP(auth); e != nil {
		t.Fatal(e)
	}
	other := auth
	other.Server = "https://other.example"
	a.saveMCP(other)
	if e := a.mcpStatus(context.Background(), s.URL); e != nil {
		t.Fatal(e)
	}
	saved, e := a.loadMCP(s.URL)
	if e != nil || saved.RefreshToken != "rotated-refresh" || !refreshed {
		t.Fatal("rotation not persisted")
	}
	if e := a.mcpLogout(context.Background(), s.URL); e != nil {
		t.Fatal(e)
	}
	if !revoked {
		t.Fatal("not revoked")
	}
	if _, e = os.Stat(a.mcpAuthPath(s.URL)); !os.IsNotExist(e) {
		t.Fatal("login retained")
	}
	if _, e = a.loadMCP(other.Server); e != nil {
		t.Fatal("other server removed")
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(a.configPath), "maker-cookies"))
	if string(b) != "unchanged" {
		t.Fatal("maker cookies changed")
	}
	output := a.o.Out.(*bytes.Buffer).String()
	for _, secret := range []string{"old-refresh", "rotated-access", "rotated-refresh", "DO-NOT-PRINT"} {
		if strings.Contains(output, secret) {
			t.Fatal("leaked secret")
		}
	}
}
func TestMCPDiscoveryRejectsForeignEndpointsAndRedirects(t *testing.T) {
	var foreignHits int
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignHits++ }))
	defer foreign.Close()
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			var origin string
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, foreign.URL, 302)
					return
				}
				if strings.Contains(r.URL.Path, "protected") {
					json.NewEncoder(w).Encode(map[string]any{"resource": origin + "/mcp", "authorization_servers": []string{origin}})
				} else {
					json.NewEncoder(w).Encode(map[string]any{"issuer": origin, "authorization_endpoint": foreign.URL + "/oauth/authorize"})
				}
			}))
			defer s.Close()
			origin = s.URL
			if _, e := discoverMCP(context.Background(), mcpHTTPClient(), origin); e == nil {
				t.Fatal("foreign metadata accepted")
			}
		})
	}
	if foreignHits != 0 {
		t.Fatal("followed foreign URL")
	}
}
func TestMCPFailuresHideServerSecretsAndKeepRevocationRetry(t *testing.T) {
	s := fakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret-token private-cookie code=secret", 500)
	})
	a := mcpTestApp(t)
	a.saveMCP(mcpAuth{Server: s.URL, ClientID: "client", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)})
	if e := a.mcpLogout(context.Background(), s.URL); e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatal("unsafe logout failure")
	}
	if _, e := a.loadMCP(s.URL); e != nil {
		t.Fatal("lost retry credentials")
	}
	if e := mcpRequest(context.Background(), mcpHTTPClient(), "POST", s.URL+"/mcp?code=secret", "", "access", nil, nil); e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatal("unsafe request error")
	}
}
func TestMCPCommandsAndOriginValidation(t *testing.T) {
	root := New(Options{Out: io.Discard, Err: io.Discard})
	for _, args := range [][]string{{"mcp", "login"}, {"mcp", "logout"}, {"mcp", "status"}, {"mcp", "connect"}, {"cloud", "connect"}} {
		c, _, e := root.Find(args)
		if e != nil || c.Name() != args[1] {
			t.Fatalf("missing %v", args)
		}
	}
	for _, raw := range []string{"https://evil.example/path", "https://user:pass@example.com", "http://example.com", "https://example.com?code=secret"} {
		if _, e := cloudOrigin(raw); e == nil {
			t.Fatal("invalid server accepted")
		}
	}
	a := mcpTestApp(t)
	e := a.mcpStatus(context.Background(), "https://example.com")
	if e == nil {
		t.Fatal("missing login status success")
	}
}

func TestMCPCanonicalOriginsAndCredentialLock(t *testing.T) {
	origin, e := mcpOrigin("https://MAKERS.EXAMPLE:443/")
	if e != nil || origin != "https://makers.example" {
		t.Fatal("origin not canonical")
	}
	a := mcpTestApp(t)
	unlock, e := a.lockMCP(origin)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.lockMCP(origin); e == nil {
		t.Fatal("concurrent credential rotation allowed")
	}
	otherUnlock, e := a.lockMCP("https://other.example")
	if e != nil {
		t.Fatal("unrelated origin locked")
	}
	otherUnlock()
	unlock()
	unlock, e = a.lockMCP(origin)
	if e != nil {
		t.Fatal("lock not released")
	}
	unlock()
}
func TestMCPStatusFailureJSONIsSafe(t *testing.T) {
	s := fakeMCP(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "DO-NOT-PRINT", 401) })
	a := mcpTestApp(t)
	a.json = true
	a.saveMCP(mcpAuth{Server: s.URL, ClientID: "client", AccessToken: "DO-NOT-PRINT", ExpiresAt: time.Now().Add(time.Hour)})
	if e := a.mcpStatus(context.Background(), s.URL); e == nil {
		t.Fatal("status failure hidden")
	}
	var result map[string]any
	if e := json.Unmarshal(a.o.Out.(*bytes.Buffer).Bytes(), &result); e != nil {
		t.Fatal("failure not JSON")
	}
	if strings.Contains(a.o.Out.(*bytes.Buffer).String(), "DO-NOT-PRINT") {
		t.Fatal("status leaked secret")
	}
}

func TestMCPCredentialPrivacyBoundsAndRedaction(t *testing.T) {
	a := mcpTestApp(t)
	origin := "https://synthetic.example"
	auth := mcpAuth{Server: origin, ClientID: "client", AccessToken: "SECRET-ACCESS", RefreshToken: "SECRET-REFRESH"}
	for _, value := range []string{fmt.Sprint(auth), fmt.Sprintf("%#v", auth), fmt.Sprint(&auth)} {
		if strings.Contains(value, "SECRET") {
			t.Fatal("format leaked credentials")
		}
	}
	// A public general configuration directory is acceptable; OAuth gets a private child.
	if e := os.Chmod(filepath.Dir(a.configPath), 0755); e != nil {
		t.Fatal(e)
	}
	if e := a.saveMCP(auth); e != nil {
		t.Fatal(e)
	}
	dir := a.mcpDir()
	if !filesec.IsPrivateDir(dir) {
		t.Fatal("OAuth child is not private")
	}
	testutil.Widen(t, dir)
	if e := a.saveMCP(auth); e == nil {
		t.Fatal("public OAuth directory accepted")
	}
	if e := filesec.ProtectPath(dir); e != nil {
		t.Fatal(e)
	}
	actual := t.TempDir()
	if e := filesec.ProtectPath(actual); e != nil {
		t.Fatal(e)
	}
	other := *a
	other.configPath = filepath.Join(t.TempDir(), "config.json")
	if e := os.Symlink(actual, other.mcpDir()); e != nil {
		t.Fatal(e)
	}
	if e := other.saveMCP(auth); e == nil {
		t.Fatal("symlink OAuth directory accepted")
	}
	f, e := os.OpenFile(a.mcpAuthPath(origin), os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(128 << 20); e != nil {
		t.Fatal(e)
	}
	f.Close()
	if _, e = a.loadMCP(origin); e == nil {
		t.Fatal("oversized auth accepted")
	}
}
func TestMCPRefreshSaveFailureLeavesDurableGuard(t *testing.T) {
	a := mcpTestApp(t)
	calls := 0
	var origin string
	s := fakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			calls++
			if _, e := os.Stat(a.mcpRefreshGuard(origin)); e != nil {
				t.Error("refresh dispatched without guard")
			}
			if !filesec.IsPrivateFile(a.mcpRefreshGuard(origin)) {
				t.Error("guard not private")
			}
			if e := os.Rename(a.mcpAuthPath(origin), a.mcpAuthPath(origin)+".old"); e != nil {
				t.Error(e)
			}
			os.Mkdir(a.mcpAuthPath(origin), 0700)
			fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":900}`)
		case "/oauth/revoke":
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	})
	origin = s.URL
	auth := mcpAuth{Server: origin, ClientID: "client", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	if e := a.saveMCP(auth); e != nil {
		t.Fatal(e)
	}
	if e := a.mcpStatus(context.Background(), origin); e == nil {
		t.Fatal("failed save hidden")
	}
	os.Remove(a.mcpAuthPath(origin))
	os.Rename(a.mcpAuthPath(origin)+".old", a.mcpAuthPath(origin))
	if e := a.mcpStatus(context.Background(), origin); e == nil || !strings.Contains(e.Error(), "uncertain") {
		t.Fatal("old refresh token replay allowed")
	}
	if calls != 1 {
		t.Fatal("refresh retried")
	}
	if e := a.mcpLogout(context.Background(), origin); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(a.mcpRefreshGuard(origin)); !os.IsNotExist(e) {
		t.Fatal("logout retained guard")
	}
}
func TestMCPLoginSaveFailureRevokesIssuedGrant(t *testing.T) {
	a := mcpTestApp(t)
	revoked := false
	s := fakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/register":
			fmt.Fprint(w, `{"client_id":"synthetic-cli"}`)
		case "/oauth/token":
			testutil.Widen(t, a.mcpDir())
			fmt.Fprint(w, `{"access_token":"issued-access","refresh_token":"issued-refresh","token_type":"Bearer","expires_in":900}`)
		case "/oauth/revoke":
			r.ParseForm()
			if r.Form.Get("token") != "issued-refresh" {
				t.Error("wrong grant revoked")
			}
			revoked = true
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	})
	pr, pw := io.Pipe()
	a.o.Err = pw
	done := make(chan error, 1)
	go func() { done <- a.mcpLogin(context.Background(), s.URL, true); pw.Close() }()
	scanner := bufio.NewScanner(pr)
	scanner.Scan()
	if !scanner.Scan() {
		t.Fatal("no authorization URL")
	}
	u, e := url.Parse(scanner.Text())
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	resp, e := http.Get(q.Get("redirect_uri") + "?" + url.Values{"state": {q.Get("state")}, "code": {"synthetic-code"}}.Encode())
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	e = <-done
	if e == nil || !strings.Contains(e.Error(), "issued grant was revoked") || !revoked {
		t.Fatal("failed persistence did not revoke grant")
	}
	if e := filesec.ProtectPath(a.mcpDir()); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(e.Error(), "issued-access") || strings.Contains(e.Error(), "issued-refresh") {
		t.Fatal("secret leaked")
	}
}
func TestMCPRefreshInvalidGrantClearsGuardAndExplains(t *testing.T) {
	a := mcpTestApp(t)
	calls := 0
	s := fakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			calls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant","error_description":"DO-NOT-PRINT"}`)
		case "/oauth/revoke":
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	})
	auth := mcpAuth{Server: s.URL, ClientID: "client", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}
	if e := a.saveMCP(auth); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		e := a.mcpStatus(context.Background(), s.URL)
		if e == nil || !strings.Contains(e.Error(), "maker mcp logout") || !strings.Contains(e.Error(), "maker mcp login") || strings.Contains(e.Error(), "uncertain") || strings.Contains(e.Error(), "DO-NOT-PRINT") {
			t.Fatal("revoked grant message", e)
		}
		if e = a.checkMCPRefreshGuard(s.URL); e != nil {
			t.Fatal("definitive rejection stranded the refresh guard", e)
		}
	}
	if calls != 2 {
		t.Fatal("rejected refresh was not retried after guard cleared", calls)
	}
	if e := a.mcpLogout(context.Background(), s.URL); e != nil {
		t.Fatal(e)
	}
	if _, e := a.loadMCP(s.URL); e == nil {
		t.Fatal("logout kept revoked login")
	}
}

func TestMCPRefreshKeepsGuardForUncertainOutcomes(t *testing.T) {
	cases := map[string]func(http.ResponseWriter, *http.Request){
		"other 400 error": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_request"}`)
		},
		"invalid_grant not a 400": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		},
		"unparseable 400 body": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(400); fmt.Fprint(w, `not json`) },
		"server error":         func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) },
		"connection dropped": func(w http.ResponseWriter, _ *http.Request) {
			conn, _, e := w.(http.Hijacker).Hijack()
			if e == nil {
				conn.Close()
			}
		},
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			a := mcpTestApp(t)
			s := fakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth/token" {
					http.NotFound(w, r)
					return
				}
				token(w, r)
			})
			auth := mcpAuth{Server: s.URL, ClientID: "client", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)}
			if e := a.saveMCP(auth); e != nil {
				t.Fatal(e)
			}
			if e := a.mcpStatus(context.Background(), s.URL); e == nil || strings.Contains(e.Error(), "no longer valid") {
				t.Fatal("uncertain failure reported as revoked", e)
			}
			if e := a.checkMCPRefreshGuard(s.URL); e == nil {
				t.Fatal("uncertain refresh lost its guard")
			}
		})
	}
}
