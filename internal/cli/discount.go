package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SENTINELITE/elgato-makers-cli/internal/discounts"
	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/pkg/discountplan"
	"github.com/spf13/cobra"
)

func (a *app) discountStore() discounts.Store {
	dir := a.stateDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(a.configPath), "releases")
	}
	return discounts.Store{Dir: filepath.Join(dir, "discounts")}
}
func (a *app) discountOrg() (string, error) {
	c, e := a.config()
	if e != nil {
		return "", e
	}
	return a.organization(c)
}
func (a *app) confirmDiscount(yes bool) error {
	if yes {
		return nil
	}
	if !a.o.IsTerminal() {
		return errors.New("discount mutation requires a terminal with two confirmations, or explicit --yes on this command")
	}
	r := bufio.NewReader(a.o.In)
	for _, prompt := range []string{"Apply this exact production discount change? [y/N]: ", "Confirm this monetary change now? [y/N]: "} {
		fmt.Fprint(a.o.Err, prompt)
		line, e := r.ReadString('\n')
		if e != nil || (strings.ToLower(strings.TrimSpace(line)) != "yes" && strings.ToLower(strings.TrimSpace(line)) != "y") {
			return errors.New("discount change cancelled")
		}
	}
	return nil
}
func discountLimit(v *int) string {
	if v == nil {
		return "unlimited"
	}
	return fmt.Sprint(*v)
}
func (a *app) discountCommands() *cobra.Command {
	r := &cobra.Command{Use: "discounts", Short: "Read discount codes and prepare guarded discounts"}
	var status string
	list := &cobra.Command{Use: "list", Short: "Read all pages, preserving raw state and nullable limits", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		org, e := a.discountOrg()
		if e != nil {
			return e
		}
		api, e := a.client(cmd.Context(), true)
		if e != nil {
			return e
		}
		v, e := api.Discounts(cmd.Context(), org, status)
		if e != nil {
			return e
		}
		return a.output(v)
	}}
	list.Flags().StringVar(&status, "status", "", "Verified status filter: scheduled or upcoming")
	show := &cobra.Command{Use: "show <discount-id>", Short: "Read a discount bound to the selected organization", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		org, e := a.discountOrg()
		if e != nil {
			return e
		}
		api, e := a.client(cmd.Context(), true)
		if e != nil {
			return e
		}
		v, e := api.Discount(cmd.Context(), org, args[0])
		if e != nil {
			return e
		}
		return a.output(v)
	}}
	var file string
	prepare := &cobra.Command{Use: "prepare", Short: "Validate local JSON and save an immutable 24-hour plan without network access", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if file == "" {
			return errors.New("provide --file")
		}
		b, e := os.ReadFile(file)
		if e != nil {
			return e
		}
		i, e := discountplan.DecodeInput(b)
		if e != nil {
			return e
		}
		p, e := discountplan.PrepareAt(i, a.o.Now())
		if e != nil {
			return e
		}
		if e = a.discountStore().SavePlan(p); e != nil {
			return e
		}
		return a.output(p)
	}}
	prepare.Flags().StringVar(&file, "file", "", "Local discount input JSON; end and both limits must be explicit, null means unlimited")
	var yes bool
	create := &cobra.Command{Use: "create <plan-digest>", Short: "Create the reviewed discount once", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		store := a.discountStore()
		op, e := store.Load(args[0])
		if e != nil {
			return e
		}
		if e = discountplan.Validate(op.Plan); e != nil {
			return e
		}
		org, e := a.discountOrg()
		if e != nil {
			return e
		}
		i := op.Plan.Input
		if org != i.OrganizationID {
			return errors.New("reviewed discount belongs to a different selected organization")
		}
		end := "no end date"
		if i.EndAt != nil {
			end = *i.EndAt
		}
		scope := "ALL PRODUCTS"
		if i.Scope == "specific_products" {
			scope = "SPECIFIC PRODUCT (" + i.ProductIDs[0] + ")"
		}
		value := fmt.Sprintf("%d%%", i.PercentageOff)
		if i.Mode == "fixed" {
			value = fmt.Sprintf("%s %d.%02d", i.Currency, i.AmountMinor/100, i.AmountMinor%100)
		}
		fmt.Fprintf(a.o.Err, "PRODUCTION discount creation\nOrganization: %s\nCode suffix: %s\nName: %s\nScope: %s\nDiscount: %s\nStart UTC: %s\nEnd UTC: %s\nUnique customers: %s\nUses per customer: %s\nPlan: %s\n", org, i.MakerCode, i.Name, scope, value, i.StartAt, end, discountLimit(i.MaxUniqueUsers), discountLimit(i.MaxUsesPerUser), op.Plan.Digest)
		if e = a.confirmDiscount(yes); e != nil {
			return e
		}
		api, e := a.client(cmd.Context(), false)
		if e != nil {
			return e
		}
		op, e = store.Create(cmd.Context(), args[0], api)
		if op.Plan.Digest != "" {
			if err := a.output(op); err != nil {
				return err
			}
		}
		return e
	}}
	create.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm this concrete plan and bypass both prompts")
	reconcile := &cobra.Command{Use: "status <plan-digest>", Short: "Read and reconcile exact organization/code without retrying creation", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		store := a.discountStore()
		op, e := store.Load(args[0])
		if e != nil {
			return e
		}
		org, e := a.discountOrg()
		if e != nil {
			return e
		}
		if org != op.Plan.Input.OrganizationID {
			return errors.New("discount plan belongs to another organization")
		}
		api, e := a.client(cmd.Context(), true)
		if e != nil {
			return e
		}
		op, e = store.Status(cmd.Context(), args[0], api)
		if outErr := a.output(op); outErr != nil {
			return outErr
		}
		return e
	}}
	var deactivateYes bool
	var confirmCode string
	deactivate := &cobra.Command{Use: "deactivate <discount-id>", Short: "Disable one exact discount with a durable attempt guard", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		org, e := a.discountOrg()
		if e != nil {
			return e
		}
		api, e := a.client(cmd.Context(), false)
		if e != nil {
			return e
		}
		d, e := api.Discount(cmd.Context(), org, args[0])
		if e != nil {
			return e
		}
		code := d["code"].(map[string]any)["maker_code"]
		if confirmCode == "" || code != confirmCode {
			return errors.New("--confirm-code must match the current exact maker_code suffix")
		}
		fmt.Fprintf(a.o.Err, "PRODUCTION discount deactivation\nOrganization: %s\nDiscount ID: %s\nCode suffix: %s\nCurrent record:\n", org, args[0], confirmCode)
		preview, _ := json.MarshalIndent(d, "", "  ")
		fmt.Fprintln(a.o.Err, string(preview))
		if e = a.confirmDiscount(deactivateYes); e != nil {
			return e
		}
		dir := a.discountStore().Dir
		if e = discounts.EnsurePrivateDir(dir); e != nil {
			return e
		}
		sum := sha256.Sum256([]byte(org + "/" + args[0]))
		path := filepath.Join(dir, "deactivate-"+hex.EncodeToString(sum[:])+".json")
		f, e := filesec.OpenPrivateFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
		if e != nil {
			return errors.New("deactivation already attempted; inspect show for exact ID and reconcile before any manual retry")
		}
		defer f.Close()
		checkpoint := func(state string, receipt *maker.DiscountReceipt) error {
			return discounts.WriteCheckpoint(path+".receipt", map[string]any{"organization_id": org, "discount_id": args[0], "maker_code": confirmCode, "state": state, "receipt": receipt})
		}

		if e = checkpoint("uncertain", nil); e != nil {
			return e
		}
		receipt, e := api.DeactivateDiscount(cmd.Context(), org, args[0], confirmCode, func(r maker.DiscountReceipt) error { return checkpoint("acknowledged", &r) })
		if e != nil {
			return e
		}
		if e = checkpoint("verified", &receipt); e != nil {
			return e
		}
		return a.output(receipt)
	}}
	deactivate.Flags().BoolVarP(&deactivateYes, "yes", "y", false, "Confirm this exact target and bypass both prompts")
	deactivate.Flags().StringVar(&confirmCode, "confirm-code", "", "Exact current maker_code suffix")
	r.AddCommand(list, show, prepare, create, reconcile, deactivate)
	return r
}
