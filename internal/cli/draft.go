package cli

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/SENTINELITE/elgato-makers-cli/internal/artifact"
	"github.com/SENTINELITE/elgato-makers-cli/internal/draft"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/spf13/cobra"
)

func (a *app) drafts(api draft.API) *draft.Manager {
	dir := a.stateDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(a.configPath), "releases")
	}
	return draft.NewManager(filepath.Join(dir, "drafts"), api)
}

// Descriptions default to the reviewed manifest, preserving the same editor
// character-count rules used by release notes.
func draftDescription(pkg model.Package, file string) (model.Notes, error) {
	if file != "" {
		return artifact.ReadNotes(file)
	}
	z, err := zip.OpenReader(pkg.Path)
	if err != nil {
		return model.Notes{}, err
	}
	defer z.Close()
	var description string
	for _, f := range z.File {
		if f.Name != pkg.Manifest.UUID+".sdPlugin/manifest.json" {
			continue
		}
		r, e := f.Open()
		if e != nil {
			return model.Notes{}, e
		}
		var m struct{ Description string }
		e = json.NewDecoder(io.LimitReader(r, 1024*1024)).Decode(&m)
		r.Close()
		if e != nil {
			return model.Notes{}, e
		}
		description = m.Description
	}
	if strings.TrimSpace(description) == "" {
		return model.Notes{}, errors.New("provide --description-file; the package has no description")
	}
	f, err := os.CreateTemp("", "maker-description-*")
	if err != nil {
		return model.Notes{}, err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(description)
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return model.Notes{}, err
	}
	return artifact.ReadNotes(f.Name())
}

func (a *app) draftCommands() *cobra.Command {
	r := &cobra.Command{Use: "draft", Short: "Prepare and create a draft product without uploading or submitting a version"}
	var file, description, extension string
	p := &cobra.Command{Use: "prepare", Short: "Review package identity and description in a local draft plan", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := a.config()
		if err != nil {
			return err
		}
		org, err := a.organization(c)
		if err != nil {
			return err
		}
		if file == "" {
			return errors.New("provide --file <plugin.streamDeckPlugin>")
		}
		pkg, err := artifact.Inspect(file)
		if err != nil {
			return err
		}
		notes, err := draftDescription(pkg, description)
		if err != nil {
			return err
		}
		if notes.PlainTextLength < 1 || notes.PlainTextLength > 4000 {
			return errors.New("description must contain 1 to 4000 editor characters")
		}
		input := maker.DraftInput{OrgID: org, Name: pkg.Manifest.Name, UUID: pkg.Manifest.UUID, Description: notes.Markdown, DescriptionPlainTextLength: notes.PlainTextLength, ExtensionID: extension}
		plan, err := a.drafts(nil).Prepare(input, pkg)
		if err != nil {
			return err
		}
		return a.output(plan)
	}}
	p.Flags().StringVar(&file, "file", "", "Existing packaged plugin; used locally for identity, never uploaded by draft create")
	p.Flags().StringVar(&description, "description-file", "", "Markdown description; defaults to the package manifest")
	p.Flags().StringVar(&extension, "extension-id", "", "Explicit plugin extension ID if current page data cannot supply it")
	show := &cobra.Command{Use: "show <draft-id>", Short: "Show the exact local draft plan", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := a.drafts(nil).Load(args[0])
		if err != nil {
			return err
		}
		return a.output(p)
	}}
	create := &cobra.Command{Use: "create <draft-id>", Short: "Create or reuse a draft SKU; never uploads a package or submits for review", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := a.drafts(nil).Load(args[0]); err != nil {
			return err
		}
		api, err := a.client(cmd.Context(), false)
		if err != nil {
			return err
		}
		p, err := a.drafts(api).Create(cmd.Context(), args[0])
		if p.ID != "" {
			if e := a.output(p); e != nil {
				return e
			}
		}
		return err
	}}
	status := &cobra.Command{Use: "status <draft-id>", Short: "Reconcile a draft creation result using read-only product requests", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := a.drafts(nil).Load(args[0])
		if err != nil {
			return err
		}
		if p.State == draft.Prepared {
			return a.output(p)
		}
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return err
		}
		p, err = a.drafts(api).Status(cmd.Context(), args[0])
		if p.ID != "" {
			if e := a.output(p); e != nil {
				return e
			}
		}
		return err
	}}
	r.AddCommand(p, show, create, status)
	return r
}
