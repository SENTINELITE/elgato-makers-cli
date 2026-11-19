package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExplicitSensitiveEncoding(t *testing.T) {
	c, err := Parse(strings.NewReader(`[{"name":"__Host-kc_at","value":"synthetic_secret","path":"/","expires":"2099-01-01T00:00:00Z"},{"name":"tracking","value":"ignored"}]`))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprintf("%v", c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(s, "synthetic_secret") {
			t.Fatal("fmt disclosed credentials")
		}
	}
	ordinary, _ := json.Marshal(c)
	if strings.Contains(string(ordinary), "synthetic_secret") {
		t.Fatal("implicit JSON disclosed credentials")
	}
	encoded, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "synthetic_secret") || strings.Contains(string(encoded), "tracking") {
		t.Fatal("incorrect explicit encoding")
	}
	roundtrip, err := Parse(strings.NewReader(string(encoded)))
	if err != nil || len(roundtrip.Cookies()) != 1 {
		t.Fatal("cannot parse encoded credentials")
	}
}
func TestBridgeOriginAndIdentifierBoundary(t *testing.T) {
	for _, base := range []string{"https://example.com", "http://maker.elgato.com", "https://maker.elgato.com/path"} {
		if _, err := New(Credentials{}, Config{BaseURL: base}); err == nil {
			t.Fatal("origin allowed")
		}
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; t.Error("invalid identifier requested") }))
	defer srv.Close()
	c, err := New(Credentials{}, Config{BaseURL: srv.URL, AllowLocalhost: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", ".", "..", "org/path", "../org", "org?x=1"} {
		if c.Check(context.Background(), id) == nil {
			t.Fatal("invalid org accepted")
		}
	}
	if _, err := c.Versions(context.Background(), "org", "product", "../variant", ""); err == nil {
		t.Fatal("invalid variant accepted")
	}
	if calls != 0 {
		t.Fatal("unexpected network")
	}
}
