package release

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

type receiptAPI struct {
	*fakeAPI
	versionsCalls   int
	observedTargets []model.Target
	onReadback      func()
	readbackErr     error
}

func (f *receiptAPI) Versions(_ context.Context, target model.Target) ([]model.Version, error) {
	f.versionsCalls++
	f.observedTargets = append(f.observedTargets, target)
	if f.submitCalls > 0 && f.onReadback != nil {
		f.onReadback()
	}
	if f.submitCalls > 0 && f.readbackErr != nil {
		return nil, f.readbackErr
	}
	return f.versions, f.versionsError
}
func (f *receiptAPI) Submit(_ context.Context, target model.Target, submission model.Submission) (model.SubmitResult, error) {
	f.submitCalls++
	f.versions = []model.Version{{ID: "canonical-version", VariantID: target.VariantID, Version: "1", Status: "review", ReleaseNotes: strings.TrimRight(submission.Notes.Markdown, "\r\n"), AutoPublish: &submission.AutoPublish}}
	return model.SubmitResult{ID: "canonical-version", ReceiptID: "opaque-receipt"}, nil
}
func receiptFixture(t *testing.T, api API) (*Manager, Plan) {
	t.Helper()
	_, base := fixture(t, &fakeAPI{})
	manifest := base.Package.Manifest
	manifest.Version = "1.0.0.0"
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(base.Package.Path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(out)
	entry, err := archive.Create("com.example.test.sdPlugin/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = entry.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err = out.Close(); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(filepath.Join(t.TempDir(), "state"), api)
	plan, err := manager.Prepare(context.Background(), base.Target, base.Package.Path, base.Notes.Path, true)
	if err != nil {
		t.Fatal(err)
	}
	return manager, plan
}
func uncertainReceiptFixture(t *testing.T) (*Manager, Plan, *receiptAPI) {
	t.Helper()
	api := &receiptAPI{fakeAPI: &fakeAPI{}}
	manager, plan := receiptFixture(t, api)
	uploaded, err := manager.Upload(context.Background(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	uploaded.State = SubmissionUnknown
	uploaded.SubmissionID = "opaque-receipt"
	if err = manager.save(uploaded, false); err != nil {
		t.Fatal(err)
	}
	plan, err = manager.Load(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	api.versions = []model.Version{{ID: "canonical-version", VariantID: plan.Target.VariantID, Version: "1", Status: "review", ReleaseNotes: strings.TrimRight(plan.Notes.Markdown, "\r\n"), AutoPublish: &plan.AutoPublish}}
	return manager, plan, api
}

func TestSubmitPersistsCanonicalVersionAndOriginalReceiptBeforeReadback(t *testing.T) {
	api := &receiptAPI{fakeAPI: &fakeAPI{}}
	manager, plan := receiptFixture(t, api)
	if _, err := manager.Upload(context.Background(), plan.ID); err != nil {
		t.Fatal(err)
	}
	observed := false
	api.onReadback = func() {
		saved, err := manager.Load(plan.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.State != SubmissionUnknown || saved.SubmissionID != "canonical-version" || saved.SubmissionReceiptID != "opaque-receipt" {
			t.Fatal("IDs not durable before remote reconciliation", saved.State, saved.SubmissionID, saved.SubmissionReceiptID)
		}
		observed = true
	}
	submitted, err := manager.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if !observed || submitted.State != Submitted || submitted.SubmissionID != "canonical-version" || submitted.SubmissionReceiptID != "opaque-receipt" {
		t.Fatal("canonical submission lost response receipt")
	}
	api.onReadback = nil
	saved, err := NewManager(manager.dir, api).Status(context.Background(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.SubmissionReceiptID != "opaque-receipt" || saved.SubmissionID != "canonical-version" {
		t.Fatal("status lost receipt audit")
	}
	if _, err = manager.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true}); err == nil || api.submitCalls != 1 || api.uploadCalls != 1 {
		t.Fatal("submitted receipt retried a remote write")
	}
}

func TestCanonicalReceiptSurvivesFailedReadbackWithoutKnownIDFallback(t *testing.T) {
	api := &receiptAPI{fakeAPI: &fakeAPI{}}
	manager, plan := receiptFixture(t, api)
	if _, err := manager.Upload(context.Background(), plan.ID); err != nil {
		t.Fatal(err)
	}
	api.readbackErr = errors.New("synthetic unavailable submission readback")
	if _, err := manager.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true}); err == nil {
		t.Fatal("expected readback uncertainty")
	}
	saved, err := manager.Load(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != SubmissionUnknown || saved.SubmissionID != "canonical-version" || saved.SubmissionReceiptID != "opaque-receipt" {
		t.Fatal("durable IDs lost after GET failure")
	}
	api.readbackErr = nil
	api.versions[0].ID = "different-canonical-version"
	if _, err = manager.Status(context.Background(), plan.ID); err == nil {
		t.Fatal("known canonical ID silently fell back to version number")
	}
	after, err := manager.Load(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Checksum != saved.Checksum {
		t.Fatal("ID conflict changed the durable unknown record")
	}
	if _, err = manager.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true}); err == nil || api.submitCalls != 1 {
		t.Fatal("uncertain canonical submission retried")
	}
}

func TestExplicitReceiptRecoveryIsReadOnlyAndPreservesOpaqueID(t *testing.T) {
	manager, plan, api := uncertainReceiptFixture(t)
	writesBefore := api.uploadCalls
	recovered, err := NewManager(manager.dir, api).ReconcileAcknowledgedVersion(context.Background(), plan.ID, "opaque-receipt", "canonical-version")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != Submitted || recovered.SubmissionID != "canonical-version" || recovered.SubmissionReceiptID != "opaque-receipt" || recovered.RemoteStatus != "review" {
		t.Fatal("receipt mapping not audited")
	}
	if api.submitCalls != 0 || api.uploadCalls != writesBefore {
		t.Fatal("receipt recovery dispatched a remote mutation")
	}
	if len(api.observedTargets) != 2 || api.observedTargets[1] != plan.Target {
		t.Fatal("recovery did not read the exact scoped target", api.observedTargets)
	}
	if _, err = manager.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true}); err == nil || api.submitCalls != 0 {
		t.Fatal("recovered receipt allowed resubmission")
	}
	saved, err := manager.Load(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.SubmissionReceiptID != "opaque-receipt" || saved.SubmissionID != "canonical-version" {
		t.Fatal("recovery was not durable")
	}
}

func TestExplicitReceiptRecoveryRejectsConflictsWithoutChangingUnknownPlan(t *testing.T) {
	for _, scenario := range []string{"stored-id", "receipt-conflict", "uploaded", "submitted", "blank-id", "same-id", "wrong-variant", "missing-variant", "wrong-number", "duplicate-normalized", "duplicate-id", "published", "draft", "notes-body", "notes-space", "autopublish-missing", "autopublish-different", "old-id-is-version", "read-error"} {
		t.Run(scenario, func(t *testing.T) {
			manager, plan, api := uncertainReceiptFixture(t)
			expected, canonical := "opaque-receipt", "canonical-version"
			switch scenario {
			case "stored-id":
				expected = "changed-receipt"
			case "receipt-conflict":
				plan.SubmissionReceiptID = "different-stored-receipt"
			case "uploaded":
				plan.State = Uploaded
			case "submitted":
				plan.State = Submitted
			case "blank-id":
				canonical = ""
			case "same-id":
				canonical = expected
			case "wrong-variant":
				api.versions[0].VariantID = "other-variant"
			case "missing-variant":
				api.versions[0].VariantID = ""
			case "wrong-number":
				api.versions[0].Version = "1.0.0.1"
			case "duplicate-normalized":
				other := api.versions[0]
				other.ID = "duplicate"
				other.Version = "1.0"
				api.versions = append(api.versions, other)
			case "duplicate-id":
				api.versions = append(api.versions, api.versions[0])
			case "published":
				api.versions[0].Status = "PUBLISHED"
			case "draft":
				api.versions[0].Status = "DRAFT"
			case "notes-body":
				api.versions[0].ReleaseNotes += " changed"
			case "notes-space":
				api.versions[0].ReleaseNotes += " "
			case "autopublish-missing":
				api.versions[0].AutoPublish = nil
			case "autopublish-different":
				different := !plan.AutoPublish
				api.versions[0].AutoPublish = &different
			case "old-id-is-version":
				other := api.versions[0]
				other.ID = "opaque-receipt"
				other.Version = "2"
				api.versions = append(api.versions, other)
			case "read-error":
				api.versionsError = errors.New("synthetic version read failed")
			}
			if err := manager.save(plan, false); err != nil {
				t.Fatal(err)
			}
			before, err := manager.Load(plan.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = manager.ReconcileAcknowledgedVersion(context.Background(), plan.ID, expected, canonical); err == nil {
				t.Fatal("unsafe receipt recovery accepted")
			}
			after, err := manager.Load(plan.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Checksum != before.Checksum || api.submitCalls != 0 || api.uploadCalls != 1 {
				t.Fatal("failed recovery changed local evidence or remote state")
			}
		})
	}
}

func TestNormalizedVersionGuardAndReconcileRejectDuplicates(t *testing.T) {
	manager, plan, api := uncertainReceiptFixture(t)
	if err := guard([]model.Version{{ID: "existing", VariantID: plan.Target.VariantID, Version: "1", Status: "published"}}, plan); err == nil {
		t.Fatal("normalized existing version bypassed upload guard")
	}
	// Normal status remains strict about a known receipt ID even when a unique
	// numerically matching canonical version exists.
	if _, err := manager.Status(context.Background(), plan.ID); err == nil {
		t.Fatal("normal status inferred an unproved receipt mapping")
	}
	plan.SubmissionID = "canonical-version"
	plan.SubmissionReceiptID = "opaque-receipt"
	if err := manager.save(plan, false); err != nil {
		t.Fatal(err)
	}
	duplicate := api.versions[0]
	duplicate.ID = "duplicate"
	duplicate.Version = "1.0.0"
	api.versions = append(api.versions, duplicate)
	if _, err := manager.Status(context.Background(), plan.ID); err == nil {
		t.Fatal("normalized duplicate version reconciled")
	}
	saved, err := manager.Load(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != SubmissionUnknown {
		t.Fatal("duplicate versions promoted uncertainty")
	}
}

func TestSubmissionReceiptOptionalFieldPreservesLegacyChecksum(t *testing.T) {
	_, plan := fixture(t, &fakeAPI{})
	plan.Checksum = ""
	typ, value := reflect.TypeOf(plan), reflect.ValueOf(plan)
	var fields []reflect.StructField
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Name != "SubmissionReceiptID" {
			fields = append(fields, typ.Field(i))
		}
	}
	old := reflect.New(reflect.StructOf(fields)).Elem()
	for i := 0; i < old.NumField(); i++ {
		old.Field(i).Set(value.FieldByName(old.Type().Field(i).Name))
	}
	raw, err := json.Marshal(old.Interface())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if checksum(plan) != hex.EncodeToString(sum[:]) {
		t.Fatal("optional receipt field changed old plan checksums")
	}
}
