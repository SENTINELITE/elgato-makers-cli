package maker

import (
	"context"
	"fmt"
)

// ReconcileListing confirms an uncertain details update from current remote
// evidence. It only reads Maker state and never retries the update action.
// Nil links preserve existing links, and unrelated tags are preserved by the
// update contract, so verification checks the requested metadata accordingly.
func (c *Client) ReconcileListing(ctx context.Context, input ListingDetailsInput) (bool, error) {
	if err := c.VerifyListing(ctx, input); err != nil {
		return false, fmt.Errorf("listing details remain unresolved: %w", err)
	}
	return true, nil
}
