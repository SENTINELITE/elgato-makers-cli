package maker

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// scopedJar receives and sends cookies only at the exact Maker origin. The
// separate signed-upload client never receives this jar.
type scopedJar struct {
	base    *url.URL
	jar     *cookiejar.Jar
	mu      sync.Mutex
	records map[string]*http.Cookie
}

func newScopedJar(base *url.URL, cookies []*http.Cookie) *scopedJar {
	jar, _ := cookiejar.New(nil)
	j := &scopedJar{base: base, jar: jar, records: map[string]*http.Cookie{}}
	j.SetCookies(base, cookies)
	return j
}
func (j *scopedJar) allowed(u *url.URL) bool {
	return u != nil && u.Scheme == j.base.Scheme && u.Host == j.base.Host
}
func (j *scopedJar) Cookies(u *url.URL) []*http.Cookie {
	if !j.allowed(u) {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.jar.Cookies(u)
}
func (j *scopedJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if !j.allowed(u) {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cookies {
		if c == nil || c.Valid() != nil {
			continue
		}
		if c.Domain != "" && strings.TrimPrefix(strings.ToLower(c.Domain), ".") != strings.ToLower(j.base.Hostname()) {
			continue
		}
		v := *c
		v.Domain = ""
		if v.Path == "" || v.Path[0] != '/' {
			v.Path = "/"
			if i := strings.LastIndex(u.Path, "/"); i > 0 {
				v.Path = u.Path[:i]
			}
		}
		// Auth imports have no browser path information and start at root.
		// A server rotation is authoritative for a recognized auth name;
		// remove stale other-path values before accepting its scoped cookie.
		if authCookieName(v.Name) {
			for oldKey, old := range j.records {
				if old.Name == v.Name && old.Path != v.Path {
					removed := *old
					removed.Value = ""
					removed.MaxAge = -1
					j.jar.SetCookies(u, []*http.Cookie{&removed})
					delete(j.records, oldKey)
				}
			}
		}
		key := v.Name + "\x00" + v.Path
		j.jar.SetCookies(u, []*http.Cookie{&v})
		if v.MaxAge < 0 || v.MaxAge == 0 && !v.Expires.IsZero() && v.Expires.Before(time.Now()) {
			delete(j.records, key)
			continue
		}
		record := v
		if record.MaxAge > 0 {
			record.Expires = time.Now().Add(time.Duration(record.MaxAge) * time.Second)
			record.MaxAge = 0
		}
		j.records[key] = &record
	}
}
func (j *scopedJar) snapshot() []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := []*http.Cookie{}
	now := time.Now()
	for key, c := range j.records {
		if !c.Expires.IsZero() && !c.Expires.After(now) {
			delete(j.records, key)
			continue
		}
		v := *c
		out = append(out, &v)
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].Name == out[k].Name {
			return out[i].Path < out[k].Path
		}
		return out[i].Name < out[k].Name
	})
	return out
}

// CookieSnapshot returns sensitive credentials for an explicit credential-store
// save. Session cookies updated by Maker responses remain in memory by default.
func (c *Client) CookieSnapshot() []*http.Cookie { return c.jar.snapshot() }

func authCookieName(name string) bool {
	return name == "__Host-kc_at" || name == "kc_user" || name == "__Secure-kc_rt"
}
