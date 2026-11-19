package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCIDiagnosticRedactionAndRecovery(t *testing.T) {
	for _, code := range []string{"CI_TOKEN_EXPIRED", "CI_MAKER_AUTH_REQUIRED", "CI_POLICY_REF_MISMATCH", "CI_POLICY_EXPIRED"} {
		t.Run(code, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]any{"error_code": code, "error": "Cookie=secret https://bad.example/?X-Amz-Signature=secret", "next_action": "leak secret", "request_id": "safe_request_123", "provider_write_state": "not_attempted"})
			}))
			defer srv.Close()
			err := mcpRequest(context.Background(), srv.Client(), "POST", srv.URL+"/v1/ci/preflight", "application/json", "", nil, nil)
			if err == nil || !strings.Contains(err.Error(), code) || !strings.Contains(err.Error(), "safe_request_123") || strings.Contains(err.Error(), "secret") || calls != 1 {
				t.Fatal(err, calls)
			}
		})
	}
	for _, tc := range []struct{ path, code, id string }{
		{"/v1/ci/preflight", "UNKNOWN_CODE", "valid_request_123"},
		{"/oauth/token", "CI_TOKEN_EXPIRED", "valid_request_123"},
	} {
		raw, _ := json.Marshal(map[string]string{"error_code": tc.code, "request_id": tc.id})
		if parseCIRemoteDiagnostic("https://maker.example"+tc.path, raw) != nil {
			t.Fatal("untrusted diagnostics accepted")
		}
	}
	raw := []byte(`{"error_code":"CI_TOKEN_EXPIRED","request_id":"injected\nCookie=secret","provider_write_state":"not_attempted"}`)
	d := parseCIRemoteDiagnostic("https://maker.example/v1/ci/operations/existing-operation", raw)
	if d == nil || d.RequestID != "" || d.Writes != "" || strings.Contains(d.Error(), "did not start") {
		t.Fatal(d)
	}
}

func TestCIDiagnosticLostStatusDoesNotReplay(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" {
			t.Error("retried a mutation")
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"error_code":"CI_TOKEN_EXPIRED","provider_write_state":"not_attempted"}`))
	}))
	defer srv.Close()
	result, err := mcpPollCIServerRelease(context.Background(), srv.URL, "expired", mcpCIReleaseResult{Operation: "accepted-operation"})
	if calls != 1 || err == nil || result.Operation != "accepted-operation" || !strings.Contains(err.Error(), "accepted-operation") || strings.Contains(err.Error(), "did not start") {
		t.Fatal(result, err, calls)
	}
}

func TestCIDiagnosticUnavailableServiceIsActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "safe_gateway_123")
		w.WriteHeader(503)
		w.Write([]byte("secret upstream response"))
	}))
	defer srv.Close()
	err := mcpRequest(context.Background(), srv.Client(), "POST", srv.URL+"/v1/ci/preflight/token", "application/json", "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "service availability") || !strings.Contains(err.Error(), "safe_gateway_123") || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}
