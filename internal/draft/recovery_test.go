package draft

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

type recoveryAPI struct {
	*fakeAPI
	catalogs     [][]model.Product
	catalogErr   error
	catalogReads int
	detailReads  int
}

func (f *recoveryAPI) DeletionProducts(context.Context, string) ([]model.Product, error) {
	f.catalogReads++
	if f.catalogErr != nil {
		return nil, f.catalogErr
	}
	i := f.catalogReads - 1
	if i >= len(f.catalogs) {
		i = len(f.catalogs) - 1
	}
	if i < 0 {
		return nil, nil
	}
	return f.catalogs[i], nil
}
func (f *recoveryAPI) Product(ctx context.Context, target model.Target) (model.Product, error) {
	f.detailReads++
	return f.fakeAPI.Product(ctx, target)
}
func recoveryFixture(t *testing.T) (*Manager, Plan, *recoveryAPI) {
	t.Helper()
	base := &fakeAPI{}
	m, p := fixture(t, base)
	p.State = Unknown
	if err := m.save(p, false); err != nil {
		t.Fatal(err)
	}
	f := &recoveryAPI{fakeAPI: base, catalogs: [][]model.Product{{}}}
	m.api = f
	return m, p, f
}
func unrelatedProduct() model.Product {
	return model.Product{ID: "other-product", Name: "Other", Status: "DRAFT", UUID: "com.example.other", Variants: []model.Variant{{ID: "other-variant", UUID: "com.example.other"}}}
}
func readPlanBytes(t *testing.T, m *Manager, p Plan) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.dir, p.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestRetireUnacknowledgedRecordsOnlyPointInTimeProof(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(map[bool]string{true: "empty", false: "unrelated"}[empty], func(t *testing.T) {
			m, p, f := recoveryFixture(t)
			count := 0
			if !empty {
				f.catalogs = [][]model.Product{{unrelatedProduct()}}
				count = 1
			}
			got, err := m.RetireUnacknowledged(context.Background(), p.ID, true)
			if err != nil || got.State != Retired || got.CreationRecovery == nil {
				t.Fatalf("recovery: %+v %v", got, err)
			}
			proof := got.CreationRecovery
			if proof.CheckedAt.IsZero() || proof.CheckedAt.Location().String() != "UTC" || len(proof.CatalogSHA256) != 64 || proof.ProductCount != count || f.catalogReads != 2 || f.calls != 0 {
				t.Fatalf("bad proof: %+v", proof)
			}
			if got.Target != p.Target || got.Input != p.Input || got.Digest != p.Digest || got.CreationAcknowledged {
				t.Fatal("recovery altered reviewed identity")
			}
			if _, err := m.RetireUnacknowledged(context.Background(), p.ID, true); err != nil || f.catalogReads != 4 {
				t.Fatalf("resume did not refresh proof: %v", err)
			}
			before := readPlanBytes(t, m, p)
			f.catalogErr = errors.New("incomplete catalog")
			if _, err := m.RetireUnacknowledged(context.Background(), p.ID, true); err == nil {
				t.Fatal("retired proof bypassed fresh read")
			}
			if string(before) != string(readPlanBytes(t, m, p)) {
				t.Fatal("failed resumed recovery changed proof")
			}
		})
	}
}
func TestRetireUnacknowledgedRejectsBeforeAPI(t *testing.T) {
	for _, kind := range []string{"no confirmation", "prepared", "created", "retired without proof", "known product", "known variant", "acknowledged", "target mismatch"} {
		t.Run(kind, func(t *testing.T) {
			m, p, f := recoveryFixture(t)
			ack := true
			switch kind {
			case "no confirmation":
				ack = false
			case "prepared":
				p.State = Prepared
			case "created":
				p.State = Created
				p.Target.ProductID = "p"
				p.Target.VariantID = "v"
			case "retired without proof":
				p.State = Retired
			case "known product":
				p.Target.ProductID = "p"
			case "known variant":
				p.Target.VariantID = "v"
			case "acknowledged":
				p.CreationAcknowledged = true
				p.Target.ProductID = "p"
				p.Target.VariantID = "v"
			case "target mismatch":
				p.Target.UUID = "other"
			}
			if err := m.save(p, false); err != nil {
				t.Fatal(err)
			}
			before := readPlanBytes(t, m, p)
			if _, err := m.RetireUnacknowledged(context.Background(), p.ID, ack); err == nil {
				t.Fatal("unsafe plan recovered")
			}
			if f.catalogReads != 0 || f.detailReads != 0 || string(before) != string(readPlanBytes(t, m, p)) {
				t.Fatal("preflight changed plan or read API")
			}
		})
	}
}
func TestRetireUnacknowledgedFailsClosedOnCatalogEvidence(t *testing.T) {
	for _, kind := range []string{"name", "product UUID", "variant UUID", "null UUID", "nil catalog", "missing name", "missing status", "missing variants", "blank product ID", "duplicate products", "blank variant ID", "duplicate variants", "detail ID", "detail name", "detail status", "detail variant set", "detail UUID conflict", "catalog changed", "unknown response", "multipage error"} {
		t.Run(kind, func(t *testing.T) {
			m, p, f := recoveryFixture(t)
			product := unrelatedProduct()
			f.catalogs = [][]model.Product{{product}}
			switch kind {
			case "name":
				product.Name = "  tEsT  "
			case "product UUID":
				product.UUID = p.Input.UUID
			case "variant UUID":
				product.Variants[0].UUID = p.Input.UUID
			case "null UUID":
				product.UUID = ""
				product.Variants[0].UUID = ""
				f.details = map[string]model.Product{product.ID: product}
			case "nil catalog":
				f.catalogs = [][]model.Product{nil}
			case "missing name":
				product.Name = " "
			case "missing status":
				product.Status = ""
			case "missing variants":
				product.Variants = nil
			case "blank product ID":
				product.ID = " "
			case "duplicate products":
				f.catalogs = [][]model.Product{{product, product}}
			case "blank variant ID":
				product.Variants[0].ID = " "
			case "duplicate variants":
				product.Variants = append(product.Variants, product.Variants[0])
			case "detail ID", "detail name", "detail status", "detail variant set", "detail UUID conflict":
				detail := unrelatedProduct()
				product.UUID = ""
				product.Variants = []model.Variant{{ID: "other-variant", UUID: ""}}
				switch kind {
				case "detail ID":
					detail.ID = "different"
				case "detail name":
					detail.Name = "Different"
				case "detail status":
					detail.Status = "PUBLISHED"
				case "detail variant set":
					detail.Variants[0].ID = "different"
				case "detail UUID conflict":
					product.UUID = "com.example.conflict"
				}
				f.details = map[string]model.Product{"other-product": detail}
			case "catalog changed":
				changed := unrelatedProduct()
				changed.Status = "PUBLISHED"
				f.catalogs = [][]model.Product{{product}, {changed}}
			case "unknown response", "multipage error":
				f.catalogErr = errors.New(kind)
			}
			if len(f.catalogs) == 1 && f.catalogs[0] != nil && len(f.catalogs[0]) == 1 {
				f.catalogs[0][0] = product
			}
			before := readPlanBytes(t, m, p)
			if _, err := m.RetireUnacknowledged(context.Background(), p.ID, true); err == nil {
				t.Fatal("incomplete or matching evidence allowed recovery")
			}
			if f.calls != 0 || string(before) != string(readPlanBytes(t, m, p)) {
				t.Fatal("failed recovery mutated state")
			}
		})
	}
}
func TestRecoveryDetailResolutionAndStableOrder(t *testing.T) {
	m, p, f := recoveryFixture(t)
	product := unrelatedProduct()
	product.UUID = ""
	product.Variants[0].UUID = ""
	detail := unrelatedProduct()
	other := unrelatedProduct()
	other.ID = "second"
	other.Name = "Second"
	other.Variants = []model.Variant{{ID: "second-variant", UUID: other.UUID}}
	f.catalogs = [][]model.Product{{product, other}, {other, product}}
	f.details = map[string]model.Product{product.ID: detail}
	got, err := m.RetireUnacknowledged(context.Background(), p.ID, true)
	if err != nil || got.State != Retired || f.catalogReads != 2 || f.detailReads != 2 {
		t.Fatalf("stable resolved snapshot: %+v %v", got, err)
	}
}

func TestRecoveryNameCollisionUsesUnicodeEqualFold(t *testing.T) {
	m, p, f := recoveryFixture(t)
	p.Input.Name = "Σ"
	p.Target.Name = p.Input.Name
	p.Digest = digest(p)
	if err := m.save(p, false); err != nil {
		t.Fatal(err)
	}
	product := unrelatedProduct()
	product.Name = " ς "
	f.catalogs = [][]model.Product{{product}}
	before := readPlanBytes(t, m, p)
	if _, err := m.RetireUnacknowledged(context.Background(), p.ID, true); err == nil {
		t.Fatal("Unicode-equivalent name allowed recovery")
	}
	if f.calls != 0 || string(before) != string(readPlanBytes(t, m, p)) {
		t.Fatal("name collision changed state")
	}
}
