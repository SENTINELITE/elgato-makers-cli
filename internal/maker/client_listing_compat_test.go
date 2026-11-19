package maker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

func TestReadUploadedManifestEnvelope(t *testing.T) {
	manifest := `{"UUID":"com.example.synthetic","Version":"1.0.0","Software":{"MinimumVersion":"7.1"}}`
	cases := []struct {
		name, body string
		valid      bool
	}{
		{"wrapped", `{"manifest":` + manifest + `}`, true},
		{"direct", manifest, true},
		{"null", `{"manifest":null}`, false},
		{"conflicting", `{"manifest":` + manifest + `,"UUID":"other"}`, false},
		{"missing", `{"manifest":{"UUID":"com.example.synthetic"}}`, false},
		{"invalid", `[]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/upload-aux/manifest/folder" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			c, _ := New(Config{BaseURL: s.URL, AllowLocalhost: true})
			m, err := c.ReadManifest(context.Background(), "folder")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if tc.valid && m.UUID != "com.example.synthetic" {
				t.Fatal("manifest identity lost")
			}
		})
	}
}

func TestPackageUploadFailureBoundary(t *testing.T) {
	for _, failAfterPUT := range []bool{false, true} {
		t.Run(fmt.Sprint(failAfterPUT), func(t *testing.T) {
			writes := 0
			var s *httptest.Server
			s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/_next/static/chunk.js":
					fmt.Fprint(w, fixtureActions("a"))
				case "/api/presigned-url":
					if !failAfterPUT {
						http.Error(w, "synthetic", 503)
						return
					}
					fmt.Fprintf(w, `{"presignedUrl":%q}`, s.URL+"/bucket?signature=synthetic")
				case "/bucket":
					writes++
					http.Error(w, "synthetic", 503)
				default:
					fmt.Fprint(w, fixturePage())
				}
			}))
			defer s.Close()
			file := t.TempDir() + "/synthetic.streamDeckPlugin"
			if err := os.WriteFile(file, []byte("synthetic"), 0600); err != nil {
				t.Fatal(err)
			}
			c, _ := New(Config{BaseURL: s.URL, AllowLocalhost: true})
			_, err := c.Upload(context.Background(), model.Target{OrgID: "org", ProductID: "product", VariantID: "variant"}, model.Package{Path: file, Size: 9})
			if err == nil {
				t.Fatal("expected error")
			}
			var marker interface{ NotUploaded() bool }
			proven := errors.As(err, &marker) && marker.NotUploaded()
			if proven == failAfterPUT {
				t.Fatalf("wrong no-write proof: %v", err)
			}
			if failAfterPUT && writes != 1 {
				t.Fatal("PUT boundary not exercised")
			}
			if strings.Contains(err.Error(), "signature=") {
				t.Fatal("signed URL leaked")
			}
		})
	}
}
