package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func policyCLIApp(t *testing.T, origin string) *app {
	t.Helper()
	a := mcpTestApp(t)
	if e := a.saveMCP(mcpAuth{Server: origin, ClientID: "owner-cli", Scope: "maker:read maker:write", AccessToken: "synthetic-owner-access", RefreshToken: "synthetic-owner-refresh", ExpiresAt: time.Now().Add(time.Hour)}); e != nil {
		t.Fatal(e)
	}
	return a
}
func TestCIPolicyCLIWaitUsesOnlyRESTAndDoesNotSkipApproval(t *testing.T) {
	var origin string
	gets, applies, creates := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "Bearer synthetic-owner-access" {
			t.Error("incorrect auth")
		}
		out := ciPolicyIntent{Operation: "operation-one", Policy: "policy-one", State: "pending_approval"}
		switch r.URL.Path {
		case "/v1/ci/policies":
			creates++
			if r.Method != "POST" {
				t.Error("incorrect method")
			}
			var cfg map[string]any
			if json.NewDecoder(r.Body).Decode(&cfg) != nil || cfg["preflight_only"] != true {
				t.Error("policy config changed")
			}
			out.ApprovalURL = origin + "/workflows/approve/operation-one"
		case "/v1/ci/policy-operations/operation-one":
			if r.Method == "GET" {
				gets++
				if gets >= 2 {
					out.State = "approved"
				}
				if applies > 0 {
					out.State = "succeeded"
				}
			} else {
				applies++
				if gets < 2 {
					t.Error("applied before approval")
				}
				out.State = "running"
			}
		default:
			t.Error("setup invoked unexpected endpoint", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	origin = srv.URL
	a := policyCLIApp(t, origin)
	a.json = true
	file := filepath.Join(t.TempDir(), "policy.json")
	os.WriteFile(file, []byte(`{"preflight_only":true}`), 0600)
	cmd := a.ciCommands()
	cmd.SetArgs([]string{"policy", "create", "--server", origin, "--file", file, "--wait"})
	if e := cmd.Execute(); e != nil {
		t.Fatal(e)
	}
	if creates != 1 || applies != 1 || gets < 3 {
		t.Fatal("unexpected retries", creates, applies, gets)
	}
	if !strings.Contains(a.o.Err.(*bytes.Buffer).String(), "Review this policy") || !strings.Contains(a.o.Out.(*bytes.Buffer).String(), `"state": "succeeded"`) {
		t.Fatal("missing approval link or final result", a.o.Out)
	}
}
func TestCIPolicyCLIUncertainApplyNeverRetries(t *testing.T) {
	applies := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			applies++
			w.WriteHeader(502)
			fmt.Fprint(w, "secret upstream cookie")
			return
		}
		json.NewEncoder(w).Encode(ciPolicyIntent{Operation: "operation-one", Policy: "policy-one", State: "approved"})
	}))
	defer srv.Close()
	a := policyCLIApp(t, srv.URL)
	_, e := a.ciPolicyWait(context.Background(), srv.URL, ciPolicyIntent{Operation: "operation-one", Policy: "policy-one"})
	if e == nil || applies != 1 || !strings.Contains(e.Error(), "operation-one") || strings.Contains(e.Error(), "secret upstream") {
		t.Fatal("unsafe retry or missing safe diagnostics", applies, e)
	}
}
func TestCIPolicyCLIRejectsRedirectAndIdentityChanges(t *testing.T) {
	for _, tc := range []string{"foreign-url", "query-url", "path-url", "changed-operation", "changed-policy"} {
		t.Run(tc, func(t *testing.T) {
			var origin string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				out := ciPolicyIntent{Operation: "operation-one", Policy: "policy-one", State: "pending_approval", ApprovalURL: origin + "/workflows/approve/operation-one"}
				if r.Method == "POST" {
					switch tc {
					case "foreign-url":
						out.ApprovalURL = "https://foreign.example/workflows/approve/operation-one"
					case "query-url":
						out.ApprovalURL += "?secret=yes"
					case "path-url":
						out.ApprovalURL += "/extra"
					}
				}
				if r.Method == "GET" {
					out.State = "approved"
					if tc == "changed-operation" {
						out.Operation = "operation-other"
					}
					if tc == "changed-policy" {
						out.Policy = "policy-other"
					}
				}
				json.NewEncoder(w).Encode(out)
			}))
			defer srv.Close()
			origin = srv.URL
			a := policyCLIApp(t, origin)
			if strings.HasPrefix(tc, "changed-") {
				if _, e := a.ciPolicyWait(context.Background(), origin, ciPolicyIntent{Operation: "operation-one", Policy: "policy-one"}); e == nil {
					t.Fatal("identity swap accepted")
				}
			} else {
				f := filepath.Join(t.TempDir(), "policy.json")
				os.WriteFile(f, []byte(`{}`), 0600)
				cmd := a.ciCommands()
				cmd.SetArgs([]string{"policy", "create", "--server", origin, "--file", f})
				if e := cmd.Execute(); e == nil {
					t.Fatal("untrusted approval URL accepted")
				}
			}
		})
	}
}
func TestCloudAndCICommandsAreAvailableWithoutMCP(t *testing.T) {
	root := New(Options{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	for _, path := range [][]string{{"cloud", "login"}, {"cloud", "logout"}, {"cloud", "status"}, {"ci", "policy", "create"}, {"ci", "preflight"}, {"ci", "release"}, {"mcp", "ci", "preflight"}, {"mcp", "ci", "policy", "create"}} {
		cmd, _, e := root.Find(path)
		if e != nil || cmd.Name() != path[len(path)-1] {
			t.Fatal("missing command", path, e)
		}
	}
}
func TestCloudStatusUsesRESTWithReadOnlyGrant(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/ci/policies" || r.Method != "GET" {
			t.Error("MCP invoked", r.URL.Path)
		}
		fmt.Fprint(w, `{"deployments":[]}`)
	}))
	defer srv.Close()
	a := policyCLIApp(t, srv.URL)
	auth, e := a.loadMCP(srv.URL)
	if e != nil {
		t.Fatal(e)
	}
	auth.Scope = "maker:read"
	if e = a.saveMCP(auth); e != nil {
		t.Fatal(e)
	}
	cmd := a.cloudCommands()
	cmd.SetArgs([]string{"status", "--server", srv.URL})
	if e = cmd.Execute(); e != nil || calls != 1 {
		t.Fatal(e, calls)
	}
}

func TestCIPolicyListKeepsIDsOutOfHumanOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"deployments":[{"policy_id":"policy-one","enabled":true,"config":{"repository":"example/plugin","ref":"refs/heads/main","preflight_only":true}}]}`)
	}))
	defer srv.Close()
	a := policyCLIApp(t, srv.URL)
	if err := a.ciPolicyList(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	out := a.o.Out.(*bytes.Buffer)
	if strings.Contains(out.String(), "policy-one") || !strings.Contains(out.String(), "validation_only=true") || !strings.Contains(a.completion, srv.URL+"/ci") {
		t.Fatal("policy list exposes IDs or omits the management link")
	}

	out.Reset()
	a.json = true
	if err := a.ciPolicyList(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"policy_id": "policy-one"`) {
		t.Fatal("explicit JSON did not include the policy ID")
	}
}
