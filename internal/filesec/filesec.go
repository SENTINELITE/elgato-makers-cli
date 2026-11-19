// Package filesec is the platform private-storage layer. POSIX keeps the
// existing exact-mode behavior (0600 files, 0700 directories). Windows has no
// Unix mode bits: privacy is established with ACLs instead. A private object
// has a DACL that grants access only to the current user plus the explicitly
// justified system principals LocalSystem and Administrators, and reparse
// points (symlinks, junctions, mount points) are refused. Objects created
// here receive a restrictive, inheritance-protected security descriptor at
// creation time; nothing is ever created world-readable and fixed later.
package filesec

import (
	"errors"
	"os"
)

// VerifyPrivateDir reports whether an already-Lstat'ed path is a real private
// directory. exact requires mode 0700 on POSIX; Windows verifies the effective
// DACL either way and ignores exact.
func VerifyPrivateDir(path string, info os.FileInfo, exact bool) error {
	if !info.IsDir() {
		return errors.New("private directory must be a real directory")
	}
	return verifyPrivate(path, info, true, exact)
}

// VerifyPrivateFile reports whether an already-Lstat'ed path is a private
// regular file. exact requires mode 0600 on POSIX.
func VerifyPrivateFile(path string, info os.FileInfo, exact bool) error {
	if !info.Mode().IsRegular() {
		return errors.New("private file must be a regular file")
	}
	return verifyPrivate(path, info, false, exact)
}

// IsPrivateDir reports whether path is a real private directory.
func IsPrivateDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && VerifyPrivateDir(path, info, false) == nil
}

// IsPrivateFile reports whether path is a private regular file.
func IsPrivateFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && VerifyPrivateFile(path, info, false) == nil
}

// MkdirPrivate creates one directory that is private from creation.
// os.IsExist applies when path already exists.
func MkdirPrivate(path string) error {
	return mkdirPrivate(path)
}

// CreatePrivateDir creates any missing directories with private access from
// the start. Pre-existing directories keep their current access, matching
// os.MkdirAll(0700) semantics on POSIX.
func CreatePrivateDir(path string) error {
	return createPrivateDir(path)
}

// SecurePrivateDir creates missing directories and then enforces and verifies
// private access on the final directory, including a pre-existing one.
func SecurePrivateDir(path string) error {
	if err := createPrivateDir(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private state directory must be a real directory")
	}
	if err = protectPath(path); err != nil {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	return VerifyPrivateDir(path, info, true)
}

// ProtectPath enforces private access on an existing file or directory:
// 0600/0700 on POSIX, a restricted inheritance-protected DACL on Windows.
func ProtectPath(path string) error {
	return protectPath(path)
}

// CreateTemp creates a uniquely named private file in dir like os.CreateTemp.
// The file is private from creation; callers still fsync, close and rename.
func CreateTemp(dir, pattern string) (*os.File, error) {
	return createTempPrivate(dir, pattern)
}

// OpenPrivateFile opens path with flag like os.OpenFile with mode 0600. Files
// created by it (O_CREATE and especially O_EXCL lock/guard files) are private
// from creation on every platform.
func OpenPrivateFile(path string, flag int) (*os.File, error) {
	return openPrivateFile(path, flag)
}

// SyncDir flushes directory metadata so creations and renames are durable.
// POSIX fsyncs an open directory handle; Windows opens the directory with
// GENERIC_WRITE and FILE_FLAG_BACKUP_SEMANTICS and calls FlushFileBuffers,
// since a read-only handle fails. Errors are never swallowed.
func SyncDir(path string) error {
	return syncDir(path)
}
