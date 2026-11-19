package cli

import (
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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/draft"
	"github.com/SENTINELITE/elgato-makers-cli/internal/listing"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

func uncertainCreationFixture(t *testing.T) (string, string, string, listing.Plan) {
	t.Helper()
	dir, cfg, file := listingFixture(t)
	config, err := listing.LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := listing.PrepareConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	manager := listing.NewManager(filepath.Join(state, "listings"), nil)
	plan, err := manager.Prepare(prepared, "org")
	if err != nil {
		t.Fatal(err)
	}
	child, err := draft.NewManager(filepath.Join(state, "listings", "drafts"), nil).Prepare(maker.DraftInput{OrgID: plan.Target.OrgID, UUID: plan.Target.UUID, Name: prepared.Config.Name, Description: prepared.Description.Markdown, DescriptionPlainTextLength: prepared.Description.PlainTextLength}, prepared.Package)
	if err != nil {
		t.Fatal(err)
	}
	child.State, child.Checksum = draft.Unknown, ""
	b, err := json.Marshal(child)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	child.Checksum = hex.EncodeToString(sum[:])
	b, err = json.Marshal(child)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(state, "listings", "drafts", child.ID+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	plan.DraftPlanID, plan.Checksum = child.ID, ""
	b, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	sum = sha256.Sum256(b)
	plan.Checksum = hex.EncodeToString(sum[:])
	b, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(state, "listings", plan.ID+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, cfg, state, plan
}

func recoveryStateBytes(t *testing.T, state string) string {
	t.Helper()
	var snapshot strings.Builder
	if err := filepath.WalkDir(state, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&snapshot, "%s:%s\n", path, b)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot.String()
}

func TestCreationRecoveryRequiresTwoExplicitInteractiveAnswers(t *testing.T) {
	dir, cfg, state, plan := uncertainCreationFixture(t)
	before := recoveryStateBytes(t, state)
	for _, input := range []string{"", "\n", "no\n", "yes\n", "yes\n\n", "yes\nno\n", "yes\nyes", "\x03\n", "yes\n\x03\n"} {
		t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
			options := Options{In: strings.NewReader(input), IsTerminal: func() bool { return true }, ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) { t.Fatal("refusal reached client"); return nil, nil }}
			out, stderr, err := command(t, options, "--config", cfg, "--state-dir", state, "--credentials-file", filepath.Join(dir, "missing"), "listing", "recover-creation", plan.ID)
			if err != nil || out != "" || !strings.Contains(stderr, "Cancelled:") || strings.Contains(stderr, "Connecting to Maker") {
				t.Fatalf("bad refusal: %v %s %s", err, out, stderr)
			}
			if !strings.Contains(stderr, "cannot prove the original request failed") || !strings.Contains(stderr, plan.DraftPlanID) || !strings.Contains(stderr, plan.Target.UUID) {
				t.Fatalf("missing review: %s", stderr)
			}
			if strings.HasPrefix(input, "yes\n") && strings.Count(stderr, "[y/N]") != 2 {
				t.Fatal("missing second default-No prompt")
			}
			if after := recoveryStateBytes(t, state); after != before {
				t.Fatal("refusal changed local records")
			}
		})
	}
	for _, suffix := range [][]string{nil, {"--yes"}, {"-y"}} {
		args := append([]string{"--config", cfg, "--state-dir", state, "listing", "recover-creation", plan.ID}, suffix...)
		_, _, err := command(t, Options{In: strings.NewReader("yes\nyes\n"), IsTerminal: func() bool { return false }, ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
			t.Fatal("noninteractive recovery reached client")
			return nil, nil
		}}, args...)
		if err == nil {
			t.Fatal("noninteractive/bypass recovery accepted")
		}
	}
	if recoveryStateBytes(t, state) != before {
		t.Fatal("noninteractive recovery changed state")
	}
}

func TestCreationRecoveryUsesReadOnlyCatalogAndRetiresLocally(t *testing.T) {
	dir, cfg, state, plan := uncertainCreationFixture(t)
	credentials := listingCredentials(t, dir)
	reads, clients := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if r.Method != http.MethodGet || r.URL.Path != "/org/products" {
			t.Errorf("recovery attempted %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 405)
			return
		}
		fmt.Fprint(w, `{"products":[]}`)
	}))
	defer server.Close()
	options := Options{In: strings.NewReader("yes\nyes\n"), IsTerminal: func() bool { return true }, ClientFactory: func(cookies []*http.Cookie, readOnly bool) (*maker.Client, error) {
		clients++
		if !readOnly {
			t.Error("recovery client must be read-only")
		}
		for _, c := range cookies {
			c.Secure = false
		}
		return maker.New(maker.Config{BaseURL: server.URL, AllowLocalhost: true, HTTPClient: server.Client(), Cookies: cookies, ReadOnly: readOnly})
	}}
	common := []string{"--config", cfg, "--state-dir", state, "--credentials-file", credentials, "--minify", "listing", "recover-creation", plan.ID}
	out, stderr, err := command(t, options, common...)
	if err != nil {
		t.Fatalf("recovery failed: %v %s", err, stderr)
	}
	var result listing.Plan
	if err = json.Unmarshal([]byte(out), &result); err != nil || result.State != listing.Retired || result.CreationRecovery == nil || reads == 0 || clients != 1 {
		t.Fatalf("bad recovery: %v %s", err, out)
	}
	if connecting := strings.Index(stderr, "Connecting to Maker for read-only catalog checks..."); connecting < 0 || connecting < strings.LastIndex(stderr, "[y/N]") {
		t.Fatalf("connecting progress did not follow both confirmations: %s", stderr)
	}
	if !strings.Contains(stderr, "Prepare a new listing using your original --file") || !strings.Contains(stderr, "retired plan cannot resume") {
		t.Fatalf("missing next step: %s", stderr)
	}
	before := recoveryStateBytes(t, state)
	out, _, err = command(t, Options{IsTerminal: func() bool { return false }, ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Fatal("idempotent recovery reached client")
		return nil, nil
	}}, common...)
	if err != nil || !strings.Contains(out, `"retired"`) || recoveryStateBytes(t, state) != before {
		t.Fatalf("idempotent recovery failed: %v %s", err, out)
	}
}

func TestCreationRecoveryContextCancellationUnblocksPrompt(t *testing.T) {
	dir, cfg, state, plan := uncertainCreationFixture(t)
	before := recoveryStateBytes(t, state)
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	prompt := make(chan struct{}, 1)
	options := Options{In: input, Out: &out, Err: recoveryPromptWriter{prompt}, IsTerminal: func() bool { return true }, ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Error("cancelled prompt reached client")
		return nil, nil
	}}
	root := New(options)
	root.SetArgs([]string{"--config", cfg, "--state-dir", state, "--credentials-file", filepath.Join(dir, "missing"), "listing", "recover-creation", plan.ID})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	select {
	case <-prompt:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled prompt remained blocked")
	}
	writer.Close() // Release the input-only reader goroutine after cancellation.
	if out.Len() != 0 || recoveryStateBytes(t, state) != before {
		t.Fatal("cancellation changed output/state")
	}
}

type recoveryPromptWriter struct{ prompt chan struct{} }

func (w recoveryPromptWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("[y/N]")) {
		select {
		case w.prompt <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func TestCreationRecoveryRejectsIneligiblePlanBeforeQuestions(t *testing.T) {
	_, cfg, state, uncertain := uncertainCreationFixture(t)
	pristineState := state + "-pristine"
	plan, err := listing.NewManager(filepath.Join(pristineState, "listings"), nil).Prepare(uncertain.Prepared, uncertain.Target.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	before := recoveryStateBytes(t, pristineState)
	_, stderr, err := command(t, Options{In: strings.NewReader("yes\nyes\n"), IsTerminal: func() bool { return true }, ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Fatal("ineligible recovery reached client")
		return nil, nil
	}}, "--config", cfg, "--state-dir", pristineState, "listing", "recover-creation", plan.ID)
	if err == nil || strings.Contains(stderr, "[y/N]") || strings.Contains(stderr, "Review uncertain creation") || recoveryStateBytes(t, pristineState) != before {
		t.Fatalf("ineligible plan reached review or changed state: %v %s", err, stderr)
	}
}

func TestCreationRecoveryRejectsPlanChangedDuringConfirmation(t *testing.T) {
	_, cfg, state, plan := uncertainCreationFixture(t)
	input := &recoveryChangingReader{reader: strings.NewReader("yes\nyes\n"), change: func() {
		plan.Target.OrgID = "other-org"
		plan.Checksum = ""
		raw, err := json.Marshal(plan)
		if err != nil {
			t.Error(err)
			return
		}
		sum := sha256.Sum256(raw)
		plan.Checksum = hex.EncodeToString(sum[:])
		raw, err = json.Marshal(plan)
		if err != nil {
			t.Error(err)
			return
		}
		if err = os.WriteFile(filepath.Join(state, "listings", plan.ID+".json"), raw, 0600); err != nil {
			t.Error(err)
		}
	}}
	_, _, err := command(t, Options{In: input, IsTerminal: func() bool { return true }, ClientFactory: func([]*http.Cookie, bool) (*maker.Client, error) {
		t.Fatal("changed plan reached client")
		return nil, nil
	}}, "--config", cfg, "--state-dir", state, "listing", "recover-creation", plan.ID)
	if err == nil || !strings.Contains(err.Error(), "changed during confirmation") {
		t.Fatalf("reviewed plan change accepted: %v", err)
	}
}

type recoveryChangingReader struct {
	reader io.Reader
	change func()
}

func (r *recoveryChangingReader) Read(p []byte) (int, error) {
	if r.change != nil {
		r.change()
		r.change = nil
	}
	return r.reader.Read(p)
}
