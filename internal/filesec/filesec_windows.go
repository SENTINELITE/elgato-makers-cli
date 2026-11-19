//go:build windows

package filesec

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows grants "private" through a DACL, not mode bits. A private object's
// effective DACL may contain access-allowed ACEs only for the current user,
// LocalSystem and Administrators; deny ACEs are restrictions and accepted.
// Objects this package creates use a DACL protected from parent inheritance
// (SE_DACL_PROTECTED) so a permissive ancestor can never widen them.

var currentUserSID = sync.OnceValues(func() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
})

func allowedSID(sid *windows.SID) bool {
	user, err := currentUserSID()
	if err != nil {
		return false
	}
	return sid.Equals(user) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid)
}

// privateSDDL describes the exact descriptor applied to created objects: an
// owner, and a protected DACL granting full access to the current user plus
// the justified system principals LocalSystem (SY) and Administrators (BA).
func privateSDDL(user *windows.SID, dir bool) string {
	s := user.String()
	flags := ""
	if dir {
		// Children inherit these ACEs; the DACL itself stays protected, so
		// only our restricted set propagates. OI and CI combine in the single
		// ace_flags field, keeping each ACE the required six fields.
		flags = "OICI"
	}
	return "O:" + s + "D:P(A;" + flags + ";FA;;;" + s + ")(A;" + flags + ";FA;;;SY)(A;" + flags + ";FA;;;BA)"
}

func privateSecurityAttributes(dir bool) (*windows.SecurityAttributes, error) {
	user, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(privateSDDL(user, dir))
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

// verifyPrivate rejects reparse points (os.Lstat only marks true symlinks;
// junctions and mount points still carry FILE_ATTRIBUTE_REPARSE_POINT) and
// then verifies the effective DACL grants only allowed principals.
func verifyPrivate(path string, info os.FileInfo, dir, exact bool) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private object must not be a symbolic link")
	}
	if attrs, ok := info.Sys().(*syscall.Win32FileAttributeData); ok && attrs.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("private object must not be a reparse point")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || !allowedSID(owner) {
		return errors.New("private object has a foreign owner")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		// A missing or NULL DACL grants everyone full access.
		return errors.New("private object has no restricted DACL")
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if !allowedSID((*windows.SID)(unsafe.Pointer(&ace.SidStart))) {
				return errors.New("private object grants access to another principal")
			}
		case windows.ACCESS_DENIED_ACE_TYPE:
			// Deny ACEs only restrict access.
		default:
			return errors.New("private object has an unrecognized access entry")
		}
	}
	return nil
}

// createPrivateDir mirrors os.MkdirAll(0700): each missing level is created
// with a restrictive ACL at creation; an existing final directory is left as
// it is. Files created below it still receive their own private descriptor.
func createPrivateDir(path string) error {
	path = filepath.Clean(path)
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() {
			return errors.New("private directory path is not a directory")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(path)
	if parent != path {
		if err := createPrivateDir(parent); err != nil {
			return err
		}
	}
	err := mkdirPrivate(path)
	if err == nil {
		return nil
	}
	// A concurrent creator may have finished between our checks.
	if info, statErr := os.Lstat(path); statErr == nil && info.IsDir() {
		return nil
	}
	return err
}

// mkdirPrivate creates one directory with a restrictive ACL at creation.
func mkdirPrivate(path string) error {
	sa, err := privateSecurityAttributes(true)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err = windows.CreateDirectory(name, sa); err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}

// protectPath replaces an existing object's DACL with the private, protected
// descriptor. Used where POSIX code chmods a file 0600 or directory 0700.
func protectPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(privateSDDL(user, info.IsDir()))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// createPrivate makes a new empty file that is private from its first byte.
func createPrivate(path string) (*os.File, error) {
	sa, err := privateSecurityAttributes(false)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func createTempPrivate(dir, pattern string) (*os.File, error) {
	// Mirror os.CreateTemp: the last '*' is replaced by the random suffix;
	// without one the suffix is appended.
	prefix, suffix := pattern, ""
	if i := strings.LastIndex(pattern, "*"); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	for i := 0; i < 10000; i++ {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(dir, prefix+strconv.Itoa(int(binary.LittleEndian.Uint32(b[:])))+suffix)
		f, err := createPrivate(name)
		if err == nil {
			return f, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
	}
	return nil, errors.New("cannot create a unique private temporary file")
}

// openPrivateFile supports the write/create flag combinations used for
// credential locks, refresh guards and attempt records. Every creation uses
// the private security descriptor; no caller may request read-only files.
func openPrivateFile(path string, flag int) (*os.File, error) {
	var access uint32
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	default:
		access = windows.GENERIC_WRITE
	}
	var createmode uint32
	switch {
	case flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0:
		createmode = windows.CREATE_NEW
	case flag&os.O_CREATE != 0 && flag&os.O_TRUNC != 0:
		createmode = windows.CREATE_ALWAYS
	case flag&os.O_CREATE != 0:
		createmode = windows.OPEN_ALWAYS
	case flag&os.O_TRUNC != 0:
		createmode = windows.TRUNCATE_EXISTING
	default:
		createmode = windows.OPEN_EXISTING
	}
	if createmode == windows.OPEN_EXISTING {
		f, err := os.OpenFile(path, flag, 0)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	sa, err := privateSecurityAttributes(false)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, access, 0, sa, createmode, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// syncDir flushes directory metadata. FlushFileBuffers on a directory handle
// requires GENERIC_WRITE and FILE_FLAG_BACKUP_SEMANTICS; a plain os.Open
// read handle fails with access denied.
func syncDir(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	return windows.FlushFileBuffers(h)
}
