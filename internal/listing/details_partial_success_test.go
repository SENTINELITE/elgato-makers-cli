package listing

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

// The final metadata write is saved remotely before a subsequent pricing
// operation fails. Reconciliation reads that saved state without writing it.
type detailsPartialSuccessAPI struct {
	*uploadTagRetryAPI
	remoteDetails    maker.ListingDetailsInput
	reconciledInputs []maker.ListingDetailsInput
	reconcileErr     error
	finalWriteErr    error
}

func copyPartialListingDetails(input maker.ListingDetailsInput) maker.ListingDetailsInput {
	copy := input
	copy.LanguageIDs = append([]string(nil), input.LanguageIDs...)
	copy.CategoryIDs = append([]string(nil), input.CategoryIDs...)
	copy.ExtraTagIDs = append([]string(nil), input.ExtraTagIDs...)
	if input.Links != nil {
		copy.Links = make(map[string]string, len(input.Links))
		for label, url := range input.Links {
			copy.Links[label] = url
		}
	}
	return copy
}
func (f *detailsPartialSuccessAPI) UpdateListing(ctx context.Context, input maker.ListingDetailsInput) error {
	if err := f.uploadTagRetryAPI.UpdateListing(ctx, input); err != nil {
		return err
	}
	f.remoteDetails = copyPartialListingDetails(input)
	if len(input.ExtraTagIDs) > 0 {
		return f.finalWriteErr
	}
	return nil
}
func (f *detailsPartialSuccessAPI) ReconcileListing(_ context.Context, input maker.ListingDetailsInput) (bool, error) {
	f.reconciledInputs = append(f.reconciledInputs, copyPartialListingDetails(input))
	if f.reconcileErr != nil {
		return false, f.reconcileErr
	}
	return reflect.DeepEqual(input, f.remoteDetails), nil
}

func partialDetailsFixture(t *testing.T) (*Manager, Plan, *detailsPartialSuccessAPI) {
	t.Helper()
	cfg := fixture(t)
	cfg.Pricing = Pricing{Type: "paid", Amount: "6.99", Currency: "USD"}
	cfg.Links = map[string]string{"Support": "https://example.test/support"}
	prepared, err := PrepareConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	failure := &maker.AmbiguousListingError{Cause: errors.New("synthetic price creation failed after metadata was saved")}
	api := &detailsPartialSuccessAPI{uploadTagRetryAPI: &uploadTagRetryAPI{workflowAPI: &workflowAPI{}}, finalWriteErr: failure}
	manager := NewManager(t.TempDir(), api)
	plan, err := manager.Prepare(prepared, "org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Upload(context.Background(), plan.ID); !errors.Is(err, failure) {
		t.Fatalf("expected ambiguous final details failure, got %v", err)
	}
	stopped, err := manager.Load(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.DetailsState != Unknown || stopped.PackageState != Done || stopped.EarlyDetailsState != Done || stopped.State == Uploaded || stopped.EarlyDetails == nil || stopped.EarlyDetails.Price != "6.99" || stopped.EarlyDetails.IsFree {
		t.Fatal("partial success did not preserve paid/package checkpoints")
	}
	if !reflect.DeepEqual(api.events, []string{"create", "details", "package", "details"}) {
		t.Fatal("partial details failure did not stop before media", api.events)
	}
	for _, media := range stopped.Media {
		if media.State != Ready || media.Result.Acknowledged {
			t.Fatal("media ran after ambiguous details write")
		}
	}
	return manager, stopped, api
}

func TestPartialDetailsStatusReconcilesAndResumesWithoutRepeatingWrites(t *testing.T) {
	manager, stopped, api := partialDetailsFixture(t)
	releaseBefore, err := manager.releases().Load(stopped.ReleasePlanID)
	if err != nil {
		t.Fatal(err)
	}
	draftBefore, err := manager.drafts().Load(stopped.DraftPlanID)
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore := append([]string(nil), api.events...)
	// A fresh manager must recover solely from the durable unknown checkpoint.
	manager = NewManager(manager.dir, api)
	reconciled, err := manager.Status(context.Background(), stopped.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.DetailsState != Done || reconciled.PackageState != Done || reconciled.EarlyDetailsState != Done || reconciled.State == Uploaded {
		t.Fatal("status did not reconcile only the saved details step")
	}
	if !reflect.DeepEqual(api.events, eventsBefore) || len(api.detailWrites) != 2 {
		t.Fatal("read-only status repeated a mutation", api.events)
	}
	if len(api.reconciledInputs) != 1 || !reflect.DeepEqual(api.reconciledInputs[0], stopped.Details) {
		t.Fatal("status did not compare the exact final details checkpoint")
	}
	if reconciled.Target != stopped.Target || reconciled.ReleasePlanID != stopped.ReleasePlanID || reconciled.DraftPlanID != stopped.DraftPlanID {
		t.Fatal("status changed target or child plan ownership")
	}
	saved, err := manager.Load(stopped.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.DetailsState != Done {
		t.Fatal("reconciled details were not saved durably")
	}
	for _, media := range saved.Media {
		if media.State != Ready {
			t.Fatal("status promoted media without evidence")
		}
	}

	manager = NewManager(manager.dir, api)
	uploaded, err := manager.Upload(context.Background(), stopped.ID)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.State != Uploaded || uploaded.DetailsState != Done || uploaded.PackageState != Done {
		t.Fatal("resumed media did not finish the listing")
	}
	wantEvents := []string{"create", "details", "package", "details", "icon:1", "thumbnail:1", "gallery:1", "gallery:2", "gallery:3"}
	if !reflect.DeepEqual(api.events, wantEvents) || len(api.detailWrites) != 2 || len(api.packageTargets) != 1 {
		t.Fatal("reconciled resume duplicated create, details, or package work", api.events)
	}
	releaseAfter, err := manager.releases().Load(uploaded.ReleasePlanID)
	if err != nil {
		t.Fatal(err)
	}
	draftAfter, err := manager.drafts().Load(uploaded.DraftPlanID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(releaseBefore, releaseAfter) || !reflect.DeepEqual(draftBefore, draftAfter) {
		t.Fatal("details reconciliation changed acknowledged child plans")
	}
	if uploaded.Details.Price != "6.99" || uploaded.Details.IsFree || !reflect.DeepEqual(uploaded.Details.ExtraTagIDs, []string{"os-mac"}) || !reflect.DeepEqual(uploaded.Details.Links, stopped.Details.Links) {
		t.Fatal("resumed listing changed saved paid metadata")
	}
	for _, media := range uploaded.Media {
		if media.State != Done || !media.Result.Acknowledged || media.Result.MediaID == "" || media.Result.FileID == "" {
			t.Fatal("resumed listing omitted a media acknowledgment")
		}
	}
}

func TestPartialDetailsMismatchRemainsUnknownAndBlocksUpload(t *testing.T) {
	for _, mismatch := range []string{"price", "paid-free", "tags", "links", "read-error"} {
		t.Run(mismatch, func(t *testing.T) {
			manager, stopped, api := partialDetailsFixture(t)
			switch mismatch {
			case "price":
				api.remoteDetails.Price = "7.99"
			case "paid-free":
				api.remoteDetails.IsFree = true
			case "tags":
				api.remoteDetails.ExtraTagIDs = []string{"different-os"}
			case "links":
				api.remoteDetails.Links["Support"] = "https://example.test/different"
			case "read-error":
				api.reconcileErr = errors.New("synthetic unavailable metadata readback")
			}
			before := append([]string(nil), api.events...)
			manager = NewManager(manager.dir, api)
			if _, err := manager.Status(context.Background(), stopped.ID); err == nil {
				t.Fatal("unmatched final details were acknowledged")
			}
			saved, err := manager.Load(stopped.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.DetailsState != Unknown || saved.PackageState != Done || saved.EarlyDetailsState != Done {
				t.Fatal("failed reconciliation promoted or lost durable checkpoints")
			}
			if len(api.reconciledInputs) != 1 || !reflect.DeepEqual(api.reconciledInputs[0], stopped.Details) {
				t.Fatal("readback did not use the exact requested details")
			}
			if !reflect.DeepEqual(api.events, before) {
				t.Fatal("failed read-only status mutated the service", api.events)
			}
			manager = NewManager(manager.dir, api)
			if _, err = manager.Upload(context.Background(), stopped.ID); err == nil {
				t.Fatal("unknown details allowed a blind retry")
			}
			if !reflect.DeepEqual(api.events, before) || len(api.detailWrites) != 2 || len(api.packageTargets) != 1 {
				t.Fatal("blocked upload repeated a mutation", api.events)
			}
		})
	}
}
