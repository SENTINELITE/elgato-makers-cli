package maker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

func syntheticSubmissionSnapshot(target model.Target) map[string]any {
	return map[string]any{"id": target.ProductID, "name": "Synthetic Product", "variants": []any{map[string]any{"id": target.VariantID, "streamdeckid": target.UUID, "versions": []any{map[string]any{"id": "canonical-version", "number": "1", "status": "review"}}}}}
}
func TestSubmitBindsCanonicalVersionToAcknowledgedResponse(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mutate        func(map[string]any)
		extra         string
		legacy, valid bool
	}{
		{"different receipt and canonical IDs", nil, "", false, true},
		{"normalized full version number", func(p map[string]any) {
			p["variants"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)["version_number"] = "1.0.0.0"
		}, "", false, true},
		{"unrelated global version ignored", nil, "\n9:[{\"id\":\"unrelated-version\",\"number\":\"1\",\"status\":\"review\"}]", false, true},
		{"wrong product", func(p map[string]any) { p["id"] = "other-product" }, "", false, false},
		{"malformed product identity", func(p map[string]any) { p["id"] = "" }, "", false, false},
		{"explicit wrong product owner", func(p map[string]any) {
			p["variants"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)["product_id"] = "other-product"
		}, "", false, false},
		{"explicit wrong variant owner", func(p map[string]any) {
			p["variants"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)["variant_id"] = "other-variant"
		}, "", false, false},
		{"explicit unknown variant owner", func(p map[string]any) {
			p["variants"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)["variant_id"] = nil
		}, "", false, false},
		{"explicit matching owners", func(p map[string]any) {
			v := p["variants"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)
			v["product_id"] = p["id"]
			v["variant_id"] = p["variants"].([]any)[0].(map[string]any)["id"]
		}, "", false, true},
		{"wrong variant", func(p map[string]any) { p["variants"].([]any)[0].(map[string]any)["id"] = "other-variant" }, "", false, false},
		{"wrong UUID", func(p map[string]any) {
			p["variants"].([]any)[0].(map[string]any)["streamdeckid"] = "com.example.other"
		}, "", false, false},
		{"missing UUID", func(p map[string]any) { p["variants"].([]any)[0].(map[string]any)["streamdeckid"] = nil }, "", false, false},
		{"duplicate matching versions", func(p map[string]any) {
			v := p["variants"].([]any)[0].(map[string]any)
			v["versions"] = append(v["versions"].([]any), map[string]any{"id": "second-version", "number": "1.0", "status": "review"})
		}, "", false, false},
		{"duplicate same version row", func(p map[string]any) {
			v := p["variants"].([]any)[0].(map[string]any)
			v["versions"] = append(v["versions"].([]any), v["versions"].([]any)[0])
		}, "", false, false},
		{"different version", func(p map[string]any) {
			p["variants"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)["number"] = "1.0.1"
		}, "", false, false},
		{"missing status", func(p map[string]any) {
			delete(p["variants"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any), "status")
		}, "", false, false},
		{"conflicting version fields", func(p map[string]any) {
			p["variants"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)["version_number"] = "2"
		}, "", false, false},
		{"malformed versions", func(p map[string]any) { p["variants"].([]any)[0].(map[string]any)["versions"] = "unsupported" }, "", false, false},
		{"malformed variants", func(p map[string]any) { p["variants"] = nil }, "", false, false},
		{"duplicate target variants", func(p map[string]any) { p["variants"] = append(p["variants"].([]any), p["variants"].([]any)[0]) }, "", false, false},
		{"legacy ID only", nil, "", true, true},
		{"legacy unowned global version ignored", nil, "\n9:[{\"id\":\"unowned-version\",\"number\":\"1\",\"status\":\"review\"}]", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, submission := syntheticSubmission()
			submission.Package.Manifest.Version = "1.0.0"
			submission.Upload.Manifest.Version = "1.0.0"
			product := syntheticSubmissionSnapshot(target)
			if tc.mutate != nil {
				tc.mutate(product)
			}
			snapshot, _ := json.Marshal(product)
			response := "1:{\"success\":true,\"id\":\"opaque-receipt\",\"message\":\"Synthetic acknowledgement\"}\n"
			if !tc.legacy {
				response += "2:{\"product\":\"$3\"}\n3:" + string(snapshot) + "\n"
			}
			response += tc.extra
			creates, validations := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					switch r.URL.Path {
					case "/api/upload-aux/manifest/folder":
						json.NewEncoder(w).Encode(submission.Upload.Manifest)
					case "/_next/static/chunk.js":
						fmt.Fprint(w, fixtureActions("a"))
					default:
						fmt.Fprint(w, fixturePage())
					}
					return
				}
				if strings.HasSuffix(r.Header.Get("Next-Action"), "1") {
					validations++
					fmt.Fprint(w, "1:{\"is_valid\":true}\n")
					return
				}
				creates++
				fmt.Fprint(w, response)
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			result, err := client.Submit(context.Background(), target, submission)
			if creates != 1 || validations != 1 {
				t.Fatalf("creates=%d validations=%d", creates, validations)
			}
			if result.ReceiptID != "opaque-receipt" {
				t.Fatalf("receipt not preserved: %+v", result)
			}
			if tc.valid {
				want := "canonical-version"
				if tc.legacy {
					want = "opaque-receipt"
				}
				if err != nil || result.ID != want {
					t.Fatalf("result=%+v error=%v", result, err)
				}
			} else {
				var ambiguous *AmbiguousSubmissionError
				var notSubmitted *NotSubmittedError
				if !errors.As(err, &ambiguous) || errors.As(err, &notSubmitted) || result.ID != "opaque-receipt" {
					t.Fatalf("unsafe acknowledged outcome: result=%+v error=%v", result, err)
				}
			}
		})
	}
}
func TestSubmissionSnapshotRejectsConflictingProductSnapshots(t *testing.T) {
	target, _ := syntheticSubmission()
	first := syntheticSubmissionSnapshot(target)
	second := syntheticSubmissionSnapshot(target)
	second["variants"].([]any)[0].(map[string]any)["versions"].([]any)[0].(map[string]any)["id"] = "conflicting-canonical"
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if _, snapshot, err := submissionVersionFromResponse([]byte("2:"+string(a)+"\n3:"+string(b)+"\n"), target, "1.0.0"); err == nil || !snapshot {
		t.Fatal("conflicting target snapshot accepted")
	}
}
