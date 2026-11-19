package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/listing"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
	"github.com/spf13/cobra"
)

func (a *app) listings(api listing.API) *listing.Manager {
	dir := a.stateDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(a.configPath), "releases")
	}
	m := listing.NewManager(filepath.Join(dir, "listings"), api)
	m.SetProgress(func(message string) { fmt.Fprintln(a.o.Err, message) })
	return m
}

// runListing reports current work on stderr and joins the heartbeat before return.
// The ticker never touches stdout, so JSON remains suitable for shell pipelines.
func (a *app) runListing(ctx context.Context, api listing.API, stage string, run func(*listing.Manager) (listing.Plan, error)) (listing.Plan, error) {
	m := a.listings(api)
	started := time.Now()
	var mu sync.Mutex
	report := func(message string) {
		mu.Lock()
		defer mu.Unlock()
		stage = message
		fmt.Fprintf(a.o.Err, "%s [elapsed %s]\n", stage, time.Since(started).Round(time.Second))
	}
	m.SetProgress(report)
	report(stage)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				mu.Lock()
				fmt.Fprintf(a.o.Err, "Still working: %s [elapsed %s]\n", stage, time.Since(started).Round(time.Second))
				mu.Unlock()
			}
		}
	}()
	p, err := run(m)
	close(stop)
	<-done
	if err != nil {
		fmt.Fprintf(a.o.Err, "Stopped during: %s [elapsed %s]\n", stage, time.Since(started).Round(time.Second))
	}
	return p, err
}

func (a *app) prepareListingFile(file, mode string) (listing.Prepared, error) {
	if file == "" {
		return listing.Prepared{}, errors.New("provide --file listing.json")
	}
	c, err := listing.LoadConfig(file)
	if err != nil {
		return listing.Prepared{}, err
	}
	if mode != "" {
		c.GalleryUploadMode = mode
	}
	return listing.PrepareConfig(c)
}

func (a *app) listingOrganization(p listing.Prepared) (string, error) {
	c, err := a.config()
	if err != nil {
		return "", err
	}
	// The listing can carry its organization so a complete JSON file works
	// without a separate CLI configuration. Explicit --org takes precedence.
	if p.Config.OrganizationID != "" {
		c.OrgID = p.Config.OrganizationID
	}
	org, err := a.organization(c)
	if err != nil {
		return "", err
	}
	if p.Config.OrganizationID != "" && org != p.Config.OrganizationID {
		return "", errors.New("listing org_id differs from --org")
	}
	return org, nil
}

func (a *app) listingResult(p listing.Plan, operationErr error, completion string) error {
	if operationErr != nil {
		if p.ID != "" {
			fmt.Fprintf(a.o.Err, "Listing %s stopped; saved state: %s.\n", p.ID, p.State)
			if p.Target.ProductID != "" {
				fmt.Fprintf(a.o.Err, "Remote product: %s; variant: %s.\n", p.Target.ProductID, p.Target.VariantID)
			}
			if p.DraftPlanID != "" {
				fmt.Fprintf(a.o.Err, "Saved draft plan: %s.\n", p.DraftPlanID)
			}
			if p.ReleasePlanID != "" {
				fmt.Fprintf(a.o.Err, "Saved release plan: %s.\n", p.ReleasePlanID)
			}
			completed, pending := []string{}, []string{}
			checkpoints := []struct{ name, state string }{}
			if p.EarlyDetailsState != "" {
				checkpoints = append(checkpoints, struct{ name, state string }{"initial pricing and details", p.EarlyDetailsState})
			}
			checkpoints = append(checkpoints, struct{ name, state string }{"listing details", p.DetailsState}, struct{ name, state string }{"package upload", p.PackageState})
			for _, checkpoint := range checkpoints {
				if checkpoint.state == listing.Done {
					completed = append(completed, checkpoint.name)
				} else {
					pending = append(pending, checkpoint.name+" ["+checkpoint.state+"]")
				}
			}
			mediaDone := 0
			for _, item := range p.Media {
				if item.State == listing.Done {
					mediaDone++
				}
			}
			if len(completed) == 0 {
				completed = append(completed, "none")
			}
			fmt.Fprintf(a.o.Err, "Completed checkpoints: %s; media %d/%d.\n", strings.Join(completed, ", "), mediaDone, len(p.Media))
			if len(pending) > 0 {
				fmt.Fprintf(a.o.Err, "Remaining checkpoints: %s.\n", strings.Join(pending, ", "))
			}
			fmt.Fprintf(a.o.Err, "Inspect: maker listing status %s\nSaved plan: maker listing show %s\n", p.ID, p.ID)
			if a.json || a.minify {
				return errors.Join(operationErr, a.output(p))
			}
			return operationErr
		}
		return operationErr
	}
	if err := a.output(p); err != nil {
		return err
	}
	a.completion = completion
	return nil
}

func (a *app) listingCommand() *cobra.Command {
	r := &cobra.Command{Use: "listing", Short: "Validate, stage, submit, and reconcile complete Marketplace listings"}
	var validateFile, validateMode string
	validate := &cobra.Command{Use: "validate", Short: "Validate listing JSON and all artifacts offline without writing state", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		p, err := a.prepareListingFile(validateFile, validateMode)
		if err != nil {
			return err
		}
		if err = a.output(p); err != nil {
			return err
		}
		a.completion = "Success: listing validated locally; no uploads or submission."
		return nil
	}}
	validate.Flags().StringVar(&validateFile, "file", "", "Complete listing JSON file")
	validate.Flags().StringVar(&validateMode, "gallery-upload-mode", "", "Override gallery upload mode (sequential)")
	var prepareFile, prepareMode string
	prepare := &cobra.Command{Use: "prepare", Short: "Validate artifacts and save a resumable local listing plan offline", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		p, err := a.prepareListingFile(prepareFile, prepareMode)
		if err != nil {
			return err
		}
		org, err := a.listingOrganization(p)
		if err != nil {
			return err
		}
		plan, err := a.listings(nil).Prepare(p, org)
		if err != nil {
			return err
		}
		return a.listingResult(plan, nil, fmt.Sprintf("Success: listing %s prepared locally; no uploads or submission.", plan.ID))
	}}
	prepare.Flags().StringVar(&prepareFile, "file", "", "Complete listing JSON file")
	prepare.Flags().StringVar(&prepareMode, "gallery-upload-mode", "", "Override gallery upload mode (sequential)")
	show := &cobra.Command{Use: "show <listing-id>", Short: "Show a local listing plan offline", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := a.listings(nil).Load(args[0])
		if err != nil {
			return err
		}
		return a.output(p)
	}}
	upload := &cobra.Command{Use: "upload <listing-id>", Short: "Create or reuse the SKU and stage listing assets without submitting", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := a.listings(nil).Load(args[0])
		if err != nil {
			return err
		}
		if err = listing.Verify(p.Prepared); err != nil {
			return err
		}
		fmt.Fprintln(a.o.Err, "Connecting to Maker to stage the listing...")
		api, err := a.client(cmd.Context(), false)
		if err != nil {
			return a.listingResult(p, err, "")
		}
		updated, err := a.runListing(cmd.Context(), api, "Staging listing", func(m *listing.Manager) (listing.Plan, error) { return m.Upload(cmd.Context(), p.ID) })
		if updated.ID != "" {
			p = updated
		}
		return a.listingResult(p, err, fmt.Sprintf("Success: listing %s staged; no review submission.", p.ID))
	}}
	var submitFile, submitMode string
	var yes bool
	submit := &cobra.Command{Use: "submit [listing-id]", Short: "Submit a JSON listing or saved plan with two confirmations, or explicit --yes", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if (len(args) == 1) == (submitFile != "") {
			return errors.New("provide either a listing ID or --file listing.json")
		}
		if submitMode != "" && submitFile == "" {
			return errors.New("--gallery-upload-mode requires --file; a saved plan is immutable")
		}
		if !yes && !a.o.IsTerminal() {
			return errors.New("listing submission requires a terminal with two confirmations, or explicit --yes on listing submit")
		}
		var p listing.Plan
		var err error
		if submitFile != "" {
			prepared, e := a.prepareListingFile(submitFile, submitMode)
			if e != nil {
				return e
			}
			org, e := a.listingOrganization(prepared)
			if e != nil {
				return e
			}
			p, err = a.listings(nil).Prepare(prepared, org)
		} else {
			p, err = a.listings(nil).Load(args[0])
		}
		if err != nil {
			return err
		}
		if err = listing.Verify(p.Prepared); err != nil {
			return err
		}
		fmt.Fprintf(a.o.Err, "Listing plan: %s [%s]\nResume: maker listing submit %s\nInspect: maker listing status %s\nConnecting to Maker...\n", p.ID, p.State, p.ID, p.ID)
		api, err := a.client(cmd.Context(), false)
		if err != nil {
			return a.listingResult(p, err, "")
		}
		updated, err := a.runListing(cmd.Context(), api, "Reviewing listing submission", func(m *listing.Manager) (listing.Plan, error) {
			return m.Submit(cmd.Context(), p.ID, release.ConfirmOptions{Yes: yes, IsTerminal: a.o.IsTerminal(), Input: a.o.In, Output: a.o.Err})
		})
		if updated.ID != "" {
			p = updated
		}
		if errors.Is(err, release.ErrCancelled) {
			return a.output(map[string]any{"cancelled": true, "submitted": false, "plan": p})
		}
		return a.listingResult(p, err, fmt.Sprintf("Success: listing %s submitted for review.", p.ID))
	}}
	submit.Flags().StringVar(&submitFile, "file", "", "Complete listing JSON file (prepares and submits directly)")
	submit.Flags().StringVar(&submitMode, "gallery-upload-mode", "", "Override gallery upload mode for --file (sequential)")
	submit.Flags().BoolVarP(&yes, "yes", "y", false, "Explicitly bypass both production submission confirmations")
	status := &cobra.Command{Use: "status <listing-id>", Short: "Reconcile remote status using read-only requests; never retry writes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := a.listings(nil).Load(args[0])
		if err != nil {
			return err
		}
		if p.State == listing.Retired || p.State == listing.Discarded || (p.DraftPlanID == "" && p.ReleasePlanID == "" && p.DetailsState == listing.Ready) {
			return a.listingStatusResult(p, nil)
		}
		fmt.Fprintln(a.o.Err, "Connecting to Maker to read listing status...")
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return a.listingStatusResult(p, err)
		}
		updated, err := a.runListing(cmd.Context(), api, "Reading listing status", func(m *listing.Manager) (listing.Plan, error) { return m.Status(cmd.Context(), p.ID) })
		if updated.ID != "" {
			p = updated
		}
		return a.listingStatusResult(p, err)
	}}
	discard := &cobra.Command{Use: "discard <listing-id>", Short: "Discard a pristine local plan without remote changes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := a.listings(nil).Discard(args[0])
		return a.listingResult(p, err, fmt.Sprintf("Success: listing %s discarded locally; no remote deletion or submission.", p.ID))
	}}
	retire := &cobra.Command{Use: "retire <listing-id>", Short: "Retire a local plan after read-only verification that its remote product is absent", Long: "Retire a saved local listing plan only after Maker confirms its known remote product is absent. This command reads remote state and updates local state. It never deletes a remote product.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := a.listings(nil).Load(args[0])
		if err != nil {
			return err
		}
		if p.State == listing.Retired {
			return a.listingResult(p, nil, fmt.Sprintf("Success: listing %s is retired locally.", p.ID))
		}
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return a.listingResult(p, err, "")
		}
		updated, err := a.runListing(cmd.Context(), api, "Verifying remote product absence for local retirement", func(m *listing.Manager) (listing.Plan, error) { return m.Retire(cmd.Context(), p.ID) })
		if updated.ID != "" {
			p = updated
		}
		return a.listingResult(p, err, fmt.Sprintf("Success: listing %s retired locally after remote absence was verified; no remote deletion.", p.ID))
	}}
	r.AddCommand(validate, prepare, show, upload, submit, status, discard, retire, a.listingRecoverCreationCommand(), a.listingReorderCommand())
	return r
}

// createListingConfig publishes a fully written private file atomically, with
// no replacement window for existing files or symbolic links.
func createListingConfig(path string, c listing.Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := filesec.CreateTemp(filepath.Dir(path), ".listing-config-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Link(f.Name(), path); err != nil {
		return fmt.Errorf("create reordered listing config (destination must not exist): %w", err)
	}
	return filesec.SyncDir(filepath.Dir(path))
}

func (a *app) listingReorderCommand() *cobra.Command {
	var file, order, output string
	cmd := &cobra.Command{Use: "reorder", Short: "Write a reordered gallery configuration locally without remote changes", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if file == "" || order == "" {
			return errors.New("provide --file listing.json and --order as a complete 1-based permutation, such as 3,1,2")
		}
		var indices []int
		for _, value := range strings.Split(order, ",") {
			index, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return errors.New("--order must contain comma-separated 1-based gallery indices")
			}
			indices = append(indices, index)
		}
		c, err := listing.LoadConfig(file)
		if err != nil {
			return err
		}
		reordered, err := listing.Reorder(c, indices)
		if err != nil {
			return err
		}
		prepared, err := listing.PrepareConfig(reordered)
		if err != nil {
			return err
		}
		if output != "" {
			if err = createListingConfig(output, prepared.Config); err != nil {
				return err
			}
		}
		if err = a.output(prepared.Config); err != nil {
			return err
		}
		a.completion = "Success: reordered local listing configuration only; no remote changes."
		return nil
	}}
	cmd.Flags().StringVar(&file, "file", "", "Complete listing JSON file")
	cmd.Flags().StringVar(&order, "order", "", "Complete 1-based gallery permutation, for example 3,1,2")
	cmd.Flags().StringVar(&output, "output", "", "Create a new private JSON file; existing destinations are refused")
	return cmd
}
