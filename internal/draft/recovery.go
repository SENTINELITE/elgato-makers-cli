package draft

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

// CreationRecoveryProof records a point-in-time catalog observation. It does not
// establish that an uncertain create request never reached Maker.
type CreationRecoveryProof struct {
	CheckedAt     time.Time `json:"checked_at"`
	CatalogSHA256 string    `json:"catalog_sha256"`
	ProductCount  int       `json:"product_count"`
}

// RetireUnacknowledged records an explicitly accepted uncertain creation in the
// local journal only. It never sends a create or delete request.
func (m *Manager) RetireUnacknowledged(ctx context.Context, id string, acknowledgedUncertainty bool) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if !acknowledgedUncertainty {
			return p, errors.New("explicit acknowledgment of uncertain creation is required")
		}
		if (p.State != Unknown && !(p.State == Retired && p.CreationRecovery != nil)) || p.CreationAcknowledged || p.Target.ProductID != "" || p.Target.VariantID != "" {
			return p, errors.New("recovery requires an unacknowledged uncertain creation without saved product or variant identifiers")
		}
		if strings.TrimSpace(p.Input.OrgID) == "" || strings.TrimSpace(p.Input.UUID) == "" || strings.TrimSpace(p.Input.Name) == "" || p.Target.OrgID != p.Input.OrgID || p.Target.UUID != p.Input.UUID || p.Target.Name != p.Input.Name {
			return p, errors.New("saved draft identity differs from reviewed recovery identity")
		}
		m.report("Checking complete catalog for uncertain creation recovery")
		first, count, err := m.recoverySnapshot(ctx, p)
		if err != nil {
			return p, err
		}
		second, secondCount, err := m.recoverySnapshot(ctx, p)
		if err != nil {
			return p, err
		}
		if first != second || count != secondCount {
			return p, errors.New("Maker catalog changed during recovery; local plan remains unchanged")
		}
		if err := ctx.Err(); err != nil {
			return p, err
		}
		p.State = Retired
		p.CreationRecovery = &CreationRecoveryProof{CheckedAt: time.Now().UTC(), CatalogSHA256: second, ProductCount: count}
		if err := m.save(p, false); err != nil {
			return p, err
		}
		return m.Load(id)
	})
}

type recoveryProduct struct {
	ID       string
	Name     string
	Status   string
	UUID     string
	Variants []recoveryVariant
}
type recoveryVariant struct {
	ID   string
	UUID string
}

func recoveryText(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func (m *Manager) recoverySnapshot(ctx context.Context, p Plan) (string, int, error) {
	products, err := m.api.DeletionProducts(ctx, p.Input.OrgID)
	if err != nil {
		return "", 0, fmt.Errorf("cannot read complete recovery catalog: %w", err)
	}
	if products == nil {
		return "", 0, errors.New("recovery catalog was not an explicit collection")
	}
	snapshot := make([]recoveryProduct, 0, len(products))
	seenProducts := map[string]bool{}
	seenVariants := map[string]bool{}
	for _, product := range products {
		if strings.TrimSpace(product.ID) == "" || product.ID != strings.TrimSpace(product.ID) || seenProducts[product.ID] || strings.TrimSpace(product.Name) == "" || strings.TrimSpace(product.Status) == "" || len(product.Variants) == 0 {
			return "", 0, errors.New("recovery catalog has incomplete or duplicate product identity")
		}
		seenProducts[product.ID] = true
		catalogVariants := map[string]string{}
		needsDetail := strings.TrimSpace(product.UUID) == ""
		for _, variant := range product.Variants {
			if strings.TrimSpace(variant.ID) == "" || variant.ID != strings.TrimSpace(variant.ID) || seenVariants[variant.ID] {
				return "", 0, errors.New("recovery catalog has incomplete or duplicate variant identity")
			}
			seenVariants[variant.ID] = true
			catalogVariants[variant.ID] = variant.UUID
			needsDetail = needsDetail || strings.TrimSpace(variant.UUID) == ""
		}
		if strings.EqualFold(strings.TrimSpace(product.Name), strings.TrimSpace(p.Input.Name)) {
			return "", 0, errors.New("a product with the reviewed name is present; recovery is blocked")
		}
		if needsDetail {
			detail, e := m.api.Product(ctx, model.Target{OrgID: p.Input.OrgID, ProductID: product.ID})
			if e != nil {
				return "", 0, fmt.Errorf("cannot resolve recovery catalog identity: %w", e)
			}
			if detail.ID != product.ID || detail.Name != product.Name || detail.Status != product.Status || len(detail.Variants) != len(catalogVariants) || (product.UUID != "" && recoveryText(detail.UUID) != recoveryText(product.UUID)) {
				return "", 0, errors.New("recovery product detail differs from catalog identity")
			}
			detailSeen := map[string]bool{}
			for _, variant := range detail.Variants {
				uuid, ok := catalogVariants[variant.ID]
				if !ok || detailSeen[variant.ID] || (uuid != "" && recoveryText(uuid) != recoveryText(variant.UUID)) {
					return "", 0, errors.New("recovery variant detail differs from catalog identity")
				}
				detailSeen[variant.ID] = true
			}
			product = detail
		}
		resolved := recoveryProduct{ID: product.ID, Name: strings.TrimSpace(product.Name), Status: strings.ToUpper(strings.TrimSpace(product.Status)), UUID: recoveryText(product.UUID), Variants: make([]recoveryVariant, 0, len(product.Variants))}
		if resolved.UUID == "" {
			return "", 0, errors.New("recovery product UUID remains unresolved")
		}
		productUUIDRecognized := false
		if resolved.UUID == recoveryText(p.Input.UUID) {
			return "", 0, errors.New("reviewed UUID is present; recovery is blocked")
		}
		for _, variant := range product.Variants {
			uuid := recoveryText(variant.UUID)
			if uuid == "" {
				return "", 0, errors.New("recovery variant UUID remains unresolved")
			}
			if uuid == recoveryText(p.Input.UUID) {
				return "", 0, errors.New("reviewed UUID is present; recovery is blocked")
			}
			productUUIDRecognized = productUUIDRecognized || uuid == resolved.UUID
			resolved.Variants = append(resolved.Variants, recoveryVariant{ID: variant.ID, UUID: uuid})
		}
		if !productUUIDRecognized {
			return "", 0, errors.New("recovery product and variant UUID evidence conflicts")
		}
		sort.Slice(resolved.Variants, func(i, j int) bool { return resolved.Variants[i].ID < resolved.Variants[j].ID })
		snapshot = append(snapshot, resolved)
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].ID < snapshot[j].ID })
	return hash(snapshot), len(snapshot), nil
}
