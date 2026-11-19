package connect

import (
	"context"
	"errors"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

// ErrDuplicateVersion indicates that the target variant already has the requested version.
var ErrDuplicateVersion = model.ErrDuplicateVersion

// ErrVersionInReview indicates that a version on the target variant is in review.
var ErrVersionInReview = model.ErrVersionInReview

// ErrUnrecognizedVersionLifecycle indicates that a version's lifecycle prevents a safe decision.
var ErrUnrecognizedVersionLifecycle = model.ErrUnrecognizedVersionLifecycle

// CheckVersionMutation reads the versions for one exact product and variant and
// checks whether remote lifecycle state permits staging or submitting the requested version.
func (c *Client) CheckVersionMutation(ctx context.Context, org, productID, variantID, uuid, requestedVersion string) error {
	if !validID(org) || !validID(productID) || !validID(variantID) || !validID(uuid) {
		return errors.New("invalid resource identifier")
	}
	if !model.SameStreamDeckVersion(requestedVersion, requestedVersion) {
		return errors.New("invalid requested Stream Deck version")
	}
	target := model.Target{OrgID: org, ProductID: productID, VariantID: variantID, UUID: uuid}
	versions, err := c.api.Versions(ctx, target)
	if err != nil {
		return err
	}
	return model.CheckVersionMutation(versions, variantID, requestedVersion)
}
