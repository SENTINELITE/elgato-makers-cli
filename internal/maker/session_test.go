package maker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sessionJWT(expires time.Time) string {
	payload := fmt.Sprintf(`{"exp":%d}`, expires.Unix())
	return "synthetic." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}
func sessionAccess(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{Name: "__Host-kc_at", Value: value, Path: "/", Secure: true, HttpOnly: true, Expires: expires}
}
func sessionRefresh() *http.Cookie {
	return &http.Cookie{Name: "__Secure-kc_rt", Value: "synthetic-private-refresh", Path: "/api", Secure: true, HttpOnly: true}
}
func sessionTestClient(t *testing.T, cookies []*http.Cookie, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	c, err := New(Config{BaseURL: server.URL, AllowLocalhost: true, HTTPClient: server.Client(), Cookies: cookies})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnsureSessionRenewsMissingOrExpiringAccess(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		access *http.Cookie
	}{
		{"missing", nil},
		{"cookie-expired", sessionAccess(sessionJWT(now.Add(-time.Minute)), now.Add(-time.Minute))},
		{"jwt-expired", sessionAccess(sessionJWT(now.Add(-time.Minute)), time.Time{})},
		{"jwt-near-expiry", sessionAccess(sessionJWT(now.Add(30*time.Second)), time.Time{})},
		{"cookie-near-expiry", sessionAccess("opaque-synthetic-access", now.Add(30*time.Second))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cookies := []*http.Cookie{sessionRefresh()}
			if tc.access != nil {
				cookies = append(cookies, tc.access)
			}
			calls := 0
			next := sessionJWT(time.Now().Add(15 * time.Minute))
			c := sessionTestClient(t, cookies, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/api/auth/session" || r.Method != http.MethodGet {
					t.Error("unexpected session request")
				}
				if _, err := r.Cookie("__Secure-kc_rt"); err != nil {
					t.Error("refresh credential missing")
				}
				http.SetCookie(w, sessionAccess(next, time.Time{}))
				http.SetCookie(w, &http.Cookie{Name: "__Secure-kc_rt", Value: "synthetic-rotated-refresh", Path: "/api", Secure: true, HttpOnly: true})
				fmt.Fprint(w, `{"synthetic":"session"}`)
			})
			info, err := c.EnsureSession(context.Background())
			if err != nil || !info.Refreshed || info.AccessExpiresAt == nil || calls != 1 {
				t.Fatalf("renewal failed: %+v %v calls=%d", info, err, calls)
			}
			if accessCookie(c.CookieSnapshot()).Value != next {
				t.Fatal("rotated access not retained")
			}
			raw, _ := json.Marshal(info)
			if strings.Contains(string(raw), "synthetic") || strings.Contains(string(raw), next) {
				t.Fatal("session metadata leaked credentials")
			}
			info, err = c.EnsureSession(context.Background())
			if err != nil || info.Refreshed || calls != 1 {
				t.Fatal("valid renewed session requested another renewal")
			}
		})
	}
}

func TestEnsureSessionSkipsValidAndUnknownExpiry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cookies []*http.Cookie
	}{
		{"valid-jwt", []*http.Cookie{sessionAccess(sessionJWT(time.Now().Add(10*time.Minute)), time.Time{}), sessionRefresh()}},
		{"opaque", []*http.Cookie{sessionAccess("synthetic-opaque", time.Time{}), sessionRefresh()}},
		{"no-refresh", []*http.Cookie{sessionAccess(sessionJWT(time.Now().Add(-time.Minute)), time.Time{})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := sessionTestClient(t, tc.cookies, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected automatic renewal") })
			info, err := c.EnsureSession(context.Background())
			if err != nil || info.Refreshed {
				t.Fatal("unexpected renewal result", err)
			}
		})
	}
}

func TestRefreshSessionFailsClosedAndPreservesCredentials(t *testing.T) {
	for _, kind := range []string{"unauthenticated-200", "rejected-401", "redirect", "expired-cookie", "opaque-cookie", "unchanged-cookie", "outside-domain"} {
		t.Run(kind, func(t *testing.T) {
			originalAccess := sessionAccess(sessionJWT(time.Now().Add(10*time.Minute)), time.Time{})
			c := sessionTestClient(t, []*http.Cookie{originalAccess, sessionRefresh()}, func(w http.ResponseWriter, r *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "__Secure-kc_rt", Value: "synthetic-must-not-save", Path: "/api", Secure: true})
				switch kind {
				case "rejected-401":
					http.SetCookie(w, sessionAccess(sessionJWT(time.Now().Add(time.Hour)), time.Time{}))
					w.WriteHeader(http.StatusUnauthorized)
				case "redirect":
					w.Header().Set("Location", "https://evil.example/synthetic-private-refresh")
					w.WriteHeader(http.StatusFound)
				case "expired-cookie":
					http.SetCookie(w, sessionAccess(sessionJWT(time.Now().Add(-time.Minute)), time.Time{}))
				case "opaque-cookie":
					http.SetCookie(w, sessionAccess("synthetic-private-access", time.Time{}))
				case "unchanged-cookie":
					http.SetCookie(w, originalAccess)
				case "outside-domain":
					cookie := sessionAccess(sessionJWT(time.Now().Add(time.Hour)), time.Time{})
					cookie.Domain = "evil.example"
					http.SetCookie(w, cookie)
				}
				fmt.Fprint(w, `{"authenticated":false,"token":"synthetic-private-refresh"}`)
			})
			before := c.CookieSnapshot()
			info, err := c.RefreshSession(context.Background())
			if err == nil || info.Refreshed {
				t.Fatal("rejected session accepted")
			}
			if strings.Contains(err.Error(), "synthetic") {
				t.Fatal("refresh failure leaked response or credential")
			}
			if !reflect.DeepEqual(before, c.CookieSnapshot()) {
				t.Fatal("rejected renewal mutated credentials")
			}
		})
	}
}

func TestRefreshSessionKnownCookieExpiryAndMissingRefresh(t *testing.T) {
	c := sessionTestClient(t, []*http.Cookie{sessionAccess("synthetic-old", time.Now().Add(time.Hour)), sessionRefresh()}, func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, sessionAccess("synthetic-renewed", time.Now().Add(2*time.Hour)))
	})
	info, err := c.RefreshSession(context.Background())
	if err != nil || !info.Refreshed || info.AccessExpiresAt == nil {
		t.Fatal("explicit known cookie expiry renewal failed", err)
	}
	missing := sessionTestClient(t, nil, func(w http.ResponseWriter, r *http.Request) { t.Error("network used without refresh credential") })
	if _, err := missing.RefreshSession(context.Background()); err == nil {
		t.Fatal("missing refresh accepted")
	}
}
