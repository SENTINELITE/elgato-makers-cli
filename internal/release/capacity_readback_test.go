package release

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

func TestRetainedDraftPreservesAcknowledgedIDsAndBlocksRetry(t *testing.T) {
	api := &receiptAPI{fakeAPI: &fakeAPI{}}
	manager, plan := receiptFixture(t, api)
	if _, err := manager.Upload(context.Background(), plan.ID); err != nil {
		t.Fatal(err)
	}
	api.onReadback = func() { api.versions[0].Status = "DRAFT" }
	observed, err := manager.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true})
	var retained *RetainedDraftError
	if !errors.As(err, &retained) || !retained.RetainedAsDraft() {
		t.Fatalf("missing draft diagnostic: %v", err)
	}
	if strings.Contains(err.Error(), "capacity") || !strings.Contains(err.Error(), "reason is unknown") {
		t.Fatalf("draft readback fabricated a capacity cause: %v", err)
	}
	if observed.State != SubmissionUnknown || observed.RemoteStatus != "DRAFT" || observed.SubmissionID != "canonical-version" || observed.SubmissionReceiptID != "opaque-receipt" {
		t.Fatalf("draft lost durable evidence: %+v", observed)
	}
	restarted := NewManager(manager.dir, api)
	saved, err := restarted.Load(plan.ID)
	if err != nil || saved.Checksum != checksum(saved) || saved.Checksum != observed.Checksum || saved.SubmissionID != observed.SubmissionID || saved.SubmissionReceiptID != observed.SubmissionReceiptID {
		t.Fatal("draft evidence was not saved", err)
	}
	if _, err = restarted.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true}); err == nil {
		t.Fatal("draft allowed automatic resubmission")
	}
	if _, err = restarted.Status(context.Background(), plan.ID); !errors.As(err, &retained) {
		t.Fatalf("status lost draft diagnostic: %v", err)
	}
	if api.submitCalls != 1 || api.uploadCalls != 1 {
		t.Fatal("draft reconciliation dispatched a write")
	}
	api.onReadback = nil
	api.versions[0].Status = "PENDING_REVIEW"
	reconciled, err := restarted.Status(context.Background(), plan.ID)
	if err != nil || reconciled.State != Submitted || reconciled.SubmissionID != observed.SubmissionID || reconciled.SubmissionReceiptID != observed.SubmissionReceiptID {
		t.Fatal("read-only review reconciliation failed", err)
	}
	if api.submitCalls != 1 || api.uploadCalls != 1 {
		t.Fatal("review transition dispatched a write")
	}
}

func TestReadbackReviewApprovedAndPublishedRemainSuccessful(t *testing.T) {
	for _, status := range []string{"review", "PENDING_REVIEW", "APPROVED", "PUBLISHED"} {
		t.Run(status, func(t *testing.T) {
			api := &receiptAPI{fakeAPI: &fakeAPI{}}
			manager, plan := receiptFixture(t, api)
			if _, err := manager.Upload(context.Background(), plan.ID); err != nil {
				t.Fatal(err)
			}
			api.onReadback = func() { api.versions[0].Status = status }
			observed, err := manager.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true})
			if err != nil || observed.State != Submitted || observed.RemoteStatus != status {
				t.Fatalf("recognized readback failed: %+v %v", observed, err)
			}
		})
	}
}

func TestInterruptedSubmissionAdoptsExactDraftWithoutClaimingReview(t *testing.T) {
	api := &fakeAPI{noID: true, submitError: errors.New("synthetic interrupted response")}
	manager, plan := fixture(t, api)
	if _, err := manager.Upload(context.Background(), plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true}); err == nil {
		t.Fatal("interrupted submission became successful")
	}
	api.versions = []model.Version{{ID: "retained-version", VariantID: plan.Target.VariantID, Version: plan.Package.Manifest.Version, Status: "DRAFT"}}
	if _, err := manager.Status(context.Background(), plan.ID); err == nil {
		t.Fatal("draft adopted without exact notes and publication evidence")
	}
	api.versions[0].ReleaseNotes = plan.Notes.Markdown
	api.versions[0].AutoPublish = &plan.AutoPublish
	reconciled, err := NewManager(manager.dir, api).Status(context.Background(), plan.ID)
	var retained *RetainedDraftError
	if !errors.As(err, &retained) || reconciled.State != SubmissionUnknown || reconciled.SubmissionID != "retained-version" || reconciled.RemoteStatus != "DRAFT" {
		t.Fatalf("interrupted draft lost checkpoint: %+v %v", reconciled, err)
	}
	if _, err := manager.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true}); err == nil || api.submitCalls != 1 {
		t.Fatal("interrupted draft was resubmitted")
	}
}

func TestMissingReviewMetadataPreservesSubmissionUncertainty(t *testing.T) {
	api := &receiptAPI{fakeAPI: &fakeAPI{}}
	manager, plan := receiptFixture(t, api)
	if _, err := manager.Upload(context.Background(), plan.ID); err != nil {
		t.Fatal(err)
	}
	api.onReadback = func() { api.versions[0].Status = "" }
	if _, err := manager.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true}); err == nil {
		t.Fatal("missing status claimed submission")
	}
	observed, err := NewManager(manager.dir, api).Load(plan.ID)
	if err != nil || observed.State != SubmissionUnknown || observed.SubmissionID != "canonical-version" || observed.SubmissionReceiptID != "opaque-receipt" || observed.RemoteStatus != "" {
		t.Fatalf("missing metadata lost uncertainty: %+v %v", observed, err)
	}
	if _, err := manager.Submit(context.Background(), plan.ID, ConfirmOptions{Yes: true}); err == nil || api.submitCalls != 1 || api.uploadCalls != 1 {
		t.Fatal("missing status permitted a duplicate write")
	}
}
