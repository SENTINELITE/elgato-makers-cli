package discounts

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/pkg/discountplan"
)

func TestEnsurePrivateDirCreatesNestedDirectoriesAndPreservesExisting(t *testing.T) {
	base := t.TempDir()
	existing := filepath.Join(base, "existing")
	if err := os.Mkdir(existing, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0750); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(existing, "releases", "discounts")
	for range 2 {
		if err := EnsurePrivateDir(nested); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(existing, "releases"), nested} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || !filesec.IsPrivateDir(path) {
			t.Fatal(path, info, err)
		}
	}
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	// Existing directory access is preserved: 0750 on POSIX, the inherited
	// (already private) ACL on Windows.
	if runtime.GOOS == "windows" {
		if !filesec.IsPrivateDir(existing) {
			t.Fatal("existing permissions changed", info)
		}
	} else if info.Mode().Perm() != 0750 {
		t.Fatal("existing permissions changed", info, err)
	}
	file := filepath.Join(base, "file")
	if err = os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if EnsurePrivateDir(filepath.Join(file, "child")) == nil {
		t.Fatal("file accepted as directory")
	}
}
func TestPlanAndCheckpointCreateDurablePrivateDirectories(t *testing.T) {
	base := t.TempDir()
	store := Store{Dir: filepath.Join(base, "plans", "releases", "discounts")}
	p, err := discountplan.Prepare(discountplan.Input{OrganizationID: "synthetic-org", Name: "Synthetic", MakerCode: "SAVE10", Mode: "percentage", Scope: "all_products", PercentageOff: 10, StartAt: "2030-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SavePlan(p); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(base, "plans"), filepath.Join(base, "plans", "releases"), store.Dir} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || !filesec.IsPrivateDir(path) {
			t.Fatal(path, info, err)
		}
	}
	if err = store.SavePlan(p); err == nil {
		t.Fatal("existing immutable plan overwritten")
	}
	if _, err = store.Load(p.Digest); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(base, "other", "nested", "receipt.json")
	if err = WriteCheckpoint(checkpoint, map[string]string{"state": "uncertain"}); err != nil {
		t.Fatal(err)
	}
	if err = WriteCheckpoint(checkpoint, map[string]string{"state": "verified"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(base, "other"), filepath.Dir(checkpoint)} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || !filesec.IsPrivateDir(path) {
			t.Fatal(path, info, err)
		}
	}
	if !filesec.IsPrivateFile(checkpoint) {
		t.Fatal("checkpoint not private")
	}
}
