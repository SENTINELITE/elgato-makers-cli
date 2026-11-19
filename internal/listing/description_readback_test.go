package listing

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

// This fake models Maker dropping the final LF from the acknowledged draft.
// It uses only temporary fixture files and never contacts a remote service.
type descriptionReadbackAPI struct{ *workflowAPI }

func (f *descriptionReadbackAPI) CreateDraft(ctx context.Context, in maker.DraftInput) (maker.DraftResult, error) {
	result, err := f.workflowAPI.CreateDraft(ctx, in)
	if err == nil {
		f.product.Description = strings.TrimRight(f.product.Description, "\r\n")
	}
	return result, err
}
func descriptionReadbackFixture(t *testing.T) (*Manager, Plan, *descriptionReadbackAPI) {
	t.Helper()
	cfg := fixture(t)
	data, err := os.ReadFile(cfg.DescriptionFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfg.DescriptionFile, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	api := &descriptionReadbackAPI{workflowAPI: &workflowAPI{nullUUID: true}}
	manager := NewManager(t.TempDir(), api)
	plan, err := manager.Prepare(prepared, "org")
	if err != nil {
		t.Fatal(err)
	}
	return manager, plan, api
}

func TestListingAcceptsRemoteDescriptionFinalLFRemoval(t *testing.T) {
	m, p, f := descriptionReadbackFixture(t)
	if !strings.HasSuffix(p.Prepared.Description.Markdown, "\n") {
		t.Fatal("fixture lacks final LF")
	}
	f.readbackErr = errors.New("synthetic temporary readback failure after creation acknowledgment")
	if _, err := m.Upload(context.Background(), p.ID); err == nil {
		t.Fatal("expected initial readback failure")
	}
	saved, err := m.Load(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Target.ProductID == "" || saved.Target.VariantID == "" {
		t.Fatal("creation IDs were not saved")
	}
	if f.product.Description == p.Prepared.Description.Markdown {
		t.Fatal("fixture did not drop final LF")
	}
	f.readbackErr = nil
	uploaded, err := m.Upload(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.State != Uploaded {
		t.Fatal(uploaded.State)
	}
	creates := 0
	for _, event := range f.events {
		if event == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatal("description normalization duplicated creation", f.events)
	}
	if uploaded.Prepared.Description.Markdown != p.Prepared.Description.Markdown || uploaded.Prepared.Digest != p.Prepared.Digest {
		t.Fatal("remote readback normalization changed reviewed local content")
	}
}

func TestListingGenuineRemoteDescriptionChangesBlockBeforeResumeWrites(t *testing.T) {
	for _, change := range []string{"body", "leading-space", "trailing-space"} {
		t.Run(change, func(t *testing.T) {
			m, p, f := descriptionReadbackFixture(t)
			f.readbackErr = errors.New("synthetic temporary readback failure after creation acknowledgment")
			if _, err := m.Upload(context.Background(), p.ID); err == nil {
				t.Fatal("expected readback failure")
			}
			f.readbackErr = nil
			switch change {
			case "body":
				f.product.Description += " changed"
			case "leading-space":
				f.product.Description = " " + f.product.Description
			case "trailing-space":
				f.product.Description += " "
			}
			if _, err := m.Upload(context.Background(), p.ID); err == nil {
				t.Fatal("genuine description change accepted")
			}
			if !reflect.DeepEqual(f.events, []string{"create"}) {
				t.Fatal("description mismatch allowed another write", f.events)
			}
		})
	}
}

func TestListingChildDescriptionRemainsByteExact(t *testing.T) {
	m, p, _ := descriptionReadbackFixture(t)
	input := maker.DraftInput{OrgID: p.Target.OrgID, UUID: p.Target.UUID, Name: p.Prepared.Config.Name, Description: strings.TrimRight(p.Prepared.Description.Markdown, "\r\n"), DescriptionPlainTextLength: p.Prepared.Description.PlainTextLength}
	child, err := m.drafts().Prepare(input, p.Prepared.Package)
	if err != nil {
		t.Fatal(err)
	}
	if !model.SameListingDescription(input.Description, p.Prepared.Description.Markdown) {
		t.Fatal("fixture is not a remote-equivalent description")
	}
	if err = validateChild(p, child); err == nil {
		t.Fatal("immutable local child description accepted normalized bytes")
	}
}
