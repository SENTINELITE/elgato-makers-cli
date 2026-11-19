package listing

import (
	"context"
	"errors"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
)

type testPhaseKey struct{}
type phaseWorkflowAPI struct {
	*workflowAPI
	t               *testing.T
	next            int
	readyPhases     []int
	reconcilePhases []int
}

func (f *phaseWorkflowAPI) BeginReadPhase(ctx context.Context) context.Context {
	if ctx.Value(testPhaseKey{}) != nil {
		return ctx
	}
	f.next++
	return context.WithValue(ctx, testPhaseKey{}, f.next)
}
func (f *phaseWorkflowAPI) ReconcileMedia(ctx context.Context, in maker.MediaInput, result maker.MediaResult) (maker.MediaResult, error) {
	phase, _ := ctx.Value(testPhaseKey{}).(int)
	f.reconcilePhases = append(f.reconcilePhases, phase)
	return f.workflowAPI.ReconcileMedia(ctx, in, result)
}
func (f *phaseWorkflowAPI) VerifyListingReady(ctx context.Context, target model.Target, details maker.ListingDetailsInput, media []maker.MediaInput) error {
	phase, _ := ctx.Value(testPhaseKey{}).(int)
	f.readyPhases = append(f.readyPhases, phase)
	return f.workflowAPI.VerifyListingReady(ctx, target, details, media)
}
func (f *phaseWorkflowAPI) UploadMedia(ctx context.Context, in maker.MediaInput) (maker.MediaResult, error) {
	if ctx.Value(testPhaseKey{}) != nil {
		f.t.Error("read phase escaped into media write")
	}
	return f.workflowAPI.UploadMedia(ctx, in)
}
func (f *phaseWorkflowAPI) Submit(ctx context.Context, target model.Target, input model.Submission) (model.SubmitResult, error) {
	if ctx.Value(testPhaseKey{}) != nil {
		f.t.Error("read phase escaped into submission")
	}
	return f.workflowAPI.Submit(ctx, target, input)
}
func TestReadPhaseEndsBeforeWritesAndSubmitRechecksFresh(t *testing.T) {
	m, p, base := workflowFixture(t)
	wrapped := &phaseWorkflowAPI{workflowAPI: base, t: t}
	m.api = wrapped
	uploaded, err := m.Upload(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.State != Uploaded || len(wrapped.readyPhases) != 1 || wrapped.readyPhases[0] == 0 {
		t.Fatalf("upload phase: %+v", wrapped.readyPhases)
	}
	for _, phase := range wrapped.reconcilePhases {
		if phase != wrapped.readyPhases[0] {
			t.Fatalf("media phase %d differs from ready phase %d", phase, wrapped.readyPhases[0])
		}
	}
	base.readyErr = errors.New("synthetic changed price or gallery")
	if _, err = m.Submit(context.Background(), p.ID, release.ConfirmOptions{Yes: true}); err == nil {
		t.Fatal("changed listing was submitted")
	}
	if len(wrapped.readyPhases) != 2 || wrapped.readyPhases[0] == wrapped.readyPhases[1] {
		t.Fatalf("submit reused upload phase: %+v", wrapped.readyPhases)
	}
	for _, event := range base.events {
		if event == "submit" {
			t.Fatal("submission dispatched after changed readback")
		}
	}
}
