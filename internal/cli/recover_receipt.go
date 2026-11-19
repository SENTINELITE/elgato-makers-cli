package cli

import (
	"errors"
	"regexp"

	"github.com/SENTINELITE/elgato-makers-cli/internal/release"
	"github.com/spf13/cobra"
)

var receiptRecoveryID = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (a *app) recoverReceiptCommand() *cobra.Command {
	var receiptID, versionID string
	cmd := &cobra.Command{Use: "recover-receipt <release-id>", Short: "Bind an acknowledged submission receipt to a verified remote version without resubmitting", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("receipt-id") || !cmd.Flags().Changed("version-id") || !receiptRecoveryID.MatchString(receiptID) || !receiptRecoveryID.MatchString(versionID) {
			return errors.New("provide explicit valid --receipt-id and --version-id")
		}
		p, err := a.plans(nil).Load(args[0])
		if err != nil {
			return err
		}
		if p.State != release.SubmissionUnknown || p.SubmissionID == "" || p.SubmissionID != receiptID || (p.SubmissionReceiptID != "" && p.SubmissionReceiptID != receiptID) {
			return errors.New("receipt recovery requires an uncertain release with the exact stored submission identifier")
		}
		if !receiptRecoveryID.MatchString(p.Target.OrgID) || !receiptRecoveryID.MatchString(p.Target.ProductID) || !receiptRecoveryID.MatchString(p.Target.VariantID) || p.Target.UUID == "" {
			return errors.New("receipt recovery requires exact saved organization, product, variant, and UUID")
		}
		if receiptID == versionID {
			return errors.New("receipt recovery requires a distinct canonical version identifier")
		}
		api, err := a.client(cmd.Context(), true)
		if err != nil {
			return err
		}
		if err = verifyTarget(cmd.Context(), api, p.Target); err != nil {
			return err
		}
		p, err = a.plans(api).ReconcileAcknowledgedVersion(cmd.Context(), p.ID, receiptID, versionID)
		if err != nil {
			return err
		}
		if err = a.preview(p); err != nil {
			return err
		}
		a.completion = "Success: acknowledged receipt reconciled to the verified remote version. No upload or submission was performed."
		return nil
	}}
	cmd.Flags().StringVar(&receiptID, "receipt-id", "", "Exact acknowledged identifier already stored in the uncertain release")
	cmd.Flags().StringVar(&versionID, "version-id", "", "Canonical version identifier independently observed for this release")
	return cmd
}
