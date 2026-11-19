// Package release manages deliberate, locally recorded releases. Upload and
// submission are separate operations; uncertain submissions cannot be retried.
package release

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/artifact"
	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

type API interface {
	Versions(context.Context, model.Target) ([]model.Version, error)
	Upload(context.Context, model.Target, model.Package) (model.UploadResult, error)
	Submit(context.Context, model.Target, model.Submission) (model.SubmitResult, error)
}

const (
	Prepared          = "prepared"
	Uploaded          = "uploaded"
	SubmissionUnknown = "submission_unknown"
	Submitted         = "submitted"
)

var ErrCancelled = errors.New("submission cancelled")

// RetainedDraftError reports exact readback of the acknowledged version. A
// draft alone does not establish why review was not entered (including whether
// the new-product capacity limit applied). Reconciliation is read-only; callers
// must not retry submission or recreate the product to resolve this condition.
type RetainedDraftError struct{}

func (*RetainedDraftError) Error() string {
	return "version is retained as draft; review is not confirmed and the reason is unknown; identifiers are saved; do not resubmit or recreate; run release status to reconcile"
}
func (*RetainedDraftError) RetainedAsDraft() bool { return true }

// UploadNotStartedError proves this attempt never reached the upload API.
type UploadNotStartedError struct{ Cause error }

func (e *UploadNotStartedError) Error() string {
	return "package upload was not started: " + e.Cause.Error()
}
func (e *UploadNotStartedError) Unwrap() error     { return e.Cause }
func (e *UploadNotStartedError) NotUploaded() bool { return true }

var validID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Plan struct {
	ID                  string             `json:"id"`
	CreatedAt           time.Time          `json:"created_at"`
	Target              model.Target       `json:"target"`
	Package             model.Package      `json:"package"`
	Notes               model.Notes        `json:"notes"`
	AutoPublish         bool               `json:"auto_publish"`
	Digest              string             `json:"reviewed_digest"`
	State               string             `json:"state"`
	Upload              model.UploadResult `json:"upload"`
	SubmissionID        string             `json:"submission_id,omitempty"`
	SubmissionReceiptID string             `json:"submission_receipt_id,omitempty"`
	RemoteStatus        string             `json:"remote_status,omitempty"`
	Checksum            string             `json:"checksum"`
}

type ConfirmOptions struct {
	Yes        bool
	IsTerminal bool
	Input      io.Reader
	Output     io.Writer
}

type Manager struct {
	dir string
	api API
}

func NewManager(dir string, api API) *Manager { return &Manager{dir: dir, api: api} }

func hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func digest(p Plan) string {
	return hash(struct {
		Target      model.Target
		Package     model.Package
		Notes       model.Notes
		AutoPublish bool
	}{p.Target, p.Package, p.Notes, p.AutoPublish})
}
func checksum(p Plan) string { p.Checksum = ""; return hash(p) }

func (m *Manager) init() error {
	return filesec.SecurePrivateDir(m.dir)
}

func (m *Manager) locked(fn func() (Plan, error)) (Plan, error) {
	if err := m.init(); err != nil {
		return Plan{}, err
	}
	path := filepath.Join(m.dir, "release.lock")
	f, err := filesec.OpenPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return Plan{}, fmt.Errorf("release state is locked; if a previous process died, verify it has stopped before removing %s: %w", path, err)
	}
	_, err = fmt.Fprintf(f, "pid=%d time=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	closeErr := f.Close()
	defer os.Remove(path)
	if err != nil {
		return Plan{}, err
	}
	if closeErr != nil {
		return Plan{}, closeErr
	}
	return fn()
}

func (m *Manager) save(p Plan, create bool) error {
	p.Checksum = checksum(p)
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	f, err := filesec.CreateTemp(m.dir, ".plan-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	path := filepath.Join(m.dir, p.ID+".json")
	if create {
		if err = os.Link(tmp, path); err != nil {
			return err
		}
	} else {
		if err = os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return filesec.SyncDir(m.dir)
}

func (m *Manager) Load(id string) (Plan, error) {
	if !validID.MatchString(id) {
		return Plan{}, errors.New("invalid release plan ID")
	}
	path := filepath.Join(m.dir, id+".json")
	info, err := os.Lstat(path)
	if err != nil {
		return Plan{}, err
	}
	if err = filesec.VerifyPrivateFile(path, info, false); err != nil {
		return Plan{}, errors.New("release plan must be a private regular file (0600)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, err
	}
	var p Plan
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(&p); err != nil {
		return Plan{}, fmt.Errorf("corrupt release plan: %w", err)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return Plan{}, errors.New("corrupt release plan: trailing data")
	}
	if p.ID != id || p.Digest != digest(p) || p.Checksum != checksum(p) {
		return Plan{}, errors.New("release plan integrity check failed")
	}
	switch p.State {
	case Prepared, Uploaded, SubmissionUnknown, Submitted:
	default:
		return Plan{}, errors.New("invalid release state")
	}
	if p.State != Prepared && (p.Upload.FileID == "" || p.Upload.FolderID == "") {
		return Plan{}, errors.New("release staging is missing")
	}
	return p, nil
}

func (m *Manager) Prepare(ctx context.Context, target model.Target, packagePath, notesPath string, autoPublish bool) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	pkg, err := artifact.Inspect(packagePath)
	if err != nil {
		return Plan{}, err
	}
	notes, err := artifact.ReadNotes(notesPath)
	if err != nil {
		return Plan{}, err
	}
	if target.OrgID == "" || target.ProductID == "" || target.VariantID == "" || target.UUID == "" {
		return Plan{}, errors.New("exact organization, product, variant, and package UUID are required")
	}
	if target.UUID != pkg.Manifest.UUID {
		return Plan{}, errors.New("target UUID does not match package manifest UUID")
	}
	pkg.Path, err = filepath.Abs(pkg.Path)
	if err != nil {
		return Plan{}, err
	}
	notes.Path, err = filepath.Abs(notes.Path)
	if err != nil {
		return Plan{}, err
	}
	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return Plan{}, err
	}
	p := Plan{ID: hex.EncodeToString(b), CreatedAt: time.Now().UTC(), Target: target, Package: pkg, Notes: notes, AutoPublish: autoPublish, State: Prepared}
	p.Digest = digest(p)
	return m.locked(func() (Plan, error) {
		if err := m.save(p, true); err != nil {
			return Plan{}, err
		}
		return m.Load(p.ID)
	})
}

func verify(p Plan) error {
	pkg, err := artifact.Inspect(p.Package.Path)
	if err != nil {
		return err
	}
	notes, err := artifact.ReadNotes(p.Notes.Path)
	if err != nil {
		return err
	}
	if hash(pkg) != hash(p.Package) || hash(notes) != hash(p.Notes) {
		return errors.New("package or release notes changed since preparation; prepare a new plan")
	}
	return nil
}

func guard(vs []model.Version, p Plan) error {
	return model.CheckVersionMutation(vs, p.Target.VariantID, p.Package.Manifest.Version)
}

// A new plan must not bypass a prior uncertain request whose remote version
// has not become visible yet.
func (m *Manager) guardLocal(p Plan) error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if id == p.ID {
			continue
		}
		other, err := m.Load(id)
		if err != nil {
			return fmt.Errorf("cannot check existing release state: %w", err)
		}
		if other.Target.OrgID != p.Target.OrgID || other.Target.ProductID != p.Target.ProductID || other.Target.VariantID != p.Target.VariantID {
			continue
		}
		if other.State == SubmissionUnknown {
			return fmt.Errorf("plan %s has an uncertain submission for this target; reconcile it with release status first", other.ID)
		}
		if other.State == Submitted && model.SameStreamDeckVersion(other.Package.Manifest.Version, p.Package.Manifest.Version) {
			return fmt.Errorf("plan %s already submitted this version", other.ID)
		}
	}
	return nil
}

func (m *Manager) Upload(ctx context.Context, id string) (plan Plan, err error) {
	apiStarted := false
	defer func() {
		if err != nil && !apiStarted {
			err = &UploadNotStartedError{Cause: err}
		}
	}()
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if p.State != Prepared {
			return p, fmt.Errorf("upload requires prepared state; current state: %s", p.State)
		}
		if err = verify(p); err != nil {
			return p, err
		}
		if err = m.guardLocal(p); err != nil {
			return p, err
		}
		vs, err := m.api.Versions(ctx, p.Target)
		if err != nil {
			return p, err
		}
		if err = guard(vs, p); err != nil {
			return p, err
		}
		apiStarted = true
		u, err := m.api.Upload(ctx, p.Target, p.Package)
		if err != nil {
			return p, err
		}
		if u.FileID == "" || u.FolderID == "" {
			return p, errors.New("upload did not return complete staging identifiers")
		}
		if hash(u.Manifest) != hash(p.Package.Manifest) {
			return p, errors.New("uploaded manifest differs from reviewed package")
		}
		p.Upload = u
		p.State = Uploaded
		if err = m.save(p, false); err != nil {
			return p, err
		}
		return m.Load(id)
	})
}

func confirm(p Plan, o ConfirmOptions) error {
	if o.Yes {
		return nil
	}
	if !o.IsTerminal {
		return errors.New("submission requires a terminal with two confirmations, or explicit --yes on release submit")
	}
	if o.Input == nil || o.Output == nil {
		return errors.New("confirmation input/output missing")
	}
	fmt.Fprintf(o.Output, "Review plan %s\nTarget: %s / %s / %s (UUID %s)\nVersion: %s\nPackage SHA-256: %s\nReviewed digest: %s\nStaging: %s / %s\nAuto-publish: %t\n", p.ID, p.Target.OrgID, p.Target.ProductID, p.Target.VariantID, p.Target.UUID, p.Package.Manifest.Version, p.Package.SHA256, p.Digest, p.Upload.FolderID, p.Upload.FileID, p.AutoPublish)
	r := bufio.NewReader(o.Input)
	for _, prompt := range []string{"Submit this exact release for production review? [y/N]: ", "Confirm production submission now? [y/N]: "} {
		fmt.Fprint(o.Output, prompt)
		line, err := r.ReadString('\n')
		if err != nil {
			return ErrCancelled
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line != "y" && line != "yes" {
			return ErrCancelled
		}
	}
	return nil
}

func (m *Manager) Submit(ctx context.Context, id string, o ConfirmOptions) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if p.State != Uploaded {
			return p, fmt.Errorf("submission requires uploaded state; current state: %s; use release status to reconcile uncertain submissions", p.State)
		}
		if err = verify(p); err != nil {
			return p, err
		}
		if err = m.guardLocal(p); err != nil {
			return p, err
		}
		vs, err := m.api.Versions(ctx, p.Target)
		if err != nil {
			return p, err
		}
		if err = guard(vs, p); err != nil {
			return p, err
		}
		if err = confirm(p, o); err != nil {
			return p, err
		}
		// Confirmation can pause indefinitely. Recheck the exact plan, files,
		// and remote release gate after the person finishes reviewing it.
		current, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if hash(current) != hash(p) {
			return p, errors.New("release plan changed during confirmation; refusing submission")
		}
		if err = verify(p); err != nil {
			return p, err
		}
		vs, err = m.api.Versions(ctx, p.Target)
		if err != nil {
			return p, err
		}
		if err = guard(vs, p); err != nil {
			return p, err
		}
		if err = ctx.Err(); err != nil {
			return p, err
		}
		// Persist uncertainty before the request can leave this process.
		p.State = SubmissionUnknown
		if err = m.save(p, false); err != nil {
			return p, err
		}
		r, err := m.api.Submit(ctx, p.Target, model.Submission{Package: p.Package, Notes: p.Notes, AutoPublish: p.AutoPublish, Upload: p.Upload})
		if r.ID != "" || r.ReceiptID != "" {
			p.SubmissionReceiptID = r.ReceiptID
			p.SubmissionID = r.ID
			if e := m.save(p, false); e != nil {
				return p, fmt.Errorf("submission uncertain; returned ID %s could not be saved: %w", r.ID, e)
			}
		}
		if err != nil {
			var notSubmitted interface{ NotSubmitted() bool }
			if r.ID == "" && r.ReceiptID == "" && errors.As(err, &notSubmitted) && notSubmitted.NotSubmitted() {
				// Only the API can establish that its create-version request
				// was never dispatched. Preserve uncertainty on contradictory
				// evidence (a returned version ID) or any other failure.
				restored := p
				restored.State = Uploaded
				restored.SubmissionID = ""
				restored.SubmissionReceiptID = ""
				restored.RemoteStatus = ""
				if saveErr := m.save(restored, false); saveErr != nil {
					return p, fmt.Errorf("submission was not dispatched, but uploaded state could not be restored; run release status: %w", saveErr)
				}
				loaded, loadErr := m.Load(id)
				if loadErr != nil {
					return restored, loadErr
				}
				return loaded, fmt.Errorf("submission was not dispatched; plan remains uploaded and may be retried after resolving the preflight error: %w", err)
			}
			return p, fmt.Errorf("submission outcome unknown; do not retry; run release status: %w", err)
		}
		if r.ID == "" {
			return p, errors.New("submission outcome unknown: response omitted version ID; run release status")
		}
		return m.reconcile(ctx, p)
	})
}

func (m *Manager) Status(ctx context.Context, id string) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if p.State == Prepared || p.State == Uploaded {
			return p, nil
		}
		return m.reconcile(ctx, p)
	})
}

func (m *Manager) reconcile(ctx context.Context, p Plan) (Plan, error) {
	if p.SubmissionID == "" && p.SubmissionReceiptID != "" {
		return p, errors.New("submission receipt has no canonical version ID; explicit receipt reconciliation is required")
	}
	vs, err := m.api.Versions(ctx, p.Target)
	if err != nil {
		return p, fmt.Errorf("cannot verify submission status; stored state %s, ID %s: %w", p.State, p.SubmissionID, err)
	}
	var matches []model.Version
	var versionMatches int
	for _, v := range vs {
		if v.VariantID != "" && v.VariantID != p.Target.VariantID {
			continue
		}
		if model.SameStreamDeckVersion(v.Version, p.Package.Manifest.Version) {
			versionMatches++
		}
		if p.SubmissionID != "" {
			if v.ID == p.SubmissionID {
				matches = append(matches, v)
			}
		} else if model.SameStreamDeckVersion(v.Version, p.Package.Manifest.Version) {
			matches = append(matches, v)
		}
	}
	if versionMatches > 1 {
		return p, errors.New("conflicting remote versions share the reviewed version number; do not retry")
	}
	if len(matches) != 1 {
		return p, fmt.Errorf("submission remains %s (stored ID %s): expected one matching remote version, found %d; do not retry", p.State, p.SubmissionID, len(matches))
	}
	v := matches[0]
	lifecycle := model.ParseVersionLifecycle(v.Status)
	if v.ID == "" || !model.SameStreamDeckVersion(v.Version, p.Package.Manifest.Version) || lifecycle == model.VersionLifecycleUnknown {
		return p, errors.New("remote submission conflicts with reviewed version; do not retry")
	}
	if p.SubmissionID == "" && (!model.SameListingDescription(v.ReleaseNotes, p.Notes.Markdown) || v.AutoPublish == nil || *v.AutoPublish != p.AutoPublish) {
		return p, errors.New("remote version lacks exact release notes and auto-publish evidence; submission remains unknown")
	}
	p.SubmissionID = v.ID
	p.RemoteStatus = v.Status
	if lifecycle == model.VersionLifecycleDraft {
		// A successful action acknowledgement and a version ID do not prove
		// entry into review. Keep the uncertainty gate and all remote IDs.
		p.State = SubmissionUnknown
		if err = m.save(p, false); err != nil {
			return p, err
		}
		loaded, loadErr := m.Load(p.ID)
		if loadErr != nil {
			return p, loadErr
		}
		return loaded, &RetainedDraftError{}
	}
	p.State = Submitted
	if err = m.save(p, false); err != nil {
		return p, err
	}
	return m.Load(p.ID)
}

// ReconcileAcknowledgedVersion repairs only a local receipt/version ID mapping.
// Callers must supply a mapping independently proved by the original successful
// submission response. Ordinary status never guesses another ID for a known ID.
func (m *Manager) ReconcileAcknowledgedVersion(ctx context.Context, id, expectedStoredID, canonicalVersionID string) (Plan, error) {
	if strings.TrimSpace(expectedStoredID) == "" || strings.TrimSpace(canonicalVersionID) == "" || expectedStoredID != strings.TrimSpace(expectedStoredID) || canonicalVersionID != strings.TrimSpace(canonicalVersionID) || expectedStoredID == canonicalVersionID {
		return Plan{}, errors.New("distinct exact stored receipt and canonical version identifiers are required")
	}
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if p.State != SubmissionUnknown || p.SubmissionID != expectedStoredID || p.SubmissionReceiptID != "" && p.SubmissionReceiptID != expectedStoredID {
			return p, errors.New("receipt recovery requires an uncertain submission with the exact unchanged stored receipt identifier")
		}
		if p.Target.OrgID == "" || p.Target.ProductID == "" || p.Target.VariantID == "" || p.Target.UUID == "" {
			return p, errors.New("receipt recovery requires an exact scoped release target")
		}
		if err = ctx.Err(); err != nil {
			return p, err
		}
		versions, err := m.api.Versions(ctx, p.Target)
		if err != nil {
			return p, err
		}
		var matches []model.Version
		sameVersion := 0
		for _, version := range versions {
			if version.ID == expectedStoredID {
				return p, errors.New("stored receipt identifier is also a remote version identifier; refusing conflicting recovery")
			}
			if version.ID == canonicalVersionID && version.VariantID != p.Target.VariantID {
				return p, errors.New("canonical version identifier belongs to a different or unrecognized variant")
			}
			if version.VariantID != p.Target.VariantID {
				continue
			}
			if model.SameStreamDeckVersion(version.Version, p.Package.Manifest.Version) {
				sameVersion++
			}
			if version.ID == canonicalVersionID {
				matches = append(matches, version)
			}
		}
		if len(matches) != 1 || sameVersion != 1 {
			return p, errors.New("receipt recovery requires exactly one scoped canonical version and one matching normalized version number")
		}
		version := matches[0]
		if !model.SameStreamDeckVersion(version.Version, p.Package.Manifest.Version) || model.ParseVersionLifecycle(version.Status) != model.VersionLifecycleReview || !model.SameListingDescription(version.ReleaseNotes, p.Notes.Markdown) || version.AutoPublish == nil || *version.AutoPublish != p.AutoPublish {
			return p, errors.New("canonical version lacks the exact reviewed number, review status, release notes, or auto-publish evidence")
		}
		p.SubmissionReceiptID = expectedStoredID
		p.SubmissionID = canonicalVersionID
		p.RemoteStatus = version.Status
		p.State = Submitted
		if err = m.save(p, false); err != nil {
			return p, err
		}
		return m.Load(id)
	})
}
