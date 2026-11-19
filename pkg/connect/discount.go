package connect

import (
	"context"
	"encoding/json"
)

func (c *Client) Discounts(ctx context.Context, org, status string) (json.RawMessage, error) {
	v, e := c.api.Discounts(ctx, org, status)
	if e != nil {
		return nil, e
	}
	return json.Marshal(v)
}
func (c *Client) Discount(ctx context.Context, org, id string) (json.RawMessage, error) {
	v, e := c.api.Discount(ctx, org, id)
	if e != nil {
		return nil, e
	}
	return json.Marshal(v)
}
