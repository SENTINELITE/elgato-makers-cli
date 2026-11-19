package listing

import (
	"bufio"
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

	"github.com/SENTINELITE/elgato-makers-cli/internal/draft"
	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
)

// API requires read-only contract discovery before any listing mutation.
type API interface {
	draft.API
	release.API
	Metadata(context.Context, string) (maker.ListingMetadata, error)
	PreflightListing(context.Context, string) error
	ValidateExistingListingPlan(context.Context, model.Target, bool, int) error
	ValidateGallery(context.Context, model.Target, []maker.MediaInput) error
	ReadUploadTags(context.Context, string) (map[string][]string, error)
	UpdateListing(context.Context, maker.ListingDetailsInput) error
	UploadMedia(context.Context, maker.MediaInput) (maker.MediaResult, error)
	VerifyListing(context.Context, maker.ListingDetailsInput) error
	VerifyListingReady(context.Context, model.Target, maker.ListingDetailsInput, []maker.MediaInput) error
	DeletionProducts(context.Context, string) ([]model.Product, error)
}

// ReadPhaseAPI optionally shares page reads within one read-only check.
type ReadPhaseAPI interface {
	BeginReadPhase(context.Context) context.Context
}

func (m *Manager) beginReadPhase(ctx context.Context) context.Context {
	if api, ok := m.api.(ReadPhaseAPI); ok {
		return api.BeginReadPhase(ctx)
	}
	return ctx
}

// Reconcilers may acknowledge an uncertain mutation only from exact remote
// evidence. Absence of a result never authorizes retrying a dispatched write.
type ListingReconciler interface {
	ReconcileListing(context.Context, maker.ListingDetailsInput) (bool, error)
}
type MediaReconciler interface {
	ReconcileMedia(context.Context, maker.MediaInput, maker.MediaResult) (maker.MediaResult, error)
}

const (
	Ready     = "prepared"
	Inflight  = "inflight"
	Unknown   = "unknown"
	Done      = "done"
	Uploaded  = "uploaded"
	Submitted = "submitted"
	Discarded = "discarded"
	Retired   = "retired"
)

type MediaCheckpoint struct {
	Asset  Asset             `json:"asset"`
	State  string            `json:"state"`
	Result maker.MediaResult `json:"result"`
}
type Plan struct {
	ID                string                       `json:"id"`
	CreatedAt         time.Time                    `json:"created_at"`
	Prepared          Prepared                     `json:"prepared"`
	Target            model.Target                 `json:"target"`
	State             string                       `json:"state"`
	DraftPlanID       string                       `json:"draft_plan_id,omitempty"`
	ReleasePlanID     string                       `json:"release_plan_id,omitempty"`
	Details           maker.ListingDetailsInput    `json:"details"`
	DetailsState      string                       `json:"details_state"`
	Media             []MediaCheckpoint            `json:"media"`
	PackageState      string                       `json:"package_state"`
	EarlyDetails      *maker.ListingDetailsInput   `json:"early_details,omitempty"`
	EarlyDetailsState string                       `json:"early_details_state,omitempty"`
	CreationRecovery  *draft.CreationRecoveryProof `json:"creation_recovery,omitempty"`
	Checksum          string                       `json:"checksum"`
}
type Manager struct {
	dir      string
	api      API
	progress func(string)
}

func NewManager(dir string, api API) *Manager  { return &Manager{dir: dir, api: api} }
func (m *Manager) SetProgress(fn func(string)) { m.progress = fn }
func (m *Manager) report(s string) {
	if m.progress != nil {
		m.progress(s)
	}
}
func (m *Manager) drafts() *draft.Manager {
	dm := draft.NewManager(filepath.Join(m.dir, "drafts"), m.api)
	dm.SetProgress(m.progress)
	return dm
}
func (m *Manager) releases() *release.Manager {
	return release.NewManager(filepath.Join(m.dir, "releases"), m.api)
}

var planID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func planHash(p Plan) string {
	p.Checksum = ""
	b, _ := json.Marshal(p)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func (m *Manager) init() error {
	return filesec.SecurePrivateDir(m.dir)
}
func (m *Manager) locked(fn func() (Plan, error)) (Plan, error) {
	if e := m.init(); e != nil {
		return Plan{}, e
	}
	path := filepath.Join(m.dir, "listing.lock")
	f, e := filesec.OpenPrivateFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if e != nil {
		return Plan{}, fmt.Errorf("listing state locked; verify previous process stopped before removing %s: %w", path, e)
	}
	defer os.Remove(path)
	_, e = fmt.Fprintf(f, "pid=%d time=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	ce := f.Close()
	if e != nil {
		return Plan{}, e
	}
	if ce != nil {
		return Plan{}, ce
	}
	return fn()
}
func (m *Manager) save(p Plan, create bool) error {
	p.Checksum = planHash(p)
	b, e := json.MarshalIndent(p, "", "  ")
	if e != nil {
		return e
	}
	f, e := filesec.CreateTemp(m.dir, ".listing-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = f.Write(append(b, '\n'))
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	path := filepath.Join(m.dir, p.ID+".json")
	if create {
		e = os.Link(f.Name(), path)
	} else {
		e = os.Rename(f.Name(), path)
	}
	if e != nil {
		return e
	}
	return filesec.SyncDir(m.dir)
}
func (m *Manager) Load(id string) (Plan, error) {
	var p Plan
	if !planID.MatchString(id) {
		return p, errors.New("invalid listing plan ID")
	}
	s, e := os.Lstat(m.dir)
	if e != nil {
		return p, e
	}
	if filesec.VerifyPrivateDir(m.dir, s, false) != nil {
		return p, errors.New("listing state directory must be private (0700)")
	}
	path := filepath.Join(m.dir, id+".json")
	s, e = os.Lstat(path)
	if e != nil {
		return p, e
	}
	if filesec.VerifyPrivateFile(path, s, false) != nil {
		return p, errors.New("listing plan must be a private regular file (0600)")
	}
	f, e := os.Open(path)
	if e != nil {
		return p, e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 8<<20))
	d.DisallowUnknownFields()
	if e = d.Decode(&p); e != nil {
		return p, e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return p, errors.New("listing plan trailing data")
	}
	if p.ID != id || p.Checksum != planHash(p) {
		return p, errors.New("listing plan integrity check failed")
	}
	return p, nil
}
func (m *Manager) Prepare(prepared Prepared, org string) (Plan, error) {
	if e := Verify(prepared); e != nil {
		return Plan{}, e
	}
	if org == "" {
		org = prepared.Config.OrganizationID
	}
	if org == "" || prepared.Config.OrganizationID != "" && prepared.Config.OrganizationID != org {
		return Plan{}, errors.New("exact matching organization required")
	}
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return Plan{}, e
	}
	p := Plan{ID: hex.EncodeToString(b), CreatedAt: time.Now().UTC(), Prepared: prepared, State: Ready, DetailsState: Ready, PackageState: Ready, Target: model.Target{OrgID: org, UUID: prepared.Package.Manifest.UUID, Name: prepared.Config.Name, ProductID: prepared.Config.ProductID, VariantID: prepared.Config.VariantID}}
	for _, a := range append([]Asset{prepared.Icon, prepared.Thumbnail}, prepared.Gallery...) {
		p.Media = append(p.Media, MediaCheckpoint{Asset: a, State: Ready})
	}
	return m.locked(func() (Plan, error) {
		if e := m.guard(p); e != nil {
			return p, e
		}
		if e := m.save(p, true); e != nil {
			return p, e
		}
		return m.Load(p.ID)
	})
}
func (m *Manager) guard(p Plan) error {
	entries, e := os.ReadDir(m.dir)
	if e != nil {
		return e
	}
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".json") || ent.Name() == p.ID+".json" {
			continue
		}
		other, e := m.Load(strings.TrimSuffix(ent.Name(), ".json"))
		if e != nil {
			return e
		}
		if other.Target.OrgID == p.Target.OrgID && other.Target.UUID == p.Target.UUID && other.State != Submitted && other.State != Discarded && other.State != Retired {
			return fmt.Errorf("listing plan %s already owns this organization and UUID; resume it", other.ID)
		}
	}
	return nil
}

// Discard releases local ownership only when no remote work has begun. Keep
// the record for review, including when its original files have since changed.
func (m *Manager) Discard(id string) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		pristine := p.State == Ready && p.PackageState == Ready && p.DetailsState == Ready && p.DraftPlanID == "" && p.ReleasePlanID == ""
		for _, item := range p.Media {
			if item.State != Ready || item.Result.FileID != "" || item.Result.MediaID != "" || item.Result.Acknowledged || item.Result.Reconciled {
				pristine = false
			}
		}
		if !pristine {
			return p, errors.New("only a pristine listing plan with no remote child plans or mutation checkpoints can be discarded")
		}
		p.State = Discarded
		if err = m.save(p, false); err != nil {
			return p, err
		}
		return m.Load(id)
	})
}

// Retire releases only local ownership after a strict read-only catalog proves
// that the known product and variant are absent. The audit records are kept.
func (m *Manager) Retire(ctx context.Context, id string) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if p.State == Retired {
			return p, nil
		}
		if p.State == Submitted || p.State == Uploaded || p.State == Discarded || p.ReleasePlanID != "" || p.PackageState != Ready || p.DetailsState != Ready || p.EarlyDetailsState != "" && p.EarlyDetailsState != Ready {
			return p, errors.New("listing with package, detail, submission, or release evidence cannot be retired")
		}
		for _, item := range p.Media {
			if item.State != Ready || item.Result.FileID != "" || item.Result.MediaID != "" || item.Result.Acknowledged || item.Result.Reconciled {
				return p, errors.New("listing with media write evidence cannot be retired")
			}
		}
		target := p.Target
		if p.DraftPlanID != "" {
			dp, e := m.drafts().Load(p.DraftPlanID)
			if e != nil {
				return p, e
			}
			if e = validateChild(p, dp); e != nil {
				return p, e
			}
			if dp.Target.ProductID == "" || dp.Target.VariantID == "" {
				return p, errors.New("draft has no complete known remote identifiers; cannot prove retirement")
			}
			target = dp.Target
		}
		if target.ProductID == "" || target.VariantID == "" {
			return p, errors.New("complete known remote identifiers are required for retirement")
		}
		m.report("Reading complete catalog to verify deleted product is absent")
		products, err := m.api.DeletionProducts(ctx, target.OrgID)
		if err != nil {
			return p, err
		}
		seen := map[string]bool{}
		for _, product := range products {
			if product.ID == "" || product.Status == "" || seen[product.ID] {
				return p, errors.New("incomplete or ambiguous catalog; retirement refused")
			}
			seen[product.ID] = true
			if product.ID == target.ProductID {
				return p, errors.New("known product remains in the remote catalog; retirement refused")
			}
			for _, variant := range product.Variants {
				if variant.ID == "" || variant.ID == target.VariantID {
					return p, errors.New("known variant remains or catalog is incomplete; retirement refused")
				}
			}
		}
		if p.DraftPlanID != "" {
			if _, err = m.drafts().RetireAfterAbsence(ctx, p.DraftPlanID); err != nil {
				return p, err
			}
		}
		p.Target = target
		p.State = Retired
		if err = m.save(p, false); err != nil {
			return p, err
		}
		return m.Load(id)
	})
}
func (m *Manager) verify(ctx context.Context, p Plan) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	return Verify(p.Prepared)
}

// creationContext grants the narrow missing-UUID allowance only from this
// listing's immutable, acknowledged child creation plan.
func (m *Manager) creationContext(ctx context.Context, p Plan) (context.Context, bool, error) {
	if p.DraftPlanID == "" {
		return ctx, false, nil
	}
	dp, err := m.drafts().Load(p.DraftPlanID)
	if err != nil {
		return ctx, false, err
	}
	if err = validateChild(p, dp); err != nil {
		return ctx, false, err
	}
	if !dp.CreationAcknowledged {
		return ctx, false, nil
	}
	receipt, err := m.drafts().Receipt(p.DraftPlanID)
	if err != nil {
		return ctx, false, err
	}
	ctx, err = maker.WithCreationReceipt(ctx, receipt)
	return ctx, err == nil, err
}
func validateChild(p Plan, dp draft.Plan) error {
	a, _ := json.Marshal(dp.Package)
	b, _ := json.Marshal(p.Prepared.Package)
	if dp.Input.OrgID != p.Target.OrgID || dp.Input.UUID != p.Prepared.Package.Manifest.UUID || dp.Input.Name != p.Prepared.Config.Name || dp.Input.Description != p.Prepared.Description.Markdown || string(a) != string(b) || dp.Target.OrgID != p.Target.OrgID || dp.Target.UUID != p.Target.UUID || dp.Target.Name != p.Prepared.Config.Name {
		return errors.New("draft child differs from the reviewed listing identity or package")
	}
	if p.Target.ProductID != "" && dp.Target.ProductID != p.Target.ProductID || p.Target.VariantID != "" && dp.Target.VariantID != p.Target.VariantID {
		return errors.New("draft child identifiers differ from listing")
	}
	return nil
}
func (m *Manager) preflight(ctx context.Context, p *Plan) error {
	ctx = m.beginReadPhase(ctx)
	var ownReceipt bool
	var contextErr error
	ctx, ownReceipt, contextErr = m.creationContext(ctx, *p)
	if contextErr != nil {
		return contextErr
	}
	if e := m.verify(ctx, *p); e != nil {
		return e
	}
	if e := m.guard(*p); e != nil {
		return e
	}
	if e := m.api.PreflightListing(ctx, p.Target.OrgID); e != nil {
		return e
	}
	meta, e := m.api.Metadata(ctx, p.Target.OrgID)
	if e != nil {
		return e
	}
	for label := range p.Prepared.Config.Links {
		found := false
		for _, category := range meta.LinkCategories {
			if category == label {
				found = true
				break
			}
		}
		if !found {
			return errors.New("listing link category is not available")
		}
	}
	details := maker.ListingDetailsInput{Target: p.Target, Price: p.Prepared.Config.Pricing.Amount, IsFree: p.Prepared.Config.Pricing.Type == "free", Links: p.Prepared.Config.Links, ExtraTagIDs: p.Details.ExtraTagIDs}
	if details.IsFree {
		details.Price = "0"
	}
	for _, v := range p.Prepared.Config.Languages {
		id, e := meta.Resolve("language", v)
		if e != nil {
			return e
		}
		details.LanguageIDs = append(details.LanguageIDs, id)
	}
	for _, v := range p.Prepared.Config.Categories {
		id, e := meta.Resolve("category", v)
		if e != nil {
			return e
		}
		details.CategoryIDs = append(details.CategoryIDs, id)
	}
	// Validate wire values before a new SKU exists. Placeholder identifiers
	// exercise the same validation as a real target without dispatching it.
	validated := details
	if validated.Target.ProductID == "" {
		validated.Target.ProductID = "preflight-product"
	}
	if validated.Target.VariantID == "" {
		validated.Target.VariantID = "preflight-variant"
	}
	if e := maker.ValidateListingDetails(validated); e != nil {
		return e
	}
	for _, item := range p.Media {
		if e := maker.ValidateMediaInput(mediaInput(validated.Target, item.Asset)); e != nil {
			return e
		}
	}
	if p.EarlyDetails != nil {
		early := details
		early.ExtraTagIDs = nil
		old := *p.EarlyDetails
		old.ExtraTagIDs = nil
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(early)
		if string(a) != string(b) {
			return errors.New("early listing details changed since mutation; refusing resume")
		}
	}
	if p.DetailsState != Ready {
		old, _ := json.Marshal(p.Details)
		fresh, _ := json.Marshal(details)
		if string(old) != string(fresh) {
			return errors.New("resolved taxonomy changed since listing mutation; refusing resume")
		}
	}
	p.Details = details
	if p.Target.ProductID != "" {
		product, e := m.api.Product(ctx, p.Target)
		if e != nil {
			return e
		}
		if product.ID != p.Target.ProductID || product.Status == "" {
			return errors.New("existing listing requires exact product and status evidence")
		}
		if product.Name != p.Prepared.Config.Name || !model.SameListingDescription(product.Description, p.Prepared.Description.Markdown) {
			return errors.New("existing draft name or description differs; update contract is not verified")
		}
		var matches []model.Variant
		for _, v := range product.Variants {
			if p.Target.VariantID == "" || v.ID == p.Target.VariantID {
				matches = append(matches, v)
			}
		}
		if len(matches) != 1 || matches[0].ID == "" {
			return errors.New("existing draft variant is ambiguous")
		}
		v := matches[0]
		if !ownReceipt && product.UUID != p.Target.UUID && v.UUID != p.Target.UUID || product.UUID != "" && product.UUID != p.Target.UUID || v.UUID != "" && v.UUID != p.Target.UUID {
			return errors.New("existing draft UUID does not match package")
		}
		p.Target.VariantID = v.ID
		p.Details.Target = p.Target
		if e := m.guardVersions(ctx, *p); e != nil {
			return e
		}
	}
	return nil
}
func (m *Manager) guardVersions(ctx context.Context, p Plan) error {
	versions, err := m.api.Versions(ctx, p.Target)
	if err != nil {
		return err
	}
	return model.CheckVersionMutation(versions, p.Target.VariantID, p.Prepared.Package.Manifest.Version)
}
func (m *Manager) Upload(ctx context.Context, id string) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, e := m.Load(id)
		if e != nil {
			return p, e
		}
		return m.upload(ctx, p)
	})
}
func (m *Manager) upload(ctx context.Context, p Plan) (Plan, error) {
	if p.State == Discarded || p.State == Retired {
		return p, fmt.Errorf("%s listing plans cannot be uploaded or submitted; prepare a new plan", p.State)
	}
	if p.State == Submitted || p.State == Uploaded {
		return p, nil
	}
	if p.EarlyDetailsState == Inflight || p.EarlyDetailsState == Unknown {
		return p, errors.New("early listing update outcome unknown; run listing status; do not retry")
	}
	if p.DetailsState == Inflight || p.DetailsState == Unknown {
		return p, errors.New("listing update outcome unknown; run listing status; do not retry")
	}
	for _, item := range p.Media {
		if item.State == Inflight || item.State == Unknown {
			return p, errors.New("media outcome unknown; run listing status; do not retry")
		}
	}
	if p.PackageState == Inflight || p.PackageState == Unknown {
		return p, errors.New("package upload outcome unknown; run listing status")
	}
	if e := m.preflight(ctx, &p); e != nil {
		return p, e
	}
	if e := m.save(p, false); e != nil {
		return p, e
	}
	if p.Target.ProductID == "" {
		m.report("Creating or adopting SKU")
		dm := m.drafts()
		if p.DraftPlanID == "" {
			dp, e := dm.Prepare(maker.DraftInput{OrgID: p.Target.OrgID, UUID: p.Target.UUID, Name: p.Prepared.Config.Name, Description: p.Prepared.Description.Markdown, DescriptionPlainTextLength: p.Prepared.Description.PlainTextLength}, p.Prepared.Package)
			if e != nil {
				return p, e
			}
			p.DraftPlanID = dp.ID
			if e = m.save(p, false); e != nil {
				return p, e
			}
		}
		if e := m.verify(ctx, p); e != nil {
			return p, e
		}
		dp, e := dm.Create(ctx, p.DraftPlanID)
		if dp.Target.ProductID != "" || dp.Target.VariantID != "" {
			if childErr := validateChild(p, dp); childErr != nil {
				return p, childErr
			}
			p.Target = dp.Target
			p.Details.Target = p.Target
			if saveErr := m.save(p, false); saveErr != nil {
				return p, saveErr
			}
		}
		if e != nil {
			return p, e
		}
		p.Target = dp.Target
		p.Details.Target = p.Target
		if e = m.save(p, false); e != nil {
			return p, e
		}
	}
	var contextErr error
	ctx, _, contextErr = m.creationContext(ctx, p)
	if contextErr != nil {
		return p, contextErr
	}
	// A known target can have locked pricing or gallery orders outside this
	// plan. Detect those before staging another package or editing any media.
	if e := m.api.ValidateExistingListingPlan(ctx, p.Target, p.Details.IsFree, len(p.Prepared.Gallery)); e != nil {
		return p, e
	}
	gallery := make([]maker.MediaInput, 0, len(p.Prepared.Gallery))
	for _, asset := range p.Prepared.Gallery {
		gallery = append(gallery, mediaInput(p.Target, asset))
	}
	if e := m.api.ValidateGallery(ctx, p.Target, gallery); e != nil {
		return p, e
	}
	// Resolve monetization and requested taxonomy before any package write.
	// Preserve legacy plans that already uploaded their package; their final
	// details gate verifies pricing without replacing analyzed compatibility tags.
	if p.EarlyDetails != nil || p.PackageState != Done {
		if p.EarlyDetails == nil {
			early := p.Details
			early.ExtraTagIDs = nil
			p.EarlyDetails = &early
			p.EarlyDetailsState = Ready
		}
		if p.EarlyDetailsState != Done {
			m.report("Applying listing pricing and requested details before package upload")
			if err := m.guardVersions(ctx, p); err != nil {
				return p, err
			}
			p.EarlyDetailsState = Inflight
			if err := m.save(p, false); err != nil {
				return p, err
			}
			err := m.api.UpdateListing(ctx, *p.EarlyDetails)
			p.EarlyDetailsState = Done
			if err != nil {
				p.EarlyDetailsState = Unknown
				var safe interface{ NotUpdated() bool }
				if errors.As(err, &safe) && safe.NotUpdated() {
					p.EarlyDetailsState = Ready
				}
			}
			if se := m.save(p, false); se != nil {
				return p, se
			}
			if err != nil {
				return p, err
			}
		}
		m.report("Verifying listing pricing before package upload")
		if err := m.api.VerifyListing(ctx, *p.EarlyDetails); err != nil {
			return p, err
		}
	}
	if p.PackageState != Done {
		rm := m.releases()
		if p.ReleasePlanID == "" {
			rp, e := rm.Prepare(ctx, p.Target, p.Prepared.Config.PackagePath, p.Prepared.Config.ReleaseNotesFile, *p.Prepared.Config.AutoPublish)
			if e != nil {
				return p, e
			}
			p.ReleasePlanID = rp.ID
			if e = m.save(p, false); e != nil {
				return p, e
			}
		}
		m.report("Uploading plugin package")
		if e := m.verify(ctx, p); e != nil {
			return p, e
		}
		p.PackageState = Inflight
		if e := m.save(p, false); e != nil {
			return p, e
		}
		rp, e := rm.Upload(ctx, p.ReleasePlanID)
		if e != nil {
			p.PackageState = Unknown
			var notUploaded interface{ NotUploaded() bool }
			if errors.As(e, &notUploaded) && notUploaded.NotUploaded() {
				p.PackageState = Ready
			}
			if se := m.save(p, false); se != nil {
				return p, se
			}
			return p, e
		}
		if rp.State != release.Uploaded {
			return p, errors.New("package upload checkpoint is not uploaded")
		}
		p.PackageState = Done

		if e := m.save(p, false); e != nil {
			return p, e
		}
	}
	rp, e := m.releases().Load(p.ReleasePlanID)
	if e != nil {
		return p, e
	}
	m.report("Reading uploaded package tags")
	uploadTags, e := m.api.ReadUploadTags(ctx, rp.Upload.FolderID)
	if e != nil {
		return p, e
	}
	m.report("Validating uploaded package tags against Marketplace metadata")
	meta, e := m.api.Metadata(ctx, p.Target.OrgID)
	if e != nil {
		return p, e
	}
	tags, e := meta.ResolveUploadTags(uploadTags)
	if e != nil {
		return p, e
	}
	if p.DetailsState != Ready {
		old, _ := json.Marshal(p.Details.ExtraTagIDs)
		fresh, _ := json.Marshal(tags)
		if string(old) != string(fresh) {
			return p, errors.New("uploaded package tags changed; refusing resume")
		}
	}
	p.Details.ExtraTagIDs = tags
	if e = maker.ValidateListingDetails(p.Details); e != nil {
		return p, e
	}
	if e = m.save(p, false); e != nil {
		return p, e
	}
	if p.DetailsState != Done {
		m.report("Updating listing details")
		if e := m.verify(ctx, p); e != nil {
			return p, e
		}
		if e := m.guardVersions(ctx, p); e != nil {
			return p, e
		}
		p.DetailsState = Inflight
		if e := m.save(p, false); e != nil {
			return p, e
		}
		e := m.api.UpdateListing(ctx, p.Details)
		p.DetailsState = Done
		if e != nil {
			p.DetailsState = Unknown
			var safe interface{ NotUpdated() bool }
			if errors.As(e, &safe) && safe.NotUpdated() {
				p.DetailsState = Ready
			}
		}
		if se := m.save(p, false); se != nil {
			return p, se
		}
		if e != nil {
			return p, e
		}
	}
	for i := range p.Media {
		item := &p.Media[i]
		if item.State == Done {
			continue
		}
		m.report(fmt.Sprintf("Uploading %s %d/%d", item.Asset.Kind, i+1, len(p.Media)))
		if e := m.verify(ctx, p); e != nil {
			return p, e
		}
		if e := m.guardVersions(ctx, p); e != nil {
			return p, e
		}
		item.State = Inflight
		if e := m.save(p, false); e != nil {
			return p, e
		}
		result, e := m.api.UploadMedia(ctx, mediaInput(p.Target, item.Asset))
		item.Result = result
		item.State = Done
		if e == nil && (!result.Acknowledged || result.MediaID == "" || result.FileID == "") {
			e = errors.New("media acknowledgment omitted identifiers")
		}
		if e != nil {
			item.State = Unknown
			var safe interface{ NotUploaded() bool }
			if result.MediaID == "" && errors.As(e, &safe) && safe.NotUploaded() {
				item.State = Ready
			}
		}
		if se := m.save(p, false); se != nil {
			return p, se
		}
		if e != nil {
			return p, e
		}
	}
	if e = m.verifyReady(ctx, p); e != nil {
		return p, e
	}
	p.State = Uploaded
	if e = m.save(p, false); e != nil {
		return p, e
	}
	return m.Load(p.ID)
}
func (m *Manager) verifyReady(ctx context.Context, p Plan) error {
	ctx = m.beginReadPhase(ctx)
	if err := m.verify(ctx, p); err != nil {
		return err
	}
	var err error
	ctx, _, err = m.creationContext(ctx, p)
	if err != nil {
		return err
	}
	if err = m.preflight(ctx, &p); err != nil {
		return err
	}
	if p.PackageState != Done || p.DetailsState != Done {
		return errors.New("listing package or details is not complete")
	}
	var inputs []maker.MediaInput
	for _, item := range p.Media {
		if item.State != Done || !item.Result.Acknowledged || item.Result.FileID == "" || item.Result.MediaID == "" {
			return errors.New("listing media acknowledgment is incomplete")
		}
		input := mediaInput(p.Target, item.Asset)
		inputs = append(inputs, input)
		if reader, ok := m.api.(MediaReconciler); ok {
			result, e := reader.ReconcileMedia(ctx, input, item.Result)
			if e != nil {
				return e
			}
			if !result.Reconciled || !result.Acknowledged || result.FileID != item.Result.FileID || result.MediaID != item.Result.MediaID {
				return errors.New("listing media readback differs from acknowledged identifiers")
			}
		} else {
			return errors.New("listing media readback is unavailable")
		}
	}
	m.report("Verifying final listing details, pricing, and gallery")
	return m.api.VerifyListingReady(ctx, p.Target, p.Details, inputs)
}
func mediaInput(t model.Target, a Asset) maker.MediaInput {
	order := a.Order
	if a.Kind == "icon" || a.Kind == "thumbnail" {
		order = 1
	}
	return maker.MediaInput{Target: t, Kind: a.Kind, Order: order, Path: a.Path, SHA256: a.SHA256, Size: a.Size}
}
func confirmListing(p Plan, o release.ConfirmOptions) error {
	if o.Yes {
		return nil
	}
	if !o.IsTerminal || o.Input == nil || o.Output == nil {
		return errors.New("listing submit requires a terminal with two confirmations or explicit --yes")
	}
	fmt.Fprintf(o.Output, "Review listing %s\nOrganization: %s\nName: %s\nUUID: %s\nVersion: %s\nPricing: %s USD %s\nAuto-publish: %t\nReviewed digest: %s\n", p.ID, p.Target.OrgID, p.Prepared.Config.Name, p.Target.UUID, p.Prepared.Package.Manifest.Version, p.Prepared.Config.Pricing.Type, p.Prepared.Config.Pricing.Amount, *p.Prepared.Config.AutoPublish, p.Prepared.Digest)
	r := bufio.NewReader(o.Input)
	for _, prompt := range []string{"Create/update this listing and submit for production review? [y/N]: ", "Confirm production submission now? [y/N]: "} {
		fmt.Fprint(o.Output, prompt)
		line, e := r.ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		if e != nil || line != "y" && line != "yes" {
			return release.ErrCancelled
		}
	}
	return nil
}
func (m *Manager) Submit(ctx context.Context, id string, o release.ConfirmOptions) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, e := m.Load(id)
		if e != nil {
			return p, e
		}
		if p.State == Discarded || p.State == Retired {
			return p, fmt.Errorf("%s listing plans cannot be submitted; prepare a new plan", p.State)
		}
		if p.State == Submitted {
			return p, nil
		}
		if e = m.verify(ctx, p); e != nil {
			return p, e
		}
		if e = confirmListing(p, o); e != nil {
			return p, e
		}
		current, e := m.Load(id)
		if e != nil {
			return p, e
		}
		if current.Checksum != p.Checksum {
			return p, errors.New("listing plan changed during confirmation")
		}
		p, e = m.upload(ctx, p)
		if e != nil {
			return p, e
		}
		ctx, _, e = m.creationContext(ctx, p)
		if e != nil {
			return p, e
		}
		if e = m.verify(ctx, p); e != nil {
			return p, e
		}
		if e = m.verifyReady(ctx, p); e != nil {
			return p, e
		}
		m.report("Submitting version for review")
		rp, e := m.releases().Submit(ctx, p.ReleasePlanID, release.ConfirmOptions{Yes: true})
		if e != nil {
			return p, e
		}
		if rp.State == release.Submitted {
			p.State = Submitted
			if e = m.save(p, false); e != nil {
				return p, e
			}
		}
		return m.Load(id)
	})
}

// Status only reads the service. Unrecognized listing/media outcomes remain
// blocked, because retrying an unacknowledged association can create duplicates.
func (m *Manager) Status(ctx context.Context, id string) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, e := m.Load(id)
		if e != nil {
			return p, e
		}
		if p.State == Discarded || p.State == Retired {
			return p, nil
		}
		if p.DraftPlanID != "" && p.Target.ProductID == "" {
			dp, e := m.drafts().Status(ctx, p.DraftPlanID)
			if e != nil {
				return p, e
			}
			if dp.State == draft.Created {
				p.Target = dp.Target
				p.Details.Target = p.Target
			}
		}
		ctx, _, e = m.creationContext(ctx, p)
		if e != nil {
			return p, e
		}
		if p.EarlyDetails != nil && (p.EarlyDetailsState == Unknown || p.EarlyDetailsState == Inflight) {
			if err := m.api.VerifyListing(ctx, *p.EarlyDetails); err != nil {
				return p, err
			}
			p.EarlyDetailsState = Done
		}
		if p.ReleasePlanID != "" {
			rp, e := m.releases().Status(ctx, p.ReleasePlanID)
			if e != nil {
				var retained *release.RetainedDraftError
				if errors.As(e, &retained) {
					// Repair older journals that treated draft readback as a
					// completed submission, while retaining every checkpoint.
					p.State = Uploaded
					if saveErr := m.save(p, false); saveErr != nil {
						return p, saveErr
					}
					loaded, loadErr := m.Load(p.ID)
					if loadErr != nil {
						return p, loadErr
					}
					p = loaded
				}
				return p, e
			}
			if rp.State == release.Uploaded || rp.State == release.Submitted {
				p.PackageState = Done
			}
			if rp.State == release.Submitted {
				p.State = Submitted
			}
		}
		if p.DetailsState == Unknown || p.DetailsState == Inflight {
			if reader, ok := m.api.(ListingReconciler); ok {
				confirmed, err := reader.ReconcileListing(ctx, p.Details)
				if err != nil {
					return p, err
				}
				if confirmed {
					p.DetailsState = Done
				}
			}
		}
		for i := range p.Media {
			item := &p.Media[i]
			if item.State != Unknown && item.State != Inflight {
				continue
			}
			if reader, ok := m.api.(MediaReconciler); ok {
				result, err := reader.ReconcileMedia(ctx, mediaInput(p.Target, item.Asset), item.Result)
				if err != nil {
					return p, err
				}
				if result.Reconciled && result.Acknowledged && result.FileID != "" && result.MediaID != "" {
					item.Result = result
					item.State = Done
				}
			}
		}
		if e = m.save(p, false); e != nil {
			return p, e
		}
		for _, item := range p.Media {
			if item.State == Unknown || item.State == Inflight {
				return p, errors.New("remote media evidence is not available; association remains unknown; do not retry")
			}
		}
		if p.DetailsState == Unknown || p.DetailsState == Inflight {
			return p, errors.New("remote listing evidence is not available; update remains unknown; do not retry")
		}
		if p.PackageState == Unknown || p.PackageState == Inflight {
			return p, errors.New("package upload remains unknown; no completed release upload checkpoint is available; do not retry")
		}
		return m.Load(id)
	})
}
