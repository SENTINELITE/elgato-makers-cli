package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	syntheticCICommit    = "1234567890abcdef1234567890abcdef12345678"
	syntheticCIPolicy    = "_policy-one"
	syntheticCIArtifact  = "-artifact-one"
	syntheticCIOperation = "_operation-one"
)

func mcpCITestRunner(t *testing.T, handler func(http.ResponseWriter, *http.Request, string)) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	origin := "http://" + srv.Listener.Addr().String()
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, origin)
	})
	srv.Start()
	t.Cleanup(srv.Close)
	base := http.DefaultTransport
	http.DefaultTransport = deliveryOIDCTransport{base: base, origin: srv.URL}
	t.Cleanup(func() { http.DefaultTransport = base })
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://token.actions.githubusercontent.com/oidc")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "fixture-runner-credential")
	return srv.URL
}

func mcpCITestOIDC(t *testing.T, w http.ResponseWriter, r *http.Request, origin string) {
	t.Helper()
	if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-runner-credential" || r.URL.Query().Get("audience") != origin {
		t.Error("request did not bind OIDC to hosted origin")
	}
	fmt.Fprint(w, `{"value":"fixture-signed-github-token"}`)
}

func mcpCITestGrant(t *testing.T, w http.ResponseWriter, r *http.Request, scope string) {
	t.Helper()
	var input map[string]string
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || json.NewDecoder(r.Body).Decode(&input) != nil ||
		len(input) != 2 || input["policy_id"] != syntheticCIPolicy || input["github_token"] != "fixture-signed-github-token" {
		t.Error("incorrect CI scoped token exchange")
	}
	json.NewEncoder(w).Encode(mcpCIGrant{Access: "fixture-scoped-token", Policy: syntheticCIPolicy, Scope: scope, SHA: syntheticCICommit})
}

func mcpCITestArtifact(t *testing.T, w http.ResponseWriter, r *http.Request, expected mcpLocalArtifact) {
	t.Helper()
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fixture-scoped-token" {
		t.Error("private file upload used incorrect auth or method")
	}
	reader, err := r.MultipartReader()
	if err != nil {
		t.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	first, err := reader.NextPart()
	if err != nil {
		t.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	kind, _ := io.ReadAll(first)
	if first.FormName() != "kind" || string(kind) != "package" {
		t.Error("private file upload did not send package kind first")
	}
	part, err := reader.NextPart()
	if err != nil {
		t.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	hash := sha256.New()
	size, err := io.Copy(hash, part)
	if err != nil || part.FormName() != "file" || size != expected.Size || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		t.Error("the uploaded bytes differ from the local package")
	}
	json.NewEncoder(w).Encode(mcpLocalArtifact{ID: syntheticCIArtifact, SHA256: expected.SHA256, Size: expected.Size})
}

func TestMCPCIPreflightUsesDedicatedRoutesAndBindsPackage(t *testing.T) {
	for _, tc := range []struct {
		name, state, review, notes                 string
		mismatchedHash, omitsMarker, reportsWrites bool
	}{
		{name: "explicit exact notes", state: "ready", review: "clear", notes: "Milestone updates.\r\n\r\n- Improved the dial.\r\n"},
		{name: "policy notes and review pending", state: "queued_for_review", review: "in_review"},
		{name: "mismatched package result", state: "ready", review: "clear", mismatchedHash: true},
		{name: "missing provider writes marker", state: "ready", review: "clear", omitsMarker: true},
		{name: "provider writes reported", state: "ready", review: "clear", reportsWrites: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg, pkg, notes := fixture(t)
			file, err := mcpFileIdentity(pkg)
			if err != nil {
				t.Fatal(err)
			}
			calls := make(chan string, 8)
			origin := mcpCITestRunner(t, func(w http.ResponseWriter, r *http.Request, origin string) {
				calls <- r.URL.Path
				switch r.URL.Path {
				case "/oidc":
					mcpCITestOIDC(t, w, r, origin)
				case "/v1/ci/preflight/token":
					mcpCITestGrant(t, w, r, "ci:preflight")
				case "/v1/files":
					mcpCITestArtifact(t, w, r, file)
				case "/v1/ci/preflight":
					var req mcpCIRequest
					if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fixture-scoped-token" ||
						json.NewDecoder(r.Body).Decode(&req) != nil || req.Artifact != syntheticCIArtifact || req.PackageSHA != file.SHA256 || req.UploadMode != "" {
						t.Error("incorrect CI preflight validation request")
					}
					if tc.notes == "" && req.Notes != nil || tc.notes != "" && (req.Notes == nil || *req.Notes != tc.notes) {
						t.Error("CLI changed explicit notes or supplied an unapproved default")
					}
					pkgHash := file.SHA256
					if tc.mismatchedHash {
						pkgHash = strings.Repeat("a", 64)
					}
					result := map[string]any{
						"state": tc.state, "policy_id": syntheticCIPolicy, "sha": syntheticCICommit,
						"package_sha256": pkgHash, "package_version": "1.0.0.5", "review_state": tc.review,
					}
					if !tc.omitsMarker {
						result["provider_writes"] = tc.reportsWrites
					}
					json.NewEncoder(w).Encode(result)
				default:
					t.Errorf("unexpected preflight endpoint %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			args := []string{"--config", cfg, "--json", "mcp", "ci", "preflight", "--file", pkg, "--policy", syntheticCIPolicy, "--server", origin}
			if tc.notes != "" {
				if err := os.WriteFile(notes, []byte(tc.notes), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--notes", notes)
			}
			out, stderr, err := command(t, Options{}, args...)
			if tc.mismatchedHash || tc.omitsMarker || tc.reportsWrites {
				if err == nil || out != "" {
					t.Fatalf("invalid preflight receipt accepted: %s %v", out, err)
				}
			} else {
				var result mcpCIPreflightResult
				if err != nil || json.Unmarshal([]byte(out), &result) != nil || result.State != tc.state || result.PackageSHA != file.SHA256 || result.SHA != syntheticCICommit || result.Policy != syntheticCIPolicy {
					t.Fatalf("preflight result: %s %s %v", out, stderr, err)
				}
				if strings.Contains(out, "operation_id") || strings.Contains(out, "access_token") || !strings.Contains(out, `"provider_writes": false`) {
					t.Fatal("preflight output did not explicitly report a non-mutating check")
				}
			}
			mcpCITestCalls(t, calls, "/oidc", "/v1/ci/preflight/token", "/v1/files", "/v1/ci/preflight")
		})
	}
}

func TestMCPCIReleaseServerMode(t *testing.T) {
	for _, tc := range []struct {
		name, initial, outcome, code, notes string
		status, pollStatus                  int
	}{
		{name: "queue receipt ends runner", initial: "queued", outcome: "queued", code: "CI_REVIEW_PENDING"},
		{name: "initial deployment uses status GET only", initial: "running", outcome: "succeeded", notes: "Improved the dial.\r\n"},
		{name: "uncertain outcome requires reconciliation", initial: "running", outcome: "reconciliation_required", code: "RELEASE_SUBMISSION_OUTCOME_UNCERTAIN"},
		{name: "revoked status retains accepted operation", initial: "running", outcome: "running", pollStatus: http.StatusUnauthorized},
		{name: "missing approved default rejects omitted notes", status: http.StatusBadRequest},
		{name: "denied release does not retry", status: http.StatusConflict},
		{name: "unconfirmed release does not retry", status: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg, pkg, notes := fixture(t)
			file, err := mcpFileIdentity(pkg)
			if err != nil {
				t.Fatal(err)
			}
			calls := make(chan string, 8)
			origin := mcpCITestRunner(t, func(w http.ResponseWriter, r *http.Request, origin string) {
				calls <- r.URL.Path
				switch r.URL.Path {
				case "/oidc":
					mcpCITestOIDC(t, w, r, origin)
				case "/v1/ci/token":
					mcpCITestGrant(t, w, r, "ci:release")
				case "/v1/files":
					mcpCITestArtifact(t, w, r, file)
				case "/v1/ci/releases":
					var req mcpCIRequest
					if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fixture-scoped-token" ||
						json.NewDecoder(r.Body).Decode(&req) != nil || req.Artifact != syntheticCIArtifact || req.PackageSHA != file.SHA256 || req.UploadMode != "server" {
						t.Error("incorrect server release request")
					}
					if tc.notes == "" && req.Notes != nil || tc.notes != "" && (req.Notes == nil || *req.Notes != tc.notes) {
						t.Error("release changed explicit notes or supplied an unapproved default")
					}
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						fmt.Fprint(w, `{"error":"fixture-private-host-detail"}`)
						return
					}
					result := mcpCIReleaseResult{State: tc.initial, Policy: syntheticCIPolicy, SHA: syntheticCICommit, Operation: syntheticCIOperation}
					if tc.initial == "queued" {
						result.ErrorCode = tc.code
					}
					json.NewEncoder(w).Encode(result)
				case "/v1/ci/operations/" + syntheticCIOperation:
					if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-scoped-token" {
						t.Error("status poll used incorrect auth or method")
					}
					if tc.pollStatus != 0 {
						w.WriteHeader(tc.pollStatus)
						return
					}
					json.NewEncoder(w).Encode(mcpCIReleaseResult{State: tc.outcome, Operation: syntheticCIOperation, Workflow: "-workflow-one", ErrorCode: tc.code})
				default:
					t.Errorf("server release touched unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			args := []string{"--config", cfg, "--json", "mcp", "ci", "release", "--file", pkg, "--policy", syntheticCIPolicy, "--server", origin}
			if tc.notes != "" {
				if err := os.WriteFile(notes, []byte(tc.notes), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--notes", notes)
			}
			out, stderr, err := command(t, Options{}, args...)
			wantErr := tc.status != 0 || tc.pollStatus != 0 || tc.outcome == "reconciliation_required"
			if (err != nil) != wantErr {
				t.Fatalf("unexpected release exit: %s %s %v", out, stderr, err)
			}
			if tc.status == 0 {
				var result mcpCIReleaseResult
				if json.Unmarshal([]byte(out), &result) != nil || result.State != tc.outcome || result.Operation != syntheticCIOperation ||
					result.Policy != syntheticCIPolicy || result.SHA != syntheticCICommit || result.ErrorCode != tc.code {
					t.Fatalf("unexpected release receipt: %s", out)
				}
			}
			for _, secret := range []string{"fixture-scoped-token", "fixture-signed-github-token", "fixture-private-host-detail", pkg} {
				if strings.Contains(out+stderr+fmt.Sprint(err), secret) {
					t.Fatal("release surfaced confidential data")
				}
			}
			expected := []string{"/oidc", "/v1/ci/token", "/v1/files", "/v1/ci/releases"}
			if tc.initial == "running" {
				expected = append(expected, "/v1/ci/operations/"+syntheticCIOperation)
			}
			mcpCITestCalls(t, calls, expected...)
		})
	}
}

func TestMCPCIPreflightRejectsReleaseGrantAndInvalidExplicitNotes(t *testing.T) {
	dir, cfg, pkg, _ := fixture(t)
	calls := make(chan string, 8)
	origin := mcpCITestRunner(t, func(w http.ResponseWriter, r *http.Request, origin string) {
		calls <- r.URL.Path
		switch r.URL.Path {
		case "/oidc":
			mcpCITestOIDC(t, w, r, origin)
		case "/v1/ci/preflight/token":
			mcpCITestGrant(t, w, r, "ci:release")
		default:
			t.Errorf("preflight used a release grant at %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	invalidNotes := filepath.Join(dir, "invalid.md")
	if err := os.WriteFile(invalidNotes, []byte("<div>Unverified raw HTML</div>\n"), 0600); err != nil {
		t.Fatal(err)
	}
	emptyNotes := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(emptyNotes, nil, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--config", cfg, "--json", "mcp", "ci", "preflight", "--file", pkg, "--policy", syntheticCIPolicy, "--server", origin}
	for _, notes := range []string{"", filepath.Join(dir, "missing.md"), invalidNotes, emptyNotes} {
		if _, _, err := command(t, Options{}, append(args, "--notes", notes)...); err == nil {
			t.Fatal("invalid explicit notes fell back to the approved default")
		}
		mcpCITestCalls(t, calls)
	}
	if _, _, err := command(t, Options{}, args...); err == nil {
		t.Fatal("preflight accepted a release grant")
	}
	mcpCITestCalls(t, calls, "/oidc", "/v1/ci/preflight/token")
}

func mcpCITestCalls(t *testing.T, calls <-chan string, expected ...string) {
	t.Helper()
	for _, want := range expected {
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("unexpected request %s, want %s", got, want)
			}
		default:
			t.Fatalf("missing request %s", want)
		}
	}
	select {
	case got := <-calls:
		t.Fatalf("unexpected extra request %s", got)
	default:
	}
}
