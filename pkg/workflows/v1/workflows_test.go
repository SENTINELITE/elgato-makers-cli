package v1

import (
	"context"
	"errors"
	"github.com/SENTINELITE/elgato-makers-cli/internal/discounts"
	"github.com/SENTINELITE/elgato-makers-cli/pkg/discountplan"
	"path/filepath"
	"testing"
)

func TestOfflineReviewLifecycle(t *testing.T) {
	s := &Service{org: "synthetic-org", discounts: discounts.Store{Dir: filepath.Join(t.TempDir(), "discounts")}}
	input := discountplan.Input{Name: "Synthetic", MakerCode: "SAVE10", Mode: "percentage", Scope: "all_products", PercentageOff: 10, StartAt: "2030-01-01T00:00:00Z"}
	p, e := s.PrepareDiscount(input)
	if e != nil {
		t.Fatal(e)
	}
	loaded, e := s.Show(Discount, p.Ref.ID)
	if e != nil || loaded.Ref != p.Ref {
		t.Fatalf("review round trip: %v", e)
	}
	changed := p.Ref
	changed.Digest = "tampered"
	if _, e = s.Apply(context.Background(), Create, changed, false); e == nil {
		t.Fatal("changed approval accepted")
	}
	if _, e = s.Apply(context.Background(), Delete, p.Ref, false); e == nil {
		t.Fatal("unsupported action accepted")
	}
	// The same root may never be used to adopt another organization's plan.
	other := &Service{org: "other-org", discounts: s.discounts}
	if _, e = other.Show(Discount, p.Ref.ID); e == nil {
		t.Fatal("cross-organization plan exposed")
	}
	input.OrganizationID = "other-org"
	if _, e = s.PrepareDiscount(input); e == nil {
		t.Fatal("cross-organization input accepted")
	}
}
func TestReturnedCheckpointSurvivesFailure(t *testing.T) {
	cause := errors.New("ambiguous dispatch")
	p, e := review(Draft, "synthetic", map[string]string{"state": "creation_unknown"}, cause)
	if !errors.Is(e, cause) || len(p.Plan) == 0 || p.Ref.Digest == "" {
		t.Fatal("lost failure checkpoint")
	}
}
func TestInvalidHostConfiguration(t *testing.T) {
	if _, e := New("relative", "synthetic-org", nil); e == nil {
		t.Fatal("invalid root accepted")
	}
}
