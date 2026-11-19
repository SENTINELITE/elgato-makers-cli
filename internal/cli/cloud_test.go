package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const cloudTestCode = "synthetic_one_time_code_01234567890123456789"
const cloudTestCookie = "synthetic_saved_cookie_secret"

func cloudFixture(t *testing.T) ([]string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"org_id":"org"}`), 0600); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(dir, "cookies.json")
	if err := os.WriteFile(creds, []byte(`[{"name":"__Host-kc_at","value":"`+cloudTestCookie+`","path":"/"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	code := filepath.Join(dir, "code")
	if err := os.WriteFile(code, []byte(cloudTestCode), 0600); err != nil {
		t.Fatal(err)
	}
	return []string{"--config", cfg, "--credentials-file", creds, "cloud", "connect", "--link", "pending_link", "--code-file", code}, creds
}
func TestCloudConnectOneTimeTransfer(t *testing.T) {
	args, creds := cloudFixture(t)
	posts := 0
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cloudTestCode {
			t.Error("missing one-time code")
			w.WriteHeader(401)
			return
		}
		if r.Method == http.MethodGet {
			gets++
			json.NewEncoder(w).Encode(map[string]any{"id": "pending_link", "org_id": "org", "expires_at": time.Now().Add(time.Minute)})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/links/pending_link/complete" {
			t.Error("unexpected request")
			w.WriteHeader(400)
			return
		}
		posts++
		if posts > 1 {
			w.WriteHeader(409)
			fmt.Fprint(w, cloudTestCookie+cloudTestCode)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var envelope struct {
			Cookies []map[string]any `json:"cookies"`
		}
		if json.Unmarshal(b, &envelope) != nil || len(envelope.Cookies) != 1 || envelope.Cookies[0]["value"] != cloudTestCookie {
			t.Error("missing saved credentials")
		}
		fmt.Fprint(w, `{"connection_id":"connection_id","org_id":"org","state":"connected"}`)
	}))
	defer srv.Close()
	before, _ := os.ReadFile(creds)
	for i := 0; i < 2; i++ {
		out, stderr, err := command(t, Options{}, append(append([]string{"--json"}, args...), "--server", srv.URL, "--yes")...)
		if i == 0 {
			if err != nil || !json.Valid([]byte(out)) || !strings.Contains(stderr, "saved") {
				t.Fatalf("first connect: %q %q %v", out, stderr, err)
			}
		} else if err == nil {
			t.Fatal("replay succeeded")
		}
		all := out + stderr
		if err != nil {
			all += err.Error()
		}
		if strings.Contains(all, cloudTestCookie) || strings.Contains(all, cloudTestCode) {
			t.Fatal("secret disclosed")
		}
	}
	after, _ := os.ReadFile(creds)
	if string(before) != string(after) {
		t.Fatal("saved credentials changed")
	}
	if gets != 2 || posts != 2 {
		t.Fatalf("requests %d/%d", gets, posts)
	}
	if _, err := os.Stat(creds + ".lock"); !os.IsNotExist(err) {
		t.Fatal("session lock remained")
	}
}
func TestCloudRejectBeforeCredentialLoad(t *testing.T) {
	for _, scenario := range []string{"wrong_org", "expired", "wrong_id", "oversized", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			args, creds := cloudFixture(t)
			os.Remove(creds)
			posts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
					t.Error("unexpected credential transfer")
					return
				}
				if scenario == "redirect" {
					http.Redirect(w, r, "https://example.invalid/", 302)
					return
				}
				if scenario == "oversized" {
					fmt.Fprint(w, strings.Repeat("x", 65537))
					return
				}
				org, id, expiry := "org", "pending_link", time.Now().Add(time.Minute)
				switch scenario {
				case "wrong_org":
					org = "other"
				case "expired":
					expiry = time.Now().Add(-time.Minute)
				case "wrong_id":
					id = "other"
				}
				json.NewEncoder(w).Encode(map[string]any{"id": id, "org_id": org, "expires_at": expiry})
			}))
			defer srv.Close()
			_, _, err := command(t, Options{}, append(args, "--server", srv.URL, "--yes")...)
			if err == nil || strings.Contains(err.Error(), "load saved") {
				t.Fatalf("did not reject link first: %v", err)
			}
			if posts != 0 {
				t.Fatal("transferred")
			}
			if _, err = os.Stat(creds + ".lock"); !os.IsNotExist(err) {
				t.Fatal("store lock created")
			}
		})
	}
}
func TestCloudCancelAndTransportValidation(t *testing.T) {
	for _, origin := range []string{"http://example.com", "http://127.0.0.2", "https://user:secret@example.com", "https://example.com/path", "https://example.com?code=secret", "https://example.com/#secret"} {
		if _, err := cloudOrigin(origin); err == nil {
			t.Fatalf("accepted %q", origin)
		}
	}
	for _, origin := range []string{"https://example.com", "http://localhost:1234", "http://127.0.0.1:1234", "http://[::1]:1234"} {
		if _, err := cloudOrigin(origin); err != nil {
			t.Fatal(err)
		}
	}
	args, creds := cloudFixture(t)
	os.Remove(creds)
	_, _, err := command(t, Options{In: strings.NewReader("n\n"), IsTerminal: func() bool { return true }}, append(args, "--server", "https://example.com")...)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancel: %v", err)
	}
	_, _, err = command(t, Options{}, append(args, "--server", "https://example.com")...)
	if err == nil || !strings.Contains(err.Error(), "consent") {
		t.Fatalf("noninteractive: %v", err)
	}
}

func TestCloudRejectInvalidCompletion(t *testing.T) {
	for _, scenario := range []string{"invalid_state", "code_echo", "cookie_echo"} {
		t.Run(scenario, func(t *testing.T) {
			args, _ := cloudFixture(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					json.NewEncoder(w).Encode(map[string]any{"id": "pending_link", "org_id": "org", "expires_at": time.Now().Add(time.Minute)})
					return
				}
				state, id := "connected", "connection_id"
				switch scenario {
				case "invalid_state":
					state = "pending"
				case "code_echo":
					id = cloudTestCode
				case "cookie_echo":
					id = cloudTestCookie
				}
				json.NewEncoder(w).Encode(map[string]string{"connection_id": id, "org_id": "org", "state": state})
			}))
			defer srv.Close()
			out, stderr, err := command(t, Options{}, append(append([]string{"--json"}, args...), "--server", srv.URL, "--yes")...)
			if err == nil {
				t.Fatal("invalid completion accepted")
			}
			if out != "" || strings.Contains(stderr, cloudTestCode) || strings.Contains(stderr, cloudTestCookie) {
				t.Fatal("completion disclosed secrets")
			}
		})
	}
}
func TestCloudRequiresCodeFileWithoutTerminal(t *testing.T) {
	args, _ := cloudFixture(t)
	args = args[:len(args)-2]
	_, _, err := command(t, Options{}, append(args, "--server", "https://example.com", "--yes")...)
	if err == nil || !strings.Contains(err.Error(), "code-file") {
		t.Fatalf("code file requirement: %v", err)
	}
	args, _ = cloudFixture(t)
	args = args[:len(args)-2]
	_, _, err = command(t, Options{In: strings.NewReader("y\n"), IsTerminal: func() bool { return true }}, append(args, "--server", "https://example.com")...)
	if err == nil || !strings.Contains(err.Error(), "requires a terminal") {
		t.Fatalf("hidden code terminal requirement: %v", err)
	}
}
func TestCloudConnectGrantsRevokedNotice(t *testing.T) {
	for name, field := range map[string]string{"absent": "", "zero": `,"grants_revoked":0`, "revoked": `,"grants_revoked":2`} {
		t.Run(name, func(t *testing.T) {
			args, _ := cloudFixture(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					json.NewEncoder(w).Encode(map[string]any{"id": "pending_link", "org_id": "org", "expires_at": time.Now().Add(time.Minute)})
					return
				}
				fmt.Fprint(w, `{"connection_id":"connection_id","org_id":"org","state":"connected"`+field+`}`)
			}))
			defer srv.Close()
			out, stderr, err := command(t, Options{}, append(append([]string{"--json"}, args...), "--server", srv.URL, "--yes")...)
			if err != nil || !json.Valid([]byte(out)) || !strings.Contains(stderr, "Maker cloud connection saved.") {
				t.Fatalf("connect: %q %q %v", out, stderr, err)
			}
			if notice := strings.Contains(stderr, "re-approve"); notice != (name == "revoked") {
				t.Fatalf("notice=%v for %s: %q", notice, name, stderr)
			}
		})
	}
}
