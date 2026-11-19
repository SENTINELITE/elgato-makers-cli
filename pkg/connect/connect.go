// Package connect exposes the existing Maker session and read-only client for
// separately built services. It does not depend on an MCP implementation.
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/auth"
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

// ContractVersion is the semantic version of this adapter contract. Additive
// changes raise the minor number; any incompatible change to the exported API
// raises the major number. See docs/ADAPTER.md.
const ContractVersion = "1.2.0"

// ErrAuthentication identifies rejected Maker authentication or access.
var ErrAuthentication = maker.ErrAuthentication

type AnalyticsRangeError = maker.AnalyticsRangeError

type Credentials = auth.Credentials

func Parse(r io.Reader) (Credentials, error) { return auth.Parse(r) }

// Encode explicitly serializes sensitive credentials. Credentials itself has no
// JSON marshaler, so ordinary JSON logging cannot expose the cookie values.
func Encode(c Credentials) ([]byte, error) {
	type record struct {
		Name     string `json:"name"`
		Value    string `json:"value"`
		Path     string `json:"path"`
		Secure   bool   `json:"secure"`
		HTTPOnly bool   `json:"httpOnly,omitempty"`
		Expires  string `json:"expires,omitempty"`
	}
	rows := []record{}
	for _, v := range c.Cookies() {
		switch v.Name {
		case "__Host-kc_at", "kc_user", "__Secure-kc_rt":
		default:
			continue
		}
		r := record{Name: v.Name, Value: v.Value, Path: v.Path, Secure: true, HTTPOnly: v.HttpOnly}
		if !v.Expires.IsZero() {
			r.Expires = v.Expires.UTC().Format(time.RFC3339)
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return nil, errors.New("no recognized Maker credentials")
	}
	return json.Marshal(rows)
}

type Config struct {
	BaseURL        string
	UploadTransport http.RoundTripper
	HTTPClient     *http.Client
	AllowLocalhost bool
}
type Client struct{ api *maker.Client }

func New(creds Credentials, cfg Config) (*Client, error) {
	api, err := maker.New(maker.Config{BaseURL: cfg.BaseURL, HTTPClient: cfg.HTTPClient, AllowLocalhost: cfg.AllowLocalhost, Cookies: creds.Cookies(), ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return &Client{api: api}, nil
}
func (c *Client) Ensure(ctx context.Context) error  { _, err := c.api.EnsureSession(ctx); return err }
func (c *Client) Refresh(ctx context.Context) error { _, err := c.api.RefreshSession(ctx); return err }
func (c *Client) Credentials() (Credentials, error) { return auth.FromCookies(c.api.CookieSnapshot()) }

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,255}$`)

func validID(v string) bool { return idPattern.MatchString(v) && v != "." && v != ".." }
func (c *Client) Check(ctx context.Context, org string) error {
	if !validID(org) {
		return errors.New("invalid organization")
	}
	return c.api.CheckAuth(ctx, org)
}
func (c *Client) Products(ctx context.Context, org string) (json.RawMessage, error) {
	if !validID(org) {
		return nil, errors.New("invalid organization")
	}
	v, err := c.api.Products(ctx, org)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
func (c *Client) Versions(ctx context.Context, org, productID, variantID, uuid string) (json.RawMessage, error) {
	if !validID(org) || !validID(productID) || variantID != "" && !validID(variantID) || uuid != "" && !validID(uuid) {
		return nil, errors.New("invalid resource identifier")
	}
	v, err := c.api.Versions(ctx, model.Target{OrgID: org, ProductID: productID, VariantID: variantID, UUID: uuid})
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
func (c *Client) Analytics(ctx context.Context, org, start, end string) (json.RawMessage, error) {
	if !validID(org) {
		return nil, errors.New("invalid organization")
	}
	v, err := c.api.Analytics(ctx, org, start, end)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
