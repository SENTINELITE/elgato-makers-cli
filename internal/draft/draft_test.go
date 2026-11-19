package draft

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/SENTINELITE/elgato-makers-cli/internal/testutil"
)

type fakeAPI struct {
	products     []model.Product
	details      map[string]model.Product
	calls, reads int
	result       maker.DraftResult
	failure      error
	onCreate     func()
	versions     []model.Version
	versionsErr  error
	deletionErr  error
	onDetail     func()
}

func (f *fakeAPI) Products(context.Context, string) ([]model.Product, error) {
	f.reads++
	return f.products, nil
}
func (f *fakeAPI) Product(_ context.Context, t model.Target) (model.Product, error) {
	if f.onDetail != nil {
		f.onDetail()
	}
	p, ok := f.details[t.ProductID]
	if !ok {
		return p, errors.New("no detail")
	}
	return p, nil
}
func (f *fakeAPI) DeletionProducts(ctx context.Context, org string) ([]model.Product, error) {
	if f.deletionErr != nil {
		return nil, f.deletionErr
	}
	return f.Products(ctx, org)
}
func (f *fakeAPI) Versions(context.Context, model.Target) ([]model.Version, error) {
	return f.versions, f.versionsErr
}
func (f *fakeAPI) CreateDraft(context.Context, maker.DraftInput) (maker.DraftResult, error) {
	f.calls++
	if f.onCreate != nil {
		f.onCreate()
	}
	return f.result, f.failure
}
func remote() model.Product {
	return model.Product{ID: "product", Name: "Test", Description: "Description", UUID: "com.example.test", Status: "DRAFT", Variants: []model.Variant{{ID: "variant", UUID: "com.example.test"}}}
}
func fixture(t *testing.T, f *fakeAPI) (*Manager, Plan) {
	t.Helper()
	m := NewManager(filepath.Join(t.TempDir(), "drafts"), f)
	p, err := m.Prepare(maker.DraftInput{OrgID: "org", Name: "Test", UUID: "com.example.test", Description: "Description", DescriptionPlainTextLength: 11}, model.Package{})
	if err != nil {
		t.Fatal(err)
	}
	return m, p
}
func TestCreateAndReuseAreIdempotent(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{true: "existing", false: "new"}[existing], func(t *testing.T) {
			f := &fakeAPI{result: maker.DraftResult{ProductID: "product", VariantID: "variant"}}
			m, p := fixture(t, f)
			if existing {
				f.products = []model.Product{remote()}
				f.details = map[string]model.Product{"product": remote()}
			}
			f.onCreate = func() {
				checkpoint, e := m.Load(p.ID)
				if e != nil || checkpoint.State != Creating {
					t.Fatalf("missing predispatch checkpoint: %v", e)
				}
				f.products = []model.Product{remote()}
				f.details = map[string]model.Product{"product": remote()}
			}
			p, err := m.Create(context.Background(), p.ID)
			if err != nil || p.State != Created || p.Target.ProductID != "product" {
				t.Fatalf("create: %+v %v", p, err)
			}
			calls, reads := f.calls, f.reads
			if _, err = m.Create(context.Background(), p.ID); err != nil || f.calls != calls || f.reads != reads {
				t.Fatal("repeat create not local idempotent")
			}
			if existing && calls != 0 || !existing && calls != 1 {
				t.Fatal("unexpected creation count")
			}
			if !filesec.IsPrivateFile(filepath.Join(m.dir, p.ID+".json")) {
				t.Fatal("plan not private")
			}
		})
	}
}
func TestLostResponseBlocksRetriesAndOtherPlansUntilReconciled(t *testing.T) {
	f := &fakeAPI{failure: errors.New("lost response")}
	m, p := fixture(t, f)
	p, err := m.Create(context.Background(), p.ID)
	if err == nil || p.State != Unknown {
		t.Fatalf("uncertainty lost: %+v %v", p, err)
	}
	if _, err = m.Create(context.Background(), p.ID); err == nil || f.calls != 1 {
		t.Fatal("uncertain create retried")
	}
	if _, err = m.Status(context.Background(), p.ID); err == nil {
		t.Fatal("empty catalog falsely confirmed")
	}
	second, err := m.Prepare(p.Input, p.Package)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Create(context.Background(), second.ID); err == nil || f.calls != 1 {
		t.Fatal("other plan bypassed uncertainty")
	}
	f.products = []model.Product{remote()}
	p, err = m.Status(context.Background(), p.ID)
	if err != nil || p.State != Created {
		t.Fatalf("reconciliation: %v", err)
	}
	if _, err = m.Create(context.Background(), second.ID); err != nil || f.calls != 1 {
		t.Fatalf("reuse after reconciliation: %v", err)
	}
}
func TestConflictingOrIncompleteCatalogRefusesCreation(t *testing.T) {
	for _, kind := range []string{"name", "status", "multiple", "missing UUID", "variant UUID", "detail"} {
		t.Run(kind, func(t *testing.T) {
			f := &fakeAPI{}
			product := remote()
			switch kind {
			case "name":
				product.Name = "Other"
			case "status":
				product.Status = "PUBLISHED"
			case "multiple":
				f.products = append(f.products, product)
				product.ID = "second"
			case "missing UUID":
				product.UUID = ""
				product.Variants[0].UUID = ""
				f.details = map[string]model.Product{"product": product}
			case "variant UUID":
				product.Variants[0].UUID = "different"
			case "detail":
				product.UUID = ""
			}
			f.products = append(f.products, product)
			m, p := fixture(t, f)
			if _, err := m.Create(context.Background(), p.ID); err == nil || f.calls != 0 {
				t.Fatal("unsafe catalog allowed creation")
			}
		})
	}
}
func TestCheckpointAndPreflightEvidence(t *testing.T) {
	for _, kind := range []string{"known not created", "ordinary error", "contradictory IDs", "verify failure"} {
		t.Run(kind, func(t *testing.T) {
			f := &fakeAPI{failure: &maker.DraftNotCreatedError{Cause: errors.New("preflight")}}
			m, p := fixture(t, f)
			switch kind {
			case "ordinary error":
				f.failure = errors.New("timeout")
			case "contradictory IDs":
				f.result = maker.DraftResult{ProductID: "product"}
			case "verify failure":
				f.failure = nil
				f.result = maker.DraftResult{ProductID: "product", VariantID: "variant"}
			}
			p, err := m.Create(context.Background(), p.ID)
			if err == nil {
				t.Fatal("expected failure")
			}
			expected := Unknown
			if kind == "known not created" {
				expected = Prepared
			}
			loaded, e := m.Load(p.ID)
			if e != nil || loaded.State != expected || loaded.Target.ProductID != f.result.ProductID {
				t.Fatalf("checkpoint: %+v %v", loaded, e)
			}
		})
	}
}
func TestCorruptPlansAndLocksFailClosed(t *testing.T) {
	for _, kind := range []string{"corrupt", "permissions", "lock"} {
		t.Run(kind, func(t *testing.T) {
			f := &fakeAPI{}
			m, p := fixture(t, f)
			path := filepath.Join(m.dir, p.ID+".json")
			switch kind {
			case "corrupt":
				if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				testutil.Widen(t, path)
			case "lock":
				if err := os.WriteFile(filepath.Join(m.dir, "draft.lock"), []byte("stale"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.Create(context.Background(), p.ID); err == nil || f.calls != 0 {
				t.Fatal("unsafe local state accepted")
			}
		})
	}
}

func TestSameNameDifferentUUIDBlocksBeforeDispatch(t *testing.T) {
	f := &fakeAPI{}
	product := remote()
	product.UUID = "com.example.other"
	product.Variants[0].UUID = product.UUID
	f.products = []model.Product{product}
	m, p := fixture(t, f)
	result, err := m.Create(context.Background(), p.ID)
	if err == nil || f.calls != 0 || result.State != Prepared {
		t.Fatalf("same-name UUID conflict was dispatched: %+v %v", result, err)
	}
	loaded, err := m.Load(p.ID)
	if err != nil || loaded.State != Prepared {
		t.Fatalf("preflight conflict changed checkpoint: %+v %v", loaded, err)
	}
}

func pendingUUID() model.Product {
	p := remote()
	p.UUID = ""
	p.Variants[0].UUID = ""
	return p
}

func TestAcknowledgedCreateAllowsPendingUUIDAndResumeNeverCreatesAgain(t *testing.T) {
	f := &fakeAPI{result: maker.DraftResult{ProductID: "product", VariantID: "variant"}}
	m, plan := fixture(t, f)
	f.onCreate = func() {
		f.products = []model.Product{pendingUUID()}
		f.details = map[string]model.Product{"product": pendingUUID()}
	}
	f.onDetail = func() {
		checkpoint, err := m.Load(plan.ID)
		if err != nil || !checkpoint.CreationAcknowledged || checkpoint.Target.ProductID != "product" || checkpoint.State != Unknown {
			t.Fatalf("acknowledgment was not saved before readback: %+v %v", checkpoint, err)
		}
	}
	p, err := m.Create(context.Background(), plan.ID)
	if err != nil || p.State != Created || !p.CreationAcknowledged {
		t.Fatalf("create: %+v %v", p, err)
	}
	f.onDetail = nil
	if _, err := m.Receipt(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), p.ID); err != nil || f.calls != 1 {
		t.Fatalf("resume created again: %d %v", f.calls, err)
	}
}

func TestAcknowledgedReadbackConflictsRemainBlocked(t *testing.T) {
	for _, kind := range []string{"UUID", "variant UUID", "name", "description", "product ID", "variant ID", "status", "multiple variants", "versions", "unrecognized versions"} {
		t.Run(kind, func(t *testing.T) {
			f := &fakeAPI{result: maker.DraftResult{ProductID: "product", VariantID: "variant"}}
			m, plan := fixture(t, f)
			product := pendingUUID()
			switch kind {
			case "UUID":
				product.UUID = "other"
			case "variant UUID":
				product.Variants[0].UUID = "other"
			case "name":
				product.Name = "Other"
			case "description":
				product.Description = "Other"
			case "product ID":
				product.ID = "other"
			case "variant ID":
				product.Variants[0].ID = "other"
			case "status":
				product.Status = "PUBLISHED"
			case "multiple variants":
				product.Variants = append(product.Variants, model.Variant{ID: "other"})
			case "versions":
				f.versions = []model.Version{{ID: "version"}}
			case "unrecognized versions":
				f.versionsErr = errors.New("unrecognized version collection")
			}
			f.onCreate = func() { f.details = map[string]model.Product{"product": product} }
			p, err := m.Create(context.Background(), plan.ID)
			if err == nil || p.State != Unknown || !p.CreationAcknowledged {
				t.Fatalf("conflict accepted: %+v %v", p, err)
			}
			if _, err := m.Create(context.Background(), plan.ID); err == nil || f.calls != 1 {
				t.Fatal("conflict retried")
			}
		})
	}
}

func TestAcknowledgmentSurvivesReadbackFailure(t *testing.T) {
	f := &fakeAPI{result: maker.DraftResult{ProductID: "product", VariantID: "variant"}}
	m, plan := fixture(t, f)
	p, err := m.Create(context.Background(), plan.ID)
	if err == nil || !p.CreationAcknowledged || p.State != Unknown {
		t.Fatalf("missing checkpoint: %+v %v", p, err)
	}
	m = NewManager(m.dir, f)
	f.products = []model.Product{pendingUUID()}
	f.details = map[string]model.Product{"product": pendingUUID()}
	p, err = m.Status(context.Background(), plan.ID)
	if err != nil || p.State != Created || f.calls != 1 {
		t.Fatalf("readback recovery: %+v %v", p, err)
	}
}

func TestPendingUUIDCannotResolveUnacknowledgedCreation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "legacy IDs"}[legacy], func(t *testing.T) {
			f := &fakeAPI{failure: errors.New("timeout")}
			m, plan := fixture(t, f)
			if legacy {
				f.result = maker.DraftResult{ProductID: "product", VariantID: "variant"}
			}
			p, err := m.Create(context.Background(), plan.ID)
			if err == nil || p.CreationAcknowledged {
				t.Fatal("error became acknowledgment")
			}
			f.products = []model.Product{pendingUUID()}
			if _, err := m.Status(context.Background(), plan.ID); err == nil {
				t.Fatal("unknown result accepted absent UUID")
			}
			if _, err := m.Receipt(plan.ID); err == nil {
				t.Fatal("legacy IDs became receipt")
			}
			if _, err := m.Create(context.Background(), plan.ID); err == nil || f.calls != 1 {
				t.Fatal("unknown creation retried")
			}
		})
	}
}

func TestOldPlanChecksumDoesNotRequireAcknowledgmentField(t *testing.T) {
	m, p := fixture(t, &fakeAPI{})
	// This is the previous on-disk schema, including its field order.
	type oldPlan struct {
		ID        string           `json:"id"`
		CreatedAt time.Time        `json:"created_at"`
		Input     maker.DraftInput `json:"input"`
		Package   model.Package    `json:"package"`
		Digest    string           `json:"reviewed_digest"`
		State     string           `json:"state"`
		Target    model.Target     `json:"target"`
		Checksum  string           `json:"checksum"`
	}
	old := oldPlan{ID: p.ID, CreatedAt: p.CreatedAt, Input: p.Input, Package: p.Package, Digest: p.Digest, State: Unknown, Target: p.Target}
	old.Target.ProductID, old.Target.VariantID = "product", "variant"
	old.Checksum = hash(old)
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(m.dir, p.ID+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := m.Load(p.ID)
	if err != nil || loaded.CreationAcknowledged {
		t.Fatalf("legacy plan incompatible: %+v %v", loaded, err)
	}
	if _, err := m.Receipt(p.ID); err == nil {
		t.Fatal("legacy plan gained receipt")
	}
}

func TestRetireAfterAbsenceIsLocalAndUnblocksReplacement(t *testing.T) {
	f := &fakeAPI{failure: errors.New("lost response"), result: maker.DraftResult{ProductID: "product", VariantID: "variant"}}
	m, plan := fixture(t, f)
	if _, err := m.Create(context.Background(), plan.ID); err == nil {
		t.Fatal("expected unknown")
	}
	p, err := m.RetireAfterAbsence(context.Background(), plan.ID)
	if err != nil || p.State != Retired || f.calls != 1 {
		t.Fatalf("retire: %+v %v", p, err)
	}
	if _, err := m.Create(context.Background(), plan.ID); err == nil {
		t.Fatal("retired plan created")
	}
	reads := f.reads
	if p, err = m.RetireAfterAbsence(context.Background(), plan.ID); err != nil || p.State != Retired || reads != f.reads {
		t.Fatal("retirement not idempotent")
	}
	if _, err := m.Status(context.Background(), plan.ID); err != nil || reads != f.reads {
		t.Fatal("retired status reads remote")
	}
	replacement, err := m.Prepare(plan.Input, plan.Package)
	if err != nil {
		t.Fatal(err)
	}
	f.failure = &maker.DraftNotCreatedError{Cause: errors.New("synthetic preflight")}
	f.result = maker.DraftResult{}
	if _, err := m.Create(context.Background(), replacement.ID); err == nil || f.calls != 2 {
		t.Fatalf("retired plan blocked replacement: %v", err)
	}
}

func TestRetirementRequiresKnownIDsAndCompleteAbsence(t *testing.T) {
	for _, kind := range []string{"unknown IDs", "present product", "present variant", "incomplete catalog", "read failure"} {
		t.Run(kind, func(t *testing.T) {
			f := &fakeAPI{failure: errors.New("lost response"), result: maker.DraftResult{ProductID: "product", VariantID: "variant"}}
			if kind == "unknown IDs" {
				f.result = maker.DraftResult{}
			}
			m, p := fixture(t, f)
			if _, err := m.Create(context.Background(), p.ID); err == nil {
				t.Fatal("expected unknown")
			}
			switch kind {
			case "present product":
				f.products = []model.Product{remote()}
			case "present variant":
				product := remote()
				product.ID = "other"
				f.products = []model.Product{product}
			case "incomplete catalog":
				f.products = []model.Product{{}}
			case "read failure":
				f.deletionErr = errors.New("unrecognized catalog")
			}
			if p, err := m.RetireAfterAbsence(context.Background(), p.ID); err == nil || p.State != Unknown || f.calls != 1 {
				t.Fatalf("unsafe retirement: %+v %v", p, err)
			}
		})
	}
}

func TestCreateReportsDispatchAndPreservesUnknownWithoutRetry(t *testing.T) {
	f := &fakeAPI{failure: &maker.AmbiguousDraftError{Cause: errors.New("synthetic-secret")}}
	m, p := fixture(t, f)
	var messages []string
	m.SetProgress(func(message string) { messages = append(messages, message) })
	f.onCreate = func() {
		if len(messages) == 0 || messages[len(messages)-1] != "Creating Marketplace product" {
			t.Fatalf("stale dispatch progress: %v", messages)
		}
	}
	result, err := m.Create(context.Background(), p.ID)
	if err == nil || !strings.Contains(err.Error(), "unknown failure") || strings.Contains(err.Error(), "synthetic-secret") || result.State != Unknown || result.CreationAcknowledged || result.Target.ProductID != "" || result.Target.VariantID != "" {
		t.Fatalf("unknown creation changed: %+v %v", result, err)
	}
	saved, loadErr := m.Load(p.ID)
	if loadErr != nil || saved.State != Unknown || saved.CreationAcknowledged || saved.Target.ProductID != "" || saved.Target.VariantID != "" {
		t.Fatalf("unknown checkpoint changed: %+v %v", saved, loadErr)
	}
	if _, err = m.Create(context.Background(), p.ID); err == nil || f.calls != 1 {
		t.Fatal("unknown creation retried")
	}
}
