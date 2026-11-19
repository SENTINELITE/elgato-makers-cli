package maker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type statsRoundTripper func(*http.Request) (*http.Response, error)

func (f statsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRequestStatsHTTPAttempts(t *testing.T) {
	storageCookie := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/get":
			_, _ = w.Write([]byte("abc"))
		case "/failure":
			http.Error(w, "failure", http.StatusInternalServerError)
		case "/storage":
			storageCookie <- r.Header.Get("Cookie")
			_, _ = w.Write([]byte("done"))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	transport := statsRoundTripper(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder.Result(), nil
	})
	const base = "http://localhost"
	c, err := New(Config{BaseURL: base, HTTPClient: &http.Client{Transport: transport}, AllowLocalhost: true, Cookies: []*http.Cookie{{Name: "synthetic", Value: "secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.request(ctx, http.MethodGet, base+"/get", "", "", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.request(ctx, http.MethodPost, base+"/failure", "", "", strings.NewReader("synthetic"), true); err == nil {
		t.Fatal("expected HTTP 500 error")
	}
	if _, err := c.request(ctx, http.MethodPut, base+"/storage", "", "", strings.NewReader("synthetic"), false); err != nil {
		t.Fatal(err)
	}
	if got := <-storageCookie; got != "" {
		t.Fatal("storage request received Maker cookie")
	}
	got := c.RequestStats()
	if got.TotalRequests != 3 || got.MakerRequests != 2 || got.StorageRequests != 1 || got.FailedRequests != 1 || got.ResponseBytes != 7 || got.RequestTime <= 0 || got.GetRequests != 1 || got.PostRequests != 1 || got.PutRequests != 1 {
		t.Fatalf("unexpected request stats: %+v", got)
	}
}

func TestRequestStatsRefreshSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		rotate bool
		failed uint64
	}{
		{"success", http.StatusOK, true, 0},
		{"rejected", http.StatusUnauthorized, false, 1},
		{"unrecognized", http.StatusOK, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := statsRoundTripper(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				if tc.rotate {
					http.SetCookie(w, sessionAccess(sessionJWT(time.Now().Add(time.Hour)), time.Time{}))
				}
				w.WriteHeader(tc.status)
				_, _ = w.WriteString("body")
				return w.Result(), nil
			})
			c, err := New(Config{BaseURL: "https://localhost", AllowLocalhost: true, HTTPClient: &http.Client{Transport: transport}, Cookies: []*http.Cookie{sessionRefresh()}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.RefreshSession(context.Background())
			if (err == nil) != tc.rotate {
				t.Fatalf("unexpected refresh error: %v", err)
			}
			got := c.RequestStats()
			wantBytes := uint64(4)
			if tc.status != http.StatusOK {
				wantBytes = 0
			}
			if got.TotalRequests != 1 || got.MakerRequests != 1 || got.GetRequests != 1 || got.FailedRequests != tc.failed || got.ResponseBytes != wantBytes || got.RequestTime <= 0 {
				t.Fatalf("unexpected stats: %+v", got)
			}
		})
	}
}

func TestRequestStatsDeleteDispatch(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			posts := 0
			transport := statsRoundTripper(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				switch {
				case r.Method == http.MethodPost:
					posts++
					w.WriteHeader(status)
					_, _ = w.WriteString("1:{\"success\":true}\n")
				case r.URL.Path == "/org/products/draft-product/versions":
					_, _ = w.WriteString(deleteProductPage("Synthetic", "DRAFT"))
				case r.URL.Path == "/org/products/draft-product":
					_, _ = w.WriteString("<html>General</html>")
				case r.URL.Path == "/_next/static/chunks/3315-current.js":
					_, _ = w.WriteString(deleteAction("a"))
				case r.URL.Path == "/org/products":
					if posts > 0 {
						_, _ = w.WriteString(`{"products":[]}`)
					} else {
						frame, _ := json.Marshal([]any{1, "a:" + deleteProductPage("Synthetic", "DRAFT") + "\n"})
						_, _ = w.WriteString(`<script src="/_next/static/chunks/3315-current.js"></script><script>self.__next_f.push(` + string(frame) + `)</script>`)
					}
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					w.WriteHeader(404)
				}
				return w.Result(), nil
			})
			c, err := New(Config{BaseURL: "http://localhost", AllowLocalhost: true, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.DeleteProduct(context.Background(), deleteTarget())
			if (err == nil) != (status == http.StatusOK) || posts != 1 || result.Deleted != (status == http.StatusOK) {
				t.Fatalf("result=%+v err=%v posts=%d", result, err, posts)
			}
			got := c.RequestStats()
			wantFailed := uint64(0)
			if status != http.StatusOK {
				wantFailed = 1
			}
			if got.PostRequests != 1 || got.MakerRequests != got.TotalRequests || got.FailedRequests != wantFailed || got.RequestTime <= 0 {
				t.Fatalf("unexpected stats: %+v", got)
			}
		})
	}
}
