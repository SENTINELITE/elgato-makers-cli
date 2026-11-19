package listing

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
)

type retainedDraftAPI struct{ *workflowAPI }

func (api *retainedDraftAPI) Submit(ctx context.Context, target model.Target, submission model.Submission) (model.SubmitResult, error) {
	result, err := api.workflowAPI.Submit(ctx, target, submission)
	api.versions[0].Status = "DRAFT"
	return result, err
}

func TestRetainedDraftListingKeepsCreationAndUploadCheckpoints(t *testing.T) {
	manager, plan, base := workflowFixture(t)
	api := &retainedDraftAPI{workflowAPI: base}
	manager = NewManager(manager.dir, api)
	observed, err := manager.Submit(context.Background(), plan.ID, release.ConfirmOptions{Yes: true})
	var retained *release.RetainedDraftError
	if !errors.As(err, &retained) {
		t.Fatalf("missing retained draft diagnostic: %v", err)
	}
	if observed.State != Uploaded || observed.Target.ProductID != "product" || observed.Target.VariantID != "variant" || observed.DraftPlanID == "" || observed.ReleasePlanID == "" || observed.PackageState != Done {
		t.Fatalf("listing lost checkpoint: %+v", observed)
	}
	writes := append([]string(nil), base.events...)
	restarted := NewManager(manager.dir, api)
	if _, err := restarted.Submit(context.Background(), plan.ID, release.ConfirmOptions{Yes: true}); err == nil {
		t.Fatal("retained draft was resubmitted")
	}
	if _, err := restarted.Status(context.Background(), plan.ID); !errors.As(err, &retained) {
		t.Fatalf("status lost retained draft diagnostic: %v", err)
	}
	if !reflect.DeepEqual(writes, base.events) {
		t.Fatal("retry or reconciliation recreated, uploaded, or resubmitted")
	}
	// Older journals may already say submitted. Exact draft readback repairs
	// that local assertion without losing the immutable child identifiers.
	old, err := restarted.Load(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	old.State = Submitted
	if err := restarted.save(old, false); err != nil {
		t.Fatal(err)
	}
	repaired, err := restarted.Status(context.Background(), plan.ID)
	if !errors.As(err, &retained) || repaired.State != Uploaded || repaired.Target != observed.Target || repaired.DraftPlanID != observed.DraftPlanID || repaired.ReleasePlanID != observed.ReleasePlanID {
		t.Fatalf("older listing journal was not repaired: %+v %v", repaired, err)
	}
	if !reflect.DeepEqual(writes, base.events) {
		t.Fatal("repair dispatched a mutation")
	}
}
