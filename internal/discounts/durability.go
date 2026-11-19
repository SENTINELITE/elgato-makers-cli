package discounts

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
)

// EnsurePrivateDir creates missing directories with private permissions and
// syncs each new directory and its parent before returning. Existing directory
// permissions remain unchanged.
func EnsurePrivateDir(path string) error {
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return errors.New("discount state path is not a directory")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return err
	}
	if err = EnsurePrivateDir(parent); err != nil {
		return err
	}
	if err = filesec.MkdirPrivate(path); err != nil {
		// Another process may have created the directory after our initial stat.
		if !os.IsExist(err) {
			return err
		}
		info, err = os.Stat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("discount state path is not a directory")
		}
	} else if err = filesec.ProtectPath(path); err != nil {
		return err
	}
	if err = syncDirectory(path); err != nil {
		return err
	}
	return syncDirectory(parent)
}
func syncDirectory(path string) error {
	return filesec.SyncDir(path)
}
