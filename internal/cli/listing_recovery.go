package cli

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"github.com/SENTINELITE/elgato-makers-cli/internal/listing"
	"github.com/spf13/cobra"
)

func (a *app) listingRecoverCreationCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "recover-creation <listing-id>",
		Short: "Review uncertain creation and retire local records after read-only catalog checks",
		Long:  "Recover an uncertain past creation only after two interactive default-No confirmations. Catalog absence reflects current visibility and cannot prove the original request failed. Recovery retires local records and enables a new explicit attempt. This command never creates, uploads, deletes, or submits remote work. There is no --yes bypass.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.listings(nil).Load(args[0])
			if err != nil {
				return err
			}
			if err = a.listings(nil).ValidateCreationRecovery(p.ID); err != nil {
				return a.listingResult(p, err, "")
			}
			if p.State == listing.Retired && p.CreationRecovery != nil {
				return a.listingResult(p, nil, "Success: uncertain creation recovery was already recorded locally; retired plan cannot resume. Prepare a new listing using your original --file.")
			}
			if !a.o.IsTerminal() || a.o.In == nil {
				return fmt.Errorf("creation recovery requires a terminal and two default-No confirmations; no --yes bypass is available")
			}
			fmt.Fprintf(a.o.Err, "Review uncertain creation for listing %s\nName: %s\nOrganization: %s\nUUID: %s\nSaved draft plan: %s\n", p.ID, p.Prepared.Config.Name, p.Target.OrgID, p.Target.UUID, p.DraftPlanID)
			fmt.Fprintln(a.o.Err, "The past creation outcome is uncertain. Catalog absence only reflects current visibility; it cannot prove the original request failed. Recovery retires local records and enables a new explicit attempt. This command never creates, uploads, deletes, or submits remote work.")
			reader := bufio.NewReader(a.o.In)
			for _, prompt := range []string{"Check the catalog and recover this uncertain creation locally? [y/N]: ", "Accept this uncertainty and retire the local records if checks allow it? [y/N]: "} {
				if err = cmd.Context().Err(); err != nil {
					return err
				}
				fmt.Fprint(a.o.Err, prompt)
				line, readErr := readRecoveryAnswer(cmd.Context(), reader)
				if err = cmd.Context().Err(); err != nil {
					return err
				}
				answer := strings.ToLower(strings.TrimSpace(line))
				if readErr != nil || (answer != "yes" && answer != "y") {
					fmt.Fprintln(a.o.Err, "Cancelled: no catalog checks or local changes were made.")
					if a.json || a.minify {
						return a.output(map[string]bool{"cancelled": true, "recovered": false})
					}
					return nil
				}
			}
			if err = cmd.Context().Err(); err != nil {
				return err
			}
			reviewed, err := a.listings(nil).Load(p.ID)
			if err != nil {
				return err
			}
			if reviewed.Checksum != p.Checksum {
				return fmt.Errorf("listing plan changed during confirmation; review recovery again")
			}
			if err = cmd.Context().Err(); err != nil {
				return err
			}
			fmt.Fprintln(a.o.Err, "Connecting to Maker for read-only catalog checks...")
			api, err := a.client(cmd.Context(), true)
			if err != nil {
				return a.listingResult(p, err, "")
			}
			updated, err := a.runListing(cmd.Context(), api, "Reading catalog for explicitly confirmed creation recovery", func(m *listing.Manager) (listing.Plan, error) { return m.RecoverCreation(cmd.Context(), p.ID, true) })
			if updated.ID != "" {
				p = updated
			}
			return a.listingResult(p, err, "Success: uncertain creation recovery recorded; local plan retired. Prepare a new listing using your original --file; the retired plan cannot resume. No remote creation, upload, deletion, or submission was performed.")
		},
	}
}

// A cancelled terminal read must not hold the command open. The reader goroutine
// only sends an answer to its buffered channel; it never writes output or state.
// Closing the input, or process exit for stdin, releases an outstanding read.
func readRecoveryAnswer(ctx context.Context, reader *bufio.Reader) (string, error) {
	type answer struct {
		line string
		err  error
	}
	result := make(chan answer, 1)
	go func() {
		line, err := reader.ReadString('\n')
		result <- answer{line, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case value := <-result:
		return value.line, value.err
	}
}
