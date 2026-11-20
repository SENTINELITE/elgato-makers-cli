package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"
)

type ciPolicyIntent struct {
	Operation   string `json:"operation_id"`
	Policy      string `json:"policy_id"`
	State       string `json:"state"`
	ApprovalURL string `json:"approval_url,omitempty"`
}
type ciPolicyRow struct {
	ID      string    `json:"policy_id"`
	Enabled bool      `json:"enabled"`
	Expires time.Time `json:"expires_at"`
	Config  struct {
		Repository    string `json:"repository"`
		Ref           string `json:"ref"`
		Workflow      string `json:"workflow_ref"`
		Environment   string `json:"environment,omitempty"`
		Product       string `json:"product_id"`
		Variant       string `json:"variant_id"`
		UUID          string `json:"uuid"`
		AutoPublish   bool   `json:"auto_publish"`
		PreflightOnly bool   `json:"preflight_only"`
	} `json:"config"`
}

func (a *app) ciCommands() *cobra.Command {
	root := &cobra.Command{Use: "ci", Short: "Manage hosted CI policies and run GitHub preflight or release"}
	root.AddCommand(a.mcpCICommand("preflight", "Validate a package under an approved GitHub OIDC policy", true), a.mcpCICommand("release", "Release a package under an approved GitHub OIDC policy", false))
	policy := &cobra.Command{Use: "policy", Short: "Set up GitHub permissions through HTTPS; no MCP client required"}
	for _, action := range []string{"create", "list", "status", "apply", "disable"} {
		action := action
		var server, file, id string
		var wait, open bool
		c := &cobra.Command{Use: action, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			origin, err := mcpOrigin(server)
			if err != nil {
				return err
			}
			unlock, err := a.lockMCP(origin)
			if err != nil {
				return err
			}
			defer unlock()
			if action == "list" {
				return a.ciPolicyList(cmd.Context(), origin)
			}
			if action != "create" && !deliveryID.MatchString(id) {
				return errors.New("use a valid policy or operation ID")
			}
			route := "/v1/ci/policy-operations/" + id
			method := http.MethodGet
			var body []byte
			if action == "create" {
				f, e := os.Open(file)
				if e != nil {
					return errors.New("cannot open CI policy JSON file")
				}
				defer f.Close()
				body, e = io.ReadAll(io.LimitReader(f, 8193))
				if e != nil || len(body) > 8192 || !json.Valid(body) {
					return errors.New("CI policy must be valid JSON of at most 8192 bytes")
				}
				route, method = "/v1/ci/policies", http.MethodPost
			}
			if action == "disable" {
				route, method = "/v1/ci/policies/"+id+"/disable", http.MethodPost
			}
			if action == "apply" {
				method = http.MethodPost
				body = []byte(`{"action":"execute"}`)
			}
			result, err := a.ciPolicyRequest(cmd.Context(), origin, method, route, body)
			if err != nil {
				return err
			}
			if (action == "status" || action == "apply") && result.Operation != id {
				return errors.New("CI policy operation changed identity")
			}
			if action == "disable" && result.Policy != id {
				return errors.New("CI policy response changed target")
			}
			if action == "create" || action == "disable" {
				if !validCIPolicyApproval(result, origin) {
					return errors.New("invalid CI policy approval URL or pending state; inspect the account CI settings")
				}
				fmt.Fprintf(a.o.Err, "Review this policy and confirm in the browser:\n%s\nOperation: %s\n", result.ApprovalURL, result.Operation)
				if open {
					if err := openMCPBrowser(result.ApprovalURL); err != nil {
						return fmt.Errorf("could not open browser; use the printed approval URL: %w", err)
					}
				}
				if wait {
					result, err = a.ciPolicyWait(cmd.Context(), origin, result)
					if err != nil {
						return err
					}
				}
			}
			message := fmt.Sprintf("CI policy request %s (operation %s).", result.State, result.Operation)
			if result.State == "succeeded" {
				switch {
				case action == "disable":
					message = "CI policy disabled."
				case action == "create":
					message = "CI policy enabled. Copy its ID at " + origin + "/ci."
				default:
					message = "CI policy change applied. View it at " + origin + "/ci."
				}
			}
			return a.mcpResult(result, message)
		}}
		c.Flags().StringVar(&server, "server", defaultMCPServer, "Hosted HTTPS origin")
		switch action {
		case "create":
			c.Short = "Prepare a policy from JSON for browser approval; does not enable it"
			c.Flags().StringVar(&file, "file", "", "CI policy JSON file")
			c.MarkFlagRequired("file")
		case "disable":
			c.Short = "Prepare disabling a policy for browser approval"
			c.Flags().StringVar(&id, "policy", "", "Policy ID")
			c.MarkFlagRequired("policy")
		case "status", "apply":
			c.Short = "Read policy operation status"
			if action == "apply" {
				c.Short = "Apply an already browser-approved policy operation"
			}
			c.Flags().StringVar(&id, "operation", "", "Existing CI policy operation ID")
			c.MarkFlagRequired("operation")
		case "list":
			c.Short = "List your connected account's CI policies"
		}
		if action == "create" || action == "disable" {
			c.Flags().BoolVar(&wait, "wait", false, "Wait up to 12 minutes, then apply only after browser confirmation")
			c.Flags().BoolVar(&open, "open", false, "Open the exact approval URL in your browser")
		}
		policy.AddCommand(c)
	}
	root.AddCommand(policy)
	return root
}
func validCIPolicyApproval(r ciPolicyIntent, origin string) bool {
	u, e := url.Parse(r.ApprovalURL)
	return e == nil && u.Scheme+"://"+u.Host == origin && u.User == nil && u.RawPath == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Path == "/workflows/approve/"+r.Operation && r.State == "pending_approval"
}
func (a *app) ciPolicyRequest(ctx context.Context, origin, method, route string, body []byte) (ciPolicyIntent, error) {
	var r ciPolicyIntent
	token, err := a.deliveryAuth(ctx, origin)
	if err != nil {
		return r, err
	}
	err = mcpRequest(ctx, mcpHTTPClient(), method, origin+route, "application/json", token, body, &r)
	if err != nil {
		return r, fmt.Errorf("CI policy request failed; do not repeat a create/apply blindly; check saved operations in CI settings (%w)", err)
	}
	if !deliveryID.MatchString(r.Operation) || !deliveryID.MatchString(r.Policy) {
		return r, errors.New("invalid CI policy operation response")
	}
	switch r.State {
	case "pending_approval", "first_confirmation", "approved", "running", "succeeded", "failed", "expired", "cancelled", "reconciliation_required":
	default:
		return r, errors.New("unknown CI policy state; inspect the saved operation")
	}
	return r, nil
}
func (a *app) ciPolicyWait(ctx context.Context, origin string, initial ciPolicyIntent) (ciPolicyIntent, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	applied := false
	for {
		result, err := a.ciPolicyRequest(ctx, origin, http.MethodGet, "/v1/ci/policy-operations/"+initial.Operation, nil)
		if err != nil {
			return result, fmt.Errorf("policy wait stopped for operation %s; resume with maker ci policy status --operation %s: %w", initial.Operation, initial.Operation, err)
		}
		if result.Operation != initial.Operation || result.Policy != initial.Policy {
			return result, errors.New("CI policy status changed identity")
		}
		switch result.State {
		case "approved":
			if !applied {
				applied = true
				result, err = a.ciPolicyRequest(ctx, origin, http.MethodPost, "/v1/ci/policy-operations/"+initial.Operation, []byte(`{"action":"execute"}`))
				if err != nil {
					return result, fmt.Errorf("inspect policy operation %s before another apply: %w", initial.Operation, err)
				}
				if result.Operation != initial.Operation || result.Policy != initial.Policy {
					return result, errors.New("CI policy apply changed identity")
				}
			}
		case "succeeded":
			return result, nil
		case "failed", "expired", "cancelled", "reconciliation_required":
			return result, fmt.Errorf("CI policy operation %s stopped in %s; no automatic retry", initial.Operation, result.State)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, fmt.Errorf("policy wait stopped; inspect operation %s; no approval was bypassed", initial.Operation)
		case <-timer.C:
		}
	}
}
func (a *app) ciPolicyList(ctx context.Context, origin string) error {
	token, err := a.hostedOAuthToken(ctx, origin, false)
	if err != nil {
		return err
	}
	var result struct {
		Policies []ciPolicyRow `json:"deployments"`
	}
	if err = mcpRequest(ctx, mcpHTTPClient(), http.MethodGet, origin+"/v1/ci/policies", "", token, nil, &result); err != nil {
		return err
	}
	for _, p := range result.Policies {
		if !deliveryID.MatchString(p.ID) {
			return errors.New("invalid hosted policy ID")
		}
	}
	if a.json {
		return a.output(result)
	}
	for _, p := range result.Policies {
		fmt.Fprintf(a.o.Out, "%s  %s  enabled=%t validation_only=%t\n", p.Config.Repository, p.Config.Ref, p.Enabled, p.Config.PreflightOnly)
	}
	a.completion = fmt.Sprintf("Hosted sign-in verified. %d CI policies for this Maker connection.", len(result.Policies))
	if len(result.Policies) > 0 {
		a.completion += " Copy policy IDs at " + origin + "/ci."
	}
	return nil
}
