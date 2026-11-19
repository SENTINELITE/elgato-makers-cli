package draft

import (
	"context"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

type descriptionReadbackAPI struct {
	*fakeAPI
	versionReads int
}

func (f *descriptionReadbackAPI) Versions(context.Context, model.Target) ([]model.Version, error) {
	f.versionReads++
	return []model.Version{}, nil
}

func TestAcknowledgedDescriptionReadbackPreservesLocalDescription(t *testing.T) {
	for _, tc := range []struct {
		name, remote string
		allowed      bool
	}{
		{"trailing LF removed", "Description", true},
		{"trailing CRLF normalized", "Description\r\n", true},
		{"changed text", "Changed", false},
		{"leading whitespace", " Description", false},
		{"trailing space", "Description ", false},
		{"internal line break", "Descrip\ntion", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &fakeAPI{}
			m, p := fixture(t, base)
			p.Input.Description = "Description\n"
			p.Digest = digest(p)
			p.State = Unknown
			p.CreationAcknowledged = true
			p.Target.ProductID = "product"
			p.Target.VariantID = "variant"
			if err := m.save(p, false); err != nil {
				t.Fatal(err)
			}
			product := pendingUUID()
			product.Description = tc.remote
			base.details = map[string]model.Product{"product": product}
			f := &descriptionReadbackAPI{fakeAPI: base}
			m.api = f
			got, err := m.Status(context.Background(), p.ID)
			if tc.allowed {
				if err != nil || got.State != Created || f.versionReads != 1 {
					t.Fatalf("readback: %+v %v, versions=%d", got, err, f.versionReads)
				}
				if _, err := m.Create(context.Background(), p.ID); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || got.State != Unknown {
				t.Fatalf("changed description accepted: %+v %v", got, err)
			}
			saved, loadErr := m.Load(p.ID)
			if loadErr != nil || saved.Input.Description != p.Input.Description || saved.Digest != p.Digest || saved.Target != p.Target {
				t.Fatalf("local evidence changed: %+v %v", saved, loadErr)
			}
			if f.calls != 0 {
				t.Fatal("readback dispatched create")
			}
		})
	}
}
