package deletion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/SENTINELITE/elgato-makers-cli/internal/testutil"
)

type fakeAPI struct {
	product      model.Product
	catalog      []model.Product
	result       maker.DeleteResult
	err          error
	calls, reads int
	beforeDelete func()
}

func (f *fakeAPI) Product(context.Context, model.Target) (model.Product, error) {
	f.reads++
	return f.product, nil
}
func (f *fakeAPI) DeletionProducts(context.Context, string) ([]model.Product, error) {
	return f.catalog, nil
}
func (f *fakeAPI) DeleteProduct(context.Context, model.Target) (maker.DeleteResult, error) {
	f.calls++
	if f.beforeDelete != nil {
		f.beforeDelete()
	}
	return f.result, f.err
}

func TestUncertaintyPersistedBeforeDispatch(t *testing.T) {
	m, f, p := setup(t)
	f.beforeDelete = func() {
		stored, err := m.Load(p.ID)
		if err != nil || stored.State != Deleting {
			t.Fatalf("dispatch without durable uncertainty: %s %v", stored.State, err)
		}
	}
	f.result = maker.DeleteResult{Deleted: true, ProductID: "p"}
	if _, err := m.Delete(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentManagerCannotDispatch(t *testing.T) {
	m, f, p := setup(t)
	if err := os.WriteFile(filepath.Join(m.dir, "deletion.lock"), []byte("busy"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(m.dir, f).Delete(context.Background(), p.ID); err == nil || f.calls != 0 {
		t.Fatal("locked manager dispatched")
	}
}
func setup(t *testing.T) (*Manager, *fakeAPI, Plan) {
	t.Helper()
	f := &fakeAPI{product: model.Product{ID: "p", Name: "Exact Name", UUID: "u", Status: "DRAFT"}}
	m := NewManager(filepath.Join(t.TempDir(), "deletions"), f)
	p, err := m.Prepare(context.Background(), model.Target{OrgID: "o", ProductID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	return m, f, p
}
func TestAmbiguousDeletionCannotRetryOrBypass(t *testing.T) {
	m, f, p := setup(t)
	f.err = errors.New("lost response")
	ctx := context.Background()
	got, err := m.Delete(ctx, p.ID)
	if err == nil || got.State != Unknown || f.calls != 1 {
		t.Fatalf("got %s %v calls %d", got.State, err, f.calls)
	}
	if _, err = m.Delete(ctx, p.ID); err == nil || f.calls != 1 {
		t.Fatal("uncertain retry dispatched")
	}
	if _, err = m.Prepare(ctx, p.Target); err == nil {
		t.Fatal("fresh plan bypassed unresolved deletion")
	}
	f.catalog = []model.Product{f.product}
	got, err = m.Status(ctx, p.ID)
	if err == nil || got.State != Unknown {
		t.Fatal("visible product reset uncertain outcome")
	}
	if _, err = m.Delete(ctx, p.ID); err == nil || f.calls != 1 {
		t.Fatal("status enabled retry")
	}
	f.catalog = nil
	got, err = m.Status(ctx, p.ID)
	if err != nil || got.State != Deleted {
		t.Fatalf("absence: %s %v", got.State, err)
	}
}
func TestOtherPreparedPlanBlockedAfterDispatch(t *testing.T) {
	m, f, p := setup(t)
	ctx := context.Background()
	other, err := m.Prepare(ctx, p.Target)
	if err != nil {
		t.Fatal(err)
	}
	f.err = errors.New("lost")
	m.Delete(ctx, p.ID)
	if _, err = m.Delete(ctx, other.ID); err == nil || f.calls != 1 {
		t.Fatal("other plan bypassed unresolved deletion")
	}
}
func TestPreflightChangesBlockDeletion(t *testing.T) {
	for _, change := range []string{"name", "published", "uuid"} {
		t.Run(change, func(t *testing.T) {
			m, f, p := setup(t)
			switch change {
			case "name":
				f.product.Name = "Renamed"
			case "published":
				f.product.Status = "PUBLISHED"
			case "uuid":
				f.product.UUID = "other"
			}
			if _, err := m.Delete(context.Background(), p.ID); err == nil || f.calls != 0 {
				t.Fatal("changed target deleted")
			}
		})
	}
}
func TestSuccessfulDeletionRecordsResult(t *testing.T) {
	m, f, p := setup(t)
	f.result = maker.DeleteResult{Deleted: true, ProductID: "p"}
	got, err := m.Delete(context.Background(), p.ID)
	if err != nil || got.State != Deleted || got.Result == nil || !got.Result.Deleted {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = m.Delete(context.Background(), p.ID); err != nil || f.calls != 1 {
		t.Fatal("confirmed deletion repeated")
	}
}
func TestMissingOrWrongConfirmationRemainsUnknown(t *testing.T) {
	for _, r := range []maker.DeleteResult{{}, {Deleted: true, ProductID: "wrong"}, {Deleted: true}} {
		m, f, p := setup(t)
		f.result = r
		got, err := m.Delete(context.Background(), p.ID)
		if err == nil || got.State != Unknown {
			t.Fatal("unconfirmed deletion accepted")
		}
	}
}

type notDeleted struct{}

func (notDeleted) Error() string    { return "preflight" }
func (notDeleted) NotDeleted() bool { return true }
func TestKnownNotDispatchedRestoresPrepared(t *testing.T) {
	for _, echoedID := range []string{"", "p"} {
		t.Run("echo_"+echoedID, func(t *testing.T) {
			m, f, p := setup(t)
			f.err = notDeleted{}
			f.result.ProductID = echoedID
			got, err := m.Delete(context.Background(), p.ID)
			if err == nil || got.State != Prepared {
				t.Fatalf("%s %v", got.State, err)
			}
			f.err = nil
			f.result = maker.DeleteResult{Deleted: true, ProductID: "p"}
			if _, err = m.Delete(context.Background(), p.ID); err != nil || f.calls != 2 {
				t.Fatal("safe retry failed")
			}
		})
	}
}
func TestNotDispatchedWithConflictingIDRemainsUnknown(t *testing.T) {
	m, f, p := setup(t)
	f.err = notDeleted{}
	f.result.ProductID = "other"
	got, err := m.Delete(context.Background(), p.ID)
	if err == nil || got.State != Unknown {
		t.Fatalf("conflicting response reset uncertainty: %s %v", got.State, err)
	}
	if _, err = m.Delete(context.Background(), p.ID); err == nil || f.calls != 1 {
		t.Fatal("conflicting response enabled retry")
	}
}
func TestCorruptAndNonprivatePlansFailClosed(t *testing.T) {
	for _, mode := range []string{"corrupt", "public", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			m, f, p := setup(t)
			path := filepath.Join(m.dir, p.ID+".json")
			switch mode {
			case "corrupt":
				if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "public":
				testutil.Widen(t, path)
			case "symlink":
				os.Rename(path, path+".backup")
				if err := os.Symlink(path+".backup", path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.Delete(context.Background(), p.ID); err == nil || f.calls != 0 {
				t.Fatal("unsafe checkpoint accepted")
			}
		})
	}
}
func TestDeletingCrashCheckpointIsUncertain(t *testing.T) {
	m, f, p := setup(t)
	p.State = Deleting
	if err := m.save(p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Delete(context.Background(), p.ID); err == nil || f.calls != 0 {
		t.Fatal("crash checkpoint retried")
	}
	f.catalog = []model.Product{f.product}
	got, err := m.Status(context.Background(), p.ID)
	if err == nil || got.State != Unknown {
		t.Fatal("crash checkpoint became retryable")
	}
}
func TestCheckpointPrivate(t *testing.T) {
	m, _, p := setup(t)
	if !filesec.IsPrivateDir(m.dir) {
		t.Fatal("state directory not private")
	}
	if !filesec.IsPrivateFile(filepath.Join(m.dir, p.ID+".json")) {
		t.Fatal("plan file not private")
	}
}
