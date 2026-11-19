package connect

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

// BeginReadPhase scopes bounded page reuse to one catalog operation.
func (c *Client) BeginReadPhase(ctx context.Context) context.Context {
	return c.api.BeginReadPhase(ctx)
}
func (c *Client) CatalogMetadata(ctx context.Context, org string) (json.RawMessage, error) {
	if !validID(org) {
		return nil, errors.New("invalid organization")
	}
	v, err := c.api.CatalogMetadata(ctx, org)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// CatalogProduct anchors all variants, including variants without versions, on
// the same existing read-only product versions route used by Versions.
func (c *Client) CatalogProduct(ctx context.Context, org, product string) (json.RawMessage, error) {
	if !validID(org) || !validID(product) {
		return nil, errors.New("invalid resource identifier")
	}
	v, err := c.api.Product(ctx, model.Target{OrgID: org, ProductID: product})
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
