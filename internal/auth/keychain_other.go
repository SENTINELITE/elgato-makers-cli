//go:build !darwin || !cgo

package auth

type unavailableStore struct{}

func NewKeychainStore(service, account string) Store { return unavailableStore{} }
func (unavailableStore) Load() (Credentials, error)  { return Credentials{}, ErrUnavailable }
func (unavailableStore) Save(Credentials) error      { return ErrUnavailable }
func (unavailableStore) Delete() error               { return ErrUnavailable }
