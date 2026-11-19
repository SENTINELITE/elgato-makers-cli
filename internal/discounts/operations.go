// Package discounts stores immutable plans and mutation uncertainty checkpoints.
package discounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/pkg/discountplan"
)

type Operation struct {
	Plan        discountplan.Plan      `json:"plan"`
	State       string                 `json:"state"`
	Receipt     *maker.DiscountReceipt `json:"receipt,omitempty"`
	RemoteState string                 `json:"remote_state,omitempty"`
	Enabled     *bool                  `json:"enabled,omitempty"`
}
type Store struct{ Dir string }

func (s Store) path(digest string) (string, error) {
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", errors.New("invalid discount plan digest")
	}
	return filepath.Join(s.Dir, digest+".json"), nil
}
func (s Store) SavePlan(p discountplan.Plan) error {
	if e := discountplan.Validate(p); e != nil {
		return e
	}
	if e := EnsurePrivateDir(s.Dir); e != nil {
		return e
	}
	path, e := s.path(p.Digest)
	if e != nil {
		return e
	}
	b, _ := json.MarshalIndent(Operation{Plan: p, State: "prepared"}, "", "  ")
	f, e := filesec.OpenPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if e != nil {
		return errors.New("cannot create unique immutable discount plan")
	}
	defer f.Close()
	if _, e = f.Write(b); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	return syncDirectory(s.Dir)
}
func (s Store) Load(digest string) (Operation, error) {
	var op Operation
	path, e := s.path(digest)
	if e != nil {
		return op, e
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return op, e
	}
	if json.Unmarshal(b, &op) != nil || op.Plan.Digest != digest {
		return op, errors.New("invalid discount operation")
	}
	return op, nil
}
func (s Store) save(op Operation) error {
	path, e := s.path(op.Plan.Digest)
	if e != nil {
		return e
	}
	return WriteCheckpoint(path, op)
}

func (s Store) Create(ctx context.Context, digest string, api *maker.Client) (Operation, error) {
	op, e := s.Load(digest)
	if e != nil {
		return op, e
	}
	if e = discountplan.Validate(op.Plan); e != nil {
		return op, e
	}
	if op.State != "prepared" {
		return op, errors.New("discount operation already attempted; use status to reconcile, never retry blindly")
	}
	lockPath, _ := s.path(digest)
	lock, e := filesec.OpenPrivateFile(lockPath+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if e != nil {
		return op, errors.New("discount operation is locked or previously interrupted; reconcile before retrying")
	}
	lock.Close()
	defer os.Remove(lockPath + ".lock")
	// Re-read under lock; another process may have completed after our first load.
	op, e = s.Load(digest)
	if e != nil {
		return op, e
	}
	if op.State != "prepared" {
		return op, errors.New("discount operation already attempted; reconcile instead of retrying")
	}
	// Bind uncertainty across newly prepared plans for the same exact account/code.
	codeHash := sha256.Sum256([]byte(op.Plan.Input.OrganizationID + "/" + op.Plan.Input.MakerCode))
	guard, e := filesec.OpenPrivateFile(filepath.Join(s.Dir, "code-"+hex.EncodeToString(codeHash[:])+".attempt"), os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if e != nil {
		return op, errors.New("this organization/code has a previous attempt; reconcile the original plan before any new creation")
	}
	_, e = guard.WriteString(digest)
	if e == nil {
		e = guard.Sync()
	}
	closeErr := guard.Close()
	if e != nil {
		return op, e
	}
	if closeErr != nil {
		return op, closeErr
	}
	if e = syncDirectory(s.Dir); e != nil {
		return op, e
	}
	op.State = "uncertain"
	if e = s.save(op); e != nil {
		return op, e
	}
	receipt, e := api.CreateDiscount(ctx, op.Plan, func(r maker.DiscountReceipt) error { op.Receipt = &r; return s.save(op) })
	if receipt.Success && receipt.DiscountID != "" {
		op.Receipt = &receipt
	}
	if e == nil {
		op.State = "verified"
	}
	if saveErr := s.save(op); saveErr != nil {
		return op, saveErr
	}
	return op, e
}
func (s Store) Status(ctx context.Context, digest string, api *maker.Client) (Operation, error) {
	op, e := s.Load(digest)
	if e != nil {
		return op, e
	}
	if e = discountplan.ValidateAt(op.Plan, op.Plan.CreatedAt); e != nil {
		return op, e
	}
	lockPath, _ := s.path(digest)
	lock, e := filesec.OpenPrivateFile(lockPath+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if e != nil {
		return op, errors.New("discount operation is running or interrupted; inspect exact provider ID/code before repairing the local lock")
	}
	lock.Close()
	defer os.Remove(lockPath + ".lock")
	op, e = s.Load(digest)
	if e != nil {
		return op, e
	}
	list, e := api.Discounts(ctx, op.Plan.Input.OrganizationID, "")
	if e != nil {
		return op, e
	}
	var match maker.Discount
	for _, d := range list {
		c := d["code"].(map[string]any)
		if c["maker_code"] == op.Plan.Input.MakerCode {
			if match != nil {
				return op, errors.New("multiple exact code matches; manual reconciliation required")
			}
			match = d
		}
	}
	if match == nil {
		return op, errors.New("exact code absent; uncertainty retained, automatic retry prohibited")
	}
	id, _ := match["id"].(string)
	if op.Receipt != nil && op.Receipt.DiscountID != id {
		return op, errors.New("receipt identity does not match exact code")
	}
	d, e := api.Discount(ctx, op.Plan.Input.OrganizationID, id)
	if e != nil {
		return op, e
	}
	if e = maker.VerifyDiscountTerms(op.Plan, d); e != nil {
		return op, e
	}
	op.Receipt = &maker.DiscountReceipt{DiscountID: id, Success: true}
	op.RemoteState, _ = d["state"].(string)
	enabled := d["enabled"].(bool)
	op.Enabled = &enabled
	op.State = "verified"
	return op, s.save(op)
}

// WriteCheckpoint atomically stores a mutation checkpoint with private permissions.
func WriteCheckpoint(path string, value any) error {
	if e := EnsurePrivateDir(filepath.Dir(path)); e != nil {
		return e
	}
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	f, e := filesec.CreateTemp(filepath.Dir(path), ".discount-checkpoint-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	_, e = f.Write(b)
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
	if e = os.Rename(name, path); e != nil {
		return e
	}
	return syncDirectory(filepath.Dir(path))
}
