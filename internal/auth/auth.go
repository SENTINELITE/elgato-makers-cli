// Package auth imports explicitly supplied Maker cookies; it never reads browser profiles.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var ErrNotFound = errors.New("credentials not found")
var ErrUnavailable = errors.New("credential store unavailable; select a private file store explicitly")

type cookieRecord struct {
	Name           string  `json:"name"`
	Value          string  `json:"value"`
	Domain         string  `json:"domain,omitempty"`
	Path           string  `json:"path,omitempty"`
	Secure         bool    `json:"secure,omitempty"`
	HttpOnly       bool    `json:"httpOnly,omitempty"`
	ExpirationDate float64 `json:"expirationDate,omitempty"`
	Expires        string  `json:"expires,omitempty"`
}

// Credentials deliberately redacts fmt output, including %#v.
type Credentials struct{ records []cookieRecord }

func (Credentials) String() string   { return "[redacted Maker credentials]" }
func (Credentials) GoString() string { return "[redacted Maker credentials]" }
func (c Credentials) Cookies() []*http.Cookie {
	out := make([]*http.Cookie, 0, len(c.records))
	for _, r := range c.records {
		v := &http.Cookie{Name: r.Name, Value: r.Value, Path: r.Path, Secure: true, HttpOnly: r.HttpOnly}
		if r.ExpirationDate > 0 {
			v.Expires = time.Unix(int64(r.ExpirationDate), 0)
		}
		if r.Expires != "" {
			v.Expires, _ = time.Parse(time.RFC3339, r.Expires)
		}
		out = append(out, v)
	}
	return out
}

// Expiry returns the earliest known expiry. Unknown expiry is nil, not proof of validity.
func (c Credentials) Expiry() *time.Time {
	var earliest *time.Time
	for _, v := range c.Cookies() {
		if !v.Expires.IsZero() && (earliest == nil || v.Expires.Before(*earliest)) {
			t := v.Expires
			earliest = &t
		}
	}
	return earliest
}
func allowed(name string) bool {
	return name == "__Host-kc_at" || name == "kc_user" || name == "__Secure-kc_rt"
}

// Parse accepts a Cookie header (optional Cookie: prefix), a JSON cookie array,
// or {"cookies":[...]}. JSON expires is RFC3339; expirationDate is Unix seconds.
// Pasted shell commands are rejected and never executed.
func Parse(r io.Reader) (Credentials, error) {
	b, err := io.ReadAll(io.LimitReader(r, 1024*1024+1))
	if err != nil {
		return Credentials{}, errors.New("cannot read credential input")
	}
	if len(b) > 1024*1024 {
		return Credentials{}, errors.New("credential input too large")
	}
	s := strings.TrimSpace(string(b))
	var records []cookieRecord
	if strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{") {
		if strings.HasPrefix(s, "[") {
			err = json.Unmarshal(b, &records)
		} else {
			var envelope struct {
				Cookies []cookieRecord `json:"cookies"`
			}
			err = json.Unmarshal(b, &envelope)
			records = envelope.Cookies
		}
		if err != nil {
			return Credentials{}, errors.New("invalid cookie JSON")
		}
	} else {
		if strings.HasPrefix(strings.ToLower(s), "curl ") || strings.ContainsAny(s, "\r\n") {
			return Credentials{}, errors.New("provide only a Cookie header or cookie JSON, not a shell command")
		}
		if len(s) >= 7 && strings.EqualFold(s[:7], "Cookie:") {
			s = strings.TrimSpace(s[7:])
		}
		req := &http.Request{Header: make(http.Header)}
		req.Header.Set("Cookie", s)
		for _, c := range req.Cookies() {
			records = append(records, cookieRecord{Name: c.Name, Value: c.Value})
		}
	}
	out, err := fromRecords(records)
	if err != nil {
		return Credentials{}, err
	}
	if len(out.records) == 0 {
		return Credentials{}, errors.New("no recognized Maker auth cookies found")
	}
	return out, nil
}

// FromCookies validates a current server-cookie snapshot for persistence. It
// retains only recognized Maker auth cookies. An empty result is valid and
// signals that the caller should delete previously stored credentials.
func FromCookies(cookies []*http.Cookie) (Credentials, error) {
	records := make([]cookieRecord, 0, len(cookies))
	for _, c := range cookies {
		if c == nil || !allowed(c.Name) {
			continue
		}
		r := cookieRecord{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure, HttpOnly: c.HttpOnly}
		if !c.Expires.IsZero() {
			r.Expires = c.Expires.UTC().Format(time.RFC3339)
		}
		records = append(records, r)
	}
	return fromRecords(records)
}

func fromRecords(records []cookieRecord) (Credentials, error) {
	out := Credentials{}
	seen := map[string]bool{}
	for _, v := range records {
		if !allowed(v.Name) {
			continue
		}
		if seen[v.Name] {
			return Credentials{}, fmt.Errorf("duplicate auth cookie %s", v.Name)
		}
		seen[v.Name] = true
		if v.Domain != "" && strings.TrimPrefix(strings.ToLower(v.Domain), ".") != "maker.elgato.com" {
			return Credentials{}, errors.New("auth cookie domain must be maker.elgato.com")
		}
		if v.Expires != "" {
			if _, err := time.Parse(time.RFC3339, v.Expires); err != nil {
				return Credentials{}, errors.New("invalid cookie expiry")
			}
		}
		if v.Path == "" {
			v.Path = "/"
		}
		if !strings.HasPrefix(v.Path, "/") || (&http.Cookie{Name: v.Name, Value: v.Value, Path: v.Path}).Valid() != nil {
			return Credentials{}, errors.New("auth cookie path must be a valid absolute cookie path")
		}
		if v.Value == "" || (&http.Cookie{Name: v.Name, Value: v.Value}).Valid() != nil {
			return Credentials{}, errors.New("invalid auth cookie value")
		}
		out.records = append(out.records, v)
	}
	return out, nil
}

type Store interface {
	Load() (Credentials, error)
	Save(Credentials) error
	Delete() error
}

func encode(c Credentials) ([]byte, error) {
	if len(c.records) == 0 {
		return nil, errors.New("no credentials to save")
	}
	return json.Marshal(c.records)
}
