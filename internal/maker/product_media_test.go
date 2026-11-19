package maker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/mediatest"
)

type mediaRoundTrip func(*http.Request) (*http.Response, error)

func (f mediaRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mediaFixture(t *testing.T, p any, cdn func(*http.Request) (*http.Response, error)) (*Client, *int) {
	t.Helper()
	calls := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Method != "GET" || r.URL.Path != "/test-org/products/test-product" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(p)
	}))
	t.Cleanup(server.Close)
	h := &http.Client{Transport: mediaRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "mp-cdn.elgato.com" {
			if cdn == nil {
				t.Fatal("unexpected CDN request")
			}
			return cdn(r)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	c, e := New(Config{BaseURL: server.URL, AllowLocalhost: true, ReadOnly: true, HTTPClient: h, Cookies: []*http.Cookie{{Name: "__Host-kc_at", Value: "synthetic-secret"}}})
	if e != nil {
		t.Fatal(e)
	}
	return c, calls
}
func mediaRow(id, kind, path string, order int) map[string]any {
	return map[string]any{"id": id, "media_type": kind, "file": path, "video": nil, "order": order}
}
func publicCDN(path string) string { return "https://mp-cdn.elgato.com/media/" + path }
func mediaResponse(r *http.Request, b []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b)), ContentLength: int64(len(b)), Request: r}
}
func TestProductMediaMetadataAndOrderedVideo(t *testing.T) {
	video := mediaRow("video", "preview", "", 2)
	video["video"] = map[string]any{"file_cdn": publicCDN("video.mp4")}
	p := syntheticProduct([]any{mediaRow("icon", "app-icon", publicCDN("icon.png"), 1), mediaRow("thumb", "thumbnail", publicCDN("thumb.png"), 1)}, []any{video, mediaRow("image", "preview", publicCDN("image.png"), 1)})
	c, calls := mediaFixture(t, p, nil)
	got, e := c.ProductMedia(context.Background(), "test-org", "test-product", false)
	if e != nil {
		t.Fatal(e)
	}
	if *calls != 1 || got.InspectionRequests != 0 || got.Icon == nil || got.Thumbnail == nil || len(got.Gallery) != 2 || got.Gallery[0].ID != "image" || got.Gallery[1].Kind != "video" || got.Gallery[1].VariantID != "test-variant" {
		t.Fatalf("unexpected result %+v", got)
	}
	if got.Icon.MIME != nil || got.Icon.ByteSize != nil || got.Icon.Width != nil || got.Icon.OriginalMetadataKnown || got.Icon.SourceKind != "cdn_rendition" || got.Icon.UnknownReason == "" || got.ObservedAt == "" {
		t.Fatal("unknown metadata was invented")
	}
}
func TestProductMediaInspectsPublicRenditionWithoutCredentials(t *testing.T) {
	var imageBytes bytes.Buffer
	png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 144, 144)))
	video := mediaRow("video", "preview", "", 1)
	video["video"] = map[string]any{"file_cdn": publicCDN("video.mp4")}
	p := syntheticProduct([]any{mediaRow("icon", "app-icon", publicCDN("icon.png"), 1)}, []any{video})
	requests := 0
	c, _ := mediaFixture(t, p, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != "GET" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Referer") != "" || r.URL.RawQuery != "" {
			t.Fatal("credential or request boundary violation")
		}
		if strings.HasSuffix(r.URL.Path, "mp4") {
			return mediaResponse(r, mediatest.MP4()), nil
		}
		return mediaResponse(r, imageBytes.Bytes()), nil
	})
	got, e := c.ProductMedia(context.Background(), "test-org", "test-product", true)
	if e != nil {
		t.Fatal(e)
	}
	if requests != 2 || *got.Icon.Width != 144 || *got.Icon.MIME != "image/png" || *got.Icon.ByteSize != int64(imageBytes.Len()) || *got.Gallery[0].Width != 1920 || *got.Gallery[0].Height != 1080 || *got.Gallery[0].MIME != "video/mp4" || got.Icon.OriginalMetadataKnown {
		t.Fatalf("bad rendition metadata %+v", got)
	}
}
func TestProductMediaURLsFailClosed(t *testing.T) {
	bad := []string{"http://mp-cdn.elgato.com/media/a", "https://mp-cdn.elgato.com:443/media/a", "https://mp-cdn.elgato.com.evil.test/media/a", "https://mp-cdn.elgato.com@127.0.0.1/media/a", "https://127.0.0.1/media/a", "https://[::1]/media/a", "https://169.254.169.254/latest/meta-data", "https://mp-cdn.elgato.com/media/a?X-Amz-Signature=synthetic-secret", "https://mp-cdn.elgato.com/media/%2e%2e/a", "https://mp-cdn.elgato.com/media/a#secret", "https://mp-cdn.elgato.com/private/a", "https://mp-cdn.elgato.com/media/../a"}
	for i, raw := range bad {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			c, _ := mediaFixture(t, syntheticProduct([]any{mediaRow("icon", "app-icon", raw, 1)}, []any{}), nil)
			out, e := c.ProductMedia(context.Background(), "test-org", "test-product", true)
			if e != nil {
				t.Fatal(e)
			}
			b, _ := json.Marshal(out)
			if out.Icon.PublicURL != "" || out.InspectionRequests != 0 || strings.Contains(string(b), "synthetic-secret") {
				t.Fatalf("unsafe URL exposed: %s", b)
			}
		})
	}
}
func TestProductMediaInspectionFailureRemainsUnknown(t *testing.T) {
	for _, kind := range []string{"redirect", "length", "stream", "bad-image", "bad-mp4"} {
		t.Run(kind, func(t *testing.T) {
			row := mediaRow("asset", "app-icon", publicCDN("asset.png"), 1)
			p := syntheticProduct([]any{row}, []any{})
			if kind == "bad-mp4" {
				row = mediaRow("video", "preview", "", 1)
				row["video"] = map[string]any{"file_cdn": publicCDN("asset.mp4")}
				p = syntheticProduct([]any{}, []any{row})
			}
			requests := 0
			c, _ := mediaFixture(t, p, func(r *http.Request) (*http.Response, error) {
				requests++
				resp := mediaResponse(r, []byte("invalid media"))
				switch kind {
				case "redirect":
					resp.StatusCode = 302
					resp.Header.Set("Location", "http://127.0.0.1/private")
				case "length":
					resp.ContentLength = 50 << 20
				case "stream":
					resp.ContentLength = -1
					resp.Body = io.NopCloser(io.LimitReader(zeroReader{}, 3<<20))
				}
				return resp, nil
			})
			out, e := c.ProductMedia(context.Background(), "test-org", "test-product", true)
			if e != nil {
				t.Fatal(e)
			}
			a := out.Icon
			if a == nil {
				a = &out.Gallery[0]
			}
			if requests != 1 || a.Width != nil || a.MIME != nil || a.ByteSize != nil || a.UnknownReason == "" {
				t.Fatal("unsafe metadata accepted")
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestProductMediaRejectsUnanchoredAndConflictingData(t *testing.T) {
	for _, kind := range []string{"product", "organization", "variant", "order", "overflow"} {
		t.Run(kind, func(t *testing.T) {
			p := syntheticProduct([]any{}, []any{mediaRow("image", "preview", publicCDN("a.png"), 1)})
			v := p["variants"].([]any)[0].(map[string]any)
			switch kind {
			case "product":
				p["id"] = "foreign"
			case "organization":
				p["organization_id"] = "foreign"
			case "variant":
				v["product_id"] = "foreign"
			case "order":
				v["previews"] = []any{mediaRow("a", "preview", "", 1), mediaRow("b", "preview", "", 1)}
			case "overflow":
				var rows []any
				for i := 0; i < 11; i++ {
					rows = append(rows, mediaRow("a", "preview", "", i+1))
				}
				v["previews"] = rows
			}
			c, _ := mediaFixture(t, p, nil)
			if _, e := c.ProductMedia(context.Background(), "test-org", "test-product", true); e == nil {
				t.Fatal("accepted conflicting media")
			}
		})
	}
}
func TestProductMediaRejectsArbitraryInputBeforeHTTP(t *testing.T) {
	c, calls := mediaFixture(t, syntheticProduct([]any{}, []any{}), nil)
	for _, id := range []string{"https://evil.test", "../foreign", ".", "..", strings.Repeat("x", 129)} {
		if _, e := c.ProductMedia(context.Background(), "test-org", id, true); e == nil {
			t.Fatal("accepted input")
		}
	}
	if *calls != 0 {
		t.Fatal("invalid input reached provider")
	}
}
func TestProductMediaInspectionHasRequestBudget(t *testing.T) {
	p := syntheticProduct([]any{mediaRow("icon", "app-icon", publicCDN("icon.png"), 1), mediaRow("thumb", "thumbnail", publicCDN("thumb.png"), 1)}, []any{})
	var variants []any
	for v := 0; v < 2; v++ {
		var previews []any
		for n := 1; n <= 10; n++ {
			previews = append(previews, mediaRow(fmt.Sprintf("image-%d-%d", v, n), "preview", publicCDN("a.png"), n))
		}
		variants = append(variants, map[string]any{"id": fmt.Sprintf("variant-%d", v), "previews": previews})
	}
	p["variants"] = variants
	c, _ := mediaFixture(t, p, func(r *http.Request) (*http.Response, error) { return mediaResponse(r, []byte("invalid")), nil })
	got, e := c.ProductMedia(context.Background(), "test-org", "test-product", true)
	if e != nil {
		t.Fatal(e)
	}
	if got.InspectionRequests != 12 || got.Gallery[19].UnknownReason != "Rendition inspection request, byte or time budget exhausted." {
		t.Fatalf("request budget not honored %+v", got)
	}
}
