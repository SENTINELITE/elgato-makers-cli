package filesec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateDirLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	if err := SecurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if !IsPrivateDir(dir) {
		t.Fatal("created directory is not private")
	}
	if err := SecurePrivateDir(dir); err != nil {
		t.Fatal("idempotent secure failed")
	}
	if err := SyncDir(dir); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateFileLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := CreatePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	f, err := CreateTemp(dir, ".tmp-")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	if _, err = f.WriteString("secret"); err != nil {
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if !IsPrivateFile(name) {
		t.Fatal("temporary file is not private")
	}
	dest := filepath.Join(dir, "final")
	if err = os.Rename(name, dest); err != nil {
		t.Fatal(err)
	}
	if !IsPrivateFile(dest) {
		t.Fatal("renamed file lost privacy")
	}
}

func TestExclusivePrivateCreationAndReparseRefusal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := SecurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "guard")
	f, err := OpenPrivateFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenPrivateFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL); err == nil {
		t.Fatal("second exclusive create succeeded")
	}
	target := filepath.Join(dir, "target")
	if err = os.WriteFile(target, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err = os.Symlink(target, link); err == nil {
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if VerifyPrivateFile(link, info, false) == nil {
			t.Fatal("symlink accepted as private file")
		}
	}
}

func TestPrivatePathsWithSpacesAndUnicode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "space dir", "données")
	if err := SecurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	f, err := CreateTemp(dir, ".café-")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if !IsPrivateFile(f.Name()) {
		t.Fatal("unicode path file is not private")
	}
}
