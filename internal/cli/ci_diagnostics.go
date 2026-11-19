package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Server text is never printed. Translate only recognized codes into local
// guidance, retaining the existing protection against signed URL/token leaks.
var ciRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
var ciHelp = map[string]string{
	"INVALID_MARKDOWN":                           "Release notes use unsupported Markdown. Validate them locally with maker release prepare and correct the reported formatting.",
	"CI_REVIEW_PENDING":                          "An existing version is in review. Wait or use approved server delivery for an intended queued release.",
	"CI_RELEASE_ALREADY_QUEUED":                  "A release is already queued. Inspect it before replacing it with an intended server-mode release.",
	"CI_QUEUE_AUTHORIZATION_OR_ARTIFACT_EXPIRED": "The queued release lost policy authority or its artifact expired. Inspect the policy and package before creating a new intended release.",
	"CI_PREPARATION_FAILED":                      "Release preparation failed before dispatch. Inspect the saved operation and contact support with its identifier.",

	"CI_IDENTITY_INVALID":             "GitHub identity is missing, expired or invalid. Request a fresh OIDC token with the hosted service as audience.",
	"CI_IDENTITY_REPLAYED":            "This GitHub identity was already used. Request a fresh OIDC token; inspect any existing release before another submission.",
	"CI_OWNER_SIGN_IN_REQUIRED":       "Sign in as the owner using maker cloud login --write, or use the CI settings website. GitHub CI tokens cannot manage policies.",
	"CI_OWNER_WRITE_REQUIRED":         "Run maker cloud logout, then maker cloud login --write and grant write access.",
	"CI_TOKEN_REQUIRED":               "Obtain a scoped CI token from the approved GitHub workflow. Inspect any already accepted operation before submitting again.",
	"CI_TOKEN_EXPIRED":                "CI access expired. Obtain fresh authorization, then inspect the existing operation; do not resubmit it.",
	"CI_TOKEN_SCOPE":                  "This credential cannot perform that action. Use preflight authorization for validation or an explicitly approved release policy.",
	"CI_POLICY_EXPIRED":               "The deployment policy expired. Ask its owner to review and renew it.",
	"CI_POLICY_REVOKED":               "The deployment policy is disabled. Its owner must explicitly enable it before a new run.",
	"CI_POLICY_REF_MISMATCH":          "The GitHub branch does not match the approved deployment policy.",
	"CI_POLICY_WORKFLOW_MISMATCH":     "The GitHub workflow file/ref does not match the approved policy.",
	"CI_POLICY_ENVIRONMENT_MISMATCH":  "The job's GitHub environment does not match the approved policy.",
	"CI_AUTHORIZATION_DENIED":         "Check the policy ID, repository identity, enabled policy and linked Maker account with the owner.",
	"CI_CONNECTION_CHANGED":           "Reconnect the Maker account on the selected hosted service and review the deployment policy, then rerun preflight.",
	"CI_MAKER_AUTH_REQUIRED":          "The Maker account could not authenticate or renew. Reconnect it on the selected hosted service, then rerun preflight.",
	"CI_DUPLICATE_VERSION":            "This version already exists. Inspect it; use a new intended version for a future release.",
	"CI_REVIEW_STATUS_UNKNOWN":        "Marketplace returned an unrecognized review state. Share the request ID with support; no automatic submission is allowed.",
	"CI_REVIEW_UNAVAILABLE":           "Marketplace review status could not be read. Retry preflight and share the request ID if it persists.",
	"CI_PACKAGE_INVALID":              "Verify the package UUID and SHA-256, then upload it with this run's current token.",
	"CI_ARTIFACT_STORAGE_UNAVAILABLE": "The hosted private artifact store is unavailable. Retry preflight after recovery; share the request ID if it persists.",
	"CI_RATE_LIMITED":                 "Wait before retrying. Inspect any accepted release operation before starting another.",
	"CI_RELEASE_NOTES_REQUIRED":       "Provide nonempty release notes or omit --notes to use an approved default.",
	"CI_RELEASE_NOTES_INVALID":        "Correct the release notes; an invalid explicit file never falls back to the approved default.",
}

type ciRemoteDiagnostic struct{ Code, RequestID, Stage, Writes string }

func (d *ciRemoteDiagnostic) Error() string {
	out := d.Code + ": " + ciHelp[d.Code]
	if d.RequestID != "" {
		out += " Request ID: " + d.RequestID + "."
	}
	if d.Writes == "not_attempted" {
		out += " This request did not start an Elgato upload or submission."
	} else {
		out += " Existing release outcome may be unknown; inspect its saved operation before retrying."
	}
	return out
}
func parseCIRemoteDiagnostic(endpoint string, raw []byte) *ciRemoteDiagnostic {
	u, e := url.Parse(endpoint)
	if e != nil || !(strings.HasPrefix(u.Path, "/v1/ci/") || u.Path == "/v1/files") {
		return nil
	}
	var v struct {
		Code      string `json:"error_code"`
		RequestID string `json:"request_id"`
		Stage     string `json:"stage"`
		Writes    string `json:"provider_write_state"`
	}
	if json.Unmarshal(raw, &v) != nil || ciHelp[v.Code] == "" {
		return nil
	}
	d := &ciRemoteDiagnostic{Code: v.Code}
	if ciRequestID.MatchString(v.RequestID) {
		d.RequestID = v.RequestID
	}
	// A status-read failure never establishes whether the accepted operation wrote.
	if v.Writes == "not_attempted" && !strings.HasPrefix(u.Path, "/v1/ci/operations/") {
		d.Writes = v.Writes
	}
	return d
}
func ciOperationGuidance(operation, code string) string {
	if ciHelp[code] != "" {
		return fmt.Sprintf("%s Operation %s: %s Inspect its status before another submission.", code, operation, ciHelp[code])
	}
	return fmt.Sprintf("Operation %s needs inspection/reconciliation before another submission (code %s).", operation, code)
}
