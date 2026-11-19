package listing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
	"github.com/SENTINELITE/elgato-makers-cli/internal/testutil"
)

type workflowAPI struct {
	product         model.Product
	events          []string
	versions        []model.Version
	failMedia       int
	ambiguous       bool
	preflightErr    error
	targetErr       error
	targetChecks    int
	versionCalls    int
	failVersionCall int
	reviewOnRead    int
	packageErr      error
	galleryErr      error
	galleryChecks   [][]maker.MediaInput
	mediaInputs     []maker.MediaInput
	afterMedia      func()
	afterPackage    func()
	verifyErr       error
	readyErr        error
	nullUUID        bool
	readbackErr     error
	catalogErr      error
	verified        []maker.ListingDetailsInput
}

func (f *workflowAPI) PreflightListing(context.Context, string) error { return f.preflightErr }
func (f *workflowAPI) ValidateExistingListingPlan(_ context.Context, target model.Target, _ bool, _ int) error {
	f.targetChecks++
	if target.ProductID == "" || target.VariantID == "" {
		return errors.New("missing target")
	}
	return f.targetErr
}
func (f *workflowAPI) ValidateGallery(_ context.Context, target model.Target, inputs []maker.MediaInput) error {
	f.galleryChecks = append(f.galleryChecks, append([]maker.MediaInput(nil), inputs...))
	for i, input := range inputs {
		if input.Target != target || input.Kind != "gallery" || input.Order != i+1 {
			return errors.New("invalid planned gallery order or target")
		}
	}
	return f.galleryErr
}
func (f *workflowAPI) ReadUploadTags(context.Context, string) (map[string][]string, error) {
	return map[string][]string{}, nil
}
func (f *workflowAPI) Metadata(context.Context, string) (maker.ListingMetadata, error) {
	return maker.ListingMetadata{Languages: []maker.ListingOption{{ID: "lang", Name: "en"}}, Categories: []maker.ListingOption{{ID: "cat", Name: "Gaming"}}, LinkCategories: []string{"Support"}}, nil
}
func (f *workflowAPI) Products(context.Context, string) ([]model.Product, error) {
	if f.product.ID == "" {
		return nil, nil
	}
	return []model.Product{f.product}, nil
}
func (f *workflowAPI) Product(context.Context, model.Target) (model.Product, error) {
	if f.readbackErr != nil {
		return model.Product{}, f.readbackErr
	}
	return f.product, nil
}
func (f *workflowAPI) CreateDraft(_ context.Context, in maker.DraftInput) (maker.DraftResult, error) {
	f.events = append(f.events, "create")
	f.product = model.Product{ID: "product", Name: in.Name, Description: in.Description, Status: "DRAFT", UUID: in.UUID, Variants: []model.Variant{{ID: "variant", UUID: in.UUID}}}
	if f.nullUUID {
		f.product.UUID = ""
		f.product.Variants[0].UUID = ""
	}
	return maker.DraftResult{ProductID: "product", VariantID: "variant"}, nil
}
func (f *workflowAPI) UpdateListing(context.Context, maker.ListingDetailsInput) error {
	f.events = append(f.events, "details")
	return nil
}
func (f *workflowAPI) DeletionProducts(ctx context.Context, org string) ([]model.Product, error) {
	if f.catalogErr != nil {
		return nil, f.catalogErr
	}
	return f.Products(ctx, org)
}
func (f *workflowAPI) VerifyListing(_ context.Context, details maker.ListingDetailsInput) error {
	f.verified = append(f.verified, details)
	return f.verifyErr
}
func (f *workflowAPI) VerifyListingReady(context.Context, model.Target, maker.ListingDetailsInput, []maker.MediaInput) error {
	return f.readyErr
}
func (f *workflowAPI) ReconcileMedia(_ context.Context, _ maker.MediaInput, result maker.MediaResult) (maker.MediaResult, error) {
	result.Reconciled = true
	return result, nil
}
func (f *workflowAPI) UploadMedia(_ context.Context, in maker.MediaInput) (maker.MediaResult, error) {
	f.mediaInputs = append(f.mediaInputs, in)
	name := fmt.Sprintf("%s:%d", in.Kind, in.Order)
	f.events = append(f.events, name)
	if in.Kind == "gallery" && in.Order == f.failMedia {
		if f.ambiguous {
			return maker.MediaResult{}, errors.New("disconnected")
		}
		return maker.MediaResult{}, &maker.MediaNotUploadedError{Cause: errors.New("preflight")}
	}
	if f.afterMedia != nil {
		fn := f.afterMedia
		f.afterMedia = nil
		fn()
	}
	return maker.MediaResult{FileID: "file-" + name, MediaID: "media-" + name, Acknowledged: true}, nil
}
func (f *workflowAPI) Versions(context.Context, model.Target) ([]model.Version, error) {
	f.versionCalls++
	if f.versionCalls == f.failVersionCall {
		return nil, errors.New("transient Versions GET failure")
	}
	if f.versionCalls == f.reviewOnRead {
		return []model.Version{{ID: "external", VariantID: "variant", Version: "2", Status: "review"}}, nil
	}
	return f.versions, nil
}
func (f *workflowAPI) Upload(_ context.Context, _ model.Target, p model.Package) (model.UploadResult, error) {
	f.events = append(f.events, "package")
	if f.packageErr != nil {
		return model.UploadResult{}, f.packageErr
	}
	if f.afterPackage != nil {
		f.afterPackage()
	}
	return model.UploadResult{FileID: "package", FolderID: "folder", Manifest: p.Manifest}, nil
}
func (f *workflowAPI) Submit(_ context.Context, t model.Target, s model.Submission) (model.SubmitResult, error) {
	f.events = append(f.events, "submit")
	f.versions = []model.Version{{ID: "version", VariantID: t.VariantID, Version: s.Package.Manifest.Version, Status: "PENDING_REVIEW"}}
	return model.SubmitResult{ID: "version"}, nil
}
func workflowFixture(t *testing.T) (*Manager, Plan, *workflowAPI) {
	t.Helper()
	prepared, e := PrepareConfig(fixture(t))
	if e != nil {
		t.Fatal(e)
	}
	f := &workflowAPI{}
	m := NewManager(t.TempDir(), f)
	p, e := m.Prepare(prepared, "org")
	if e != nil {
		t.Fatal(e)
	}
	return m, p, f
}
func TestDirectSubmitCreatesAndPopulatesInOrder(t *testing.T) {
	m, p, f := workflowFixture(t)
	p, e := m.Submit(context.Background(), p.ID, release.ConfirmOptions{Yes: true})
	if e != nil {
		t.Fatal(e)
	}
	want := []string{"create", "details", "package", "details", "icon:1", "thumbnail:1", "gallery:1", "gallery:2", "gallery:3", "submit"}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatalf("events %v", f.events)
	}
	if p.State != Submitted || p.DraftPlanID == "" || p.ReleasePlanID == "" {
		t.Fatalf("bad checkpoint %+v", p)
	}
	if _, e = m.Submit(context.Background(), p.ID, release.ConfirmOptions{Yes: true}); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatal("submitted plan repeated writes")
	}
}
func TestCancelBeforeAnyRemoteWrite(t *testing.T) {
	for _, input := range []string{"n\n", "yes\nn\n", "yes\n"} {
		m, p, f := workflowFixture(t)
		_, e := m.Submit(context.Background(), p.ID, release.ConfirmOptions{IsTerminal: true, Input: strings.NewReader(input), Output: &strings.Builder{}})
		if !errors.Is(e, release.ErrCancelled) {
			t.Fatalf("cancel: %v", e)
		}
		if len(f.events) != 0 {
			t.Fatal("write before confirmation")
		}
	}
}
func TestMediaResumeSkipsAcknowledgedAndStopsOnFailure(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.failMedia = 2
	p, e := m.Upload(context.Background(), p.ID)
	if e == nil {
		t.Fatal("expected failure")
	}
	want := []string{"create", "details", "package", "details", "icon:1", "thumbnail:1", "gallery:1", "gallery:2"}
	if !reflect.DeepEqual(f.events, want) {
		t.Fatal(f.events)
	}
	f.failMedia = 0
	p, e = m.Upload(context.Background(), p.ID)
	if e != nil {
		t.Fatal(e)
	}
	want = append(want, "gallery:2", "gallery:3")
	if !reflect.DeepEqual(f.events, want) || p.State != Uploaded {
		t.Fatal(f.events, p.State)
	}
}
func TestAmbiguousMediaCannotRetryOrBypassWithNewPlan(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.failMedia = 2
	f.ambiguous = true
	p, e := m.Upload(context.Background(), p.ID)
	if e == nil {
		t.Fatal("expected failure")
	}
	count := len(f.events)
	if _, e = m.Upload(context.Background(), p.ID); e == nil {
		t.Fatal("ambiguous retry allowed")
	}
	if len(f.events) != count {
		t.Fatal("write after unknown outcome")
	}
	if _, e = m.Prepare(p.Prepared, "org"); e == nil {
		t.Fatal("new plan bypass allowed")
	}
	if _, e = m.Status(context.Background(), p.ID); e == nil {
		t.Fatal("status falsely reconciled ambiguous media")
	}
}
func TestPreflightFailureBeforeCreate(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.preflightErr = errors.New("unknown wire contract")
	if _, e := m.Upload(context.Background(), p.ID); e == nil {
		t.Fatal("expected error")
	}
	if len(f.events) != 0 {
		t.Fatal("remote write despite failed preflight")
	}
}
func TestChangedFileStopsNextMedia(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.afterMedia = func() {
		if e := os.WriteFile(p.Prepared.Notes.Path, []byte("changed"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := m.Upload(context.Background(), p.ID); e == nil {
		t.Fatal("changed file accepted")
	}
	if !reflect.DeepEqual(f.events, []string{"create", "details", "package", "details", "icon:1"}) {
		t.Fatal(f.events)
	}
}
func TestCrashJournalBlocksReplay(t *testing.T) {
	m, p, f := workflowFixture(t)
	p.Media[0].State = Inflight
	if e := m.save(p, false); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Upload(context.Background(), p.ID); e == nil {
		t.Fatal("crash replay accepted")
	}
	if len(f.events) != 0 {
		t.Fatal("crash replay wrote")
	}
}

func TestResolvedTaxonomyDuplicatesFailBeforeCreate(t *testing.T) {
	c := fixture(t)
	c.Languages = []string{"en", "lang"}
	prepared, e := PrepareConfig(c)
	if e != nil {
		t.Fatal(e)
	}
	f := &workflowAPI{}
	m := NewManager(t.TempDir(), f)
	p, e := m.Prepare(prepared, "org")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.Upload(context.Background(), p.ID); e == nil {
		t.Fatal("duplicate resolved taxonomy accepted")
	}
	if len(f.events) != 0 {
		t.Fatal("created SKU before wire validation")
	}
}
func TestNonTerminalRequiresExplicitYesBeforeWrites(t *testing.T) {
	m, p, f := workflowFixture(t)
	if _, e := m.Submit(context.Background(), p.ID, release.ConfirmOptions{}); e == nil {
		t.Fatal("nonterminal submission accepted")
	}
	if len(f.events) != 0 {
		t.Fatal("nonterminal mutated remote")
	}
}
func TestLoadRejectsEditedPlanAndBroadPermissions(t *testing.T) {
	m, p, _ := workflowFixture(t)
	path := m.dir + "/" + p.ID + ".json"
	testutil.Widen(t, path)
	if _, e := m.Load(p.ID); e == nil {
		t.Fatal("broad permissions accepted")
	}
	if e := filesec.ProtectPath(path); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	b = []byte(strings.Replace(string(b), `"state": "prepared"`, `"state": "submitted"`, 1))
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Load(p.ID); e == nil {
		t.Fatal("edited plan accepted")
	}
}

func TestPackageReconciliationDoesNotSkipRemainingMedia(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.failMedia = 1
	p, e := m.Upload(context.Background(), p.ID)
	if e == nil {
		t.Fatal("expected gallery failure")
	}
	p.PackageState = Inflight
	if e = m.save(p, false); e != nil {
		t.Fatal(e)
	}
	p, e = m.Status(context.Background(), p.ID)
	if e != nil {
		t.Fatal(e)
	}
	if p.State == Uploaded || p.PackageState != Done {
		t.Fatal("package acknowledgment completed unfinished listing")
	}
	count := len(f.events)
	f.failMedia = 0
	if _, e = m.Upload(context.Background(), p.ID); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(f.events[count:], []string{"gallery:1", "gallery:2", "gallery:3"}) {
		t.Fatal(f.events[count:])
	}
}
func TestExistingPublishedExactTargetAndVersionGuard(t *testing.T) {
	for _, tc := range []struct {
		name         string
		version      model.Version
		reviewOnRead int
		want         error
	}{
		{"another variant", model.Version{Version: "1", Status: "review", VariantID: "other"}, 0, nil},
		{"raw review", model.Version{Version: "2", Status: "review", VariantID: "variant"}, 0, model.ErrVersionInReview},
		{"equivalent duplicate", model.Version{Version: "1", Status: "published", VariantID: "variant"}, 0, model.ErrDuplicateVersion},
		{"unknown lifecycle", model.Version{Version: "2", Status: "reviewed", VariantID: "variant"}, 0, model.ErrUnrecognizedVersionLifecycle},
		{"review after preflight", model.Version{Version: "2", Status: "published", VariantID: "variant"}, 2, model.ErrVersionInReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			c.ProductID = "product"
			c.VariantID = "variant"
			prepared, e := PrepareConfig(c)
			if e != nil {
				t.Fatal(e)
			}
			f := &workflowAPI{
				product:      model.Product{ID: "product", Name: c.Name, Description: prepared.Description.Markdown, Status: "PUBLISHED", UUID: prepared.Package.Manifest.UUID, Variants: []model.Variant{{ID: "variant", UUID: prepared.Package.Manifest.UUID}}},
				versions:     []model.Version{tc.version},
				reviewOnRead: tc.reviewOnRead,
			}
			m := NewManager(t.TempDir(), f)
			p, e := m.Prepare(prepared, "org")
			if e != nil {
				t.Fatal(e)
			}
			_, e = m.Upload(context.Background(), p.ID)
			if tc.want != nil {
				if !errors.Is(e, tc.want) || len(f.events) != 0 {
					t.Fatalf("got writes %v and error %v, want no writes and %v", f.events, e, tc.want)
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				if f.events[0] != "details" || f.events[1] != "package" {
					t.Fatal("existing product created again", f.events)
				}
			}
		})
	}
}

func TestPendingReviewAppearingDuringPackageUploadStopsDetails(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.afterPackage = func() {
		f.versions = []model.Version{{Version: "2", Status: "review", VariantID: "variant"}}
	}
	if _, e := m.Upload(context.Background(), p.ID); e == nil {
		t.Fatal("pending review accepted after package upload")
	}
	if !reflect.DeepEqual(f.events, []string{"create", "details", "package"}) {
		t.Fatal("wrote listing after pending review", f.events)
	}
}

func TestReviewAppearingAtFinalListingSubmitCheckStopsSubmission(t *testing.T) {
	m, p, f := workflowFixture(t)
	p, err := m.Upload(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	writes := len(f.events)
	// Listing readiness reads once, then the release manager reads before and after confirmation.
	f.reviewOnRead = f.versionCalls + 3
	if _, err = m.Submit(context.Background(), p.ID, release.ConfirmOptions{Yes: true}); !errors.Is(err, model.ErrVersionInReview) {
		t.Fatalf("expected final read to stop submission, got %v", err)
	}
	if len(f.events) != writes || f.versionCalls != f.reviewOnRead {
		t.Fatalf("unexpected writes or missing final read: %v, %d reads", f.events[writes:], f.versionCalls)
	}
}

func TestExistingTargetPreflightRejectsBeforeAnyWrite(t *testing.T) {
	for _, reason := range []string{"published pricing cannot change from paid to free", "existing gallery has surplus orders"} {
		t.Run(reason, func(t *testing.T) {
			c := fixture(t)
			c.ProductID = "product"
			c.VariantID = "variant"
			prepared, e := PrepareConfig(c)
			if e != nil {
				t.Fatal(e)
			}
			f := &workflowAPI{targetErr: errors.New(reason), product: model.Product{ID: "product", Name: c.Name, Description: prepared.Description.Markdown, Status: "PUBLISHED", UUID: prepared.Package.Manifest.UUID, Variants: []model.Variant{{ID: "variant", UUID: prepared.Package.Manifest.UUID}}}}
			m := NewManager(t.TempDir(), f)
			p, e := m.Prepare(prepared, "org")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = m.Upload(context.Background(), p.ID); e == nil {
				t.Fatal("invalid remote target accepted")
			}
			if f.targetChecks != 1 || len(f.events) != 0 {
				t.Fatal("target preflight did not run before writes", f.targetChecks, f.events)
			}
		})
	}
}
func TestNewTargetPreflightRunsAfterCreateBeforePackage(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.targetErr = errors.New("invalid gallery layout")
	if _, e := m.Upload(context.Background(), p.ID); e == nil {
		t.Fatal("invalid new target accepted")
	}
	if f.targetChecks != 1 || !reflect.DeepEqual(f.events, []string{"create"}) {
		t.Fatal("target preflight ordering", f.targetChecks, f.events)
	}
}
func TestChildReleasePreflightFailureCanResumeWithoutDuplicateSKU(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.failVersionCall = 3
	p, e := m.Upload(context.Background(), p.ID)
	if e == nil {
		t.Fatal("expected transient release preflight failure")
	}
	if p.PackageState != Ready {
		t.Fatal("proven pre-dispatch failure became unknown", p.PackageState, e)
	}
	if !reflect.DeepEqual(f.events, []string{"create", "details"}) {
		t.Fatal("package upload dispatched despite failed Versions GET", f.events)
	}
	draftID, releaseID := p.DraftPlanID, p.ReleasePlanID
	p, e = m.Upload(context.Background(), p.ID)
	if e != nil {
		t.Fatal(e)
	}
	if p.State != Uploaded || p.DraftPlanID != draftID || p.ReleasePlanID != releaseID {
		t.Fatal("resume lost original child plans")
	}
	var creates, packages int
	for _, event := range f.events {
		if event == "create" {
			creates++
		}
		if event == "package" {
			packages++
		}
	}
	if creates != 1 || packages != 1 {
		t.Fatal("resume repeated writes", f.events)
	}
}

func TestAmbiguousPackageUploadRemainsBlocked(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.packageErr = errors.New("connection lost after upload dispatch")
	p, e := m.Upload(context.Background(), p.ID)
	if e == nil || p.PackageState != Unknown {
		t.Fatal("ambiguous upload was not blocked", p.PackageState, e)
	}
	count := len(f.events)
	f.packageErr = nil
	if _, e = m.Upload(context.Background(), p.ID); e == nil {
		t.Fatal("ambiguous package upload retried")
	}
	if len(f.events) != count {
		t.Fatal("ambiguous package upload dispatched again")
	}
	if _, e = m.Status(context.Background(), p.ID); e == nil {
		t.Fatal("missing package acknowledgment falsely reconciled")
	}
}

func TestUnknownLinkCategoryFailsBeforeAnyWrite(t *testing.T) {
	for _, label := range []string{"Unknown", "support"} {
		t.Run(label, func(t *testing.T) {
			c := fixture(t)
			c.Links = map[string]string{label: "https://example.com/support"}
			prepared, e := PrepareConfig(c)
			if e != nil {
				t.Fatal(e)
			}
			f := &workflowAPI{}
			m := NewManager(t.TempDir(), f)
			p, e := m.Prepare(prepared, "org")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = m.Upload(context.Background(), p.ID); e == nil {
				t.Fatal("unknown link category accepted")
			}
			if len(f.events) != 0 {
				t.Fatal("remote write before link category validation", f.events)
			}
		})
	}
}

func TestCanceledPristinePlanCanBeDiscardedAfterFilesChange(t *testing.T) {
	m, p, f := workflowFixture(t)
	if _, e := m.Submit(context.Background(), p.ID, release.ConfirmOptions{IsTerminal: true, Input: strings.NewReader("n\n"), Output: &strings.Builder{}}); !errors.Is(e, release.ErrCancelled) {
		t.Fatal(e)
	}
	if e := os.WriteFile(p.Prepared.Notes.Path, []byte("Revised notes."), 0600); e != nil {
		t.Fatal(e)
	}
	discarded, e := m.Discard(p.ID)
	if e != nil {
		t.Fatal(e)
	}
	if discarded.State != Discarded || discarded.ID != p.ID {
		t.Fatal("discard record lost")
	}
	fresh, e := PrepareConfig(p.Prepared.Config)
	if e != nil {
		t.Fatal(e)
	}
	replacement, e := m.Prepare(fresh, "org")
	if e != nil {
		t.Fatal("discarded plan retained UUID ownership", e)
	}
	if replacement.ID == p.ID || replacement.Prepared.Digest == p.Prepared.Digest {
		t.Fatal("new plan did not review changed files")
	}
	if len(f.events) != 0 {
		t.Fatal("discard performed remote mutation", f.events)
	}
}
func TestDiscardAllowsPristineKnownTarget(t *testing.T) {
	m, p, _ := workflowFixture(t)
	p.Target.ProductID = "existing-product"
	p.Target.VariantID = "existing-variant"
	if e := m.save(p, false); e != nil {
		t.Fatal(e)
	}
	p, e := m.Discard(p.ID)
	if e != nil || p.State != Discarded {
		t.Fatal("pristine existing target was not discardable", e)
	}
}
func TestDiscardRejectsChildPlansAndMutationEvidence(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Plan)
	}{
		{"draft child", func(p *Plan) { p.DraftPlanID = "child" }},
		{"release child", func(p *Plan) { p.ReleasePlanID = "child" }},
		{"uploaded listing", func(p *Plan) { p.State = Uploaded }},
		{"unknown package", func(p *Plan) { p.PackageState = Unknown }},
		{"details acknowledged", func(p *Plan) { p.DetailsState = Done }},
		{"media in flight", func(p *Plan) { p.Media[0].State = Inflight }},
		{"media file identity", func(p *Plan) { p.Media[0].Result.FileID = "file" }},
		{"media identity", func(p *Plan) { p.Media[0].Result.MediaID = "media" }},
		{"media acknowledgment", func(p *Plan) { p.Media[0].Result.Acknowledged = true }},
		{"media reconciliation", func(p *Plan) { p.Media[0].Result.Reconciled = true }},
	}
	prepared, e := PrepareConfig(fixture(t))
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &workflowAPI{}
			m := NewManager(t.TempDir(), f)
			p, e := m.Prepare(prepared, "org")
			if e != nil {
				t.Fatal(e)
			}
			tc.edit(&p)
			if e = m.save(p, false); e != nil {
				t.Fatal(e)
			}
			if _, e = m.Discard(p.ID); e == nil {
				t.Fatal("mutation evidence was discarded")
			}
			stored, e := m.Load(p.ID)
			if e != nil {
				t.Fatal(e)
			}
			if stored.State == Discarded {
				t.Fatal("refused discard changed state")
			}
			if len(f.events) != 0 {
				t.Fatal("discard attempted remote mutation")
			}
		})
	}
}
func TestDiscardedPlanCannotMutateAndStatusIsLocal(t *testing.T) {
	m, p, _ := workflowFixture(t)
	p, e := m.Discard(p.ID)
	if e != nil {
		t.Fatal(e)
	}
	// A nil API makes any accidental service access fail this test.
	m.api = nil
	if _, e = m.Upload(context.Background(), p.ID); e == nil {
		t.Fatal("discarded plan uploaded")
	}
	if _, e = m.Submit(context.Background(), p.ID, release.ConfirmOptions{Yes: true}); e == nil {
		t.Fatal("discarded plan submitted with --yes")
	}
	status, e := m.Status(context.Background(), p.ID)
	if e != nil || status.State != Discarded {
		t.Fatal("discarded status was not local", status.State, e)
	}
}

func mixedWorkflowConfig(t *testing.T) Config {
	t.Helper()
	c := fixture(t)
	video := writeMP4(t, filepath.Join(t.TempDir(), "preview.mp4"))
	jpg := filepath.Join(t.TempDir(), "preview.jpg")
	file, e := os.Create(jpg)
	if e != nil {
		t.Fatal(e)
	}
	if e = jpeg.Encode(file, image.NewRGBA(image.Rect(0, 0, 1920, 960)), nil); e != nil {
		t.Fatal(e)
	}
	if e = file.Close(); e != nil {
		t.Fatal(e)
	}
	// The thumbnail is also a valid gallery image. Its configured position is
	// independent of the order of the distinct video and JPEG paths.
	c.Assets.Gallery = []string{video, jpg, c.Assets.Thumbnail}
	return c
}
func TestMixedGalleryOrderVideoAcknowledgmentAndResume(t *testing.T) {
	c := mixedWorkflowConfig(t)
	prepared, e := PrepareConfig(c)
	if e != nil {
		t.Fatal(e)
	}
	if prepared.Gallery[0].MIME != "video/mp4" || prepared.Gallery[1].MIME != "image/jpeg" || prepared.Gallery[2].Path != prepared.Thumbnail.Path {
		t.Fatal("mixed gallery or thumbnail reuse lost")
	}
	f := &workflowAPI{failMedia: 2}
	m := NewManager(t.TempDir(), f)
	p, e := m.Prepare(prepared, "org")
	if e != nil {
		t.Fatal(e)
	}
	p, e = m.Upload(context.Background(), p.ID)
	if e == nil {
		t.Fatal("expected safe second-slot failure")
	}
	if p.Media[2].State != Done || !p.Media[2].Result.Acknowledged || p.Media[3].State != Ready {
		t.Fatal("video acknowledgment was not checkpointed before following failure")
	}
	if len(f.galleryChecks) != 1 || len(f.galleryChecks[0]) != 3 {
		t.Fatal("whole mixed gallery was not preflighted")
	}
	for i, input := range f.galleryChecks[0] {
		if input.Path != prepared.Gallery[i].Path || input.Order != i+1 {
			t.Fatal("gallery preflight reordered entries")
		}
	}
	f.failMedia = 0
	p, e = m.Upload(context.Background(), p.ID)
	if e != nil {
		t.Fatal(e)
	}
	if p.State != Uploaded {
		t.Fatal(p.State)
	}
	videoCalls := 0
	var galleryOrders []int
	for _, input := range f.mediaInputs {
		if input.Kind == "gallery" {
			galleryOrders = append(galleryOrders, input.Order)
		}
		if input.Path == prepared.Gallery[0].Path {
			videoCalls++
		}
	}
	if videoCalls != 1 || !reflect.DeepEqual(galleryOrders, []int{1, 2, 2, 3}) {
		t.Fatal("mixed gallery resume repeated video or lost order", videoCalls, galleryOrders)
	}
}
func TestCrossTypeGalleryReplacementFailsBeforeExistingProductWrites(t *testing.T) {
	c := mixedWorkflowConfig(t)
	c.ProductID = "product"
	c.VariantID = "variant"
	prepared, e := PrepareConfig(c)
	if e != nil {
		t.Fatal(e)
	}
	f := &workflowAPI{galleryErr: errors.New("gallery slot replacement changes image/video type"), product: model.Product{ID: "product", Name: c.Name, Description: prepared.Description.Markdown, Status: "PUBLISHED", UUID: prepared.Package.Manifest.UUID, Variants: []model.Variant{{ID: "variant", UUID: prepared.Package.Manifest.UUID}}}}
	m := NewManager(t.TempDir(), f)
	p, e := m.Prepare(prepared, "org")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.Upload(context.Background(), p.ID); e == nil {
		t.Fatal("cross-type gallery replacement accepted")
	}
	if len(f.galleryChecks) != 1 || len(f.events) != 0 {
		t.Fatal("gallery preflight did not block before writes", f.events)
	}
}

func TestAcknowledgedUUIDPendingPricesBeforePackage(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.nullUUID = true
	p, e := m.Submit(context.Background(), p.ID, release.ConfirmOptions{Yes: true})
	if e != nil {
		t.Fatal(e)
	}
	if p.State != Submitted {
		t.Fatal(p.State)
	}
	if len(f.events) < 3 || !reflect.DeepEqual(f.events[:3], []string{"create", "details", "package"}) {
		t.Fatal(f.events)
	}
	if len(f.verified) == 0 {
		t.Fatal("pricing not read back")
	}
}
func TestPricingReadbackBlocksPackageAndUploadedSubmit(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.verifyErr = errors.New("price differs")
	if _, err := m.Upload(context.Background(), p.ID); err == nil {
		t.Fatal("accepted price mismatch")
	}
	if !reflect.DeepEqual(f.events, []string{"create", "details"}) {
		t.Fatal(f.events)
	}
	f.verifyErr = nil
	p, err := m.Upload(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.readyErr = errors.New("current pricing changed")
	if _, err = m.Submit(context.Background(), p.ID, release.ConfirmOptions{Yes: true}); err == nil {
		t.Fatal("submitted changed listing")
	}
	for _, event := range f.events {
		if event == "submit" {
			t.Fatal("submitted")
		}
	}
}
func TestKnownIDsPersistAfterCreateReadbackFailure(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.nullUUID = true
	f.readbackErr = errors.New("temporary GET failure")
	if _, err := m.Upload(context.Background(), p.ID); err == nil {
		t.Fatal("expected readback error")
	}
	saved, err := m.Load(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Target.ProductID != "product" || saved.Target.VariantID != "variant" {
		t.Fatal(saved.Target)
	}
	f.readbackErr = nil
	if _, err = m.Upload(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	creates := 0
	for _, event := range f.events {
		if event == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatal(f.events)
	}
}
func TestRetireDeletedCreationPreservesAuditAndAllowsNewPlan(t *testing.T) {
	m, p, f := workflowFixture(t)
	f.readbackErr = errors.New("temporary GET failure")
	if _, err := m.Upload(context.Background(), p.ID); err == nil {
		t.Fatal("expected failure")
	}
	f.readbackErr = nil
	f.product = model.Product{}
	retired, err := m.Retire(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retired.State != Retired {
		t.Fatal(retired.State)
	}
	if _, err = m.Load(p.ID); err != nil {
		t.Fatal(err)
	}
	cfg := p.Prepared.Config
	cfg.Name += " Recovered"
	prepared, err := PrepareConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Prepare(prepared, "org"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Upload(context.Background(), p.ID); err == nil {
		t.Fatal("retired upload accepted")
	}
	if _, err = m.Status(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.events, []string{"create"}) {
		t.Fatal(f.events)
	}
}
func TestRetireRefusesLiveAmbiguousOrSubmitted(t *testing.T) {
	for _, scenario := range []string{"live", "ambiguous", "submitted", "missingIDs", "mismatchedChild"} {
		t.Run(scenario, func(t *testing.T) {
			m, p, f := workflowFixture(t)
			f.readbackErr = errors.New("temporary GET failure")
			if _, err := m.Upload(context.Background(), p.ID); err == nil {
				t.Fatal("expected failure")
			}
			p, _ = m.Load(p.ID)
			f.readbackErr = nil
			switch scenario {
			case "ambiguous":
				f.catalogErr = errors.New("invalid catalog")
			case "submitted":
				p.State = Submitted
				if err := m.save(p, false); err != nil {
					t.Fatal(err)
				}
			case "missingIDs":
				p.DraftPlanID = ""
				p.Target.ProductID = ""
				p.Target.VariantID = ""
				if err := m.save(p, false); err != nil {
					t.Fatal(err)
				}
			case "mismatchedChild":
				p.Target.ProductID = "other"
				if err := m.save(p, false); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.Retire(context.Background(), p.ID); err == nil {
				t.Fatal("unsafe retirement accepted")
			}
		})
	}
}
func TestNewOptionalCheckpointsPreserveLegacyPlanEncoding(t *testing.T) {
	_, p, _ := workflowFixture(t)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "early_details") {
		t.Fatal(string(b))
	}
	var legacy map[string]any
	if err = json.Unmarshal(b, &legacy); err != nil {
		t.Fatal(err)
	}
	// A legacy zero-value plan has no new serialized keys, keeping its hash stable.
	original := planHash(p)
	delete(legacy, "early_details")
	delete(legacy, "early_details_state")
	var restored Plan
	raw, _ := json.Marshal(legacy)
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if planHash(restored) != original {
		t.Fatal("legacy hash changed")
	}
}

func TestExternalDraftMissingUUIDStillRefused(t *testing.T) {
	m, p, f := workflowFixture(t)
	p.Target.ProductID = "external-product"
	p.Target.VariantID = "external-variant"
	if err := m.save(p, false); err != nil {
		t.Fatal(err)
	}
	f.product = model.Product{ID: p.Target.ProductID, Name: p.Prepared.Config.Name, Description: p.Prepared.Description.Markdown, Status: "DRAFT", Variants: []model.Variant{{ID: p.Target.VariantID}}}
	if _, err := m.Upload(context.Background(), p.ID); err == nil {
		t.Fatal("unacknowledged UUID-less draft accepted")
	}
	if len(f.events) != 0 {
		t.Fatal(f.events)
	}
}

func TestConfiguredDraftPricesBeforePackage(t *testing.T) {
	m, p, f := workflowFixture(t)
	p.Target.ProductID = "existing-product"
	p.Target.VariantID = "existing-variant"
	if err := m.save(p, false); err != nil {
		t.Fatal(err)
	}
	f.product = model.Product{ID: p.Target.ProductID, Name: p.Prepared.Config.Name, Description: p.Prepared.Description.Markdown, Status: "DRAFT", UUID: p.Target.UUID, Variants: []model.Variant{{ID: p.Target.VariantID, UUID: p.Target.UUID}}}
	f.verifyErr = errors.New("pricing differs")
	if _, err := m.Upload(context.Background(), p.ID); err == nil {
		t.Fatal("price mismatch accepted")
	}
	if !reflect.DeepEqual(f.events, []string{"details"}) {
		t.Fatal(f.events)
	}
	f.verifyErr = nil
	if _, err := m.Upload(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.events) < 2 || f.events[1] != "package" {
		t.Fatal(f.events)
	}
	for _, event := range f.events {
		if event == "create" {
			t.Fatal("existing draft recreated")
		}
	}
}
