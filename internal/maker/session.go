package maker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const sessionRenewalWindow = 60 * time.Second

// SessionInfo contains scheduling metadata only, never session credentials.
type SessionInfo struct {
	Refreshed       bool       `json:"refreshed"`
	AccessExpiresAt *time.Time `json:"access_expires_at,omitempty"`
}

// accessExpiry decodes unverified JWT metadata solely for renewal scheduling.
// Neither the JWT claims nor a cookie expiration prove authentication.
func accessExpiry(cookie *http.Cookie) (time.Time, bool) {
	if cookie == nil || cookie.Value == "" {
		return time.Time{}, false
	}
	expires := cookie.Expires
	var parts []string
	if len(cookie.Value) <= 64*1024 {
		parts = strings.Split(cookie.Value, ".")
	}
	if len(parts) == 3 {
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err == nil {
			var claims struct {
				Exp json.Number `json:"exp"`
			}
			if json.Unmarshal(payload, &claims) == nil {
				if seconds, err := claims.Exp.Int64(); err == nil && seconds > 0 && seconds < 253402300800 {
					jwtExpires := time.Unix(seconds, 0).UTC()
					if expires.IsZero() || jwtExpires.Before(expires) {
						expires = jwtExpires
					}
				}
			}
		}
	}
	return expires, !expires.IsZero()
}

func accessCookie(cookies []*http.Cookie) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == "__Host-kc_at" && cookie.Value != "" {
			return cookie
		}
	}
	return nil
}
func sessionInfo(cookies []*http.Cookie) SessionInfo {
	var info SessionInfo
	if expires, known := accessExpiry(accessCookie(cookies)); known {
		info.AccessExpiresAt = &expires
	}
	return info
}

// EnsureSession renews an absent or nearly expired access credential when a
// refresh credential is available. Opaque access values with no expiry retain
// their existing behavior; the server remains the authority on authentication.
func (c *Client) EnsureSession(ctx context.Context) (SessionInfo, error) {
	cookies := c.CookieSnapshot()
	info := sessionInfo(cookies)
	endpoint := *c.base
	endpoint.Path = "/api/auth/session"
	refreshAvailable := false
	for _, cookie := range c.jar.Cookies(&endpoint) {
		if cookie.Name == "__Secure-kc_rt" && cookie.Value != "" {
			refreshAvailable = true
		}
	}
	if !refreshAvailable {
		return info, nil
	}
	access := accessCookie(cookies)
	if access == nil || info.AccessExpiresAt != nil && !info.AccessExpiresAt.After(time.Now().Add(sessionRenewalWindow)) {
		return c.RefreshSession(ctx)
	}
	return info, nil
}

// RefreshSession requests a session rotation. Rejected or unrecognized responses
// leave existing credentials untouched. There is no retry of the original action.
func (c *Client) RefreshSession(ctx context.Context) (SessionInfo, error) {
	original := c.CookieSnapshot()
	unchanged := sessionInfo(original)
	endpoint := *c.base
	endpoint.Path = "/api/auth/session"
	temporary := newScopedJar(c.base, original)
	hasRefresh := false
	for _, cookie := range temporary.Cookies(&endpoint) {
		if cookie.Name == "__Secure-kc_rt" && cookie.Value != "" {
			hasRefresh = true
		}
	}
	if !hasRefresh {
		return unchanged, errors.New("Maker session refresh requires a refresh credential; sign in again")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return unchanged, errors.New("cannot create Maker session request")
	}
	request.Header.Set("Accept", "application/json")
	client := *c.http
	client.Jar = temporary
	started := time.Now()
	var responseBytes int64
	failed := true
	defer func() { c.recordRequest(true, http.MethodGet, time.Since(started), responseBytes, failed) }()
	response, err := client.Do(request)
	if err != nil {
		return unchanged, errors.New("Maker session refresh transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return unchanged, errors.New("Maker session refresh rejected; sign in again")
	}
	// Consume only a bounded body; no body fields or transport errors are exposed.
	responseBytes, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1024*1024))
	if err != nil {
		return unchanged, errors.New("Maker session refresh response read failed")
	}
	failed = false
	updated := temporary.snapshot()
	access := accessCookie(updated)
	expires, known := accessExpiry(access)
	issued := false
	for _, cookie := range response.Cookies() {
		if cookie.Name == "__Host-kc_at" && access != nil && cookie.Value == access.Value && cookie.MaxAge >= 0 && cookie.Secure && cookie.Path == "/" && cookie.Domain == "" {
			issued = true
		}
	}
	previous := accessCookie(original)
	changed := access != nil && (previous == nil || access.Value != previous.Value || !access.Expires.Equal(previous.Expires))
	if !issued || !changed || !known || !expires.After(time.Now()) {
		return unchanged, errors.New("Maker session refresh did not renew access; sign in again")
	}
	c.jar.SetCookies(&endpoint, response.Cookies())
	info := sessionInfo(c.CookieSnapshot())
	info.Refreshed = true
	return info, nil
}
