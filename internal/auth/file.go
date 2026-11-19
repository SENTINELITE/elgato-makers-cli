package auth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
)

type fileStore struct{ path string }

// NewFileStore is an explicit fallback. Its dedicated parent directory must be 0700.
func NewFileStore(path string) Store { return &fileStore{path: path} }
func (s *fileStore) checkDir(create bool) error {
	dir := filepath.Dir(s.path)
	if create {
		if err := filesec.CreatePrivateDir(dir); err != nil {
			return errors.New("cannot create private credential directory")
		}
	}
	v, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err = filesec.VerifyPrivateDir(dir, v, true); err != nil {
		return errors.New("credential directory must be a real private directory (0700)")
	}
	return nil
}
func (s *fileStore) Load() (Credentials, error) {
	if err := s.checkDir(false); err != nil {
		if os.IsNotExist(err) {
			return Credentials{}, ErrNotFound
		}
		return Credentials{}, err
	}
	v, err := os.Lstat(s.path)
	if os.IsNotExist(err) {
		return Credentials{}, ErrNotFound
	}
	if err != nil {
		return Credentials{}, errors.New("cannot inspect credentials")
	}
	if err = filesec.VerifyPrivateFile(s.path, v, true); err != nil {
		return Credentials{}, errors.New("credential file must be a private regular file (0600)")
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return Credentials{}, errors.New("cannot read credentials")
	}
	return Parse(bytes.NewReader(b))
}
func (s *fileStore) Save(c Credentials) error {
	b, err := encode(c)
	if err != nil {
		return err
	}
	if err = s.checkDir(true); err != nil {
		return err
	}
	if v, e := os.Lstat(s.path); e == nil && filesec.VerifyPrivateFile(s.path, v, true) != nil {
		return errors.New("existing credential file must be a private regular file (0600)")
	}
	f, err := filesec.CreateTemp(filepath.Dir(s.path), ".maker-credentials-*")
	if err != nil {
		return errors.New("cannot create credential file")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return errors.New("cannot write credentials")
	}
	if err = f.Close(); err != nil {
		return errors.New("cannot close credentials")
	}
	if err = os.Rename(name, s.path); err != nil {
		return errors.New("cannot replace credentials")
	}
	return nil
}
func (s *fileStore) Delete() error {
	if err := s.checkDir(false); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	err := os.Remove(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
