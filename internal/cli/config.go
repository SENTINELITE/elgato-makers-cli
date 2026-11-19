package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

type configuration struct {
	OrgID    string                  `json:"org_id"`
	Products map[string]model.Target `json:"products"`
}

var resourceID = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var aliasID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func loadConfig(path string) (configuration, error) {
	c := configuration{Products: map[string]model.Target{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if len(b) > 1024*1024 {
		return c, errors.New("configuration exceeds 1 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid configuration: %w", err)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("configuration contains trailing data")
	}
	if c.Products == nil {
		c.Products = map[string]model.Target{}
	}
	return c, nil
}

func saveConfig(path string, c configuration) error {
	if err := filesec.CreatePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := filesec.CreateTemp(filepath.Dir(path), ".config-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
