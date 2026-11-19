package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/artifact"
	"github.com/spf13/cobra"
)

var mcpCICommitSHA = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var mcpCIErrorCode = regexp.MustCompile(`^(?:(?:CI|RELEASE|WORKFLOW)_[A-Z0-9_]{1,64}|SUBMISSION_RETAINED_DRAFT)$`)

type mcpCIGrant struct {
	Access string `json:"access_token"`
	Scope  string `json:"scope"`
	Policy string `json:"policy_id"`
	SHA    string `json:"sha"`
}

type mcpCIRequest struct {
	Artifact   string  `json:"package_artifact_id"`
	PackageSHA string  `json:"package_sha256,omitempty"`
	Notes      *string `json:"release_notes,omitempty"`
	UploadMode string  `json:"upload_mode,omitempty"`
}

type mcpCIPreflightResult struct {
	State          string `json:"state"`
	Policy         string `json:"policy_id"`
	SHA            string `json:"sha"`
	PackageSHA     string `json:"package_sha256"`
	PackageVersion string `json:"package_version"`
	ReviewState    string `json:"review_state"`
	ProviderWrites *bool  `json:"provider_writes"`
}

type mcpCIReleaseResult struct {
	State     string `json:"state,omitempty"`
	Policy    string `json:"policy_id,omitempty"`
	SHA       string `json:"sha,omitempty"`
	Workflow  string `json:"workflow_id,omitempty"`
	Operation string `json:"operation_id,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}

func mcpExchangeCI(ctx context.Context, origin, policy string, preflight bool) (mcpCIGrant, error) {
	var grant mcpCIGrant
	github, err := mcpGitHubToken(ctx, origin)
	if err != nil {
		return grant, err
	}
	route, scope := "/v1/ci/token", "ci:release"
	action := "release"
	if preflight {
		route, scope = "/v1/ci/preflight/token", "ci:preflight"
		action = "preflight"
	}
	body, _ := json.Marshal(map[string]string{"policy_id": policy, "github_token": github})
	err = mcpRequest(ctx, mcpHTTPClient(), http.MethodPost, origin+route, "application/json", "", body, &grant)
	if err != nil {
		return grant, fmt.Errorf("CI %s identity exchange failed (%w)", action, err)
	}
	if grant.Access == "" || grant.Scope != scope || grant.Policy != policy || !mcpCICommitSHA.MatchString(grant.SHA) {
		return grant, errors.New("invalid hosted CI authorization response")
	}
	return grant, nil
}

func (a *app) mcpCICommand(name, description string, preflight bool) *cobra.Command {
	var packagePath, notesPath, policy, server, uploadMode string
	cmd := &cobra.Command{Use: name, Short: description, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		origin, err := mcpOrigin(server)
		if err != nil {
			return err
		}
		if !deliveryID.MatchString(policy) {
			return errors.New("invalid CI policy identifier")
		}
		if !preflight && uploadMode != "server" && uploadMode != "direct" {
			return errors.New("choose server or direct CI upload mode")
		}
		file, err := mcpFileIdentity(packagePath)
		if err != nil {
			return err
		}
		pkg, err := artifact.Inspect(file.Path)
		if err != nil {
			return err
		}
		if pkg.SHA256 != file.SHA256 || pkg.Size != file.Size {
			return errors.New("local CI package changed during validation")
		}
		request := mcpCIRequest{PackageSHA: file.SHA256}
		if cmd.Flags().Changed("notes") {
			notes, err := artifact.ReadNotes(notesPath)
			if err != nil {
				return err
			}
			if notes.PlainTextLength == 0 {
				return errors.New("explicit CI release notes must contain text")
			}
			request.Notes = &notes.Markdown
		}
		ctx := cmd.Context()
		grant, err := mcpExchangeCI(ctx, origin, policy, preflight)
		if err != nil {
			return err
		}
		file, err = mcpUploadArtifact(ctx, origin, grant.Access, file, "package")
		if err != nil {
			return err
		}
		if err := verifyDeliveryFiles(map[string]mcpLocalArtifact{file.ID: file}); err != nil {
			return err
		}
		request.Artifact = file.ID
		if preflight {
			return a.mcpRunCIPreflight(ctx, origin, grant, request, pkg.Manifest.Version)
		}
		request.UploadMode = uploadMode
		if uploadMode == "direct" {
			// Older hosts accept the direct request without a separate hash field.
			// The artifact ID is already bound to the hash checked during upload.
			request.PackageSHA = ""
		}
		return a.mcpRunCIRelease(ctx, origin, grant, request, file)
	}}
	cmd.Flags().StringVar(&packagePath, "file", "", "Signed Stream Deck package")
	cmd.Flags().StringVar(&notesPath, "notes", "", "Release notes Markdown file (defaults to the approved policy notes)")
	cmd.Flags().StringVar(&policy, "policy", "", "Owner-approved GitHub release policy ID")
	cmd.Flags().StringVar(&server, "server", defaultMCPServer, "Hosted MCP origin")
	cmd.MarkFlagRequired("file")
	cmd.MarkFlagRequired("policy")
	if !preflight {
		cmd.Flags().StringVar(&uploadMode, "upload-mode", "server", "Server delivery supports the review queue. Direct delivery requires an available review slot.")
	}
	return cmd
}

func (a *app) mcpRunCIPreflight(ctx context.Context, origin string, grant mcpCIGrant, request mcpCIRequest, version string) error {
	body, _ := json.Marshal(request)
	var result mcpCIPreflightResult
	if err := mcpRequest(ctx, mcpHTTPClient(), http.MethodPost, origin+"/v1/ci/preflight", "application/json", grant.Access, body, &result); err != nil {
		return fmt.Errorf("CI preflight checks failed (%w)", err)
	}
	if result.ProviderWrites == nil || *result.ProviderWrites || result.Policy != grant.Policy || result.SHA != grant.SHA || result.PackageSHA != request.PackageSHA || result.PackageVersion != version ||
		!(result.State == "ready" && result.ReviewState == "clear" || result.State == "queued_for_review" && result.ReviewState == "in_review") {
		return errors.New("invalid hosted CI preflight result")
	}
	message := fmt.Sprintf("CI preflight passed for package %s. The review slot is available.", result.PackageVersion)
	if result.State == "queued_for_review" {
		message = fmt.Sprintf("CI preflight passed for package %s. A release would wait for the active review.", result.PackageVersion)
	}
	return a.mcpResult(result, message)
}

func (a *app) mcpRunCIRelease(ctx context.Context, origin string, grant mcpCIGrant, request mcpCIRequest, file mcpLocalArtifact) error {
	body, _ := json.Marshal(request)
	var result mcpCIReleaseResult
	if err := mcpRequest(ctx, mcpHTTPClient(), http.MethodPost, origin+"/v1/ci/releases", "application/json", grant.Access, body, &result); err != nil {
		var rejected *mcpHTTPError
		if errors.As(err, &rejected) && rejected.CIDiagnostic != nil && rejected.CIDiagnostic.Writes == "not_attempted" {
			return fmt.Errorf("CI release request rejected: %w", err)
		}
		return fmt.Errorf("CI release start requires reconciliation before retrying (%w)", err)
	}
	// An older host can return only the workflow and operation identifiers.
	if !result.matches(grant.Policy, grant.SHA) || (result.Operation == "" && result.State != "attention_required") ||
		(result.State == "queued" && (result.Policy == "" || result.SHA == "")) {
		return errors.New("invalid hosted CI release status; inspect the saved release before retrying")
	}
	result.Policy, result.SHA = grant.Policy, grant.SHA
	var err error
	switch result.State {
	case "queued":
		if request.UploadMode != "server" {
			err = errors.New("unexpected queue status for direct release; inspect the saved release")
		}
	case "", "running", "upload_pending", "uploading":
		if request.UploadMode == "direct" {
			if result.Workflow == "" {
				err = errors.New("missing direct CI workflow identifier; inspect the saved release")
				break
			}
			var state map[string]any
			state, err = mcpPollDelivery(ctx, origin, grant.Access, result.Operation, map[string]mcpLocalArtifact{file.ID: file}, true)
			if state != nil {
				result.State, _ = state["state"].(string)
				result.ErrorCode, _ = state["error_code"].(string)
			}
		} else {
			result, err = mcpPollCIServerRelease(ctx, origin, grant.Access, result)
		}
	default:
		err = mcpCIOutcome(result.State)
	}
	if !mcpCIErrorCode.MatchString(result.ErrorCode) {
		result.ErrorCode = ""
	}
	if !mcpCIStateKnown(result.State) {
		result.State = "attention_required"
	}
	if err == nil && result.State == "queued" {
		return a.mcpResult(result, fmt.Sprintf("CI release queued. The server will continue operation %s when it can proceed.", result.Operation))
	}
	message := "CI release state " + result.State
	if result.Operation != "" {
		message += " for operation " + result.Operation
	}
	if outputErr := a.mcpResult(result, message+"."); outputErr != nil {
		return outputErr
	}
	if err != nil && result.Operation != "" && result.ErrorCode != "" {
		return fmt.Errorf("%s: %w", ciOperationGuidance(result.Operation, result.ErrorCode), err)
	}
	return err
}

func (r mcpCIReleaseResult) matches(policy, sha string) bool {
	return (r.Policy == "" || r.Policy == policy) && (r.SHA == "" || r.SHA == sha) &&
		(r.Workflow == "" || deliveryID.MatchString(r.Workflow)) &&
		(r.Operation == "" || deliveryID.MatchString(r.Operation))
}

func mcpPollCIServerRelease(ctx context.Context, origin, token string, result mcpCIReleaseResult) (mcpCIReleaseResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	for {
		var status mcpCIReleaseResult
		err := mcpRequest(ctx, mcpHTTPClient(), http.MethodGet, origin+"/v1/ci/operations/"+result.Operation, "", token, nil, &status)
		if err != nil {
			return result, fmt.Errorf("CI release status unavailable; inspect the existing operation %s before retrying (%w)", result.Operation, err)
		}
		if !status.matches(result.Policy, result.SHA) || (status.Operation != "" && status.Operation != result.Operation) ||
			(result.Workflow != "" && status.Workflow != "" && status.Workflow != result.Workflow) {
			return result, errors.New("hosted CI release status changed identity; inspect the saved release")
		}
		result.State, result.ErrorCode = status.State, status.ErrorCode
		if status.Workflow != "" {
			result.Workflow = status.Workflow
		}
		switch status.State {
		case "running", "upload_pending", "uploading":
		default:
			return result, mcpCIOutcome(status.State)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, errors.New("CI release wait stopped; inspect the saved release")
		case <-timer.C:
		}
	}
}

func mcpCIOutcome(state string) error {
	switch state {
	case "queued", "succeeded", "completed":
		return nil
	case "failed", "cancelled", "reconciliation_required", "expired", "approved", "attention_required", "superseded":
		return errors.New("CI release stopped; inspect the saved release before retrying")
	default:
		return errors.New("unrecognized CI release state; inspect the saved release")
	}
}

func mcpCIStateKnown(state string) bool {
	switch state {
	case "queued", "succeeded", "completed", "running", "upload_pending", "uploading",
		"failed", "cancelled", "reconciliation_required", "expired", "approved", "attention_required", "superseded":
		return true
	default:
		return false
	}
}
