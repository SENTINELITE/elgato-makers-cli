package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
)

type receiptFixtureAPI struct{}

func (receiptFixtureAPI) Versions(context.Context, model.Target) ([]model.Version, error) {
	return []model.Version{}, nil
}
func (receiptFixtureAPI) Upload(_ context.Context, _ model.Target, p model.Package) (model.UploadResult, error) {
	return model.UploadResult{FileID: "synthetic-file", FolderID: "synthetic-folder", Manifest: p.Manifest}, nil
}
func (receiptFixtureAPI) Submit(context.Context, model.Target, model.Submission) (model.SubmitResult, error) {
	return model.SubmitResult{ID: "opaque-receipt", ReceiptID: "opaque-receipt"}, errors.New("synthetic uncertain response")
}
func receiptRecoveryFixture(t *testing.T) (string, string, string, release.Plan) {
	t.Helper()
	dir, cfg, pkg, notes := fixture(t)
	state := filepath.Join(dir, "releases")
	m := release.NewManager(state, receiptFixtureAPI{})
	p, err := m.Prepare(context.Background(), model.Target{OrgID: "org", ProductID: "product", VariantID: "variant", UUID: "com.example.demo", Name: "Demo"}, pkg, notes, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err = m.Upload(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err = m.Submit(context.Background(), p.ID, release.ConfirmOptions{Yes: true})
	if err == nil || p.State != release.SubmissionUnknown || p.SubmissionID != "opaque-receipt" {
		t.Fatalf("fixture not uncertain: %+v %v", p, err)
	}
	return dir, cfg, state, p
}
func TestRecoverReceiptValidatesBeforeCredentialsOrNetwork(t *testing.T) {
	dir, cfg, state, p := receiptRecoveryFixture(t)
	before, err := os.ReadFile(filepath.Join(state, p.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{nil, {"--receipt-id", "opaque-receipt"}, {"--version-id", "canonical-version"}, {"--receipt-id", "wrong", "--version-id", "canonical-version"}, {"--receipt-id", "opaque-receipt", "--version-id", "bad/path"}, {"--receipt-id", "opaque-receipt", "--version-id", "canonical-version", "--yes"}} {
		args := []string{"--config", cfg, "--state-dir", state, "--credentials-file", filepath.Join(dir, "missing"), "release", "recover-receipt", p.ID}
		args = append(args, flags...)
		_, _, err := command(t, Options{ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
			t.Fatal("invalid recovery reached client")
			return nil, nil
		}}, args...)
		if err == nil {
			t.Fatalf("invalid flags accepted: %v", flags)
		}
		after, _ := os.ReadFile(filepath.Join(state, p.ID+".json"))
		if string(before) != string(after) {
			t.Fatal("invalid command rewrote plan")
		}
	}
}
func TestRecoverReceiptUsesOnlyReadOnlyVersionProof(t *testing.T) {
	for _, kind := range []string{"matching", "wrong version", "wrong notes", "wrong auto publish", "wrong variant", "wrong canonical ID", "wrong review status", "wrong UUID"} {
		t.Run(kind, func(t *testing.T) {
			dir, cfg, state, p := receiptRecoveryFixture(t)
			credentials := listingCredentials(t, dir)
			before, _ := os.ReadFile(filepath.Join(state, p.ID+".json"))
			version := map[string]any{"id": "canonical-version", "version_number": p.Package.Manifest.Version, "release_notes": p.Notes.Markdown, "status": "REVIEW", "is_ready_for_publish": false}
			variant := map[string]any{"id": "variant", "streamdeckid": "com.example.demo", "versions": []any{version}}
			switch kind {
			case "wrong version":
				version["version_number"] = "99"
			case "wrong notes":
				version["release_notes"] = "Other"
			case "wrong auto publish":
				version["is_ready_for_publish"] = true
			case "wrong UUID":
				variant["streamdeckid"] = "other"
			case "wrong variant":
				variant["id"] = "other"
			case "wrong canonical ID":
				version["id"] = "other"
			case "wrong review status":
				version["status"] = "DRAFT"
			}
			reads, writes, clients := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					w.WriteHeader(405)
					return
				}
				reads++
				if r.URL.Path != "/org/products/product/versions" && r.URL.Path != "/org/products/product" {
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "product", "name": "Demo", "status": "DRAFT", "variants": []any{variant}})
			}))
			defer server.Close()
			options := Options{ClientFactory: func(cookies []*http.Cookie, readOnly bool) (*maker.Client, error) {
				clients++
				if !readOnly {
					t.Error("receipt recovery client allows writes")
				}
				for _, cookie := range cookies {
					cookie.Secure = false
				}
				return maker.New(maker.Config{BaseURL: server.URL, AllowLocalhost: true, HTTPClient: server.Client(), Cookies: cookies, ReadOnly: readOnly})
			}}
			out, stderr, err := command(t, options, "--config", cfg, "--state-dir", state, "--credentials-file", credentials, "--minify", "release", "recover-receipt", p.ID, "--receipt-id", "opaque-receipt", "--version-id", "canonical-version")
			if kind == "matching" {
				if err != nil {
					t.Fatalf("recovery: %v %s", err, stderr)
				}
				var got release.Plan
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatal(err)
				}
				if got.State != release.Submitted || got.SubmissionID != "canonical-version" || got.SubmissionReceiptID != "opaque-receipt" || !strings.Contains(stderr, "Success:") {
					t.Fatalf("unexpected result: %s %s", out, stderr)
				}
			} else {
				if err == nil {
					t.Fatal("mismatched remote proof accepted")
				}
				after, _ := os.ReadFile(filepath.Join(state, p.ID+".json"))
				if string(before) != string(after) {
					t.Fatal("mismatched proof rewrote plan")
				}
				if strings.Contains(stderr, "Success:") {
					t.Fatal("failure emitted success")
				}
			}
			if clients != 1 || reads < 1 || writes != 0 {
				t.Fatalf("requests clients=%d reads=%d writes=%d", clients, reads, writes)
			}
		})
	}
}
