package cli

import (
	"bufio"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/SENTINELITE/elgato-makers-cli/internal/deletion"
	"github.com/spf13/cobra"
)

func (a *app) deletions(api deletion.API) *deletion.Manager {
	dir := a.stateDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(a.configPath), "releases")
	}
	return deletion.NewManager(filepath.Join(dir, "deletions"), api)
}

func (a *app) deleteCommand() *cobra.Command {
	var selector, expected string
	var yes bool
	c := &cobra.Command{Use: "delete", Short: "Permanently delete a draft product with explicit confirmation", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if selector == "" {
			return errors.New("provide --product <alias-or-id>")
		}
		if yes && expected == "" {
			return errors.New("--yes requires --confirm-name with the exact product name")
		}
		if !yes && !a.o.IsTerminal() {
			return errors.New("deletion requires a terminal with two confirmations, or --yes with --confirm-name")
		}
		api, err := a.client(cmd.Context(), false)
		if err != nil {
			return err
		}
		t, err := a.target(cmd.Context(), selector, api)
		if err != nil {
			return err
		}
		manager := a.deletions(api)
		plan, err := manager.Prepare(cmd.Context(), t)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.o.Err, "Permanent deletion of draft %q\nOrganization: %s\nProduct ID: %s\nUUID: %s\nOperation: %s\nThis cannot be reversed.\n", plan.Target.Name, plan.Target.OrgID, plan.Target.ProductID, plan.Target.UUID, plan.ID)
		if expected != "" && expected != plan.Target.Name {
			return errors.New("--confirm-name does not match the current product name; nothing deleted")
		}
		if !yes {
			r := bufio.NewReader(a.o.In)
			for _, prompt := range []string{"Delete this exact draft permanently? [y/N]: ", "Confirm permanent deletion now? [y/N]: "} {
				fmt.Fprint(a.o.Err, prompt)
				line, e := r.ReadString('\n')
				if e != nil || (strings.ToLower(strings.TrimSpace(line)) != "yes" && strings.ToLower(strings.TrimSpace(line)) != "y") {
					return a.output(map[string]bool{"cancelled": true, "deleted": false})
				}
			}
		}
		plan, err = manager.Delete(cmd.Context(), plan.ID)
		if plan.ID != "" {
			if e := a.output(plan); e != nil {
				return e
			}
		}
		return err
	}}
	c.Flags().StringVar(&selector, "product", "", "Draft product alias or ID")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "Bypass both prompts; requires exact --confirm-name")
	c.Flags().StringVar(&expected, "confirm-name", "", "Exact current product name, required with --yes")
	return c
}

func (a *app) deleteStatusCommand() *cobra.Command {
	return &cobra.Command{Use: "delete-status <operation-id>", Short: "Check a deletion outcome without retrying deletion", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := a.deletions(nil).Load(args[0]); err != nil {
			return err
		}
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return err
		}
		p, err := a.deletions(api).Status(cmd.Context(), args[0])
		if p.ID != "" {
			if e := a.output(p); e != nil {
				return e
			}
		}
		return err
	}}
}
