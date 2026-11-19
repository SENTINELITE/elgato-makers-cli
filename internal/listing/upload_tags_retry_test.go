package listing

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
)

type uploadTagRetryAPI struct {
	*workflowAPI
	tagErr         error
	tagFolders     []string
	detailWrites   []maker.ListingDetailsInput
	packageTargets []model.Target
}

func (f *uploadTagRetryAPI) ReadUploadTags(_ context.Context, folder string) (map[string][]string, error) {
	f.tagFolders = append(f.tagFolders, folder)
	if f.tagErr != nil {
		return nil, f.tagErr
	}
	return map[string][]string{"OS": {"Mac"}}, nil
}
func (f *uploadTagRetryAPI) Metadata(ctx context.Context, org string) (maker.ListingMetadata, error) {
	meta, err := f.workflowAPI.Metadata(ctx, org)
	meta.Tagsets = append(meta.Tagsets, maker.ListingTagset{ID: "group-os", Label: "OS", Slug: "os", Tags: []maker.ListingOption{{ID: "os-mac", Name: "Mac"}}})
	return meta, err
}
func (f *uploadTagRetryAPI) UpdateListing(ctx context.Context, input maker.ListingDetailsInput) error {
	f.detailWrites = append(f.detailWrites, input)
	return f.workflowAPI.UpdateListing(ctx, input)
}
func (f *uploadTagRetryAPI) Upload(ctx context.Context, target model.Target, pkg model.Package) (model.UploadResult, error) {
	f.packageTargets = append(f.packageTargets, target)
	return f.workflowAPI.Upload(ctx, target, pkg)
}

func TestUploadTagsReadFailureResumesCheckpointedPaidPackage(t *testing.T) {
	cfg := fixture(t)
	cfg.Pricing = Pricing{Type: "paid", Amount: "6.99", Currency: "USD"}
	prepared, err := PrepareConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tagFailure := errors.New("synthetic analyzed tag response not recognized")
	api := &uploadTagRetryAPI{workflowAPI: &workflowAPI{}, tagErr: tagFailure}
	manager := NewManager(t.TempDir(), api)
	var progress []string
	manager.SetProgress(func(message string) { progress = append(progress, message) })
	plan, err := manager.Prepare(prepared, "org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Upload(context.Background(), plan.ID); !errors.Is(err, tagFailure) {
		t.Fatalf("expected analyzed tag read failure, got %v", err)
	}
	stopped, err := manager.Load(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.PackageState != Done || stopped.EarlyDetailsState != Done || stopped.DetailsState != Ready || stopped.State == Uploaded || stopped.ReleasePlanID == "" || stopped.DraftPlanID == "" {
		t.Fatalf("tag failure did not preserve resumable package/details checkpoints: package=%s early=%s details=%s listing=%s", stopped.PackageState, stopped.EarlyDetailsState, stopped.DetailsState, stopped.State)
	}
	if stopped.Target.ProductID == "" || stopped.Target.VariantID == "" || stopped.EarlyDetails == nil || stopped.EarlyDetails.Price != "6.99" || stopped.EarlyDetails.IsFree {
		t.Fatal("tag failure lost the known target or paid pricing checkpoint")
	}
	for _, media := range stopped.Media {
		if media.State != Ready || media.Result.Acknowledged || media.Result.FileID != "" || media.Result.MediaID != "" {
			t.Fatal("media ran before analyzed tags were readable")
		}
	}
	childRelease, err := manager.releases().Load(stopped.ReleasePlanID)
	if err != nil {
		t.Fatal(err)
	}
	if childRelease.State != release.Uploaded || childRelease.Upload.FileID == "" || childRelease.Upload.FolderID == "" {
		t.Fatal("child release lost its upload acknowledgment")
	}
	childDraft, err := manager.drafts().Load(stopped.DraftPlanID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.events, []string{"create", "details", "package"}) {
		t.Fatal("writes after tag failure", api.events)
	}
	if len(api.detailWrites) != 1 || len(api.verified) != 1 || api.verified[0].Price != "6.99" || api.verified[0].IsFree {
		t.Fatal("paid pricing was not applied and verified before package upload")
	}

	if len(progress) == 0 || progress[len(progress)-1] != "Reading uploaded package tags" {
		t.Fatal("failed tag read did not identify the stopped stage", progress)
	}
	api.tagErr = nil
	// A fresh manager proves the retry uses the durable package checkpoint.
	manager = NewManager(manager.dir, api)
	progress = nil
	manager.SetProgress(func(message string) { progress = append(progress, message) })
	resumed, err := manager.Upload(context.Background(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	readStage, validationStage := -1, -1
	for i, message := range progress {
		if message == "Reading uploaded package tags" {
			readStage = i
		}
		if message == "Validating uploaded package tags against Marketplace metadata" {
			validationStage = i
		}
	}
	if readStage < 0 || validationStage <= readStage {
		t.Fatal("resumed tag read and validation stages were not reported in order", progress)
	}
	if resumed.State != Uploaded || resumed.PackageState != Done || resumed.DetailsState != Done || resumed.EarlyDetailsState != Done {
		t.Fatal("resumed listing did not finish its remaining steps")
	}
	if resumed.Target != stopped.Target || resumed.DraftPlanID != stopped.DraftPlanID || resumed.ReleasePlanID != stopped.ReleasePlanID {
		t.Fatal("tag retry changed the target or child plans")
	}
	releaseAfter, err := manager.releases().Load(resumed.ReleasePlanID)
	if err != nil {
		t.Fatal(err)
	}
	draftAfter, err := manager.drafts().Load(resumed.DraftPlanID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(childRelease, releaseAfter) || !reflect.DeepEqual(childDraft, draftAfter) {
		t.Fatal("tag retry rewrote acknowledged draft/package child checkpoints")
	}
	if !reflect.DeepEqual(api.tagFolders, []string{childRelease.Upload.FolderID, childRelease.Upload.FolderID}) {
		t.Fatal("tag retry read a different uploaded folder", api.tagFolders)
	}
	if !reflect.DeepEqual(api.packageTargets, []model.Target{stopped.Target}) {
		t.Fatal("tag retry uploaded the package again or changed its target", api.packageTargets)
	}
	wantEvents := []string{"create", "details", "package", "details", "icon:1", "thumbnail:1", "gallery:1", "gallery:2", "gallery:3"}
	if !reflect.DeepEqual(api.events, wantEvents) {
		t.Fatal("tag retry duplicated work or skipped remaining media", api.events)
	}
	if len(api.detailWrites) != 2 || !reflect.DeepEqual(resumed.Details.ExtraTagIDs, []string{"os-mac"}) {
		t.Fatal("resumed final details omitted analyzed compatibility tags")
	}
	for _, details := range api.detailWrites {
		if details.Target != stopped.Target || details.Price != "6.99" || details.IsFree || !reflect.DeepEqual(details.LanguageIDs, []string{"lang"}) || !reflect.DeepEqual(details.CategoryIDs, []string{"cat"}) {
			t.Fatal("tag retry changed paid pricing or requested taxonomy", details)
		}
	}
	for _, details := range api.verified {
		if details.Price != "6.99" || details.IsFree {
			t.Fatal("tag retry verified different pricing")
		}
	}
	for _, media := range resumed.Media {
		if media.State != Done || !media.Result.Acknowledged || media.Result.FileID == "" || media.Result.MediaID == "" {
			t.Fatal("resumed listing omitted a media acknowledgment")
		}
	}
}
