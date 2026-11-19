package release

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

type fakeAPI struct {
	versions                 []model.Version
	uploadCalls, submitCalls int
	submitError              error
	versionsError            error
	onSubmit                 func()
	visible                  bool
	noID                     bool
}

func (f *fakeAPI) Versions(context.Context, model.Target) ([]model.Version, error) {
	return f.versions, f.versionsError
}
func (f *fakeAPI) Upload(_ context.Context, _ model.Target, p model.Package) (model.UploadResult, error) {
	f.uploadCalls++
	return model.UploadResult{FileID: "file", FolderID: "folder", Manifest: p.Manifest}, nil
}
func (f *fakeAPI) Submit(_ context.Context, _ model.Target, s model.Submission) (model.SubmitResult, error) {
	f.submitCalls++
	if f.onSubmit != nil {
		f.onSubmit()
	}
	if f.visible {
		f.versions = []model.Version{{ID: "remote", Version: s.Package.Manifest.Version, Status: "pendingReview"}}
	}
	if f.noID {
		return model.SubmitResult{}, f.submitError
	}
	return model.SubmitResult{ID: "remote"}, f.submitError
}

func fixture(t *testing.T, f *fakeAPI) (*Manager, Plan) {
	t.Helper()
	d := t.TempDir()
	path := filepath.Join(d, "test.streamDeckPlugin")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(out)
	w, err := z.Create("com.example.test.sdPlugin/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Write([]byte(`{"Name":"Test","UUID":"com.example.test","Version":"1.0.0.4","Software":{"MinimumVersion":"6.9"},"OS":[{"Platform":"mac","MinimumVersion":"12"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	if err = out.Close(); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(d, "notes.md")
	if err = os.WriteFile(notes, []byte("Improved behavior.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(filepath.Join(d, "state"), f)
	p, err := m.Prepare(context.Background(), model.Target{OrgID: "org", ProductID: "product", VariantID: "variant", UUID: "com.example.test"}, path, notes, false)
	if err != nil {
		t.Fatal(err)
	}
	return m, p
}

func TestUploadNeverSubmits(t *testing.T) {
	f := &fakeAPI{}
	m, p := fixture(t, f)
	p, err := m.Upload(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != Uploaded || f.uploadCalls != 1 || f.submitCalls != 0 {
		t.Fatalf("bad upload state: %+v", p)
	}
	if !filesec.IsPrivateFile(filepath.Join(m.dir, p.ID+".json")) {
		t.Fatal("plan must be private")
	}
	if _, err = m.Upload(context.Background(), p.ID); err == nil {
		t.Fatal("repeat upload allowed")
	}
}

func TestConfirmations(t *testing.T) {
	for _, tc := range []struct {
		name, input     string
		tty, yes, allow bool
	}{{"nonterminal", "y\ny\n", false, false, false}, {"first default no", "\ny\n", true, false, false}, {"second default no", "y\n\n", true, false, false}, {"one yes", "y\n", true, false, false}, {"two yes", "y\nyes\n", true, false, true}, {"explicit yes", "", false, true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAPI{visible: true}
			m, p := fixture(t, f)
			if _, err := m.Upload(context.Background(), p.ID); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			p, err := m.Submit(context.Background(), p.ID, ConfirmOptions{Yes: tc.yes, IsTerminal: tc.tty, Input: strings.NewReader(tc.input), Output: &output})
			if tc.allow {
				if err != nil {
					t.Fatal(err)
				}
				if p.State != Submitted || f.submitCalls != 1 {
					t.Fatal("submission not recorded")
				}
			} else {
				if err == nil || f.submitCalls != 0 {
					t.Fatal("unconfirmed submission sent")
				}
				loaded, e := m.Load(p.ID)
				if e != nil {
					t.Fatal(e)
				}
				if loaded.State != Uploaded {
					t.Fatal("cancellation changed state")
				}
			}
		})
	}
}

func TestUnknownCannotRetryOrBypass(t *testing.T) {
	f := &fakeAPI{submitError: errors.New("lost response")}
	m, p := fixture(t, f)
	if _, err := m.Upload(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	f.onSubmit = func() {
		loaded, err := m.Load(p.ID)
		if err != nil || loaded.State != SubmissionUnknown {
			t.Errorf("uncertainty not persisted before POST: %v", err)
		}
	}
	p, err := m.Submit(context.Background(), p.ID, ConfirmOptions{Yes: true})
	if err == nil || p.State != SubmissionUnknown || p.SubmissionID != "remote" {
		t.Fatal("uncertain result lost")
	}
	if _, err = m.Submit(context.Background(), p.ID, ConfirmOptions{Yes: true}); err == nil || f.submitCalls != 1 {
		t.Fatal("uncertain submission retried")
	}
	if _, err = m.Status(context.Background(), p.ID); err == nil {
		t.Fatal("empty remote read falsely confirms")
	}
	second, err := m.Prepare(context.Background(), p.Target, p.Package.Path, p.Notes.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Upload(context.Background(), second.ID); err == nil || f.uploadCalls != 1 {
		t.Fatal("new plan bypassed uncertainty")
	}
	f.versions = []model.Version{{ID: "remote", Version: p.Package.Manifest.Version, Status: "pendingReview"}}
	p, err = m.Status(context.Background(), p.ID)
	if err != nil || p.State != Submitted {
		t.Fatalf("reconcile failed: %v", err)
	}
}

func TestDuplicateReviewAndUnknownBlockUpload(t *testing.T) {
	for _, tc := range []struct {
		version model.Version
		want    error
	}{
		{model.Version{ID: "one", Version: "1.0.0.4", Status: "published"}, model.ErrDuplicateVersion},
		{model.Version{ID: "two", Version: "0.9.0", Status: "pending_review"}, model.ErrVersionInReview},
		{model.Version{ID: "three", Version: "0.9.0", Status: "review"}, model.ErrVersionInReview},
		{model.Version{ID: "four", Version: "0.9.0", Status: "unsupported"}, model.ErrUnrecognizedVersionLifecycle},
	} {
		t.Run(tc.version.Status, func(t *testing.T) {
			f := &fakeAPI{versions: []model.Version{tc.version}}
			m, p := fixture(t, f)
			if _, err := m.Upload(context.Background(), p.ID); !errors.Is(err, tc.want) || f.uploadCalls != 0 {
				t.Fatalf("upload calls = %d, got %v, want %v", f.uploadCalls, err, tc.want)
			}
		})
	}
}

func TestChangedArtifactsAndCorruptStateRefused(t *testing.T) {
	for _, kind := range []string{"notes", "package", "state"} {
		t.Run(kind, func(t *testing.T) {
			f := &fakeAPI{}
			m, p := fixture(t, f)
			path := p.Notes.Path
			if kind == "package" {
				path = p.Package.Path
			}
			if kind == "state" {
				path = filepath.Join(m.dir, p.ID+".json")
			}
			if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Upload(context.Background(), p.ID); err == nil || f.uploadCalls != 0 {
				t.Fatal("changed input accepted")
			}
		})
	}
}

func TestStaleLockDoesNotAutoApprove(t *testing.T) {
	f := &fakeAPI{}
	m, p := fixture(t, f)
	if err := os.WriteFile(filepath.Join(m.dir, "release.lock"), []byte("pid=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Upload(context.Background(), p.ID); err == nil || f.uploadCalls != 0 {
		t.Fatal("stale lock silently removed")
	}
}

func TestUnknownWithoutIDRequiresExactEvidence(t *testing.T) {
	f := &fakeAPI{noID: true, submitError: errors.New("request timed out")}
	m, p := fixture(t, f)
	if _, err := m.Upload(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit(context.Background(), p.ID, ConfirmOptions{Yes: true}); err == nil {
		t.Fatal("expected uncertainty")
	}
	f.versions = []model.Version{{ID: "external", Version: p.Package.Manifest.Version, Status: "pendingReview"}}
	if _, err := m.Status(context.Background(), p.ID); err == nil {
		t.Fatal("version number alone cannot identify this submission")
	}
	f.versions[0].ReleaseNotes = p.Notes.Markdown
	f.versions[0].AutoPublish = &p.AutoPublish
	loaded, err := m.Status(context.Background(), p.ID)
	if err != nil || loaded.State != Submitted || loaded.SubmissionID != "external" {
		t.Fatalf("exact reconciliation failed: %v", err)
	}
}

func TestSubmissionRechecksNotesAndRemoteStatus(t *testing.T) {
	for _, change := range []string{"notes", "remote"} {
		t.Run(change, func(t *testing.T) {
			f := &fakeAPI{}
			m, p := fixture(t, f)
			if _, err := m.Upload(context.Background(), p.ID); err != nil {
				t.Fatal(err)
			}
			if change == "notes" {
				if err := os.WriteFile(p.Notes.Path, []byte("New release notes"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				f.versions = []model.Version{{ID: "other", Version: "0.9", Status: "review"}}
			}
			_, err := m.Submit(context.Background(), p.ID, ConfirmOptions{Yes: true})
			if err == nil || f.submitCalls != 0 {
				t.Fatal("submission skipped revalidation")
			}
			if change == "remote" && !errors.Is(err, model.ErrVersionInReview) {
				t.Fatalf("expected review gate, got %v", err)
			}
		})
	}
}

func TestUnsupportedLifecyclePreservesSubmissionUncertainty(t *testing.T) {
	api := &receiptAPI{fakeAPI: &fakeAPI{}}
	m, p := receiptFixture(t, api)
	if _, err := m.Upload(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	api.onReadback = func() { api.versions[0].Status = "reviewed" }
	if _, err := m.Submit(context.Background(), p.ID, ConfirmOptions{Yes: true}); err == nil {
		t.Fatal("unsupported status confirmed the submission")
	}
	saved, err := m.Load(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != SubmissionUnknown || saved.SubmissionID != "canonical-version" || saved.SubmissionReceiptID != "opaque-receipt" {
		t.Fatalf("unexpected stored submission evidence: %+v", saved)
	}
}

func TestPlanTamperingFailsIntegrity(t *testing.T) {
	f := &fakeAPI{}
	m, p := fixture(t, f)
	path := filepath.Join(m.dir, p.ID+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte(`"auto_publish": false`), []byte(`"auto_publish": true`), 1)
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Load(p.ID); err == nil {
		t.Fatal("modified reviewed choice accepted")
	}
}

type confirmationReader struct {
	input   *strings.Reader
	change  func()
	changed bool
}

func (r *confirmationReader) Read(b []byte) (int, error) {
	if !r.changed {
		r.changed = true
		r.change()
	}
	return r.input.Read(b)
}

func TestConfirmationPauseRechecksRelease(t *testing.T) {
	for _, change := range []string{"notes", "package", "review", "unknown status", "duplicate", "plan"} {
		t.Run(change, func(t *testing.T) {
			f := &fakeAPI{visible: true}
			m, p := fixture(t, f)
			p, err := m.Upload(context.Background(), p.ID)
			if err != nil {
				t.Fatal(err)
			}
			input := &confirmationReader{input: strings.NewReader("y\ny\n"), change: func() {
				switch change {
				case "notes":
					err = os.WriteFile(p.Notes.Path, []byte("Changed while reviewing"), 0600)
				case "package":
					err = os.WriteFile(p.Package.Path, []byte("Changed archive"), 0600)
				case "review":
					f.versions = []model.Version{{ID: "new", Version: "0.9", Status: "review"}}
				case "unknown status":
					f.versions = []model.Version{{ID: "new", Version: "0.9", Status: "reviewed"}}
				case "duplicate":
					f.versions = []model.Version{{ID: "new", Version: p.Package.Manifest.Version, Status: "PUBLISHED"}}
				case "plan":
					changed := p
					changed.AutoPublish = !p.AutoPublish
					changed.Digest = digest(changed)
					err = m.save(changed, false)
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			var output bytes.Buffer
			if _, err = m.Submit(context.Background(), p.ID, ConfirmOptions{IsTerminal: true, Input: input, Output: &output}); err == nil || f.submitCalls != 0 {
				t.Fatal("changes during confirmation were not refused")
			}
			loaded, e := m.Load(p.ID)
			if e != nil {
				t.Fatal(e)
			}
			if loaded.State != Uploaded {
				t.Fatal("preflight failure marked uncertain")
			}
		})
	}
}

func TestReconcileRejectsDuplicateRemoteVersion(t *testing.T) {
	f := &fakeAPI{}
	m, p := fixture(t, f)
	if _, err := m.Upload(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	p, err := m.Submit(context.Background(), p.ID, ConfirmOptions{Yes: true})
	if err == nil || p.State != SubmissionUnknown {
		t.Fatal("expected uncertain status")
	}
	f.versions = []model.Version{{ID: "remote", Version: p.Package.Manifest.Version, Status: "PENDING_REVIEW"}, {ID: "conflict", Version: p.Package.Manifest.Version, Status: "PUBLISHED"}}
	if _, err = m.Status(context.Background(), p.ID); err == nil {
		t.Fatal("conflicting remote versions accepted")
	}
	p, err = m.Load(p.ID)
	if err != nil || p.State != SubmissionUnknown {
		t.Fatal("conflicting reconciliation changed state")
	}
}

type notSubmittedFailure struct{ known bool }

func (e *notSubmittedFailure) Error() string      { return "synthetic preflight failure" }
func (e *notSubmittedFailure) NotSubmitted() bool { return e.known }

func TestKnownNotDispatchedAllowsRetry(t *testing.T) {
	f := &fakeAPI{noID: true, submitError: fmt.Errorf("wrapped preflight: %w", &notSubmittedFailure{known: true})}
	m, p := fixture(t, f)
	p, err := m.Upload(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	staging := p.Upload
	reviewed := p.Digest
	f.onSubmit = func() {
		loaded, e := m.Load(p.ID)
		if e != nil || loaded.State != SubmissionUnknown {
			t.Fatalf("uncertainty must still precede API call: %v", e)
		}
	}
	p, err = m.Submit(context.Background(), p.ID, ConfirmOptions{Yes: true})
	if err == nil || p.State != Uploaded || p.SubmissionID != "" || p.RemoteStatus != "" {
		t.Fatalf("preflight result not restored: %+v %v", p, err)
	}
	loaded, e := m.Load(p.ID)
	if e != nil {
		t.Fatal(e)
	}
	if loaded.State != Uploaded || hash(loaded.Upload) != hash(staging) || loaded.Digest != reviewed {
		t.Fatal("restoration altered reviewed artifacts or staging")
	}
	f.noID = false
	f.submitError = nil
	f.visible = true
	p, err = m.Submit(context.Background(), p.ID, ConfirmOptions{Yes: true})
	if err != nil || p.State != Submitted || f.submitCalls != 2 || f.uploadCalls != 1 {
		t.Fatalf("known-not-dispatched retry failed: %v", err)
	}
}

func TestNotSubmittedEvidenceMustBeUnambiguous(t *testing.T) {
	for _, tc := range []struct {
		name    string
		noID    bool
		failure error
	}{{"method false", true, &notSubmittedFailure{known: false}}, {"ordinary timeout", true, errors.New("timeout")}, {"contradictory returned ID", false, &notSubmittedFailure{known: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAPI{noID: tc.noID, submitError: tc.failure}
			m, p := fixture(t, f)
			if _, err := m.Upload(context.Background(), p.ID); err != nil {
				t.Fatal(err)
			}
			p, err := m.Submit(context.Background(), p.ID, ConfirmOptions{Yes: true})
			if err == nil || p.State != SubmissionUnknown {
				t.Fatal("uncertain failure restored to retryable state")
			}
			if _, err = m.Submit(context.Background(), p.ID, ConfirmOptions{Yes: true}); err == nil || f.submitCalls != 1 {
				t.Fatal("uncertain failure was retried")
			}
		})
	}
}
