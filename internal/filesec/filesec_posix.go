//go:build !windows

package filesec

import (
	"errors"
	"os"
)

func verifyPrivate(path string, info os.FileInfo, dir, exact bool) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private object must not be a symbolic link")
	}
	perm := info.Mode().Perm()
	if exact {
		if dir && perm != 0700 || !dir && perm != 0600 {
			return errors.New("private object must have exact owner-only permissions")
		}
		return nil
	}
	if perm&0077 != 0 {
		return errors.New("private object must deny group and other access")
	}
	return nil
}

func createPrivateDir(path string) error {
	return os.MkdirAll(path, 0700)
}

func mkdirPrivate(path string) error {
	return os.Mkdir(path, 0700)
}

func protectPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.Chmod(path, 0700)
	}
	return os.Chmod(path, 0600)
}

func createTempPrivate(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

func openPrivateFile(path string, flag int) (*os.File, error) {
	return os.OpenFile(path, flag, 0600)
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	err = d.Sync()
	ce := d.Close()
	if err != nil {
		return err
	}
	return ce
}
