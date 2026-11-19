package listing

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/SENTINELITE/elgato-makers-cli/internal/draft"
)

var recoveryCatalogHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validCreationRecovery(proof *draft.CreationRecoveryProof) bool {
	return proof != nil && !proof.CheckedAt.IsZero() && proof.ProductCount >= 0 && recoveryCatalogHash.MatchString(proof.CatalogSHA256)
}

// ValidateCreationRecovery reads local records only. It allows callers to reject
// ineligible plans before confirmation, authentication, or network activity.
func (m *Manager) ValidateCreationRecovery(id string) error {
	_, _, err := m.creationRecoveryPlans(id)
	return err
}

func (m *Manager) creationRecoveryPlans(id string) (Plan, draft.Plan, error) {
	p, err := m.Load(id)
	if err != nil {
		return p, draft.Plan{}, err
	}
	if p.State != Ready && p.State != Retired || p.State == Retired && !validCreationRecovery(p.CreationRecovery) || p.State != Retired && p.CreationRecovery != nil {
		return p, draft.Plan{}, errors.New("only an untouched listing with an unresolved creation, or its completed recovery record, can be recovered")
	}
	if p.DraftPlanID == "" || p.ReleasePlanID != "" || p.Target.ProductID != "" || p.Target.VariantID != "" || p.Prepared.Config.ProductID != "" || p.Prepared.Config.VariantID != "" || p.Details.Target.ProductID != "" || p.Details.Target.VariantID != "" || p.DetailsState != Ready || p.PackageState != Ready || p.EarlyDetails != nil || p.EarlyDetailsState != "" {
		return p, draft.Plan{}, errors.New("creation recovery requires a linked draft with no known remote identifiers, release, package, or listing writes")
	}
	if p.Target.OrgID == "" || p.Target.UUID == "" || p.Target.UUID != p.Prepared.Package.Manifest.UUID || p.Target.Name != p.Prepared.Config.Name || p.Prepared.Config.OrganizationID != "" && p.Prepared.Config.OrganizationID != p.Target.OrgID {
		return p, draft.Plan{}, errors.New("creation recovery listing identity does not match the reviewed plan")
	}
	for _, media := range p.Media {
		if media.State != Ready || media.Result.FileID != "" || media.Result.MediaID != "" || media.Result.Acknowledged || media.Result.Reconciled {
			return p, draft.Plan{}, errors.New("listing with media write evidence cannot recover an uncertain creation")
		}
	}
	dp, err := m.drafts().Load(p.DraftPlanID)
	if err != nil {
		return p, dp, err
	}
	if err = validateChild(p, dp); err != nil {
		return p, dp, err
	}
	if dp.Input.DescriptionPlainTextLength != p.Prepared.Description.PlainTextLength || dp.Input.ExtensionID != "" || dp.Target.UUID != dp.Input.UUID || dp.CreationAcknowledged || dp.Target.ProductID != "" || dp.Target.VariantID != "" {
		return p, dp, errors.New("draft creation evidence or identity is not eligible for unacknowledged recovery")
	}
	if p.State == Retired {
		if dp.State != draft.Retired || !validCreationRecovery(dp.CreationRecovery) || !sameRecoveryProof(p.CreationRecovery, dp.CreationRecovery) {
			return p, dp, errors.New("completed listing recovery does not match its retired child proof")
		}
	} else if dp.State == draft.Unknown {
		if dp.CreationRecovery != nil {
			return p, dp, errors.New("unresolved child contains unexpected recovery evidence")
		}
	} else if dp.State != draft.Retired || !validCreationRecovery(dp.CreationRecovery) {
		return p, dp, errors.New("only an unacknowledged unknown draft, or its retired recovery record, can be recovered")
	}
	return p, dp, nil
}

func sameRecoveryProof(a, b *draft.CreationRecoveryProof) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.CheckedAt.Equal(b.CheckedAt) && a.CatalogSHA256 == b.CatalogSHA256 && a.ProductCount == b.ProductCount
}

func sameRecoveryChild(a, b draft.Plan) bool {
	a.State, b.State = "", ""
	a.Checksum, b.Checksum = "", ""
	a.CreationRecovery, b.CreationRecovery = nil, nil
	first, _ := json.Marshal(a)
	second, _ := json.Marshal(b)
	return string(first) == string(second)
}

// RecoverCreation records an operator's explicit acceptance of a past unknown
// outcome. Catalog checks reduce uncertainty; they do not prove non-creation.
// This operation never creates, updates, uploads, deletes, or submits remotely.
func (m *Manager) RecoverCreation(ctx context.Context, id string, acknowledgedUncertainty bool) (Plan, error) {
	if !acknowledgedUncertainty {
		return Plan{}, errors.New("explicit acknowledgment of the uncertain past creation outcome is required")
	}
	return m.locked(func() (Plan, error) {
		p, before, err := m.creationRecoveryPlans(id)
		if err != nil {
			return p, err
		}
		if p.State == Retired {
			return p, nil
		}
		if err = ctx.Err(); err != nil {
			return p, err
		}
		m.report("Checking catalog before recording explicit creation uncertainty recovery")
		after, err := m.drafts().RetireUnacknowledged(ctx, p.DraftPlanID, true)
		if err != nil {
			return p, err
		}
		if !sameRecoveryChild(before, after) || after.State != draft.Retired || !validCreationRecovery(after.CreationRecovery) {
			return p, errors.New("draft child changed during creation recovery; listing ownership remains unchanged")
		}
		if err = validateChild(p, after); err != nil {
			return p, err
		}
		current, err := m.drafts().Load(p.DraftPlanID)
		if err != nil {
			return p, err
		}
		if current.Checksum != after.Checksum {
			return p, errors.New("draft recovery record changed before listing recovery could be committed")
		}
		proof := *after.CreationRecovery
		p.CreationRecovery = &proof
		p.State = Retired
		m.report("Recording accepted creation uncertainty in the local listing audit")
		if err = m.save(p, false); err != nil {
			return p, err
		}
		return m.Load(id)
	})
}
