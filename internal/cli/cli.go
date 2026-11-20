package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/artifact"
	"github.com/SENTINELITE/elgato-makers-cli/internal/auth"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var Version = "0.9.1"

type Options struct {
	In         io.Reader
	Out        io.Writer
	Err        io.Writer
	IsTerminal func() bool
	Now        func() time.Time
	// ClientFactory exists for synthetic CLI integration tests. It is not a flag.
	ClientFactory func([]*http.Cookie, bool) (*maker.Client, error)
}

type app struct {
	completion                                 string // Set after successful output; emit after session persistence.
	o                                          Options
	configPath, stateDir, credentialsFile, org string
	json                                       bool
	minify                                     bool
	verbose                                    bool
	sessions                                   []session
	unlockSession                              func()
}

type session struct {
	client  *maker.Client
	store   auth.Store
	initial string
}

func New(o Options) *cobra.Command {
	if o.In == nil {
		o.In = os.Stdin
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Err == nil {
		o.Err = os.Stderr
	}
	if o.IsTerminal == nil {
		o.IsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	a := &app{o: o, configPath: filepath.Join(base, "Elgato Makers CLI", "config.json")}
	r := &cobra.Command{Use: "maker", Short: "Elgato Makers CLI", Long: "Read Maker products and prepare deliberate Stream Deck releases using HTTP and imported session cookies.", Version: Version, SilenceErrors: true, SilenceUsage: true}
	r.SetIn(o.In)
	r.SetOut(o.Out)
	r.SetErr(o.Err)
	configHelp := "~/.config/Elgato Makers CLI/config.json (or under XDG_CONFIG_HOME)"
	switch runtime.GOOS {
	case "darwin":
		configHelp = "~/Library/Application Support/Elgato Makers CLI/config.json"
	case "windows":
		configHelp = `%AppData%\Elgato Makers CLI\config.json`
	}
	r.PersistentFlags().StringVar(&a.configPath, "config", a.configPath, "Configuration file (default: "+configHelp+")")
	r.PersistentFlags().Lookup("config").DefValue = ""
	r.PersistentFlags().StringVar(&a.stateDir, "state-dir", "", "Release-state directory (defaults beside configuration)")
	r.PersistentFlags().StringVar(&a.credentialsFile, "credentials-file", "", "Use an explicit private credential file instead of Keychain")
	r.PersistentFlags().StringVar(&a.org, "org", "", "Override the configured organization")
	r.PersistentFlags().BoolVar(&a.json, "json", false, "Write JSON results")
	r.PersistentFlags().BoolVar(&a.minify, "minify", false, "Write compact single-line JSON (implies --json)")
	r.PersistentFlags().BoolVarP(&a.verbose, "verbose", "v", false, "Show aggregate HTTP timing and cache statistics on stderr")
	r.AddCommand(a.mcpCommands(), a.cloudCommands(), a.ciCommands(), a.authCommands(), a.configCommands(), a.productsCommand(), a.productCommand(), a.versionsCommand(false), a.versionsCommand(true), a.releaseCommands(), a.inspectCommand(), a.doctorCommand(), a.analyticsCommand(), a.listingCommand(), a.discountCommands())
	r.CompletionOptions.DisableDefaultCmd = true
	a.flushAfterCommands(r)
	return r
}

// Persist server-issued cookie changes even if a later command step fails.
// Credential persistence never retries a request or changes release state.
func (a *app) flushAfterCommands(c *cobra.Command) {
	for _, child := range c.Commands() {
		a.flushAfterCommands(child)
	}
	if c.RunE == nil {
		return
	}
	run := c.RunE
	c.RunE = func(cmd *cobra.Command, args []string) error {
		defer func() {
			if a.unlockSession != nil {
				a.unlockSession()
				a.unlockSession = nil
			}
		}()
		a.completion = ""
		originalJSON := a.json
		if a.minify {
			a.json = true
		}
		defer func() { a.json = originalJSON }()
		commandErr := run(cmd, args)
		var saveErr error
		for _, s := range a.sessions {
			cookies := s.client.CookieSnapshot()
			var recognized []*http.Cookie
			for _, cookie := range cookies {
				if cookie.Name == "__Host-kc_at" || cookie.Name == "kc_user" || cookie.Name == "__Secure-kc_rt" {
					recognized = append(recognized, cookie)
				}
			}
			var err error
			if len(recognized) == 0 {
				err = s.store.Delete()
			} else {
				var creds auth.Credentials
				creds, err = auth.FromCookies(recognized)
				if err == nil && sessionSignature(creds.Cookies()) != s.initial {
					err = s.store.Save(creds)
				}
			}
			if err != nil {
				saveErr = errors.Join(saveErr, fmt.Errorf("could not persist the rotated Maker session: %w; reimport fresh cookies; release state was preserved, do not repeat a mutation just because credential saving failed", err))
			}
		}
		result := errors.Join(commandErr, saveErr)
		if a.verbose {
			if err := a.writeRequestStats(a.sessions); err != nil {
				result = errors.Join(result, fmt.Errorf("write request statistics: %w", err))
			}
		}
		a.sessions = nil
		if result == nil && a.completion != "" {
			if _, err := fmt.Fprintln(a.o.Err, a.completion); err != nil {
				result = fmt.Errorf("write completion message: %w", err)
			}
		}
		a.completion = ""
		return result
	}
}

func sessionSignature(cookies []*http.Cookie) string {
	copy := append([]*http.Cookie(nil), cookies...)
	sort.Slice(copy, func(i, j int) bool { return copy[i].Name < copy[j].Name })
	b, _ := json.Marshal(copy)
	return string(b)
}

func (a *app) config() (configuration, error) { return loadConfig(a.configPath) }
func (a *app) organization(c configuration) (string, error) {
	org := c.OrgID
	if a.org != "" {
		org = a.org
	}
	if !resourceID.MatchString(org) {
		return "", errors.New("select an organization with --org or maker config init --org")
	}
	return org, nil
}
func (a *app) store() auth.Store {
	if a.credentialsFile != "" {
		return auth.NewFileStore(a.credentialsFile)
	}
	return auth.NewKeychainStore("com.sentinelite.elgato-makers-cli", "maker.elgato.com")
}
func (a *app) client(ctx context.Context, readOnly bool) (*maker.Client, error) {
	return a.clientWithRenewal(ctx, readOnly, true)
}

func (a *app) clientWithRenewal(ctx context.Context, readOnly, automatic bool) (*maker.Client, error) {
	if err := a.lockSession(); err != nil {
		return nil, err
	}
	store := a.store()
	c, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("load Maker session: %w; import fresh cookies with maker auth import --stdin", err)
	}
	var api *maker.Client
	if a.o.ClientFactory != nil {
		api, err = a.o.ClientFactory(c.Cookies(), readOnly)
	} else {
		api, err = maker.New(maker.Config{Cookies: c.Cookies(), ReadOnly: readOnly})
	}
	if err != nil {
		return nil, err
	}
	a.sessions = append(a.sessions, session{client: api, store: store, initial: sessionSignature(c.Cookies())})
	if automatic {
		var info maker.SessionInfo
		if info, err = api.EnsureSession(ctx); err != nil {
			return nil, fmt.Errorf("renew Maker session: %w; import fresh cookies with maker auth import --stdin", err)
		}
		// Save renewal before an operation can perform a production write.
		if info.Refreshed {
			if err = persistCookies(store, api); err != nil {
				return nil, fmt.Errorf("save renewed Maker session: %w", err)
			}
		}
	}
	return api, nil
}
func (a *app) plans(api release.API) *release.Manager {
	dir := a.stateDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(a.configPath), "releases")
	}
	return release.NewManager(dir, api)
}
func (a *app) output(v any) error {
	e := json.NewEncoder(a.o.Out)
	if !a.minify {
		e.SetIndent("", "  ")
	}
	return e.Encode(v)
}
func (a *app) target(ctx context.Context, selector string, api *maker.Client) (model.Target, error) {
	c, err := a.config()
	if err != nil {
		return model.Target{}, err
	}
	org, err := a.organization(c)
	if err != nil {
		return model.Target{}, err
	}
	if t, ok := c.Products[selector]; ok {
		if t.OrgID == "" {
			t.OrgID = org
		}
		if t.OrgID != org {
			return t, errors.New("alias belongs to a different organization")
		}
		return t, nil
	}
	if !resourceID.MatchString(selector) {
		return model.Target{}, errors.New("select a configured alias or product ID with --product")
	}
	t := model.Target{OrgID: org, ProductID: selector}
	p, err := api.Product(ctx, t)
	if err != nil {
		return t, err
	}
	t.Name = p.Name
	t.UUID = p.UUID
	if len(p.Variants) == 1 {
		t.VariantID = p.Variants[0].ID
		t.UUID = p.Variants[0].UUID
	}
	return t, nil
}

func verifyTarget(ctx context.Context, api *maker.Client, t model.Target) error {
	p, err := api.Product(ctx, t)
	if err != nil {
		return err
	}
	for _, v := range p.Variants {
		if v.ID == t.VariantID {
			if v.UUID == "" || v.UUID != t.UUID {
				return errors.New("remote variant UUID does not match the configured release target")
			}
			return nil
		}
	}
	return errors.New("configured variant does not belong to the remote product")
}

func (a *app) authCommands() *cobra.Command {
	r := &cobra.Command{Use: "auth", Short: "Import, check, or forget your Maker session"}
	var stdin bool
	var input string
	i := &cobra.Command{Use: "import", Short: "Import a Cookie header or cookie JSON into the selected credential store", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if stdin == (input != "") {
			return errors.New("choose exactly one of --stdin or --cookie-file")
		}
		reader := a.o.In
		if input != "" {
			f, err := os.Open(input)
			if err != nil {
				return err
			}
			defer f.Close()
			reader = f
		}
		c, err := auth.Parse(reader)
		if err != nil {
			return err
		}
		if err = a.lockSession(); err != nil {
			return err
		}
		if err = a.store().Save(c); err != nil {
			return err
		}
		return a.output(map[string]any{"imported": true, "cookie_count": len(c.Cookies()), "known_expiry": c.Expiry(), "message": "Session imported. Run maker auth check to verify access."})
	}}
	i.Flags().BoolVar(&stdin, "stdin", false, "Read credentials from standard input")
	i.Flags().StringVar(&input, "cookie-file", "", "Read credentials from this input file")
	check := &cobra.Command{Use: "check", Short: "Check organization access with a read-only request", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := a.config()
		if err != nil {
			return err
		}
		org, err := a.organization(c)
		if err != nil {
			return err
		}
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return err
		}
		if err = api.CheckAuth(cmd.Context(), org); err != nil {
			return fmt.Errorf("session check failed: %w; import fresh Maker cookies if expired", err)
		}
		return a.output(map[string]any{"authenticated": true, "org_id": org})
	}}
	forget := &cobra.Command{Use: "forget", Short: "Remove locally stored Maker credentials", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := a.lockSession(); err != nil {
			return err
		}
		if err := a.store().Delete(); err != nil {
			return err
		}
		return a.output(map[string]bool{"forgotten": true})
	}}
	refresh := &cobra.Command{Use: "refresh", Short: "Renew the imported session using Maker HTTP and persist rotated cookies", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		api, err := a.clientWithRenewal(cmd.Context(), true, false)
		if err != nil {
			return err
		}
		info, err := api.RefreshSession(cmd.Context())
		if err != nil {
			return err
		}
		return a.output(info)
	}}
	r.AddCommand(i, check, forget, refresh)
	return r
}

func (a *app) configCommands() *cobra.Command {
	r := &cobra.Command{Use: "config", Short: "Manage local organization and product aliases"}
	init := &cobra.Command{Use: "init", Short: "Set the organization without changing existing aliases", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !resourceID.MatchString(a.org) {
			return errors.New("provide --org <organization-id>")
		}
		c, err := a.config()
		if err != nil {
			return err
		}
		c.OrgID = a.org
		if err = saveConfig(a.configPath, c); err != nil {
			return err
		}
		return a.output(c)
	}}
	var selector, variant, uuid string
	bind := &cobra.Command{Use: "bind <alias>", Short: "Bind a local alias to a verified product and variant", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !aliasID.MatchString(args[0]) {
			return errors.New("alias must use lowercase letters, digits and hyphens")
		}
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return err
		}
		t, err := a.target(cmd.Context(), selector, api)
		if err != nil {
			return err
		}
		p, err := api.Product(cmd.Context(), t)
		if err != nil {
			return err
		}
		if variant != "" {
			t.VariantID = variant
		}
		if t.VariantID == "" {
			return errors.New("this product has multiple variants; provide --variant")
		}
		for _, v := range p.Variants {
			if v.ID == t.VariantID {
				t.UUID = v.UUID
			}
		}
		if uuid != "" && t.UUID != "" && uuid != t.UUID {
			return errors.New("--uuid conflicts with the remote variant")
		}
		if uuid != "" {
			t.UUID = uuid
		}
		if t.UUID == "" {
			return errors.New("remote variant UUID missing; release target cannot be verified")
		}
		if err = verifyTarget(cmd.Context(), api, t); err != nil {
			return err
		}
		t.Alias = args[0]
		t.Name = p.Name
		c, err := a.config()
		if err != nil {
			return err
		}
		c.Products[args[0]] = t
		if err = saveConfig(a.configPath, c); err != nil {
			return err
		}
		return a.output(t)
	}}
	bind.Flags().StringVar(&selector, "product", "", "Product ID or existing alias")
	bind.Flags().StringVar(&variant, "variant", "", "Variant ID if the product has multiple variants")
	bind.Flags().StringVar(&uuid, "uuid", "", "Expected plugin UUID for additional verification")
	show := &cobra.Command{Use: "show", Short: "Show nonsensitive local configuration", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := a.config()
		if err != nil {
			return err
		}
		return a.output(c)
	}}
	r.AddCommand(init, bind, show)
	return r
}

func (a *app) productsCommand() *cobra.Command {
	return &cobra.Command{Use: "products", Short: "List products in the selected organization", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := a.config()
		if err != nil {
			return err
		}
		org, err := a.organization(c)
		if err != nil {
			return err
		}
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return err
		}
		ps, err := api.Products(cmd.Context(), org)
		if err != nil {
			return err
		}
		if a.json {
			return a.output(ps)
		}
		w := tabwriter.NewWriter(a.o.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tTYPE\tSTATUS")
		for _, p := range ps {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.ID, p.Name, p.Type, p.Status)
		}
		return w.Flush()
	}}
}

func (a *app) productCommand() *cobra.Command {
	r := &cobra.Command{Use: "product", Short: "Inspect a product"}
	var selector string
	s := &cobra.Command{Use: "show", Short: "Show product and variant details", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return err
		}
		t, err := a.target(cmd.Context(), selector, api)
		if err != nil {
			return err
		}
		p, err := api.Product(cmd.Context(), t)
		if err != nil {
			return err
		}
		return a.output(p)
	}}
	s.Flags().StringVar(&selector, "product", "", "Product alias or ID")
	r.AddCommand(s, a.draftCommands(), a.deleteCommand(), a.deleteStatusCommand())
	return r
}

func (a *app) versionsCommand(notes bool) *cobra.Command {
	var selector string
	var count int
	name, short := "versions", "List a product's versions and review status"
	if notes {
		name, short = "notes", "Read prior release notes"
	}
	c := &cobra.Command{Use: name, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return err
		}
		t, err := a.target(cmd.Context(), selector, api)
		if err != nil {
			return err
		}
		vs, err := api.Versions(cmd.Context(), t)
		if err != nil {
			return err
		}
		if notes && count < 1 {
			return errors.New("--count must be positive")
		}
		if notes && len(vs) > count {
			vs = vs[:count]
		}
		if a.json {
			return a.output(vs)
		}
		if notes {
			for _, v := range vs {
				fmt.Fprintf(a.o.Out, "%s (%s)\n%s\n\n", v.Version, v.Status, v.ReleaseNotes)
			}
			return nil
		}
		w := tabwriter.NewWriter(a.o.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "VERSION\tSTATUS\tID")
		for _, v := range vs {
			fmt.Fprintf(w, "%s\t%s\t%s\n", v.Version, v.Status, v.ID)
		}
		return w.Flush()
	}}
	c.Flags().StringVar(&selector, "product", "", "Product alias or ID")
	if notes {
		c.Flags().IntVar(&count, "count", 2, "Maximum releases to display")
	}
	return c
}

func (a *app) preview(p release.Plan) error {
	if a.json {
		return a.output(p)
	}
	fmt.Fprintf(a.o.Out, "Release %s [%s]\nProduct: %s (%s)\nVersion: %s\nUUID: %s\nSHA-256: %s\nAuto-publish after approval: %t\nRelease notes (%d characters):\n%s\n", p.ID, p.State, p.Target.Name, p.Target.ProductID, p.Package.Manifest.Version, p.Package.Manifest.UUID, p.Package.SHA256, p.AutoPublish, p.Notes.PlainTextLength, p.Notes.Markdown)
	fmt.Fprintf(a.o.Out, "Minimum Stream Deck: %s\n", p.Package.Manifest.Software.MinimumVersion)
	for _, platform := range p.Package.Manifest.OS {
		fmt.Fprintf(a.o.Out, "Minimum %s: %s\n", platform.Platform, platform.MinimumVersion)
	}
	if p.SubmissionID != "" {
		fmt.Fprintf(a.o.Out, "Remote version: %s\nRemote status: %s\n", p.SubmissionID, p.RemoteStatus)
	}
	return nil
}

func (a *app) releaseCommands() *cobra.Command {
	r := &cobra.Command{Use: "release", Short: "Prepare, upload, explicitly submit, and reconcile releases"}
	var selector, file, notes string
	var auto, noAuto, offline bool
	p := &cobra.Command{Use: "prepare", Short: "Create a local review plan without uploading or submitting", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("auto-publish") == cmd.Flags().Changed("no-auto-publish") {
			return errors.New("choose exactly one of --auto-publish or --no-auto-publish")
		}
		if (cmd.Flags().Changed("auto-publish") && !auto) || (cmd.Flags().Changed("no-auto-publish") && !noAuto) {
			return errors.New("publication-choice flags must be enabled, not assigned false")
		}
		if file == "" || notes == "" {
			return errors.New("provide --file and --notes")
		}
		var api *maker.Client
		var t model.Target
		var err error
		if offline {
			c, e := a.config()
			if e != nil {
				return e
			}
			org, e := a.organization(c)
			if e != nil {
				return e
			}
			var ok bool
			t, ok = c.Products[selector]
			if !ok {
				return errors.New("offline preparation requires a configured product alias")
			}
			if t.OrgID == "" {
				t.OrgID = org
			}
			if t.OrgID != org {
				return errors.New("alias belongs to a different organization")
			}
		} else {
			api, err = a.client(cmd.Context(), true)
			if err != nil {
				return err
			}
			t, err = a.target(cmd.Context(), selector, api)
			if err != nil {
				return err
			}
			if err = verifyTarget(cmd.Context(), api, t); err != nil {
				return err
			}
		}
		plan, err := a.plans(api).Prepare(cmd.Context(), t, file, notes, auto)
		if err != nil {
			return err
		}
		return a.preview(plan)
	}}
	p.Flags().StringVar(&selector, "product", "", "Configured product alias or single-variant product ID")
	p.Flags().StringVar(&file, "file", "", "Existing .streamDeckPlugin archive")
	p.Flags().StringVar(&notes, "notes", "", "Markdown release-notes file")
	p.Flags().BoolVar(&auto, "auto-publish", false, "Publish automatically after Elgato approves")
	p.Flags().BoolVar(&noAuto, "no-auto-publish", false, "Keep publication manual after approval")
	p.Flags().BoolVar(&offline, "offline", false, "Use the configured target for local preparation only")
	show := &cobra.Command{Use: "show <release-id>", Short: "Show a local review plan without network access", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		plan, err := a.plans(nil).Load(args[0])
		if err != nil {
			return err
		}
		return a.preview(plan)
	}}
	u := &cobra.Command{Use: "upload <release-id>", Short: "Stage the reviewed package; never submits for review", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		api, err := a.client(cmd.Context(), false)
		if err != nil {
			return err
		}
		m := a.plans(api)
		plan, err := m.Load(args[0])
		if err != nil {
			return err
		}
		if err = verifyTarget(cmd.Context(), api, plan.Target); err != nil {
			return err
		}
		plan, err = m.Upload(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return a.preview(plan)
	}}
	var yes bool
	s := &cobra.Command{Use: "submit <release-id>", Short: "Submit once with two confirmations, or explicit --yes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !yes && !a.o.IsTerminal() {
			return errors.New("submission requires a terminal with two confirmations, or explicit --yes on release submit")
		}
		api, err := a.client(cmd.Context(), false)
		if err != nil {
			return err
		}
		m := a.plans(api)
		plan, err := m.Load(args[0])
		if err != nil {
			return err
		}
		if !a.json {
			if err = a.preview(plan); err != nil {
				return err
			}
		} else if !yes {
			preview := *a
			preview.json = false
			preview.o.Out = a.o.Err
			if err = preview.preview(plan); err != nil {
				return err
			}
		}
		if err = verifyTarget(cmd.Context(), api, plan.Target); err != nil {
			return err
		}
		plan, err = m.Submit(cmd.Context(), args[0], release.ConfirmOptions{Yes: yes, IsTerminal: a.o.IsTerminal(), Input: a.o.In, Output: a.o.Err})
		if errors.Is(err, release.ErrCancelled) {
			return a.output(map[string]bool{"cancelled": true, "submitted": false})
		}
		if err != nil {
			if plan.ID != "" && plan.State == release.SubmissionUnknown {
				if displayErr := a.preview(plan); displayErr != nil {
					return displayErr
				}
			}
			return err
		}
		return a.preview(plan)
	}}
	s.Flags().BoolVarP(&yes, "yes", "y", false, "Explicitly bypass both submission confirmations")
	status := &cobra.Command{Use: "status <release-id>", Short: "Read and reconcile submission status without resubmitting", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		m := a.plans(nil)
		plan, err := m.Load(args[0])
		if err != nil {
			return err
		}
		if plan.State == release.Prepared || plan.State == release.Uploaded {
			return a.preview(plan)
		}
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return err
		}
		plan, err = a.plans(api).Status(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return a.preview(plan)
	}}
	r.AddCommand(p, show, u, s, status, a.recoverReceiptCommand())
	return r
}

func (a *app) inspectCommand() *cobra.Command {
	return &cobra.Command{Use: "inspect <package.streamDeckPlugin>", Short: "Inspect and hash an archive locally", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := artifact.Inspect(args[0])
		if err != nil {
			return err
		}
		return a.output(p)
	}}
}

func (a *app) doctorCommand() *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "Show local configuration and credential availability; no network", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := a.config()
		if err != nil {
			return err
		}
		creds, loadErr := a.store().Load()
		store := "macOS Keychain"
		if a.credentialsFile != "" {
			store = "explicit private file"
		}
		message := "available"
		if loadErr != nil {
			message = loadErr.Error()
		}
		state := a.stateDir
		if state == "" {
			state = filepath.Join(filepath.Dir(a.configPath), "releases")
		}
		return a.output(map[string]any{"version": Version, "config": a.configPath, "state_dir": state, "org_id": c.OrgID, "product_aliases": len(c.Products), "credential_store": store, "credentials": message, "known_expiry": creds.Expiry(), "browser_automation": false})
	}}
}
