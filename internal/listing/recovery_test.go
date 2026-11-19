package listing

import (
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

	"github.com/SENTINELITE/elgato-makers-cli/internal/draft"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

type recoveryAPI struct {
	*workflowAPI
	catalog      []model.Product
	catalogCalls int
	recoveryErr  error
}

func (f *recoveryAPI) CreateDraft(context.Context, maker.DraftInput) (maker.DraftResult, error) {
	f.events = append(f.events, "create")
	return maker.DraftResult{}, errors.New("synthetic ambiguous creation without acknowledged identifiers")
}
func (f *recoveryAPI) DeletionProducts(context.Context, string) ([]model.Product, error) {
	f.catalogCalls++
	if f.recoveryErr != nil {
		return nil, f.recoveryErr
	}
	if f.catalog == nil {
		return []model.Product{}, nil
	}
	return f.catalog, nil
}
func recoveryFixture(t *testing.T) (*Manager, Plan, *recoveryAPI) {
	t.Helper()
	m, p, f := workflowFixture(t)
	api := &recoveryAPI{workflowAPI: f}
	m.api = api
	if _, err := m.Upload(context.Background(), p.ID); err == nil {
		t.Fatal("expected ambiguous creation")
	}
	p, err := m.Load(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.DraftPlanID == "" || p.Target.ProductID != "" || p.Target.VariantID != "" {
		t.Fatal("invalid uncertain creation fixture")
	}
	return m, p, api
}
func writeRecoveryChild(t *testing.T, m *Manager, p draft.Plan) {
	t.Helper()
	digestBytes, err := json.Marshal(struct {
		Input   maker.DraftInput
		Package model.Package
	}{p.Input, p.Package})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(digestBytes)
	p.Digest = hex.EncodeToString(digest[:])
	p.Checksum = ""
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	p.Checksum = hex.EncodeToString(sum[:])
	raw, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(m.dir, "drafts", p.ID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCreationRecoveryRequiresExplicitAcknowledgmentBeforeAPI(t *testing.T) {
	m, p, f := recoveryFixture(t)
	if _, err := m.RecoverCreation(context.Background(), p.ID, false); err == nil {
		t.Fatal("missing uncertainty acknowledgment accepted")
	}
	if f.catalogCalls != 0 {
		t.Fatal("read catalog before acknowledgment")
	}
	current, err := m.Load(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Checksum != p.Checksum {
		t.Fatal("recovery changed the record without acknowledgment")
	}
	local := NewManager(m.dir, nil)
	if err = local.ValidateCreationRecovery(p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCreationRecoveryRetiresLocallyAndPreservesAudit(t *testing.T) {
	m, p, f := recoveryFixture(t)
	retired, err := m.RecoverCreation(context.Background(), p.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if retired.State != Retired || !validCreationRecovery(retired.CreationRecovery) || f.catalogCalls != 2 {
		t.Fatal(retired.State, retired.CreationRecovery, f.catalogCalls)
	}
	child, err := m.drafts().Load(p.DraftPlanID)
	if err != nil {
		t.Fatal(err)
	}
	if child.State != draft.Retired || !sameRecoveryProof(retired.CreationRecovery, child.CreationRecovery) {
		t.Fatal("parent lost child recovery proof")
	}
	f.recoveryErr = errors.New("catalog unavailable")
	current, err := m.RecoverCreation(context.Background(), p.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if current.Checksum != retired.Checksum || f.catalogCalls != 2 {
		t.Fatal("completed recovery was not locally idempotent")
	}
	if err = NewManager(m.dir, nil).ValidateCreationRecovery(p.ID); err != nil {
		t.Fatal(err)
	}
	cfg := p.Prepared.Config
	cfg.Name += " Recovered"
	prepared, err := PrepareConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Prepare(prepared, p.Target.OrgID); err != nil {
		t.Fatal("retired plan still owns UUID", err)
	}
	if _, err = m.Upload(context.Background(), p.ID); err == nil {
		t.Fatal("recovered plan uploaded")
	}
	if _, err = m.Status(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.events, []string{"create"}) || f.catalogCalls != 2 {
		t.Fatal("recovery mutated remote state", f.events, f.catalogCalls)
	}
}

func TestCreationRecoveryResumesAfterChildRetirementWithFreshReads(t *testing.T) {
	m, p, f := recoveryFixture(t)
	child, err := m.drafts().RetireUnacknowledged(context.Background(), p.DraftPlanID, true)
	if err != nil {
		t.Fatal(err)
	}
	if child.State != draft.Retired || f.catalogCalls != 2 {
		t.Fatal("child retirement failed")
	}
	if err = m.ValidateCreationRecovery(p.ID); err != nil {
		t.Fatal(err)
	}
	f.recoveryErr = errors.New("catalog temporarily unavailable")
	if _, err = m.RecoverCreation(context.Background(), p.ID, true); err == nil {
		t.Fatal("partial recovery skipped fresh catalog checks")
	}
	current, err := m.Load(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != Ready || current.CreationRecovery != nil {
		t.Fatal("partial commit promoted without fresh proof")
	}
	f.recoveryErr = nil
	calls := f.catalogCalls
	current, err = m.RecoverCreation(context.Background(), p.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != Retired || f.catalogCalls != calls+2 {
		t.Fatal("did not resume with two fresh snapshots")
	}
	child, err = m.drafts().Load(p.DraftPlanID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameRecoveryProof(current.CreationRecovery, child.CreationRecovery) {
		t.Fatal("resumed proof mismatch")
	}
}

func TestCreationRecoveryRejectsWriteEvidenceAndUnownedChildren(t *testing.T) {
	cases := []string{"submitted", "uploaded", "discarded", "unknown-parent", "known-product", "known-variant", "configured-product", "details-identity", "release", "package", "details", "early-details", "early-state", "media-state", "media-id", "media-ack", "media-reconciled", "missing-child", "child-prepared", "child-creating", "child-known-ids", "child-retired-no-proof", "child-name", "child-plain-length", "child-extension", "child-package", "parent-retired-no-proof", "wrong-parent-uuid"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			m, p, f := recoveryFixture(t)
			child, err := m.drafts().Load(p.DraftPlanID)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "submitted":
				p.State = Submitted
			case "uploaded":
				p.State = Uploaded
			case "discarded":
				p.State = Discarded
			case "unknown-parent":
				p.State = Unknown
			case "known-product":
				p.Target.ProductID = "known"
			case "known-variant":
				p.Target.VariantID = "known"
			case "configured-product":
				p.Prepared.Config.ProductID = "known"
			case "details-identity":
				p.Details.Target.ProductID = "known"
			case "release":
				p.ReleasePlanID = strings.Repeat("a", 32)
			case "package":
				p.PackageState = Done
			case "details":
				p.DetailsState = Unknown
			case "early-details":
				p.EarlyDetails = &p.Details
			case "early-state":
				p.EarlyDetailsState = Ready
			case "media-state":
				p.Media[0].State = Inflight
			case "media-id":
				p.Media[0].Result.FileID = "known"
			case "media-ack":
				p.Media[0].Result.Acknowledged = true
			case "media-reconciled":
				p.Media[0].Result.Reconciled = true
			case "missing-child":
				p.DraftPlanID = ""
			case "child-prepared":
				child.State = draft.Prepared
			case "child-creating":
				child.State = draft.Creating
			case "child-known-ids":
				child.Target.ProductID = "known"
				child.Target.VariantID = "known-variant"
				child.CreationAcknowledged = true
			case "child-retired-no-proof":
				child.State = draft.Retired
			case "child-name":
				child.Input.Name = "different"
			case "child-plain-length":
				child.Input.DescriptionPlainTextLength++
			case "child-extension":
				child.Input.ExtensionID = "different"
			case "child-package":
				child.Package.Path += ".different"
			case "parent-retired-no-proof":
				p.State = Retired
			case "wrong-parent-uuid":
				p.Target.UUID = "different"
			}
			writeRecoveryChild(t, m, child)
			if err = m.save(p, false); err != nil {
				t.Fatal(err)
			}
			if err = m.ValidateCreationRecovery(p.ID); err == nil {
				t.Fatal("unsafe local eligibility accepted")
			}
			if _, err = m.RecoverCreation(context.Background(), p.ID, true); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			if f.catalogCalls != 0 || !reflect.DeepEqual(f.events, []string{"create"}) {
				t.Fatal("ineligible recovery reached remote API", f.catalogCalls, f.events)
			}
		})
	}
}

func TestCreationRecoveryCatalogErrorsPreserveOwnership(t *testing.T) {
	for _, name := range []string{"catalog-error", "same-name", "same-uuid", "unresolved-uuid"} {
		t.Run(name, func(t *testing.T) {
			m, p, f := recoveryFixture(t)
			switch name {
			case "catalog-error":
				f.recoveryErr = errors.New("unrecognized catalog")
			case "same-name":
				f.catalog = []model.Product{{ID: "other", Name: p.Prepared.Config.Name, Status: "DRAFT", UUID: "other.uuid", Variants: []model.Variant{{ID: "other-variant", UUID: "other.uuid"}}}}
			case "same-uuid":
				f.catalog = []model.Product{{ID: "other", Name: "Other name", Status: "PUBLISHED", UUID: p.Target.UUID, Variants: []model.Variant{{ID: "other-variant", UUID: p.Target.UUID}}}}
			case "unresolved-uuid":
				f.catalog = []model.Product{{ID: "other", Name: "Other name", Status: "DRAFT", Variants: []model.Variant{{ID: "other-variant"}}}}
			}
			if _, err := m.RecoverCreation(context.Background(), p.ID, true); err == nil {
				t.Fatal("unsafe catalog accepted")
			}
			current, err := m.Load(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Checksum != p.Checksum {
				t.Fatal("failed proof changed listing ownership")
			}
		})
	}
}

func TestCreationRecoveryChildComparisonProtectsImmutableFields(t *testing.T) {
	m, p, _ := recoveryFixture(t)
	before, err := m.drafts().Load(p.DraftPlanID)
	if err != nil {
		t.Fatal(err)
	}
	after := before
	after.State = draft.Retired
	if !sameRecoveryChild(before, after) {
		t.Fatal("allowed retirement state changed immutable comparison")
	}
	after.Input.Name += " different"
	if sameRecoveryChild(before, after) {
		t.Fatal("changed child identity accepted")
	}
}

func TestCreationRecoveryOptionalFieldPreservesLegacyHash(t *testing.T) {
	_, p, _ := workflowFixture(t)
	p.Checksum = ""
	typ := reflect.TypeOf(p)
	value := reflect.ValueOf(p)
	var fields []reflect.StructField
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Name != "CreationRecovery" {
			fields = append(fields, typ.Field(i))
		}
	}
	legacy := reflect.New(reflect.StructOf(fields)).Elem()
	for i := 0; i < legacy.NumField(); i++ {
		legacy.Field(i).Set(value.FieldByName(legacy.Type().Field(i).Name))
	}
	raw, err := json.Marshal(legacy.Interface())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if planHash(p) != hex.EncodeToString(sum[:]) {
		t.Fatal("optional recovery field changed legacy checksum")
	}
}
