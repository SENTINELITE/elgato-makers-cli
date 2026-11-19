// Package draft records exact local creation plans and refuses uncertain retries.
package draft

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

type API interface {
	CreateDraft(context.Context, maker.DraftInput) (maker.DraftResult, error)
	Products(context.Context, string) ([]model.Product, error)
	Product(context.Context, model.Target) (model.Product, error)
	Versions(context.Context, model.Target) ([]model.Version, error)
	DeletionProducts(context.Context, string) ([]model.Product, error)
}

const (
	Prepared = "prepared"
	Creating = "creating"
	Unknown  = "creation_unknown"
	Created  = "created"
	Retired  = "retired"
)

type Plan struct {
	ID                   string                 `json:"id"`
	CreatedAt            time.Time              `json:"created_at"`
	Input                maker.DraftInput       `json:"input"`
	Package              model.Package          `json:"package"`
	Digest               string                 `json:"reviewed_digest"`
	State                string                 `json:"state"`
	Target               model.Target           `json:"target"`
	CreationAcknowledged bool                   `json:"creation_acknowledged,omitempty"`
	CreationRecovery     *CreationRecoveryProof `json:"creation_recovery,omitempty"`
	Checksum             string                 `json:"checksum"`
}
type Manager struct {
	dir      string
	api      API
	progress func(string)
}

func NewManager(dir string, api API) *Manager { return &Manager{dir: dir, api: api} }

// SetProgress reports local checkpoints without response payloads or credentials.
func (m *Manager) SetProgress(callback func(string)) { m.progress = callback }

func (m *Manager) report(message string) {
	if m.progress != nil {
		m.progress(message)
	}
}

var validID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func digest(p Plan) string {
	return hash(struct {
		Input   maker.DraftInput
		Package model.Package
	}{p.Input, p.Package})
}
func checksum(p Plan) string { p.Checksum = ""; return hash(p) }

func (m *Manager) init() error {
	return filesec.SecurePrivateDir(m.dir)
}

func (m *Manager) locked(fn func() (Plan, error)) (Plan, error) {
	if err := m.init(); err != nil {
		return Plan{}, err
	}
	path := filepath.Join(m.dir, "draft.lock")
	f, err := filesec.OpenPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return Plan{}, fmt.Errorf("draft state is locked; if a previous process died, verify it has stopped before removing %s: %w", path, err)
	}
	_, err = fmt.Fprintf(f, "pid=%d time=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	closeErr := f.Close()
	defer os.Remove(path)
	if err != nil {
		return Plan{}, err
	}
	if closeErr != nil {
		return Plan{}, closeErr
	}
	return fn()
}

func (m *Manager) save(p Plan, create bool) error {
	p.Checksum = checksum(p)
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	f, err := filesec.CreateTemp(m.dir, ".plan-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	path := filepath.Join(m.dir, p.ID+".json")
	if create {
		if err = os.Link(tmp, path); err != nil {
			return err
		}
	} else {
		if err = os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return filesec.SyncDir(m.dir)
}

func (m *Manager) Load(id string) (Plan, error) {
	info, err := os.Lstat(m.dir)
	if err != nil {
		return Plan{}, err
	}
	if err = filesec.VerifyPrivateDir(m.dir, info, false); err != nil {
		return Plan{}, errors.New("draft state directory must be a private real directory (0700)")
	}
	if !validID.MatchString(id) {
		return Plan{}, errors.New("invalid draft plan ID")
	}
	path := filepath.Join(m.dir, id+".json")
	info, err = os.Lstat(path)
	if err != nil {
		return Plan{}, err
	}
	if err = filesec.VerifyPrivateFile(path, info, false); err != nil {
		return Plan{}, errors.New("draft plan must be a private regular file (0600)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, err
	}
	var p Plan
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(&p); err != nil {
		return Plan{}, fmt.Errorf("corrupt draft plan: %w", err)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return Plan{}, errors.New("corrupt draft plan: trailing data")
	}
	if p.ID != id || p.Digest != digest(p) || p.Checksum != checksum(p) {
		return Plan{}, errors.New("draft plan integrity check failed")
	}
	switch p.State {
	case Prepared, Creating, Unknown, Created, Retired:
	default:
		return Plan{}, errors.New("invalid draft state")
	}
	if (p.State == Created || p.CreationAcknowledged) && (p.Target.ProductID == "" || p.Target.VariantID == "") {
		return Plan{}, errors.New("created draft identifiers are missing")
	}
	return p, nil
}

// Prepare only writes local state. Package metadata is retained for the later
// release plan; creating a draft never reads or uploads its archive.
func (m *Manager) Prepare(input maker.DraftInput, pkg model.Package) (Plan, error) {
	if strings.TrimSpace(input.OrgID) == "" || strings.TrimSpace(input.UUID) == "" || strings.TrimSpace(input.Name) == "" || len(utf16.Encode([]rune(input.Name))) > 50 || strings.TrimSpace(input.Description) == "" || input.DescriptionPlainTextLength < 1 || input.DescriptionPlainTextLength > 4000 {
		return Plan{}, errors.New("organization, UUID, name (at most 50 characters), and description are required")
	}
	if pkg.Manifest.UUID != "" && input.UUID != pkg.Manifest.UUID {
		return Plan{}, errors.New("draft UUID differs from package UUID")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return Plan{}, err
	}
	p := Plan{ID: hex.EncodeToString(b), CreatedAt: time.Now().UTC(), Input: input, Package: pkg, State: Prepared, Target: model.Target{OrgID: input.OrgID, UUID: input.UUID, Name: input.Name}}
	p.Digest = digest(p)
	return m.locked(func() (Plan, error) {
		if err := m.save(p, true); err != nil {
			return Plan{}, err
		}
		return m.Load(p.ID)
	})
}

func (m *Manager) guardLocal(p Plan) error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == p.ID+".json" {
			continue
		}
		other, err := m.Load(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return fmt.Errorf("cannot check existing draft plan: %w", err)
		}
		if other.Input.OrgID == p.Input.OrgID && other.Input.UUID == p.Input.UUID && (other.State == Unknown || other.State == Creating) {
			return fmt.Errorf("plan %s has an unresolved creation for this organization and UUID; run draft status on that plan", other.ID)
		}
	}
	return nil
}

// lookup requires UUID evidence for every catalog entry. Otherwise the missing
// UUID could hide the very product that would make another create a duplicate.
func (m *Manager) lookup(ctx context.Context, p Plan) (*model.Target, error) {
	products, err := m.api.Products(ctx, p.Input.OrgID)
	if err != nil {
		return nil, err
	}
	var matches []model.Target
	seen := map[string]bool{}
	for _, product := range products {
		if product.ID == "" || seen[product.ID] {
			return nil, errors.New("incomplete or duplicate product catalog identifiers")
		}
		seen[product.ID] = true
		if product.UUID == "" || len(product.Variants) == 0 || product.Name == "" || product.Status == "" {
			detail, e := m.api.Product(ctx, model.Target{OrgID: p.Input.OrgID, ProductID: product.ID})
			if e != nil {
				return nil, fmt.Errorf("cannot resolve product UUID before creation: %w", e)
			}
			if detail.ID != product.ID {
				return nil, errors.New("product detail identifier mismatch")
			}
			product = detail
		}
		uuid := product.UUID
		if uuid == "" && len(product.Variants) == 1 {
			uuid = product.Variants[0].UUID
		}
		if uuid == "" {
			return nil, errors.New("product catalog lacks UUID evidence; refusing creation")
		}
		if product.Name == p.Input.Name && uuid != p.Input.UUID {
			return nil, errors.New("existing product uses this exact name with a different UUID; refusing creation")
		}
		matching := uuid == p.Input.UUID
		for _, variant := range product.Variants {
			if variant.UUID == p.Input.UUID {
				matching = true
			}
		}
		if !matching {
			continue
		}
		if !strings.EqualFold(product.Status, "DRAFT") || product.Name != p.Input.Name || len(product.Variants) != 1 || product.Variants[0].ID == "" {
			return nil, errors.New("existing product with this UUID conflicts with the exact draft name, status, or single variant")
		}
		// Some Maker catalogs expose the UUID only on the product. A single
		// identified variant plus that exact product UUID is sufficient; any
		// explicit variant UUID must agree with it.
		variant := product.Variants[0]
		if uuid != p.Input.UUID || (variant.UUID != "" && variant.UUID != p.Input.UUID) {
			return nil, errors.New("product and variant UUID evidence conflicts")
		}
		matches = append(matches, model.Target{OrgID: p.Input.OrgID, ProductID: product.ID, VariantID: variant.ID, UUID: p.Input.UUID, Name: p.Input.Name})
	}
	if len(matches) > 1 {
		return nil, errors.New("multiple matching drafts; refusing ambiguous adoption")
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return &matches[0], nil
}

func (m *Manager) adopt(p Plan, target model.Target) (Plan, error) {
	if p.Target.ProductID != "" && p.Target.ProductID != target.ProductID || p.Target.VariantID != "" && p.Target.VariantID != target.VariantID {
		return p, errors.New("remote draft differs from checkpointed identifiers; creation remains unresolved")
	}
	confirmed := p
	confirmed.Target = target
	confirmed.State = Created
	if err := m.save(confirmed, false); err != nil {
		return p, err
	}
	return m.Load(p.ID)
}

func (m *Manager) Create(ctx context.Context, id string) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if p.State == Created {
			return p, nil
		}
		if p.State == Retired {
			return p, errors.New("draft plan is retired; prepare a new plan")
		}
		if p.State != Prepared {
			return p, errors.New("draft creation outcome unresolved; do not retry; run draft status")
		}
		if err = m.guardLocal(p); err != nil {
			return p, err
		}
		m.report("Checking catalog for an existing draft")
		target, err := m.lookup(ctx, p)
		if err != nil {
			return p, err
		}
		if target != nil {
			return m.adopt(p, *target)
		}
		if err = ctx.Err(); err != nil {
			return p, err
		}
		p.State = Creating
		if err = m.save(p, false); err != nil {
			return p, err
		}
		m.report("Creating Marketplace product")
		result, dispatchErr := m.api.CreateDraft(ctx, p.Input)
		p.State = Unknown
		p.Target.ProductID = result.ProductID
		p.Target.VariantID = result.VariantID
		p.CreationAcknowledged = dispatchErr == nil && result.ProductID != "" && result.VariantID != ""
		if err = m.save(p, false); err != nil {
			return p, fmt.Errorf("creation outcome unknown; checkpoint could not be saved: %w", err)
		}
		if p.CreationAcknowledged {
			m.report("Draft creation acknowledged and saved locally")
		}
		if dispatchErr != nil {
			var notCreated interface{ NotCreated() bool }
			if result.ProductID == "" && result.VariantID == "" && errors.As(dispatchErr, &notCreated) && notCreated.NotCreated() {
				restored := p
				restored.State = Prepared
				if err = m.save(restored, false); err != nil {
					return p, fmt.Errorf("creation was not dispatched, but prepared state could not be restored: %w", err)
				}
				p, err = m.Load(id)
				if err != nil {
					return p, err
				}
				return p, fmt.Errorf("creation was not dispatched; plan remains prepared: %w", dispatchErr)
			}
			return p, fmt.Errorf("draft creation outcome unknown; do not retry; run draft status: %w", dispatchErr)
		}
		if result.ProductID == "" || result.VariantID == "" {
			return p, errors.New("draft creation response omitted identifiers; outcome unknown; run draft status")
		}
		return m.reconcile(ctx, p)
	})
}
func (m *Manager) Status(ctx context.Context, id string) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if p.State == Prepared || p.State == Retired {
			return p, nil
		}
		return m.reconcile(ctx, p)
	})
}
func (m *Manager) reconcile(ctx context.Context, p Plan) (Plan, error) {
	m.report("Reading draft to verify saved creation identifiers")
	var target *model.Target
	var err error
	if p.CreationAcknowledged {
		target, err = m.lookupAcknowledged(ctx, p)
	} else {
		target, err = m.lookup(ctx, p)
	}
	if err != nil {
		return p, fmt.Errorf("cannot reconcile draft; stored state remains %s: %w", p.State, err)
	}
	if target == nil {
		return p, errors.New("matching draft is not visible; creation remains unresolved; do not retry")
	}
	return m.adopt(p, *target)
}

// lookupAcknowledged permits a UUID that Maker has not populated yet only for a
// successful create response durably checkpointed by this plan.
func (m *Manager) lookupAcknowledged(ctx context.Context, p Plan) (*model.Target, error) {
	if !p.CreationAcknowledged || (p.State != Unknown && p.State != Created) || p.Target.OrgID != p.Input.OrgID || p.Target.UUID != p.Input.UUID || p.Target.Name != p.Input.Name || p.Target.ProductID == "" || p.Target.VariantID == "" {
		return nil, errors.New("creation receipt does not match the reviewed draft")
	}
	product, err := m.api.Product(ctx, p.Target)
	if err != nil {
		return nil, err
	}
	if product.ID != p.Target.ProductID || product.Name != p.Input.Name || !model.SameListingDescription(product.Description, p.Input.Description) || !strings.EqualFold(product.Status, "DRAFT") || len(product.Variants) != 1 || product.Variants[0].ID != p.Target.VariantID {
		return nil, errors.New("acknowledged draft conflicts with the exact identifiers, name, description, status, or single variant")
	}
	if product.UUID != "" && product.UUID != p.Input.UUID || product.Variants[0].UUID != "" && product.Variants[0].UUID != p.Input.UUID {
		return nil, errors.New("acknowledged draft UUID conflicts with the reviewed UUID")
	}
	versions, err := m.api.Versions(ctx, p.Target)
	if err != nil {
		return nil, fmt.Errorf("cannot verify acknowledged draft versions: %w", err)
	}
	if len(versions) != 0 {
		return nil, errors.New("acknowledged draft already has versions")
	}
	target := p.Target
	return &target, nil
}

// Receipt returns only the explicit, checksummed acknowledgment of this plan's
// successful create response. Legacy saved identifiers do not establish it.
func (m *Manager) Receipt(id string) (maker.CreationReceipt, error) {
	p, err := m.Load(id)
	if err != nil {
		return maker.CreationReceipt{}, err
	}
	if !p.CreationAcknowledged || (p.State != Unknown && p.State != Created) || p.Target.OrgID != p.Input.OrgID || p.Target.UUID != p.Input.UUID || p.Target.Name != p.Input.Name || p.Target.ProductID == "" || p.Target.VariantID == "" {
		return maker.CreationReceipt{}, errors.New("draft plan has no matching creation acknowledgment")
	}
	return maker.CreationReceipt{Target: p.Target, Name: p.Input.Name, Description: p.Input.Description}, nil
}

// RetireAfterAbsence retires only this local journal after a complete read proves
// its identified remote product absent. It never deletes a Maker product.
func (m *Manager) RetireAfterAbsence(ctx context.Context, id string) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if p.State == Retired {
			return p, nil
		}
		if p.Target.OrgID != p.Input.OrgID || p.Target.ProductID == "" || p.Target.VariantID == "" {
			return p, errors.New("cannot retire draft without exact saved organization, product, and variant identifiers")
		}
		products, err := m.api.DeletionProducts(ctx, p.Input.OrgID)
		if err != nil {
			return p, fmt.Errorf("cannot establish draft deletion: %w", err)
		}
		seen := map[string]bool{}
		for _, product := range products {
			if product.ID == "" || seen[product.ID] {
				return p, errors.New("cannot establish deletion from incomplete or duplicate product identifiers")
			}
			seen[product.ID] = true
			if product.ID == p.Target.ProductID {
				return p, errors.New("draft product is still present; local plan remains active")
			}
			for _, variant := range product.Variants {
				if variant.ID == p.Target.VariantID {
					return p, errors.New("draft variant is still present; local plan remains active")
				}
			}
		}
		p.State = Retired
		if err := m.save(p, false); err != nil {
			return p, err
		}
		return m.Load(id)
	})
}
