// Package deletion checkpoints destructive requests and refuses uncertain retries.
package deletion

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

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

type API interface {
	Product(context.Context, model.Target) (model.Product, error)
	// DeletionProducts must reject incomplete/unrecognized catalogs: an empty
	// result is authoritative absence evidence for this organization.
	DeletionProducts(context.Context, string) ([]model.Product, error)
	DeleteProduct(context.Context, model.Target) (maker.DeleteResult, error)
}

const (
	Prepared = "prepared"
	Deleting = "deleting"
	Unknown  = "deletion_unknown"
	Deleted  = "deleted"
)

type Plan struct {
	ID        string              `json:"id"`
	Target    model.Target        `json:"target"`
	Product   model.Product       `json:"product"`
	State     string              `json:"state"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
	Result    *maker.DeleteResult `json:"result,omitempty"`
	Checksum  string              `json:"checksum"`
}
type Manager struct {
	dir string
	api API
}

func NewManager(dir string, api API) *Manager { return &Manager{dir: dir, api: api} }

var validID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func checksum(p Plan) string {
	p.Checksum = ""
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (m *Manager) privateDir() error {
	st, err := os.Lstat(m.dir)
	if err != nil {
		return err
	}
	if err = filesec.VerifyPrivateDir(m.dir, st, true); err != nil {
		return errors.New("deletion state directory must be a private real directory (0700)")
	}
	return nil
}
func (m *Manager) locked(fn func() (Plan, error)) (Plan, error) {
	if err := filesec.CreatePrivateDir(m.dir); err != nil {
		return Plan{}, err
	}
	if err := m.privateDir(); err != nil {
		return Plan{}, err
	}
	path := filepath.Join(m.dir, "deletion.lock")
	f, err := filesec.OpenPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return Plan{}, fmt.Errorf("deletion state is locked; verify any previous process has stopped before removing %s: %w", path, err)
	}
	defer os.Remove(path)
	_, err = fmt.Fprintf(f, "pid=%d time=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	ce := f.Close()
	if err != nil {
		return Plan{}, err
	}
	if ce != nil {
		return Plan{}, ce
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
		err = os.Link(tmp, path)
	} else {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return err
	}
	return filesec.SyncDir(m.dir)
}
func (m *Manager) Load(id string) (Plan, error) {
	if err := m.privateDir(); err != nil {
		return Plan{}, err
	}
	if !validID.MatchString(id) {
		return Plan{}, errors.New("invalid deletion plan ID")
	}
	path := filepath.Join(m.dir, id+".json")
	st, err := os.Lstat(path)
	if err != nil {
		return Plan{}, err
	}
	if err = filesec.VerifyPrivateFile(path, st, true); err != nil {
		return Plan{}, errors.New("deletion plan must be a private regular file (0600)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, err
	}
	var p Plan
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(&p); err != nil {
		return p, fmt.Errorf("corrupt deletion plan: %w", err)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return p, errors.New("corrupt deletion plan: trailing data")
	}
	if p.ID != id || p.Checksum != checksum(p) || p.Target.OrgID == "" || p.Target.ProductID == "" || p.Target.Name == "" || p.Product.ID != p.Target.ProductID || p.Product.Name != p.Target.Name || p.Product.Status != "DRAFT" {
		return p, errors.New("deletion plan integrity check failed")
	}
	switch p.State {
	case Prepared, Deleting, Unknown, Deleted:
	default:
		return p, errors.New("invalid deletion state")
	}
	return p, nil
}
func (m *Manager) guard(p Plan) error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == p.ID+".json" {
			continue
		}
		other, e := m.Load(strings.TrimSuffix(entry.Name(), ".json"))
		if e != nil {
			return fmt.Errorf("cannot check existing deletion plan: %w", e)
		}
		if other.Target.OrgID == p.Target.OrgID && other.Target.ProductID == p.Target.ProductID && (other.State == Deleting || other.State == Unknown) {
			return fmt.Errorf("plan %s has an unresolved deletion for this product; run maker product delete-status %s", other.ID, other.ID)
		}
	}
	return nil
}
func validate(t model.Target, p model.Product) error {
	if t.OrgID == "" || t.ProductID == "" || p.ID != t.ProductID || p.Name == "" || p.Status != "DRAFT" {
		return errors.New("deletion requires the exact product ID and a DRAFT product")
	}
	if t.Name != "" && p.Name != t.Name {
		return errors.New("product name changed; deletion blocked")
	}
	if t.UUID != "" && p.UUID != t.UUID {
		return errors.New("product UUID changed or unavailable; deletion blocked")
	}
	return nil
}
func (m *Manager) Prepare(ctx context.Context, t model.Target) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p := Plan{Target: t}
		if err := m.guard(p); err != nil {
			return p, err
		}
		product, err := m.api.Product(ctx, t)
		if err != nil {
			return p, err
		}
		if err = validate(t, product); err != nil {
			return p, err
		}
		b := make([]byte, 16)
		if _, err = rand.Read(b); err != nil {
			return p, err
		}
		t.Name = product.Name
		t.UUID = product.UUID
		product.Raw = nil
		now := time.Now().UTC()
		p = Plan{ID: hex.EncodeToString(b), Target: t, Product: product, State: Prepared, CreatedAt: now, UpdatedAt: now}
		if err = m.save(p, true); err != nil {
			return p, err
		}
		return m.Load(p.ID)
	})
}
func (m *Manager) Delete(ctx context.Context, id string) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if p.State == Deleted {
			return p, nil
		}
		if p.State != Prepared {
			return p, fmt.Errorf("deletion outcome unresolved; do not retry; run maker product delete-status %s", id)
		}
		if err = m.guard(p); err != nil {
			return p, err
		}
		product, err := m.api.Product(ctx, p.Target)
		if err != nil {
			return p, err
		}
		if err = validate(p.Target, product); err != nil {
			return p, err
		}
		// Even an empty UUID is a snapshot: newly appearing UUID evidence needs review.
		if product.UUID != p.Product.UUID {
			return p, errors.New("product UUID changed; deletion blocked")
		}
		if err = ctx.Err(); err != nil {
			return p, err
		}
		p.State = Deleting
		p.UpdatedAt = time.Now().UTC()
		if err = m.save(p, false); err != nil {
			return p, err
		}
		result, dispatchErr := m.api.DeleteProduct(ctx, p.Target)
		p.State = Unknown
		p.Result = &result
		p.UpdatedAt = time.Now().UTC()
		if err = m.save(p, false); err != nil {
			return p, fmt.Errorf("deletion outcome unknown; result checkpoint could not be saved: %w", err)
		}
		if dispatchErr != nil {
			var notDeleted interface{ NotDeleted() bool }
			if !result.Deleted && (result.ProductID == "" || result.ProductID == p.Target.ProductID) && errors.As(dispatchErr, &notDeleted) && notDeleted.NotDeleted() {
				uncertain := p
				p.State = Prepared
				p.Result = nil
				if err = m.save(p, false); err != nil {
					return uncertain, fmt.Errorf("deletion not dispatched but prepared checkpoint could not be saved: %w", err)
				}
				p, err = m.Load(id)
				if err != nil {
					return p, err
				}
				return p, fmt.Errorf("deletion was not dispatched; plan remains prepared: %w", dispatchErr)
			}
			return p, fmt.Errorf("deletion outcome unknown; do not retry; run maker product delete-status %s: %w", id, dispatchErr)
		}
		if !result.Deleted || result.ProductID != p.Target.ProductID {
			return p, fmt.Errorf("deletion response did not confirm the exact product; outcome unknown; run maker product delete-status %s", id)
		}
		uncertain := p
		p.State = Deleted
		p.UpdatedAt = time.Now().UTC()
		if err = m.save(p, false); err != nil {
			return uncertain, err
		}
		return m.Load(id)
	})
}
func (m *Manager) Status(ctx context.Context, id string) (Plan, error) {
	return m.locked(func() (Plan, error) {
		p, err := m.Load(id)
		if err != nil {
			return p, err
		}
		if p.State == Prepared || p.State == Deleted {
			return p, nil
		}
		products, err := m.api.DeletionProducts(ctx, p.Target.OrgID)
		if err != nil {
			return p, fmt.Errorf("cannot reconcile deletion; stored state remains %s: %w", p.State, err)
		}
		seen := map[string]bool{}
		present := false
		for _, product := range products {
			if product.ID == "" || seen[product.ID] {
				return p, errors.New("incomplete or duplicate catalog identifiers; deletion remains unresolved")
			}
			seen[product.ID] = true
			if product.ID == p.Target.ProductID {
				present = true
			}
		}
		if present {
			p.State = Unknown
			p.UpdatedAt = time.Now().UTC()
			if err = m.save(p, false); err != nil {
				return p, err
			}
			return p, errors.New("product remains visible; deletion remains unresolved; do not retry")
		}
		uncertain := p
		p.State = Deleted
		p.UpdatedAt = time.Now().UTC()
		if err = m.save(p, false); err != nil {
			return uncertain, err
		}
		return m.Load(id)
	})
}
