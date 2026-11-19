// Package v1 exposes the CLI's durable business workflows to authenticated hosts.
// Hosts validate owner isolation and artifact paths, obtain approval bound to Ref,
// and retain the same owner state root across requests. No approval UI runs here.
package v1

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/SENTINELITE/elgato-makers-cli/internal/artifact"
	"github.com/SENTINELITE/elgato-makers-cli/internal/deletion"
	"github.com/SENTINELITE/elgato-makers-cli/internal/discounts"
	"github.com/SENTINELITE/elgato-makers-cli/internal/draft"
	"github.com/SENTINELITE/elgato-makers-cli/internal/listing"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/media"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
	"github.com/SENTINELITE/elgato-makers-cli/pkg/connect"
	"github.com/SENTINELITE/elgato-makers-cli/pkg/discountplan"
)

const Version = "v1"

type ListingConfig = listing.Config
type ListingPricing = listing.Pricing
type ListingAssets = listing.Assets
type Target = model.Target
type DraftInput = maker.DraftInput
type Kind string

const (
	Listing  Kind = "listing"
	Release  Kind = "release"
	Draft    Kind = "draft"
	Deletion Kind = "deletion"
	Discount Kind = "discount"
)

type Action string

const (
	Stage           Action = "stage"
	Submit          Action = "submit"
	Create          Action = "create"
	Delete          Action = "delete"
	Discard         Action = "discard"
	Retire          Action = "retire"
	RecoverCreation Action = "recover_creation"
)

// Ref binds approval to the complete reviewed checkpoint, including its state.
type Ref struct {
	Version string `json:"version"`
	Kind    Kind   `json:"kind"`
	ID      string `json:"id"`
	Digest  string `json:"digest"`
}
type Review struct {
	Ref  Ref             `json:"ref"`
	Plan json.RawMessage `json:"plan"`
}
type Service struct {
	mu        sync.Mutex
	org       string
	api       *maker.Client
	listings  *listing.Manager
	releases  *release.Manager
	drafts    *draft.Manager
	deletions *deletion.Manager
	discounts discounts.Store
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,255}$`)

// New requires a host-validated absolute private per-owner root. It does not
// accept owner IDs or state paths from tool callers. Reuse one Service per root.
func New(root, org string, client *connect.Client) (*Service, error) {
	if !filepath.IsAbs(root) || !identifier.MatchString(org) || org == "." || org == ".." || client == nil {
		return nil, errors.New("invalid workflow host configuration")
	}
	api := client.WorkflowBackend()
	return &Service{org: org, api: api, listings: listing.NewManager(filepath.Join(root, "listings"), api), releases: release.NewManager(filepath.Join(root, "releases"), api), drafts: draft.NewManager(filepath.Join(root, "drafts"), api), deletions: deletion.NewManager(filepath.Join(root, "deletions"), api), discounts: discounts.Store{Dir: filepath.Join(root, "discounts")}}, nil
}
func review(k Kind, id string, p any, err error) (Review, error) {
	b, e := json.Marshal(p)
	if e != nil {
		return Review{}, e
	}
	sum := sha256.Sum256(b)
	return Review{Ref: Ref{Version: Version, Kind: k, ID: id, Digest: hex.EncodeToString(sum[:])}, Plan: b}, err
}
func (s *Service) target(t Target) (Target, error) {
	if t.OrgID != "" && t.OrgID != s.org {
		return t, errors.New("organization mismatch")
	}
	t.OrgID = s.org
	return t, nil
}
func (s *Service) PrepareListing(c ListingConfig) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.OrganizationID != "" && c.OrganizationID != s.org {
		return Review{}, errors.New("organization mismatch")
	}
	c.OrganizationID = s.org
	p, e := listing.PrepareConfig(c)
	if e != nil {
		return Review{}, e
	}
	v, e := s.listings.Prepare(p, s.org)
	return review(Listing, v.ID, v, e)
}

// ReorderListing returns a config to prepare and review as a new plan.
func ReorderListing(c ListingConfig, order []int) (ListingConfig, error) {
	return listing.Reorder(c, order)
}
func (s *Service) PrepareRelease(ctx context.Context, t Target, pkg, notes string, autoPublish bool) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, e := s.target(t)
	if e != nil {
		return Review{}, e
	}
	v, e := s.releases.Prepare(ctx, t, pkg, notes, autoPublish)
	return review(Release, v.ID, v, e)
}
func (s *Service) PrepareDraft(input DraftInput, packagePath string) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if input.OrgID != "" && input.OrgID != s.org {
		return Review{}, errors.New("organization mismatch")
	}
	input.OrgID = s.org
	p, e := artifact.Inspect(packagePath)
	if e != nil {
		return Review{}, e
	}
	v, e := s.drafts.Prepare(input, p)
	return review(Draft, v.ID, v, e)
}
func (s *Service) PrepareDeletion(ctx context.Context, t Target) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, e := s.target(t)
	if e != nil {
		return Review{}, e
	}
	v, e := s.deletions.Prepare(ctx, t)
	return review(Deletion, v.ID, v, e)
}
func (s *Service) PrepareDiscount(i discountplan.Input) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i.OrganizationID != "" && i.OrganizationID != s.org {
		return Review{}, errors.New("organization mismatch")
	}
	i.OrganizationID = s.org
	p, e := discountplan.Prepare(i)
	if e != nil {
		return Review{}, e
	}
	if e = s.discounts.SavePlan(p); e != nil {
		return Review{}, e
	}
	return s.show(Discount, p.Digest)
}
func (s *Service) Show(k Kind, id string) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.show(k, id)
}
func (s *Service) show(k Kind, id string) (Review, error) {
	switch k {
	case Listing:
		p, e := s.listings.Load(id)
		if e == nil && p.Target.OrgID != s.org {
			return Review{}, errors.New("organization mismatch")
		}
		return review(k, id, p, e)
	case Release:
		p, e := s.releases.Load(id)
		if e == nil && p.Target.OrgID != s.org {
			return Review{}, errors.New("organization mismatch")
		}
		return review(k, id, p, e)
	case Draft:
		p, e := s.drafts.Load(id)
		if e == nil && p.Input.OrgID != s.org {
			return Review{}, errors.New("organization mismatch")
		}
		return review(k, id, p, e)
	case Deletion:
		p, e := s.deletions.Load(id)
		if e == nil && p.Target.OrgID != s.org {
			return Review{}, errors.New("organization mismatch")
		}
		return review(k, id, p, e)
	case Deactivation:
		return s.showDeactivation(id)
	case Discount:
		p, e := s.discounts.Load(id)
		if e == nil && p.Plan.Input.OrganizationID != s.org {
			return Review{}, errors.New("organization mismatch")
		}
		return review(k, id, p, e)
	}
	return Review{}, errors.New("unsupported workflow kind")
}

// Apply must only be invoked after the host verifies approval of this exact Ref.
// RecoverCreation additionally requires explicit acknowledgment of uncertainty.
func (s *Service) Apply(ctx context.Context, a Action, r Ref, acknowledgedUncertainty bool) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, e := s.show(r.Kind, r.ID)
	if e != nil {
		return Review{}, e
	}
	if current.Ref != r {
		return Review{}, errors.New("reviewed plan changed; review again")
	}
	switch r.Kind {
	case Listing:
		var p listing.Plan
		switch a {
		case Stage:
			p, e = s.listings.Upload(ctx, r.ID)
		case Submit:
			p, e = s.listings.Submit(ctx, r.ID, release.ConfirmOptions{Yes: true})
		case Discard:
			p, e = s.listings.Discard(r.ID)
		case Retire:
			p, e = s.listings.Retire(ctx, r.ID)
		case RecoverCreation:
			p, e = s.listings.RecoverCreation(ctx, r.ID, acknowledgedUncertainty)
		default:
			return Review{}, errors.New("unsupported listing action")
		}
		return review(r.Kind, r.ID, p, e)
	case Release:
		var p release.Plan
		switch a {
		case Stage:
			p, e = s.releases.Upload(ctx, r.ID)
		case Submit:
			p, e = s.releases.Submit(ctx, r.ID, release.ConfirmOptions{Yes: true})
		default:
			return Review{}, errors.New("unsupported release action")
		}
		return review(r.Kind, r.ID, p, e)
	case Draft:
		var p draft.Plan
		switch a {
		case Create:
			p, e = s.drafts.Create(ctx, r.ID)
		case Retire:
			p, e = s.drafts.RetireAfterAbsence(ctx, r.ID)
		case RecoverCreation:
			p, e = s.drafts.RetireUnacknowledged(ctx, r.ID, acknowledgedUncertainty)
		default:
			return Review{}, errors.New("unsupported draft action")
		}
		return review(r.Kind, r.ID, p, e)
	case Deletion:
		if a == Delete {
			p, e := s.deletions.Delete(ctx, r.ID)
			return review(r.Kind, r.ID, p, e)
		}
	case Discount:
		if a == Create {
			p, e := s.discounts.Create(ctx, r.ID, s.api)
			return review(r.Kind, r.ID, p, e)
		}
	}
	return Review{}, errors.New("unsupported workflow action")
}

// Status performs the managers' read-only remote reconciliation and checkpoints it.
func (s *Service) Status(ctx context.Context, k Kind, id string) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, e := s.show(k, id); e != nil {
		return Review{}, e
	}
	switch k {
	case Listing:
		p, e := s.listings.Status(ctx, id)
		return review(k, id, p, e)
	case Release:
		p, e := s.releases.Status(ctx, id)
		return review(k, id, p, e)
	case Draft:
		p, e := s.drafts.Status(ctx, id)
		return review(k, id, p, e)
	case Deletion:
		p, e := s.deletions.Status(ctx, id)
		return review(k, id, p, e)
	case Discount:
		p, e := s.discounts.Status(ctx, id, s.api)
		return review(k, id, p, e)
	}
	return Review{}, errors.New("unsupported workflow kind")
}
func (s *Service) Product(ctx context.Context, t Target) (json.RawMessage, error) {
	t, e := s.target(t)
	if e != nil {
		return nil, e
	}
	p, e := s.api.Product(ctx, t)
	if e != nil {
		return nil, e
	}
	return json.Marshal(p)
}

// RecoverReleaseReceipt reconciles exact original and canonical IDs without submitting.
func (s *Service) RecoverReleaseReceipt(ctx context.Context, r Ref, storedID, canonicalID string) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, e := s.show(Release, r.ID)
	if e != nil {
		return Review{}, e
	}
	if p.Ref != r {
		return Review{}, errors.New("reviewed plan changed")
	}
	v, e := s.releases.ReconcileAcknowledgedVersion(ctx, r.ID, storedID, canonicalID)
	return review(Release, r.ID, v, e)
}

// SetProgress configures credential-free checkpoint messages for host jobs.
func (s *Service) SetProgress(fn func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listings.SetProgress(fn)
	s.drafts.SetProgress(fn)
}

// Package and Notes expose the exact validated artifact representations.
type Package = model.Package
type Notes = model.Notes
type DiscountInput = discountplan.Input

// InspectPackage validates a host-resolved local upload using the CLI validator.
func InspectPackage(path string) (Package, error) { return artifact.Inspect(path) }

// ReadNotes validates a host-resolved notes upload using the CLI validator.
func ReadNotes(path string) (Notes, error) { return artifact.ReadNotes(path) }

// MediaAsset is the validated media metadata representation.
type MediaAsset = media.Asset

// InspectMedia validates an image or video with the CLI's existing requirements.
func InspectMedia(path, kind string) (MediaAsset, error) { return media.Inspect(path, kind) }
