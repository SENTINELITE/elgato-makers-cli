package auth

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/testutil"
)

func TestParseAndRedact(t *testing.T) {
	c, err := Parse(strings.NewReader("Cookie: analytics=no; __Host-kc_at=synthetic-secret; kc_user=user"))
	if err != nil {
		t.Fatal(err)
	}
	cookies := c.Cookies()
	if len(cookies) != 2 {
		t.Fatal("unexpected cookie count")
	}
	cookies[0].Value = "changed"
	if c.Cookies()[0].Value != "synthetic-secret" {
		t.Fatal("Cookies exposes internal state")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", c, c), "synthetic-secret") {
		t.Fatal("credential formatting leaks secret")
	}
}
func TestCookieJSON(t *testing.T) {
	c, err := Parse(strings.NewReader(`{"cookies":[{"name":"__Secure-kc_rt","value":"synthetic","domain":"maker.elgato.com","expirationDate":1893456000}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Expiry() == nil || !c.Expiry().Equal(time.Unix(1893456000, 0)) {
		t.Fatal("expiry missing")
	}
}
func TestParseErrorsAreRedacted(t *testing.T) {
	for _, input := range []string{`curl --cookie 'kc_user=synthetic-secret' https://example.com`, `[{"name":"kc_user","value":"synthetic-secret","domain":"evil.example"}]`, `kc_user=synthetic-secret; kc_user=duplicate`, `{"cookies":"synthetic-secret"}`, "analytics=synthetic-secret"} {
		_, err := Parse(strings.NewReader(input))
		if err == nil {
			t.Fatal("expected rejection")
		}
		if strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("error leaks secret")
		}
	}
}
func TestPrivateFileStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	s := NewFileStore(filepath.Join(dir, "credentials.json"))
	c, _ := Parse(strings.NewReader("kc_user=synthetic"))
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	v, err := s.Load()
	if err != nil || len(v.Cookies()) != 1 {
		t.Fatal("cannot reload")
	}
	if !filesec.IsPrivateDir(dir) {
		t.Fatal("directory mode")
	}
	if !filesec.IsPrivateFile(filepath.Join(dir, "credentials.json")) {
		t.Fatal("file mode")
	}
	testutil.Widen(t, filepath.Join(dir, "credentials.json"))
	if _, err = s.Load(); err == nil {
		t.Fatal("accepted insecure permissions")
	}
	if err = s.Delete(); err != nil {
		t.Fatal(err)
	}
}
func TestFileStoreRejectsSymlink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := filesec.CreatePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("preserve"), 0600)
	os.Symlink(target, filepath.Join(dir, "credentials"))
	c, _ := Parse(strings.NewReader("kc_user=synthetic"))
	if err := NewFileStore(filepath.Join(dir, "credentials")).Save(c); err == nil {
		t.Fatal("accepted symlink")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "preserve" {
		t.Fatal("changed symlink target")
	}
}

func TestFromCookiesPreservesSnapshot(t *testing.T) {
	expiry := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	snapshot := []*http.Cookie{nil, {Name: "analytics", Value: "ignored"}, {Name: "__Secure-kc_rt", Value: "synthetic-refresh", Path: "/api", Domain: "maker.elgato.com", Expires: expiry, HttpOnly: true}}
	c, err := FromCookies(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Cookies()) != 1 || c.Cookies()[0].Path != "/api" || !c.Cookies()[0].Expires.Equal(expiry) || !c.Cookies()[0].HttpOnly {
		t.Fatal("snapshot attributes changed")
	}
	snapshot[2].Value = "changed"
	if c.Cookies()[0].Value != "synthetic-refresh" {
		t.Fatal("snapshot not copied")
	}
	dir := filepath.Join(t.TempDir(), "private")
	s := NewFileStore(filepath.Join(dir, "credentials.json"))
	if err = s.Save(c); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Cookies()[0].Path != "/api" {
		t.Fatal("persistence lost cookie path")
	}
}

func TestFromCookiesEmptyAndValidation(t *testing.T) {
	c, err := FromCookies([]*http.Cookie{{Name: "analytics", Value: "ignored"}})
	if err != nil || len(c.Cookies()) != 0 {
		t.Fatal("empty auth snapshot must permit caller deletion")
	}
	for _, cookies := range [][]*http.Cookie{{{Name: "kc_user", Value: "synthetic-secret", Path: "relative"}}, {{Name: "kc_user", Value: "synthetic-secret", Path: "/bad;path"}}, {{Name: "kc_user", Value: "synthetic-secret"}, {Name: "kc_user", Value: "duplicate", Path: "/api"}}} {
		_, err := FromCookies(cookies)
		if err == nil {
			t.Fatal("expected rejection")
		}
		if strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("validation leaks secret")
		}
	}
}

func TestParsePaths(t *testing.T) {
	for _, input := range []string{"kc_user=synthetic", `[{"name":"kc_user","value":"synthetic"}]`} {
		c, err := Parse(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		if c.Cookies()[0].Path != "/" {
			t.Fatal("default path missing")
		}
	}
	c, err := Parse(strings.NewReader(`[{"name":"__Secure-kc_rt","value":"synthetic","path":"/api"}]`))
	if err != nil || c.Cookies()[0].Path != "/api" {
		t.Fatal("import path lost")
	}
	if _, err = Parse(strings.NewReader(`[{"name":"kc_user","value":"synthetic-secret","path":"relative"}]`)); err == nil {
		t.Fatal("accepted relative path")
	}
}
