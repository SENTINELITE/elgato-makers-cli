package connect

import (
	"github.com/SENTINELITE/elgato-makers-cli/internal/maker"
)

// NewWorkflowClient explicitly enables writes for authenticated workflow hosts.
// New continues to construct a read-only client.
func NewWorkflowClient(creds Credentials, cfg Config) (*Client, error) {
	api, err := maker.New(maker.Config{BaseURL: cfg.BaseURL,UploadTransport:cfg.UploadTransport, HTTPClient: cfg.HTTPClient, AllowLocalhost: cfg.AllowLocalhost, Cookies: creds.Cookies()})
	if err != nil {
		return nil, err
	}
	return &Client{api: api}, nil
}

// WorkflowBackend is reserved for the versioned workflows adapter. Hosts should
// use that adapter so writes retain the CLI managers' durable uncertainty guards.
func (c *Client) WorkflowBackend() *maker.Client { return c.api }
