package connect

import (
	"context"
	"encoding/json"
	"errors"
)

// ProductMedia reads existing owned listing media; optional inspection is bounded
// to reviewed public CDN renditions, without forwarding Maker credentials.
func (c *Client) ProductMedia(ctx context.Context, org, product string, inspect bool) (json.RawMessage, error) {
	if !validID(org) || !validID(product) {
		return nil, errors.New("invalid media resource identifier")
	}
	v, err := c.api.ProductMedia(ctx, org, product, inspect)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
