package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
)

func TestMCPDeliveryListingUploadsAndPrivateMapping(t *testing.T) {
	_, _, config := listingFixture(t)
	uploads := 0
	prepared := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-write" || r.Header.Get("Cookie") != "" {
			t.Error("wrong isolated authorization")
		}
		if r.URL.Path == "/v1/files" {
			uploads++
			reader, e := r.MultipartReader()
			if e != nil {
				t.Error(e)
				w.WriteHeader(400)
				return
			}
			first, e := reader.NextPart()
			if e != nil || first.FormName() != "kind" {
				t.Error("kind must be first")
				return
			}
			kind, _ := io.ReadAll(first)
			second, e := reader.NextPart()
			if e != nil || second.FormName() != "file" {
				t.Error("file must be second")
				return
			}
			body, _ := io.ReadAll(second)
			sum := sha256.Sum256(body)
			if len(kind) == 0 {
				t.Error("empty kind")
			}
			json.NewEncoder(w).Encode(map[string]any{"artifact_id": fmt.Sprintf("artifact-%d", uploads), "sha256": hex.EncodeToString(sum[:]), "size": len(body)})
			return
		}
		if r.URL.Path == "/mcp" {
			var call struct {
				Params struct {
					Name string            `json:"name"`
					Args map[string]string `json:"arguments"`
				} `json:"params"`
			}
			json.NewDecoder(r.Body).Decode(&call)
			if call.Params.Name != "prepare_listing" {
				t.Error("wrong tool")
			}
			var input map[string]any
			json.Unmarshal([]byte(call.Params.Args["config_json"]), &input)
			if input["package"] != "artifact-1" || input["description"] != listingTestDescription || input["release_notes"] == nil || input["org_id"] != nil || input["description_file"] != nil {
				t.Error("local config not converted safely")
			}
			prepared = true
			fmt.Fprint(w, `{"result":{"structuredContent":{"data":{"workflow_id":"workflow-one"}}}}`)
			return
		}
		t.Errorf("unexpected route %s", r.URL.Path)
	}))
	defer srv.Close()
	a := mcpTestApp(t)
	result, e := a.mcpPrepareLocalListing(context.Background(), srv.URL, "synthetic-write", config)
	if e != nil {
		t.Fatal(e)
	}
	if uploads != 6 || !prepared || result["workflow_id"] != "workflow-one" {
		t.Fatal("incomplete listing upload")
	}
	mapping, e := a.loadDelivery(srv.URL, "workflow-one")
	if e != nil || len(mapping.Files) != 6 {
		t.Fatal("private mapping missing", e)
	}
	if !filesec.IsPrivateFile(a.deliveryPath(srv.URL, "workflow-one")) {
		t.Fatal("mapping not private")
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "path") || strings.Contains(string(raw), "synthetic-write") {
		t.Fatal("private local data exposed")
	}
}
func TestMCPDeliveryLeaseSuccessAndNoRetry(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			_, _, pkg, _ := fixture(t)
			file, e := mcpFileIdentity(pkg)
			if e != nil {
				t.Fatal(e)
			}
			file.ID = "artifact-one"
			puts, acks, polls := 0, 0, 0
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/storage":
					puts++
					if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
						t.Error("auth leaked to storage")
					}
					body, _ := io.ReadAll(r.Body)
					sum := sha256.Sum256(body)
					if hex.EncodeToString(sum[:]) != file.SHA256 {
						t.Error("wrong stored bytes")
					}
					if fail {
						w.WriteHeader(500)
					}
				case r.URL.Path == "/mcp":
					polls++
					state := "running"
					if acks > 0 {
						state = "succeeded"
					}
					fmt.Fprintf(w, `{"result":{"structuredContent":{"data":{"state":%q}}}}`, state)
				case strings.HasSuffix(r.URL.Path, "/uploads/next"):
					json.NewEncoder(w).Encode(mcpUploadLease{ID: "lease-one", Artifact: file.ID, SHA256: file.SHA256, Size: file.Size, URL: srv.URL + "/storage", Expires: time.Now().Add(time.Minute)})
				case strings.HasSuffix(r.URL.Path, "/complete"):
					acks++
					if r.Header.Get("Authorization") != "Bearer synthetic" {
						t.Error("missing broker authorization")
					}
					var ack struct {
						Success bool `json:"success"`
						Status  int  `json:"status"`
					}
					json.NewDecoder(r.Body).Decode(&ack)
					if ack.Success == fail || ack.Status == 0 {
						t.Error("wrong completion acknowledgement")
					}
					w.WriteHeader(204)
				default:
					t.Error("unexpected path")
				}
			}))
			defer srv.Close()
			result, e := mcpPollDelivery(context.Background(), srv.URL, "synthetic", "operation-one", map[string]mcpLocalArtifact{file.ID: file}, false)
			if fail && e == nil || !fail && e != nil {
				t.Fatalf("wrong outcome: %v", e)
			}
			if puts != 1 || acks != 1 {
				t.Fatalf("unexpected retries: puts=%d acks=%d", puts, acks)
			}
			if !fail && result["state"] != "succeeded" {
				t.Fatal("completion missing")
			}
			if polls > 2 {
				t.Fatal("unexpected repeated status")
			}
		})
	}
}
func TestMCPDeliveryRefusesChangedFilesAndForeignDestinations(t *testing.T) {
	_, _, pkg, _ := fixture(t)
	file, e := mcpFileIdentity(pkg)
	if e != nil {
		t.Fatal(e)
	}
	file.ID = "artifact-one"
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Location", "http://127.0.0.1:1/leak")
		w.WriteHeader(307)
	}))
	defer srv.Close()
	lease := mcpUploadLease{ID: "lease", Artifact: file.ID, SHA256: file.SHA256, Size: file.Size, URL: srv.URL, Expires: time.Now().Add(time.Minute)}
	if _, e = mcpPutLease(context.Background(), srv.URL, lease, file); e == nil || calls != 1 {
		t.Fatal("redirect accepted")
	}
	os.WriteFile(pkg, []byte("changed"), 0600)
	if _, e = mcpPutLease(context.Background(), srv.URL, lease, file); e == nil || calls != 1 {
		t.Fatal("changed file uploaded")
	}
	for _, raw := range []string{"https://evil.example/object", "https://other-bucket.s3.amazonaws.com/object", "https://s3.us-east-1.amazonaws.com/other/object", "https://s3.us-east-1.amazonaws.com/production.maker.elgato.com/../../evil", "https://user@s3.us-east-1.amazonaws.com/production.maker.elgato.com/media/id", "http://127.0.0.1:9000/object"} {
		if validDeliveryURL(raw, defaultMCPServer) {
			t.Fatal("foreign upload target accepted", raw)
		}
	}
	for _, raw := range []string{"https://s3.us-east-1.amazonaws.com/production.maker.elgato.com/org-123.streamDeckPlugin?X-Amz-Signature=synthetic", "https://s3.us-east-1.amazonaws.com/production.maker.elgato.com/media/org/product/12345678-1234-5123-8123-123456789abc.png?X-Amz-Signature=synthetic"} {
		if !validDeliveryURL(raw, defaultMCPServer) {
			t.Fatal("valid exact bucket refused")
		}
	}
}
func TestMCPDeliveryWriteConsentAndOIDCOrigin(t *testing.T) {
	a := mcpTestApp(t)
	auth := mcpAuth{Server: defaultMCPServer, ClientID: "client", AccessToken: "secret", ExpiresAt: time.Now().Add(time.Hour)}
	if e := a.saveMCP(auth); e != nil {
		t.Fatal(e)
	}
	if _, e := a.deliveryAuth(context.Background(), defaultMCPServer); e == nil {
		t.Fatal("legacy read grant escalated")
	}
	auth.Scope = "maker:read maker:write"
	a.saveMCP(auth)
	if token, e := a.deliveryAuth(context.Background(), defaultMCPServer); e != nil || token != "secret" {
		t.Fatal("explicit write grant refused", e)
	}
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "synthetic")
	for _, raw := range []string{"http://token.actions.githubusercontent.com/token", "https://actions.githubusercontent.com.evil.example/token", "https://evil.example/token", "https://user@token.actions.githubusercontent.com/token"} {
		t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", raw)
		if _, e := mcpGitHubToken(context.Background(), defaultMCPServer); e == nil {
			t.Fatal("untrusted OIDC endpoint accepted")
		}
	}
}
func TestMCPDeliveryWriteLoginScope(t *testing.T) {
	srv := fakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/register":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["scope"] != "maker:read maker:write" {
				t.Error("write registration scope missing")
			}
			fmt.Fprint(w, `{"client_id":"synthetic"}`)
		case "/oauth/token":
			fmt.Fprint(w, `{"access_token":"secret-access","refresh_token":"secret-refresh","token_type":"Bearer","expires_in":900,"scope":"maker:read maker:write"}`)
		default:
			http.NotFound(w, r)
		}
	})
	a := mcpTestApp(t)
	pr, pw := io.Pipe()
	a.o.Err = pw
	done := make(chan error, 1)
	go func() { done <- a.mcpLoginScope(context.Background(), srv.URL, true, true); pw.Close() }()
	scan := bufio.NewScanner(pr)
	if !scan.Scan() || !scan.Scan() {
		t.Fatal("missing authorization URL")
	}
	u, e := url.Parse(scan.Text())
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	if q.Get("scope") != "maker:read maker:write" {
		t.Fatal("write consent not requested")
	}
	resp, e := http.Get(q.Get("redirect_uri") + "?" + url.Values{"state": {q.Get("state")}, "code": {"synthetic"}}.Encode())
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	auth, e := a.loadMCP(srv.URL)
	if e != nil || !mcpScopeContains(auth.Scope, "maker:write") {
		t.Fatal("write scope not saved")
	}
	if strings.Contains(a.o.Out.(*bytes.Buffer).String(), "secret-") {
		t.Fatal("token printed")
	}
}

func TestMCPDeliveryAttemptGuardSurvivesInvocation(t *testing.T) {
	a := mcpTestApp(t)
	if e := a.guardDeliveryAttempt(defaultMCPServer, "operation", "upload"); e != nil {
		t.Fatal(e)
	}
	if e := a.guardDeliveryAttempt(defaultMCPServer, "operation", "upload"); e == nil {
		t.Fatal("attempt replay accepted")
	}
	if e := a.guardDeliveryAttempt(defaultMCPServer, "other-operation", "upload"); e != nil {
		t.Fatal("independent operation blocked")
	}
}

type deliveryOIDCTransport struct {
	base   http.RoundTripper
	origin string
}

func (t deliveryOIDCTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "token.actions.githubusercontent.com" {
		r = r.Clone(r.Context())
		u := *r.URL
		target, _ := url.Parse(t.origin)
		u.Scheme, u.Host = target.Scheme, target.Host
		r.URL = &u
	}
	return t.base.RoundTrip(r)
}
func TestMCPDeliveryCICommandSyntheticFlow(t *testing.T) {
	_, cfg, pkg, notes := fixture(t)
	var file mcpLocalArtifact
	posts, acks := 0, 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oidc":
			if r.Header.Get("Authorization") != "Bearer runner-request" || r.URL.Query().Get("audience") != srv.URL {
				t.Error("OIDC binding")
			}
			fmt.Fprint(w, `{"value":"synthetic-github-token"}`)
		case "/v1/ci/token":
			var input map[string]string
			json.NewDecoder(r.Body).Decode(&input)
			if input["policy_id"] != "policy-one" || input["github_token"] != "synthetic-github-token" {
				t.Error("policy exchange")
			}
			fmt.Fprint(w, `{"access_token":"synthetic-ci-token","scope":"ci:release","policy_id":"policy-one","sha":"`+syntheticCICommit+`"}`)
		case "/v1/files":
			if r.Header.Get("Authorization") != "Bearer synthetic-ci-token" {
				t.Error("wrong CI upload token")
			}
			reader, _ := r.MultipartReader()
			first, _ := reader.NextPart()
			kind, _ := io.ReadAll(first)
			if first.FormName() != "kind" || string(kind) != "package" {
				t.Error("CI package kind")
			}
			part, _ := reader.NextPart()
			body, _ := io.ReadAll(part)
			sum := sha256.Sum256(body)
			file = mcpLocalArtifact{ID: "ci-artifact", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
			json.NewEncoder(w).Encode(file)
		case "/v1/ci/releases":
			var input map[string]any
			json.NewDecoder(r.Body).Decode(&input)
			if r.Header.Get("Authorization") != "Bearer synthetic-ci-token" || input["package_artifact_id"] != "ci-artifact" || input["upload_mode"] != "direct" || input["release_notes"] == "" {
				t.Error("CI release contract")
			}
			fmt.Fprint(w, `{"workflow_id":"ci-workflow","operation_id":"ci-operation"}`)
		case "/v1/ci/operations/ci-operation":
			state := "running"
			if acks > 0 {
				state = "succeeded"
			}
			json.NewEncoder(w).Encode(map[string]string{"state": state})
		case "/v1/operations/ci-operation/uploads/next":
			json.NewEncoder(w).Encode(mcpUploadLease{ID: "ci-upload", Artifact: file.ID, SHA256: file.SHA256, Size: file.Size, URL: srv.URL + "/storage", Expires: time.Now().Add(time.Minute)})
		case "/storage":
			posts++
			if r.Header.Get("Authorization") != "" {
				t.Error("CI token leaked to storage")
			}
			io.Copy(io.Discard, r.Body)
		case "/v1/operations/ci-operation/uploads/ci-upload/complete":
			acks++
			if r.Header.Get("Authorization") != "Bearer synthetic-ci-token" {
				t.Error("CI ACK token")
			}
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected CI route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	base := http.DefaultTransport
	http.DefaultTransport = deliveryOIDCTransport{base: base, origin: srv.URL}
	defer func() { http.DefaultTransport = base }()
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://token.actions.githubusercontent.com/oidc")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "runner-request")
	out, stderr, e := command(t, Options{}, "--config", cfg, "--json", "mcp", "ci", "release", "--upload-mode", "direct", "--file", pkg, "--notes", notes, "--policy", "policy-one", "--server", srv.URL)
	if e != nil {
		t.Fatalf("synthetic CI command failed: %v %s", e, stderr)
	}
	if posts != 1 || acks != 1 || !strings.Contains(out, `"state": "succeeded"`) {
		t.Fatalf("CI delivery incomplete: %s posts=%d acks=%d", out, posts, acks)
	}
	for _, secret := range []string{"synthetic-ci-token", "synthetic-github-token", "runner-request", "/storage", pkg} {
		if strings.Contains(out+stderr, secret) {
			t.Fatal("CI output exposed confidential data")
		}
	}
}

func TestMCPDeliveryListingReusesThumbnailArtifact(t *testing.T) {
	_, _, config := listingFixture(t)
	rawConfig, e := os.ReadFile(config)
	if e != nil {
		t.Fatal(e)
	}
	var local map[string]any
	json.Unmarshal(rawConfig, &local)
	assets := local["assets"].(map[string]any)
	assets["gallery"].([]any)[0] = assets["thumbnail"]
	rawConfig, _ = json.Marshal(local)
	if e = os.WriteFile(config, rawConfig, 0600); e != nil {
		t.Fatal(e)
	}
	uploads := 0
	prepared := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-write" || r.Header.Get("Cookie") != "" {
			t.Error("wrong isolated authorization")
		}
		if r.URL.Path == "/v1/files" {
			uploads++
			reader, e := r.MultipartReader()
			if e != nil {
				t.Error(e)
				w.WriteHeader(400)
				return
			}
			first, e := reader.NextPart()
			if e != nil || first.FormName() != "kind" {
				t.Error("kind must be first")
				return
			}
			kind, _ := io.ReadAll(first)
			second, e := reader.NextPart()
			if e != nil || second.FormName() != "file" {
				t.Error("file must be second")
				return
			}
			body, _ := io.ReadAll(second)
			sum := sha256.Sum256(body)
			if len(kind) == 0 {
				t.Error("empty kind")
			}
			json.NewEncoder(w).Encode(map[string]any{"artifact_id": fmt.Sprintf("artifact-%d", uploads), "sha256": hex.EncodeToString(sum[:]), "size": len(body)})
			return
		}
		if r.URL.Path == "/mcp" {
			var call struct {
				Params struct {
					Name string            `json:"name"`
					Args map[string]string `json:"arguments"`
				} `json:"params"`
			}
			json.NewDecoder(r.Body).Decode(&call)
			if call.Params.Name != "prepare_listing" {
				t.Error("wrong tool")
			}
			var input map[string]any
			json.Unmarshal([]byte(call.Params.Args["config_json"]), &input)
			if input["package"] != "artifact-1" || input["description"] != listingTestDescription || input["release_notes"] == nil || input["org_id"] != nil || input["description_file"] != nil {
				t.Error("local config not converted safely")
			}
			remoteAssets := input["assets"].(map[string]any)
			if remoteAssets["gallery"].([]any)[0] != remoteAssets["thumbnail"] {
				t.Error("thumbnail/gallery ID reuse lost")
			}
			prepared = true
			fmt.Fprint(w, `{"result":{"structuredContent":{"data":{"workflow_id":"workflow-one"}}}}`)
			return
		}
		t.Errorf("unexpected route %s", r.URL.Path)
	}))
	defer srv.Close()
	a := mcpTestApp(t)
	result, e := a.mcpPrepareLocalListing(context.Background(), srv.URL, "synthetic-write", config)
	if e != nil {
		t.Fatal(e)
	}
	if uploads != 5 || !prepared || result["workflow_id"] != "workflow-one" {
		t.Fatal("incomplete listing upload")
	}
	mapping, e := a.loadDelivery(srv.URL, "workflow-one")
	if e != nil || len(mapping.Files) != 5 {
		t.Fatal("private mapping missing", e)
	}
	if !filesec.IsPrivateFile(a.deliveryPath(srv.URL, "workflow-one")) {
		t.Fatal("mapping not private")
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "path") || strings.Contains(string(raw), "synthetic-write") {
		t.Fatal("private local data exposed")
	}
}

func TestMCPDeliveryExecuteChecksFilesBeforeDispatch(t *testing.T) {
	_, _, pkg, _ := fixture(t)
	file, e := mcpFileIdentity(pkg)
	if e != nil {
		t.Fatal(e)
	}
	file.ID = "artifact-one"
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		t.Error("changed mapped artifact reached hosted execution")
		w.WriteHeader(500)
	}))
	defer srv.Close()
	a := mcpTestApp(t)
	if e = a.saveMCP(mcpAuth{Server: srv.URL, Scope: "maker:read maker:write", ClientID: "client", AccessToken: "synthetic", ExpiresAt: time.Now().Add(time.Hour)}); e != nil {
		t.Fatal(e)
	}
	if e = a.saveDelivery(mcpDeliveryMap{Server: srv.URL, Workflow: "workflow", Files: map[string]mcpLocalArtifact{file.ID: file}}); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(pkg, []byte("changed"), 0600); e != nil {
		t.Fatal(e)
	}
	_, _, e = command(t, Options{}, "--config", a.configPath, "mcp", "action", "execute", "--workflow", "workflow", "--operation", "operation", "--server", srv.URL)
	if e == nil || calls != 0 {
		t.Fatal("execution did not fail before hosted writes")
	}
}

func TestMCPDeliveryCIChangedPackageStopsBeforeRelease(t *testing.T) {
	_, cfg, pkg, notes := fixture(t)
	var file mcpLocalArtifact
	posts, acks := 0, 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oidc":
			if r.Header.Get("Authorization") != "Bearer runner-request" || r.URL.Query().Get("audience") != srv.URL {
				t.Error("OIDC binding")
			}
			fmt.Fprint(w, `{"value":"synthetic-github-token"}`)
		case "/v1/ci/token":
			var input map[string]string
			json.NewDecoder(r.Body).Decode(&input)
			if input["policy_id"] != "policy-one" || input["github_token"] != "synthetic-github-token" {
				t.Error("policy exchange")
			}
			fmt.Fprint(w, `{"access_token":"synthetic-ci-token","scope":"ci:release","policy_id":"policy-one","sha":"`+syntheticCICommit+`"}`)
		case "/v1/files":
			if r.Header.Get("Authorization") != "Bearer synthetic-ci-token" {
				t.Error("wrong CI upload token")
			}
			reader, _ := r.MultipartReader()
			first, _ := reader.NextPart()
			kind, _ := io.ReadAll(first)
			if first.FormName() != "kind" || string(kind) != "package" {
				t.Error("CI package kind")
			}
			part, _ := reader.NextPart()
			body, _ := io.ReadAll(part)
			sum := sha256.Sum256(body)
			file = mcpLocalArtifact{ID: "ci-artifact", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
			if e := os.WriteFile(pkg, []byte("changed after hosted upload"), 0600); e != nil {
				t.Error(e)
			}
			json.NewEncoder(w).Encode(file)
		case "/v1/ci/releases":
			t.Error("changed package reached hosted release creation")
			var input map[string]any
			json.NewDecoder(r.Body).Decode(&input)
			if r.Header.Get("Authorization") != "Bearer synthetic-ci-token" || input["package_artifact_id"] != "ci-artifact" || input["upload_mode"] != "direct" || input["release_notes"] == "" {
				t.Error("CI release contract")
			}
			fmt.Fprint(w, `{"workflow_id":"ci-workflow","operation_id":"ci-operation"}`)
		case "/v1/ci/operations/ci-operation":
			state := "running"
			if acks > 0 {
				state = "succeeded"
			}
			json.NewEncoder(w).Encode(map[string]string{"state": state})
		case "/v1/operations/ci-operation/uploads/next":
			json.NewEncoder(w).Encode(mcpUploadLease{ID: "ci-upload", Artifact: file.ID, SHA256: file.SHA256, Size: file.Size, URL: srv.URL + "/storage", Expires: time.Now().Add(time.Minute)})
		case "/storage":
			posts++
			if r.Header.Get("Authorization") != "" {
				t.Error("CI token leaked to storage")
			}
			io.Copy(io.Discard, r.Body)
		case "/v1/operations/ci-operation/uploads/ci-upload/complete":
			acks++
			if r.Header.Get("Authorization") != "Bearer synthetic-ci-token" {
				t.Error("CI ACK token")
			}
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected CI route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	base := http.DefaultTransport
	http.DefaultTransport = deliveryOIDCTransport{base: base, origin: srv.URL}
	defer func() { http.DefaultTransport = base }()
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "https://token.actions.githubusercontent.com/oidc")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "runner-request")
	out, stderr, e := command(t, Options{}, "--config", cfg, "--json", "mcp", "ci", "release", "--upload-mode", "direct", "--file", pkg, "--notes", notes, "--policy", "policy-one", "--server", srv.URL)
	if e == nil {
		t.Fatal("changed CI package reached release creation")
	}
	if posts != 0 || acks != 0 {
		t.Fatalf("CI delivery incomplete: %s posts=%d acks=%d", out, posts, acks)
	}
	for _, secret := range []string{"synthetic-ci-token", "synthetic-github-token", "runner-request", "/storage", pkg} {
		if strings.Contains(out+stderr, secret) {
			t.Fatal("CI output exposed confidential data")
		}
	}
}

func TestMCPDeliveryOpaqueIDs(t *testing.T) {
	for _, id := range []string{"legacy.workflow-1", "-artifact_0123456789abcdef", "_operation-0123456789abcdef"} {
		if !deliveryID.MatchString(id) {
			t.Errorf("rejected valid hosted identifier %q", id)
		}
	}
	for _, id := range []string{"", ".", "..", ".hidden", "_/operation", "-\\artifact", "%2e%2e", "artifact?query", "artifact#fragment", "artifact space", "artifact\n", strings.Repeat("a", 129)} {
		if deliveryID.MatchString(id) {
			t.Errorf("accepted invalid hosted identifier %q", id)
		}
	}
}

func TestMCPDeliveryProductionMediaPaths(t *testing.T) {
	const base = "https://s3.us-east-1.amazonaws.com/production.maker.elgato.com/"
	const id = "12345678-1234-5123-8123-123456789abc"
	for name, path := range map[string]string{
		"icon":      "media/org/product/" + id + ".png",
		"thumbnail": "media/org/product/" + id + ".jpg",
		"jpeg":      "media/org/product/" + id + ".jpeg",
		"gallery":   "media/org/product/variant/" + id + ".png",
		"video":     "media/org/product/variant/" + id + ".mp4",
	} {
		t.Run(name, func(t *testing.T) {
			if !validDeliveryURL(base+path+"?X-Amz-Signature=synthetic", defaultMCPServer) {
				t.Fatal("real-shaped production media grant rejected")
			}
		})
	}
	for _, path := range []string{
		"media/" + id + ".png", "media/org/" + id + ".png", "media/org/product/variant/extra/" + id + ".png",
		"media//product/" + id + ".png", "media/org/product//" + id + ".png", "media/org/product/" + id + ".png/",
		"media/-org/product/" + id + ".png", "media/org/_product/" + id + ".png",
		"media/org/../" + id + ".png", "media/org/%2e%2e/" + id + ".png", "media/org/%252e%252e/" + id + ".png",
		"media/org/product%2fvariant/" + id + ".png", "media/org/product/%2F" + id + ".png",
		"media/org/product/%5c" + id + ".png", "media/org/product/..%5c" + id + ".png",
		"media/org/product/" + id + ".gif", "media/org/product/not-a-uuid.png", "media/org/product/" + id + ".png.exe",
		"media/org/product/" + id + ".png#fragment", "media/org/product/" + id + ".png%00",
	} {
		if validDeliveryURL(base+path, defaultMCPServer) {
			t.Fatal("invalid production media path accepted", path)
		}
	}
	// Package guards remain unchanged.
	if !validDeliveryURL(base+"org-123.streamDeckPlugin?X-Amz-Signature=synthetic", defaultMCPServer) {
		t.Fatal("package guard changed")
	}
}

func TestMCPLegacyReadGrantRefreshScopeCompatibility(t *testing.T) {
	for _, scope := range []string{"maker:read", "maker:read maker:write"} {
		t.Run(scope, func(t *testing.T) {
			srv := fakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/oauth/token":
					if e := r.ParseForm(); e != nil {
						t.Error(e)
					}
					if r.Form.Get("scope") != "" {
						t.Error("refresh requested scope escalation")
					}
					json.NewEncoder(w).Encode(map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "token_type": "Bearer", "expires_in": 900, "scope": scope})
				case "/mcp":
					if scope != "maker:read" {
						t.Error("broadened refresh reached status")
					}
					fmt.Fprint(w, `{"result":{"structuredContent":{"data":{"state":"connected","org_id":"synthetic-org"}}}}`)
				default:
					http.NotFound(w, r)
				}
			})
			a := mcpTestApp(t)
			legacy := mcpAuth{Server: srv.URL, ClientID: "legacy-client", AccessToken: "expired-access", RefreshToken: "legacy-refresh", ExpiresAt: time.Now().Add(-time.Hour)}
			if e := a.saveMCP(legacy); e != nil {
				t.Fatal(e)
			}
			before, e := a.loadMCP(srv.URL)
			if e != nil || before.Scope != "maker:read" || mcpScopeContains(before.Scope, "maker:write") {
				t.Fatal("legacy grant not normalized read-only", e)
			}
			e = a.mcpStatus(context.Background(), srv.URL)
			after, loadErr := a.loadMCP(srv.URL)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if scope == "maker:read" {
				if e != nil || after.AccessToken != "rotated-access" || after.RefreshToken != "rotated-refresh" || after.Scope != "maker:read" {
					t.Fatal("legacy read refresh failed", e)
				}
				if e = a.checkMCPRefreshGuard(srv.URL); e != nil {
					t.Fatal("successful refresh left uncertainty guard")
				}
			} else {
				if e == nil || after.AccessToken != "expired-access" || mcpScopeContains(after.Scope, "maker:write") {
					t.Fatal("legacy read grant escalated")
				}
				if e = a.checkMCPRefreshGuard(srv.URL); e == nil {
					t.Fatal("rejected refresh lost uncertainty guard")
				}
			}
		})
	}
}

func TestMCPDeliveryRefreshesDuringLeaseAndACK(t *testing.T) {
	_, _, pkg, _ := fixture(t)
	file, e := mcpFileIdentity(pkg)
	if e != nil {
		t.Fatal(e)
	}
	file.ID = "artifact-one"
	a := mcpTestApp(t)
	base := time.Now()
	var advance atomic.Int64
	a.o.Now = func() time.Time { return base.Add(time.Duration(advance.Load()) * time.Second) }
	refreshes, puts, acks := 0, 0, 0
	var srv *httptest.Server
	srv = fakeMCP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			refreshes++
			r.ParseForm()
			expected := "initial-refresh"
			if refreshes > 1 {
				expected = "refresh-1"
			}
			if r.Form.Get("refresh_token") != expected || r.Form.Get("client_id") != "same-client" || r.Form.Get("scope") != "" {
				t.Error("refresh changed grant/client/scope")
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("access-%d", refreshes), "refresh_token": fmt.Sprintf("refresh-%d", refreshes), "token_type": "Bearer", "expires_in": 900, "scope": "maker:read maker:write"})
		case "/mcp":
			state := "running"
			expected := "initial-access"
			if acks > 0 {
				state = "succeeded"
				expected = "access-2"
			}
			if r.Header.Get("Authorization") != "Bearer "+expected {
				t.Error("status used expired token")
			}
			fmt.Fprintf(w, `{"result":{"structuredContent":{"data":{"state":%q}}}}`, state)
			if acks == 0 {
				advance.Store(3600)
			}
		case "/v1/operations/operation/uploads/next":
			if r.Header.Get("Authorization") != "Bearer access-1" {
				t.Error("lease used expired token")
			}
			json.NewEncoder(w).Encode(mcpUploadLease{ID: "lease", Artifact: file.ID, SHA256: file.SHA256, Size: file.Size, URL: srv.URL + "/storage", Expires: time.Now().Add(time.Minute)})
		case "/storage":
			puts++
			if r.Header.Get("Authorization") != "" {
				t.Error("OAuth token reached storage")
			}
			advance.Store(4500)
		case "/v1/operations/operation/uploads/lease/complete":
			acks++
			if r.Header.Get("Authorization") != "Bearer access-2" {
				t.Error("ACK used expired token")
			}
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected refresh fixture request: %s", r.URL.Path)
		}
	})
	if e = a.saveMCP(mcpAuth{Server: srv.URL, ClientID: "same-client", Scope: "maker:read maker:write", AccessToken: "initial-access", RefreshToken: "initial-refresh", ExpiresAt: base.Add(time.Hour)}); e != nil {
		t.Fatal(e)
	}
	result, e := mcpPollDeliveryAuthorized(context.Background(), srv.URL, "initial-access", "operation", map[string]mcpLocalArtifact{file.ID: file}, false, nil, a.liveDeliveryAuth(srv.URL, "same-client"))
	if e != nil {
		t.Fatal(e)
	}
	if result["state"] != "succeeded" || refreshes != 2 || puts != 1 || acks != 1 {
		t.Fatal("long delivery refresh failed")
	}
	saved, e := a.loadMCP(srv.URL)
	if e != nil || saved.ClientID != "same-client" || saved.AccessToken != "access-2" || saved.Scope != "maker:read maker:write" {
		t.Fatal("rotated saved grant changed identity", e)
	}
	if _, e = a.liveDeliveryAuth(srv.URL, "different-client")(context.Background()); e == nil {
		t.Fatal("changed client identity allowed")
	}
}

type mcpApprovalCall struct {
	Name string
	Args map[string]any
}

// approvalServer records tool calls. Older servers report named tools unknown.
func approvalServer(t *testing.T, kind string, named bool, calls *[]mcpApprovalCall) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw struct {
			Params struct {
				Name string         `json:"name"`
				Args map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if r.URL.Path != "/mcp" || json.NewDecoder(r.Body).Decode(&raw) != nil {
			t.Error("unexpected request")
			return
		}
		*calls = append(*calls, mcpApprovalCall{raw.Params.Name, raw.Params.Args})
		switch name := raw.Params.Name; {
		case name == "show_workflow":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{"data":{"workflow_id":"workflow","kind":%q}}}}`, kind)
		case strings.HasPrefix(name, "request_") && name != "request_workflow_execution" && !named:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"unknown tool \"%s\""}}`, name)
		default:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{"data":{"operation_id":"operation","state":"pending_approval"}}}}`)
		}
	}))
}
func TestMCPApprovalUsesNamedTools(t *testing.T) {
	for _, tc := range []struct {
		action, kind, tool string
		upload             bool
	}{
		{"stage", "", "request_upload_staging_approval", true}, {"submit", "", "request_review_submission_approval", true},
		{"delete", "", "request_product_deletion_approval", false}, {"deactivate", "", "request_discount_deactivation_approval", false},
		{"discard", "", "request_listing_discard_approval", false}, {"create", "draft", "request_draft_creation_approval", true},
		{"create", "discount", "request_discount_creation_approval", false}, {"retire", "draft", "request_draft_retirement_approval", false},
		{"retire", "listing", "request_listing_retirement_approval", false},
	} {
		var calls []mcpApprovalCall
		srv := approvalServer(t, tc.kind, true, &calls)
		r, e := mcpRequestApproval(context.Background(), srv.URL, "synthetic-write", "workflow", tc.action, "direct")
		srv.Close()
		if e != nil || r["operation_id"] != "operation" {
			t.Fatal(tc.action, e)
		}
		last := calls[len(calls)-1]
		if last.Name != tc.tool || last.Args["workflow_id"] != "workflow" || last.Args["action"] != nil || (last.Args["upload_mode"] == "direct") != tc.upload {
			t.Fatalf("%s/%s called %+v", tc.kind, tc.action, last)
		}
		if want := map[bool]int{false: 1, true: 2}[tc.kind != ""]; len(calls) != want || tc.kind != "" && calls[0].Name != "show_workflow" {
			t.Fatalf("%s/%s unexpected calls %+v", tc.kind, tc.action, calls)
		}
	}
}
func TestMCPApprovalFallsBackOnlyForUnknownTools(t *testing.T) {
	var calls []mcpApprovalCall
	srv := approvalServer(t, "discount", false, &calls)
	defer srv.Close()
	for _, action := range []string{"submit", "create"} {
		calls = nil
		r, e := mcpRequestApproval(context.Background(), srv.URL, "synthetic-write", "workflow", action, "direct")
		last := calls[len(calls)-1]
		if e != nil || r["operation_id"] != "operation" || last.Name != "request_workflow_execution" || last.Args["action"] != action || last.Args["upload_mode"] != "direct" || !strings.HasPrefix(calls[len(calls)-2].Name, "request_") {
			t.Fatalf("legacy fallback failed for %s: %+v %v", action, calls, e)
		}
	}
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&raw)
		calls = append(calls, mcpApprovalCall{Name: raw.Params.Name})
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"structuredContent":{"error":{"code":"INVALID_WORKFLOW_ACTION"}}}}`)
	}))
	defer rejected.Close()
	calls = nil
	if _, e := mcpRequestApproval(context.Background(), rejected.URL, "synthetic-write", "workflow", "delete", "direct"); e == nil || errors.Is(e, errMCPUnknownTool) || len(calls) != 1 {
		t.Fatalf("rejection retried through legacy tool: %+v", calls)
	}
	calls = nil
	other := approvalServer(t, "listing", true, &calls)
	defer other.Close()
	if _, e := mcpRequestApproval(context.Background(), other.URL, "synthetic-write", "workflow", "create", "direct"); e == nil || len(calls) != 1 || calls[0].Name != "show_workflow" {
		t.Fatalf("unsupported kind reached an approval tool: %+v", calls)
	}
}

func TestDeliveryCompletionBetweenStatusAndLease(t *testing.T) {
	for _, ci := range []bool{false, true} {
		for _, final := range []string{"succeeded", "running", "reconciliation_required"} {
			t.Run(fmt.Sprintf("ci_%v_%s", ci, final), func(t *testing.T) {
				reads, polls := 0, 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer synthetic" {
						t.Error("missing existing grant")
						w.WriteHeader(401)
						return
					}
					if strings.HasSuffix(r.URL.Path, "/uploads/next") {
						polls++
						w.WriteHeader(403)
						return
					}
					if (ci && r.URL.Path == "/v1/ci/operations/operation") || (!ci && r.URL.Path == "/mcp") {
						reads++
						state := "running"
						if reads > 1 {
							state = final
						}
						if ci {
							fmt.Fprintf(w, `{"state":%q}`, state)
						} else {
							fmt.Fprintf(w, `{"result":{"structuredContent":{"data":{"state":%q}}}}`, state)
						}
						return
					}
					t.Errorf("unexpected action: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
				}))
				defer srv.Close()
				result, err := mcpPollDelivery(context.Background(), srv.URL, "synthetic", "operation", nil, ci)
				if final == "succeeded" {
					if err != nil || result["state"] != "succeeded" {
						t.Fatalf("completion lost: %v %v", result, err)
					}
				} else if err == nil {
					t.Fatal("unconfirmed completion accepted")
				}
				if reads != 2 || polls != 1 {
					t.Fatalf("unexpected retry: reads=%d polls=%d", reads, polls)
				}
			})
		}
	}
}
