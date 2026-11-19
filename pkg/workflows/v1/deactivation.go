package v1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/SENTINELITE/elgato-makers-cli/internal/discounts"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"os"
	"path/filepath"
)

const Deactivation Kind = "discount_deactivation"

type deactivationPlan struct {
	Org     string                 `json:"organization_id"`
	ID      string                 `json:"discount_id"`
	Code    string                 `json:"maker_code"`
	Record  json.RawMessage        `json:"record"`
	State   string                 `json:"state"`
	Receipt *maker.DiscountReceipt `json:"receipt,omitempty"`
}

func (s *Service) deactivationPath(id string) (string, error) {
	if len(id) != 64 {
		return "", errors.New("invalid deactivation ID")
	}
	if _, e := hex.DecodeString(id); e != nil {
		return "", errors.New("invalid deactivation ID")
	}
	return filepath.Join(s.discounts.Dir, "deactivate-"+id+".json"), nil
}
func (s *Service) showDeactivation(id string) (Review, error) {
	path, e := s.deactivationPath(id)
	if e != nil {
		return Review{}, e
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return Review{}, e
	}
	var p deactivationPlan
	if json.Unmarshal(b, &p) != nil || p.Org != s.org {
		return Review{}, errors.New("invalid deactivation checkpoint")
	}
	return review(Deactivation, id, p, nil)
}
func (s *Service) PrepareDeactivation(ctx context.Context, id, expectedCode string) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !identifier.MatchString(id) || expectedCode == "" {
		return Review{}, errors.New("invalid discount target")
	}
	d, e := s.api.Discount(ctx, s.org, id)
	if e != nil {
		return Review{}, e
	}
	code, _ := d["code"].(map[string]any)
	if code["maker_code"] != expectedCode {
		return Review{}, errors.New("discount code mismatch")
	}
	b, e := json.Marshal(d)
	if e != nil {
		return Review{}, e
	}
	sum := sha256.Sum256([]byte(s.org + "/" + id))
	key := hex.EncodeToString(sum[:])
	path, _ := s.deactivationPath(key)
	if e = discounts.EnsurePrivateDir(s.discounts.Dir); e != nil {
		return Review{}, e
	}
	p := deactivationPlan{Org: s.org, ID: id, Code: expectedCode, Record: b, State: "prepared"}
	b, _ = json.Marshal(p)
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return Review{}, errors.New("deactivation already prepared or attempted; reconcile exact discount")
	}
	defer f.Close()
	if _, e = f.Write(b); e != nil {
		return Review{}, e
	}
	if e = f.Sync(); e != nil {
		return Review{}, e
	}
	return review(Deactivation, key, p, nil)
}

// Deactivate requires host approval of the exact reviewed discount record.
// The exclusive attempt guard persists even if checkpointing or dispatch fails.
func (s *Service) Deactivate(ctx context.Context, r Ref) (Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, e := s.showDeactivation(r.ID)
	if e != nil {
		return Review{}, e
	}
	if v.Ref != r {
		return Review{}, errors.New("reviewed plan changed")
	}
	var p deactivationPlan
	json.Unmarshal(v.Plan, &p)
	if p.State != "prepared" {
		return Review{}, errors.New("deactivation already attempted; reconcile exact discount")
	}
	fresh, e := s.api.Discount(ctx, s.org, p.ID)
	if e != nil {
		return Review{}, e
	}
	b, e := json.Marshal(fresh)
	if e != nil {
		return Review{}, e
	}
	if !bytes.Equal(b, p.Record) {
		return Review{}, errors.New("reviewed discount changed; review again")
	}
	path, _ := s.deactivationPath(r.ID)
	guard, e := os.OpenFile(path+".attempt", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return Review{}, errors.New("deactivation attempt already recorded")
	}
	defer guard.Close()
	if e = guard.Sync(); e != nil {
		return Review{}, e
	}
	checkpoint := func(state string, receipt *maker.DiscountReceipt) error {
		p.State = state
		p.Receipt = receipt
		return discounts.WriteCheckpoint(path, p)
	}
	if e = checkpoint("uncertain", nil); e != nil {
		return review(Deactivation, r.ID, p, e)
	}
	receipt, e := s.api.DeactivateDiscount(ctx, s.org, p.ID, p.Code, func(r maker.DiscountReceipt) error { return checkpoint("acknowledged", &r) })
	if e == nil {
		e = checkpoint("verified", &receipt)
	}
	return review(Deactivation, r.ID, p, e)
}

// DeactivationStatus reads the exact discount and preserves local attempt evidence.
func (s *Service) DeactivationStatus(ctx context.Context, id string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, e := s.showDeactivation(id)
	if e != nil {
		return nil, e
	}
	var p deactivationPlan
	json.Unmarshal(v.Plan, &p)
	remote, e := s.api.Discount(ctx, s.org, p.ID)
	if e != nil {
		return nil, e
	}
	// Reconcile only a recorded attempt. Never infer that an unattempted plan
	// performed a deactivation, and never fabricate a dispatch acknowledgement.
	if p.State != "prepared" && remote["enabled"] == false && remote["state"] == "disabled" {
		path, _ := s.deactivationPath(id)
		guard, err := os.Lstat(path + ".attempt")
		if err != nil || !guard.Mode().IsRegular() {
			return nil, errors.New("deactivation attempt evidence missing")
		}
		var original map[string]any
		if json.Unmarshal(p.Record, &original) != nil {
			return nil, errors.New("invalid reviewed discount record")
		}
		// enabled/state are the only intended differences. All other reviewed fields
		// must still match, including exact organization, ID, code, limits and terms.
		comparison := make(map[string]any, len(remote))
		for key, value := range remote {
			comparison[key] = value
		}
		comparison["enabled"], comparison["state"] = original["enabled"], original["state"]
		normalized, err := json.Marshal(comparison)
		if err != nil || !bytes.Equal(normalized, p.Record) {
			return nil, errors.New("disabled discount differs from reviewed record")
		}
		p.State = "verified"
		if err = discounts.WriteCheckpoint(path, p); err != nil {
			return nil, err
		}
		v, err = review(Deactivation, id, p, nil)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(struct {
		Local  Review         `json:"local"`
		Remote map[string]any `json:"remote"`
	}{v, remote})
}
