//go:build windows

package filesec

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// grantEveryone adds an Everyone full-control ACE, simulating a broad DACL.
func grantEveryone(t *testing.T, path string) {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}
	merged, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, dacl)
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, merged, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateACLCurrentUserOnlyAccepted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private state")
	if err := SecurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if !IsPrivateDir(dir) {
		t.Fatal("current-user ACL rejected")
	}
	f, err := OpenPrivateFile(filepath.Join(dir, "cred"), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if !IsPrivateFile(f.Name()) {
		t.Fatal("private file ACL rejected")
	}
}

func TestBroadAndInheritedACLRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := SecurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	grantEveryone(t, dir)
	if IsPrivateDir(dir) {
		t.Fatal("Everyone grant accepted on directory")
	}
	path := filepath.Join(dir, "cred")
	f, err := OpenPrivateFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	grantEveryone(t, path)
	if IsPrivateFile(path) {
		t.Fatal("Everyone grant accepted on file")
	}
	// A file inheriting a broad parent ACE is refused as well.
	broad := filepath.Join(t.TempDir(), "broad")
	if err = os.Mkdir(broad, 0); err != nil {
		t.Fatal(err)
	}
	grantEveryone(t, broad)
	child := filepath.Join(broad, "child")
	if err = os.WriteFile(child, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if IsPrivateFile(child) {
		t.Fatal("inherited broad ACL accepted")
	}
}

func TestReparsePointsRefused(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := SecurePrivateDir(real); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(real, "target")
	if err := os.WriteFile(target, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err == nil {
		if IsPrivateFile(link) {
			t.Fatal("symlink accepted")
		}
	} else {
		t.Log("symlink privilege unavailable; symlink check skipped")
	}
	dirLink := filepath.Join(base, "dirlink")
	if err := os.Symlink(real, dirLink); err == nil {
		if IsPrivateDir(dirLink) {
			t.Fatal("directory symlink accepted")
		}
	}
	junction := filepath.Join(base, "junction")
	if err := exec.Command("cmd", "/c", "mklink", "/J", junction, real).Run(); err == nil {
		if IsPrivateDir(junction) {
			t.Fatal("junction accepted as private directory")
		}
	} else {
		t.Log("mklink /J unavailable; junction check skipped")
	}
}

func TestAtomicReplaceAndInterruptedWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := SecurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "plan.json")
	f, err := CreateTemp(dir, ".tmp-")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("first")
	f.Close()
	if err = os.Rename(f.Name(), dest); err != nil {
		t.Fatal(err)
	}
	// Windows requires MOVEFILE_REPLACE_EXISTING to swap over an existing
	// name; os.Rename must succeed and keep private access.
	f, err = CreateTemp(dir, ".tmp-")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("second")
	f.Close()
	if err = os.Rename(f.Name(), dest); err != nil {
		t.Fatal("atomic replacement over existing file failed", err)
	}
	b, err := os.ReadFile(dest)
	if err != nil || string(b) != "second" || !IsPrivateFile(dest) {
		t.Fatal("replacement lost content or privacy")
	}
	// An interrupted write leaves only the temporary file; the destination
	// is never partially replaced.
	f, err = CreateTemp(dir, ".tmp-")
	if err != nil {
		t.Fatal(err)
	}
	tmp := f.Name()
	f.WriteString("interrupted")
	f.Close()
	os.Remove(tmp)
	b, err = os.ReadFile(dest)
	if err != nil || string(b) != "second" {
		t.Fatal("interrupted write corrupted destination")
	}
}

func TestContentionLockIsExclusiveAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := SecurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "session.lock")
	first, err := OpenPrivateFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenPrivateFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL); err == nil {
		t.Fatal("concurrent lock acquisition allowed")
	}
	if !IsPrivateFile(lock) {
		t.Fatal("lock not private")
	}
	// Closing the handle alone must not release the lock: durable locks are
	// released only by removing the lock file.
	first.Close()
	if _, err = OpenPrivateFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL); err == nil {
		t.Fatal("closing the handle released the lock")
	}
	if err = os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	second, err := OpenPrivateFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		t.Fatal("lock not released by removal")
	}
	second.Close()
}

func TestUnicodeAndSpacePathsOnWindows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "datos privados", "état")
	if err := SecurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	f, err := CreateTemp(dir, ".clave-")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if !IsPrivateFile(f.Name()) {
		t.Fatal("unicode path not private")
	}
}
