package connect

import (
	"context"
	"encoding/json"
	"errors"
)

// ProductDetails selects owned listing fields without forwarding raw provider state.
func (c *Client) ProductDetails(ctx context.Context, org, product string) (json.RawMessage, error) {
	if !validID(org) || !validID(product) {
		return nil, errors.New("invalid product details target")
	}
	value, err := c.api.ProductDetails(ctx, org, product)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
