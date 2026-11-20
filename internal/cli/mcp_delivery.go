package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/artifact"
	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/SENTINELITE/elgato-makers-cli/internal/listing"
	"github.com/SENTINELITE/elgato-makers-cli/internal/media"
	"github.com/spf13/cobra"
)

type mcpLocalArtifact struct {
	ID     string `json:"artifact_id"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Path   string `json:"path"`
}
type mcpDeliveryMap struct {
	Server   string                      `json:"server"`
	Workflow string                      `json:"workflow_id"`
	Files    map[string]mcpLocalArtifact `json:"files"`
}

func (mcpLocalArtifact) String() string   { return "[private local artifact]" }
func (mcpLocalArtifact) GoString() string { return "[private local artifact]" }
func mcpFileIdentity(path string) (mcpLocalArtifact, error) {
	var out mcpLocalArtifact
	abs, e := filepath.Abs(path)
	if e != nil {
		return out, errors.New("invalid local artifact")
	}
	st, e := os.Lstat(abs)
	if e != nil || !st.Mode().IsRegular() {
		return out, errors.New("artifact must be a regular local file")
	}
	f, e := os.Open(abs)
	if e != nil {
		return out, errors.New("cannot read artifact")
	}
	defer f.Close()
	actual, e := f.Stat()
	if e != nil || !os.SameFile(st, actual) {
		return out, errors.New("artifact changed while opening")
	}
	h := sha256.New()
	n, e := io.Copy(h, f)
	if e != nil {
		return out, errors.New("cannot hash artifact")
	}
	return mcpLocalArtifact{Path: abs, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
func (a *app) deliveryPath(origin, workflow string) string {
	sum := sha256.Sum256([]byte(origin + "\x00" + workflow))
	return filepath.Join(a.mcpDir(), fmt.Sprintf("delivery-%x.json", sum))
}
func (a *app) saveDelivery(m mcpDeliveryMap) error {
	dir, e := a.privateMCPDir(true)
	if e != nil {
		return e
	}
	f, e := filesec.CreateTemp(dir, ".delivery-*")
	if e != nil {
		return errors.New("cannot save local delivery mapping")
	}
	defer os.Remove(f.Name())
	e = json.NewEncoder(f).Encode(m)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil {
		return errors.New("cannot save local delivery mapping")
	}
	if os.Rename(f.Name(), a.deliveryPath(m.Server, m.Workflow)) != nil {
		return errors.New("cannot save local delivery mapping")
	}
	return syncMCPDir(dir)
}
func (a *app) loadDelivery(origin, workflow string) (mcpDeliveryMap, error) {
	var out mcpDeliveryMap
	if _, e := a.privateMCPDir(false); e != nil {
		return out, e
	}
	path := a.deliveryPath(origin, workflow)
	st, e := os.Lstat(path)
	if e != nil || filesec.VerifyPrivateFile(path, st, false) != nil {
		return out, errors.New("private delivery mapping unavailable; prepare this workflow on this computer")
	}
	f, e := os.Open(path)
	if e != nil {
		return out, errors.New("cannot read delivery mapping")
	}
	defer f.Close()
	actual, e := f.Stat()
	if e != nil || !os.SameFile(st, actual) {
		return out, errors.New("delivery mapping changed")
	}
	b, e := io.ReadAll(io.LimitReader(f, 1<<20))
	if e != nil || json.Unmarshal(b, &out) != nil || out.Server != origin || out.Workflow != workflow || out.Files == nil {
		return out, errors.New("invalid delivery mapping")
	}
	return out, nil
}

// verifyDeliveryFiles fails before any hosted execution can create a product.
func verifyDeliveryFiles(files map[string]mcpLocalArtifact) error {
	if len(files) == 0 {
		return errors.New("local delivery mapping is empty")
	}
	for id, file := range files {
		digest, e := hex.DecodeString(file.SHA256)
		if !deliveryID.MatchString(id) || file.ID != id || e != nil || len(digest) != sha256.Size || file.Size <= 0 || !filepath.IsAbs(file.Path) {
			return errors.New("invalid local delivery mapping")
		}
		current, e := mcpFileIdentity(file.Path)
		if e != nil || current.SHA256 != file.SHA256 || current.Size != file.Size {
			return errors.New("a mapped local artifact changed; execution refused before hosted writes")
		}
	}
	return nil
}
func (a *app) deliveryAuth(ctx context.Context, origin string) (string, error) {
	return a.hostedOAuthToken(ctx, origin, true)
}
func (a *app) hostedOAuthToken(ctx context.Context, origin string, write bool) (string, error) {
	auth, e := a.loadMCP(origin)
	if e != nil {
		return "", e
	}
	if write && !mcpScopeContains(auth.Scope, "maker:write") {
		return "", errors.New("write consent required; run maker cloud logout, then maker cloud login --write")
	}
	if e = a.checkMCPRefreshGuard(origin); e != nil {
		return "", e
	}
	if !auth.ExpiresAt.After(a.o.Now().Add(90 * time.Second)) {
		c := mcpHTTPClient()
		m, e := discoverMCP(ctx, c, origin)
		if e != nil {
			return "", e
		}
		if e = a.refreshMCP(ctx, c, m, &auth); e != nil {
			return "", e
		}
	}
	if write && !mcpScopeContains(auth.Scope, "maker:write") {
		return "", errors.New("refreshed grant no longer authorizes writes; inspect operation status")
	}
	return auth.AccessToken, nil
}

// errMCPUnknownTool keeps the generic rejection text but lets callers detect a
// server that predates a tool.
var errMCPUnknownTool = errors.New("hosted workflow rejected request; inspect workflow status in your account")

// mcpDeliveryTool accepts only sanitized structured content; raw tool text and
// upstream errors are deliberately excluded from terminal output.
func mcpDeliveryTool(ctx context.Context, origin, token, name string, args any) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	var reply struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			IsError    bool            `json:"isError"`
			Structured json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if e := mcpRequest(ctx, mcpHTTPClient(), "POST", origin+"/mcp", "application/json", token, body, &reply); e != nil {
		return nil, e
	}
	var rpc struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if len(reply.Error) > 0 && json.Unmarshal(reply.Error, &rpc) == nil && rpc.Code == -32602 && strings.HasPrefix(rpc.Message, "unknown tool") {
		return nil, errMCPUnknownTool
	}
	if reply.Result.IsError || len(reply.Error) > 0 && string(reply.Error) != "null" {
		return nil, errors.New("hosted workflow rejected request; inspect workflow status in your account")
	}
	var data map[string]any
	if json.Unmarshal(reply.Result.Structured, &data) != nil || data == nil {
		return nil, errors.New("invalid hosted workflow response")
	}
	if nested, ok := data["data"].(map[string]any); ok {
		return nested, nil
	}
	return data, nil
}

// mcpRequestApproval asks the hosted service for one pending owner approval of
// the explicit action on a saved plan; it executes nothing. upload_mode applies
// only to stage, submit and draft create, so create first reads the saved plan
// kind that get_workflow reports.
func mcpRequestApproval(ctx context.Context, origin, token, workflowID, action, mode string) (map[string]any, error) {
	args := map[string]any{"workflow_id": workflowID, "action": action}
	switch action {
	case "stage", "submit":
		args["upload_mode"] = mode
	case "create":
		kind, e := mcpWorkflowKind(ctx, origin, token, workflowID)
		if e != nil {
			return nil, e
		}
		if kind == "draft" {
			args["upload_mode"] = mode
		}
	}
	return mcpDeliveryTool(ctx, origin, token, "request_approval", args)
}

// mcpWorkflowKind reads the saved plan kind reported by get_workflow.
func mcpWorkflowKind(ctx context.Context, origin, token, workflowID string) (string, error) {
	plan, e := mcpDeliveryTool(ctx, origin, token, "get_workflow", map[string]any{"workflow_id": workflowID})
	if e != nil {
		return "", e
	}
	kind, _ := plan["kind"].(string)
	switch kind {
	case "listing", "release", "draft", "deletion", "discount", "discount_deactivation", "ci_deployment":
		return kind, nil
	}
	return "", errors.New("invalid hosted workflow response")
}
func mcpUploadFile(ctx context.Context, origin, token, path, kind string) (mcpLocalArtifact, error) {
	identity, e := mcpFileIdentity(path)
	if e != nil {
		return identity, e
	}
	switch kind {
	case "package":
		_, e = artifact.Inspect(path)
	case "icon", "thumbnail", "gallery":
		_, e = media.Inspect(path, kind)
	default:
		e = errors.New("kind must be package, icon, thumbnail or gallery")
	}
	if e != nil {
		return identity, e
	}
	return mcpUploadArtifact(ctx, origin, token, identity, kind)
}

func mcpUploadArtifact(ctx context.Context, origin, token string, identity mcpLocalArtifact, kind string) (mcpLocalArtifact, error) {
	path := identity.Path
	// A private temporary multipart spool keeps packages out of memory and makes
	// field ordering explicit. It never contains credentials or signed URLs.
	spool, e := os.CreateTemp("", "maker-upload-*")
	if e != nil {
		return identity, errors.New("cannot prepare upload")
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	writer := multipart.NewWriter(spool)
	if e = writer.WriteField("kind", kind); e != nil {
		return identity, errors.New("cannot encode upload")
	}
	part, e := writer.CreateFormFile("file", filepath.Base(path))
	if e != nil {
		return identity, errors.New("cannot encode upload")
	}
	f, e := os.Open(path)
	if e != nil {
		return identity, errors.New("cannot open upload")
	}
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(part, h), f)
	f.Close()
	if e != nil || n != identity.Size || hex.EncodeToString(h.Sum(nil)) != identity.SHA256 {
		return identity, errors.New("artifact changed before upload")
	}
	if writer.Close() != nil {
		return identity, errors.New("cannot encode upload")
	}
	length, e := spool.Seek(0, io.SeekCurrent)
	if e != nil {
		return identity, errors.New("cannot prepare upload")
	}
	spool.Seek(0, io.SeekStart)
	req, e := http.NewRequestWithContext(ctx, "POST", origin+"/v1/files", spool)
	if e != nil {
		return identity, errors.New("invalid upload request")
	}
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	client := mcpHTTPClient()
	client.Timeout = 10 * time.Minute
	resp, e := client.Do(req)
	if e != nil {
		return identity, errors.New("artifact upload failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return identity, fmt.Errorf("artifact upload rejected (HTTP %d)", resp.StatusCode)
	}
	var result struct {
		ID     string `json:"artifact_id"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result) != nil || !deliveryID.MatchString(result.ID) || result.SHA256 != identity.SHA256 || result.Size != identity.Size {
		return identity, errors.New("uploaded artifact identity mismatch")
	}
	identity.ID = result.ID
	return identity, nil
}

var deliveryMediaFile = regexp.MustCompile(`^[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}\.(png|jpg|jpeg|mp4)$`)
var deliveryMediaPathID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Hosted opaque IDs use unpadded base64url and may start with '-' or '_'.
var deliveryID = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)

type mcpUploadLease struct {
	ID          string    `json:"upload_id"`
	Artifact    string    `json:"artifact_id"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	URL         string    `json:"url"`
	Expires     time.Time `json:"expires_at"`
}

func (mcpUploadLease) String() string   { return "[confidential upload lease]" }
func (mcpUploadLease) GoString() string { return "[confidential upload lease]" }
func validDeliveryURL(raw, origin string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" {
		return false
	}
	if u.Scheme == "https" && u.Host == "s3.us-east-1.amazonaws.com" && strings.HasPrefix(u.Path, "/production.maker.elgato.com/") {
		tail := strings.TrimPrefix(u.Path, "/production.maker.elgato.com/")
		if strings.Contains(tail, "..") || strings.ContainsAny(tail, "\\\x00") {
			return false
		}
		if strings.HasPrefix(tail, "media/") {
			parts := strings.Split(strings.TrimPrefix(tail, "media/"), "/")
			if len(parts) != 3 && len(parts) != 4 {
				return false
			}
			for _, part := range parts[:len(parts)-1] {
				if !deliveryMediaPathID.MatchString(part) {
					return false
				}
			}
			return deliveryMediaFile.MatchString(parts[len(parts)-1])
		}
		return !strings.Contains(tail, "/") && strings.HasSuffix(tail, ".streamDeckPlugin") && regexp.MustCompile(`^[A-Za-z0-9_-]+-[0-9]+\.streamDeckPlugin$`).MatchString(tail)
	}
	host, _ := url.Parse(origin)
	ip := net.ParseIP(host.Hostname())
	local := ip != nil && ip.IsLoopback()
	targetIP := net.ParseIP(u.Hostname())
	return local && targetIP != nil && targetIP.IsLoopback() && (u.Scheme == "http" || u.Scheme == "https")
}
func mcpPutLease(ctx context.Context, origin string, lease mcpUploadLease, file mcpLocalArtifact) (int, error) {
	if !deliveryID.MatchString(lease.ID) || lease.Artifact != file.ID || lease.SHA256 != file.SHA256 || lease.Size != file.Size || !lease.Expires.After(time.Now()) || !validDeliveryURL(lease.URL, origin) {
		return 0, errors.New("invalid or expired upload lease")
	}
	current, e := mcpFileIdentity(file.Path)
	if e != nil || current.SHA256 != file.SHA256 || current.Size != file.Size {
		return 0, errors.New("local artifact changed; upload refused")
	}
	// Rehash the exact opened descriptor, then seek and stream that same descriptor.
	f, e := os.Open(file.Path)
	if e != nil {
		return 0, errors.New("cannot read delivery artifact")
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, f)
	if e != nil || n != file.Size || hex.EncodeToString(h.Sum(nil)) != file.SHA256 {
		return 0, errors.New("artifact changed before delivery")
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		return 0, errors.New("cannot read delivery artifact")
	}
	req, e := http.NewRequestWithContext(ctx, "PUT", lease.URL, f)
	if e != nil {
		return 0, errors.New("invalid delivery request")
	}
	req.ContentLength = file.Size
	if lease.ContentType != "" {
		req.Header.Set("Content-Type", lease.ContentType)
	}
	client := &http.Client{Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, e := client.Do(req)
	if e != nil {
		return 0, errors.New("direct upload failed; reconcile operation before retrying")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, errors.New("direct upload rejected; reconcile operation before retrying")
	}
	return resp.StatusCode, nil
}
func mcpNextLease(ctx context.Context, origin, token, operation string) (*mcpUploadLease, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", origin+"/v1/operations/"+operation+"/uploads/next", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, e := mcpHTTPClient().Do(req)
	if e != nil {
		return nil, errors.New("cannot read pending upload; reconcile operation")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		return nil, errors.New("pending upload rejected; reconcile operation")
	}
	var lease mcpUploadLease
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&lease) != nil {
		return nil, errors.New("invalid pending upload response")
	}
	return &lease, nil
}
func mcpPollDelivery(ctx context.Context, origin, token, operation string, files map[string]mcpLocalArtifact, ci bool) (map[string]any, error) {
	return mcpPollDeliveryGuarded(ctx, origin, token, operation, files, ci, nil)
}
func (a *app) guardDeliveryAttempt(origin, operation, upload string) error {
	dir, e := a.privateMCPDir(true)
	if e != nil {
		return e
	}
	sum := sha256.Sum256([]byte(origin + "\x00" + operation + "\x00" + upload))
	f, e := filesec.OpenPrivateFile(filepath.Join(dir, fmt.Sprintf("attempt-%x", sum)), os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if e != nil {
		return errors.New("direct upload previously attempted; reconcile operation without repeating PUT")
	}
	e = f.Sync()
	ce := f.Close()
	if e != nil || ce != nil {
		return errors.New("cannot persist direct upload attempt guard")
	}
	return syncMCPDir(dir)
}
func mcpPollDeliveryGuarded(ctx context.Context, origin, token, operation string, files map[string]mcpLocalArtifact, ci bool, guard func(string) error) (map[string]any, error) {
	return mcpPollDeliveryAuthorized(ctx, origin, token, operation, files, ci, guard, nil)
}

// Interactive delivery refreshes only the existing locked saved grant. CI uses
// its fixed short-lived policy token and never enters the OAuth refresh path.
func (a *app) liveDeliveryAuth(origin, clientID string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		saved, e := a.loadMCP(origin)
		if e != nil {
			return "", e
		}
		if saved.ClientID != clientID {
			return "", errors.New("saved OAuth client changed; reconcile operation before continuing")
		}
		return a.deliveryAuth(ctx, origin)
	}
}
func mcpPollDeliveryAuthorized(ctx context.Context, origin, token, operation string, files map[string]mcpLocalArtifact, ci bool, guard func(string) error, authorize func(context.Context) (string, error)) (map[string]any, error) {
	if !deliveryID.MatchString(operation) {
		return nil, errors.New("invalid operation identifier")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	seen := map[string]bool{}
	for {
		var state map[string]any
		var e error
		if !ci && authorize != nil {
			token, e = authorize(ctx)
			if e != nil {
				return nil, e
			}
		}
		if ci {
			e = mcpRequest(ctx, mcpHTTPClient(), "GET", origin+"/v1/ci/operations/"+operation, "", token, nil, &state)
		} else {
			state, e = mcpDeliveryTool(ctx, origin, token, "get_workflow", map[string]any{"operation_id": operation})
		}
		if e != nil {
			return nil, e
		}
		value, _ := state["state"].(string)
		uploadMode, _ := state["upload_mode"].(string)
		safe := map[string]any{"operation_id": operation, "state": value}
		if code, ok := state["error_code"].(string); ok {
			safe["error_code"] = code
		}
		switch value {
		case "succeeded", "completed":
			return safe, nil
		case "running", "queued", "upload_pending", "uploading":
		case "failed", "cancelled", "reconciliation_required", "expired":
			return safe, errors.New("operation stopped; reconcile its saved status before any retry")
		default:
			return safe, errors.New("operation requires attention; execution stopped")
		}
		if !ci && authorize != nil {
			token, e = authorize(ctx)
			if e != nil {
				return safe, e
			}
		}
		// Upload leases exist only for direct delivery; server-mode operations
		// report no pending uploads and the lease route rejects them.
		if !ci && uploadMode == "server" {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return safe, errors.New("operation wait cancelled or timed out; inspect saved status")
			case <-timer.C:
			}
			continue
		}
		lease, e := mcpNextLease(ctx, origin, token, operation)
		if e != nil {
			// The operation can finish after the status read but before this poll.
			// Confirm completion through the authenticated status route; never retry a PUT.
			var final map[string]any
			var statusErr error
			if ci {
				statusErr = mcpRequest(ctx, mcpHTTPClient(), "GET", origin+"/v1/ci/operations/"+operation, "", token, nil, &final)
			} else {
				final, statusErr = mcpDeliveryTool(ctx, origin, token, "get_workflow", map[string]any{"operation_id": operation})
			}
			if statusErr == nil && (final["state"] == "succeeded" || final["state"] == "completed") {
				return map[string]any{"operation_id": operation, "state": final["state"]}, nil
			}
			return safe, e
		}
		if lease != nil {
			if seen[lease.ID] {
				return safe, errors.New("upload lease already attempted; reconcile operation")
			}
			seen[lease.ID] = true
			file, ok := files[lease.Artifact]
			status := 0
			var putErr error
			if !ok {
				putErr = errors.New("no exact local artifact mapping for upload lease")
			} else {
				if guard != nil {
					if e := guard(lease.ID); e != nil {
						return safe, e
					}
				}
				if putErr == nil {
					status, putErr = mcpPutLease(ctx, origin, *lease, file)
				}
			}
			if !deliveryID.MatchString(lease.ID) {
				return safe, errors.New("invalid upload lease identifier")
			}
			body, _ := json.Marshal(map[string]any{"success": putErr == nil, "status": status})
			if !ci && authorize != nil {
				token, e = authorize(ctx)
				if e != nil {
					return safe, errors.New("upload acknowledgment authorization unavailable; reconcile without repeating PUT")
				}
			}
			ackErr := mcpRequest(ctx, mcpHTTPClient(), "POST", origin+"/v1/operations/"+operation+"/uploads/"+lease.ID+"/complete", "application/json", token, body, nil)
			if putErr != nil {
				return safe, putErr
			}
			if ackErr != nil {
				return safe, errors.New("upload acknowledgment uncertain; reconcile operation without repeating PUT")
			}
			continue
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return safe, errors.New("operation wait cancelled or timed out; inspect saved status")
		case <-timer.C:
		}
	}
}
func mcpGitHubToken(ctx context.Context, origin string) (string, error) {
	raw := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
	secret := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Port() != "" || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".actions.githubusercontent.com") || secret == "" {
		return "", errors.New("Run CI preflight from the approved GitHub Actions workflow with permissions: id-token: write; a local terminal does not provide GitHub OIDC identity")
	}
	q := u.Query()
	q.Set("audience", origin)
	u.RawQuery = q.Encode()
	var token struct {
		Value string `json:"value"`
	}
	if e = mcpRequest(ctx, mcpHTTPClient(), "GET", u.String(), "", secret, nil, &token); e != nil || token.Value == "" {
		return "", errors.New("GitHub could not issue an OIDC identity. Check job permissions: id-token: write, then start a fresh workflow run")
	}
	return token.Value, nil
}
func (a *app) mcpPrepareLocalListing(ctx context.Context, origin, token, path string) (map[string]any, error) {
	config, e := listing.LoadConfig(path)
	if e != nil {
		return nil, e
	}
	p, e := listing.PrepareConfig(config)
	if e != nil {
		return nil, e
	}
	files := map[string]mcpLocalArtifact{}
	byPath := map[string]mcpLocalArtifact{}
	upload := func(path, kind string) (string, error) {
		abs, e := filepath.Abs(path)
		if e != nil {
			return "", errors.New("invalid local artifact path")
		}
		if existing, ok := byPath[abs]; ok {
			if e := verifyDeliveryFiles(map[string]mcpLocalArtifact{existing.ID: existing}); e != nil {
				return "", e
			}
			return existing.ID, nil
		}
		f, e := mcpUploadFile(ctx, origin, token, path, kind)
		if e == nil {
			files[f.ID] = f
			byPath[abs] = f
		}
		return f.ID, e
	}
	pkg, e := upload(config.PackagePath, "package")
	if e != nil {
		return nil, e
	}
	icon, e := upload(config.Assets.Icon, "icon")
	if e != nil {
		return nil, e
	}
	thumb, e := upload(config.Assets.Thumbnail, "thumbnail")
	if e != nil {
		return nil, e
	}
	gallery := []string{}
	for _, path := range config.Assets.Gallery {
		id, e := upload(path, "gallery")
		if e != nil {
			return nil, e
		}
		gallery = append(gallery, id)
	}
	input := map[string]any{"schema_version": 1, "name": config.Name, "package": pkg, "description": p.Description.Markdown, "release_notes": p.Notes.Markdown, "pricing": config.Pricing, "languages": config.Languages, "categories": config.Categories, "links": config.Links, "assets": map[string]any{"icon": icon, "thumbnail": thumb, "gallery": gallery}, "auto_publish": config.AutoPublish, "gallery_upload_mode": "sequential"}
	if config.ProductID != "" {
		input["product_id"] = config.ProductID
	}
	if config.VariantID != "" {
		input["variant_id"] = config.VariantID
	}
	if e = verifyDeliveryFiles(files); e != nil {
		return nil, e
	}
	result, e := mcpDeliveryTool(ctx, origin, token, "prepare_listing", map[string]any{"config": input})
	if e != nil {
		return nil, e
	}
	id, _ := result["workflow_id"].(string)
	if !deliveryID.MatchString(id) {
		return nil, errors.New("server returned invalid workflow identifier")
	}
	if e = a.saveDelivery(mcpDeliveryMap{Server: origin, Workflow: id, Files: files}); e != nil {
		return nil, e
	}
	return map[string]any{"workflow_id": id, "state": "prepared", "artifact_count": len(files)}, nil
}
func (a *app) addMCPDeliveryCommands(root *cobra.Command) {
	upload := &cobra.Command{Use: "upload", Short: "Upload an exact local artifact to your authenticated hosted account", Args: cobra.NoArgs}
	var uploadFile, kind, uploadServer string
	upload.Flags().StringVar(&uploadFile, "file", "", "Local package, image or video")
	upload.Flags().StringVar(&kind, "kind", "", "package, icon, thumbnail or gallery")
	upload.Flags().StringVar(&uploadServer, "server", defaultMCPServer, "Hosted MCP origin")
	upload.MarkFlagRequired("file")
	upload.MarkFlagRequired("kind")
	upload.RunE = func(cmd *cobra.Command, _ []string) error {
		origin, e := mcpOrigin(uploadServer)
		if e != nil {
			return e
		}
		unlock, e := a.lockMCP(origin)
		if e != nil {
			return e
		}
		defer unlock()
		token, e := a.deliveryAuth(cmd.Context(), origin)
		if e != nil {
			return e
		}
		f, e := mcpUploadFile(cmd.Context(), origin, token, uploadFile, kind)
		if e != nil {
			return e
		}
		m, e := a.loadDelivery(origin, "")
		if e != nil {
			if _, statErr := os.Lstat(a.deliveryPath(origin, "")); !os.IsNotExist(statErr) {
				return e
			}
			m = mcpDeliveryMap{Server: origin, Files: map[string]mcpLocalArtifact{}}
		}
		m.Files[f.ID] = f
		if e = a.saveDelivery(m); e != nil {
			return e
		}
		return a.mcpResult(map[string]any{"artifact_id": f.ID, "sha256": f.SHA256, "size": f.Size}, "Artifact uploaded: "+f.ID)
	}
	listingCmd := &cobra.Command{Use: "listing", Short: "Prepare hosted listings from validated local files"}
	var listingFile, listingServer string
	prepare := &cobra.Command{Use: "prepare", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		origin, e := mcpOrigin(listingServer)
		if e != nil {
			return e
		}
		unlock, e := a.lockMCP(origin)
		if e != nil {
			return e
		}
		defer unlock()
		token, e := a.deliveryAuth(cmd.Context(), origin)
		if e != nil {
			return e
		}
		r, e := a.mcpPrepareLocalListing(cmd.Context(), origin, token, listingFile)
		if e != nil {
			return e
		}
		return a.mcpResult(r, "Listing prepared: "+r["workflow_id"].(string))
	}}
	prepare.Flags().StringVar(&listingFile, "file", "", "CLI listing JSON (validated offline before uploads)")
	prepare.Flags().StringVar(&listingServer, "server", defaultMCPServer, "Hosted MCP origin")
	prepare.MarkFlagRequired("file")
	listingCmd.AddCommand(prepare)
	action := &cobra.Command{Use: "action", Short: "Request approval and execute hosted workflow actions"}
	var workflowID, requestedAction, mode, actionServer string
	var noPrompt bool
	request := &cobra.Command{Use: "request", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !deliveryID.MatchString(workflowID) || !oneDeliveryAction(requestedAction) || (mode != "direct" && mode != "server") {
			return errors.New("invalid workflow, action or upload mode")
		}
		origin, e := mcpOrigin(actionServer)
		if e != nil {
			return e
		}
		unlock, e := a.lockMCP(origin)
		if e != nil {
			return e
		}
		defer unlock()
		token, e := a.deliveryAuth(cmd.Context(), origin)
		if e != nil {
			return e
		}
		r, e := mcpRequestApproval(cmd.Context(), origin, token, workflowID, requestedAction, mode)
		if e != nil {
			return e
		}
		return a.mcpApprovalFlow(cmd.Context(), origin, token, workflowID, r, noPrompt)
	}}
	request.Flags().StringVar(&workflowID, "workflow", "", "Saved workflow identifier")
	request.Flags().StringVar(&requestedAction, "action", "", "Explicit workflow action")
	request.Flags().StringVar(&mode, "upload-mode", "direct", "direct or server")
	request.Flags().StringVar(&actionServer, "server", defaultMCPServer, "Hosted MCP origin")
	request.Flags().BoolVar(&noPrompt, "no-prompt", false, "Leave the approval pending at its URL instead of asking to confirm")
	request.MarkFlagRequired("workflow")
	request.MarkFlagRequired("action")
	var executeWorkflow, operation, executeServer string
	execute := &cobra.Command{Use: "execute", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !deliveryID.MatchString(executeWorkflow) || !deliveryID.MatchString(operation) {
			return errors.New("invalid workflow or operation identifier")
		}
		origin, e := mcpOrigin(executeServer)
		if e != nil {
			return e
		}
		unlock, e := a.lockMCP(origin)
		if e != nil {
			return e
		}
		defer unlock()
		token, e := a.deliveryAuth(cmd.Context(), origin)
		if e != nil {
			return e
		}
		return a.executeApprovedOperation(cmd.Context(), origin, token, executeWorkflow, operation)
	}}
	execute.Flags().StringVar(&executeWorkflow, "workflow", "", "Workflow prepared on this computer")
	execute.Flags().StringVar(&operation, "operation", "", "Owner-approved operation identifier")
	execute.Flags().StringVar(&executeServer, "server", defaultMCPServer, "Hosted MCP origin")
	execute.MarkFlagRequired("workflow")
	execute.MarkFlagRequired("operation")
	action.AddCommand(request, execute)
	root.AddCommand(upload, listingCmd, action, a.ciCommands())
}
func oneDeliveryAction(s string) bool {
	switch s {
	case "stage", "submit", "create", "delete", "deactivate", "discard", "retire":
		return true
	}
	return false
}
func safeApprovalURL(r map[string]any, origin string) string {
	raw, _ := r["approval_url"].(string)
	u, e := url.Parse(raw)
	if e != nil || u.Scheme+"://"+u.Host != origin || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/workflows/approve/") {
		return "See the hosted account page."
	}
	return raw
}

// mcpApprovalFlow presents the pending approval and, on a terminal, asks once
// whether to confirm it now. The confirmation token stays in memory and is
// never printed. Confirmation auto-starts server-mode runs; direct mode then
// continues through the file-lease execute path.
func (a *app) mcpApprovalFlow(ctx context.Context, origin, token, workflowID string, r map[string]any, noPrompt bool) error {
	operationID, _ := r["operation_id"].(string)
	confirmToken, _ := r["confirmation_token"].(string)
	autoStart, _ := r["auto_start"].(bool)
	summary, _ := r["summary"].(string)
	if !deliveryID.MatchString(operationID) {
		return errors.New("invalid hosted workflow response")
	}
	if summary == "" {
		summary = "Owner approval requested for this operation."
	}
	next := "After confirmation run maker mcp action execute --workflow " + workflowID + " --operation " + operationID + " to upload the mapped local files and start."
	if autoStart {
		next = "Confirmation starts the run; track it with maker mcp action execute --workflow " + workflowID + " --operation " + operationID + "."
	}
	pending := summary + ". Open " + safeApprovalURL(r, origin) + " to confirm; the approval expires at " + fmt.Sprint(r["expires_at"]) + ". " + next
	if confirmToken == "" || noPrompt || !a.o.IsTerminal() {
		return a.mcpResult(r, pending)
	}
	fmt.Fprint(a.o.Err, summary+". Confirm this operation now? [y/N] ")
	answer, e := bufio.NewReader(a.o.In).ReadString('\n')
	if e != nil || (strings.ToLower(strings.TrimSpace(answer)) != "y" && strings.ToLower(strings.TrimSpace(answer)) != "yes") {
		return a.mcpResult(r, pending)
	}
	if _, e = mcpDeliveryTool(ctx, origin, token, "confirm_operation", map[string]any{"operation_id": operationID, "confirmation_token": confirmToken}); e != nil {
		return e
	}
	if autoStart {
		return a.finishDeliveryPoll(ctx, origin, token, operationID, nil)
	}
	return a.executeApprovedOperation(ctx, origin, token, workflowID, operationID)
}

// executeApprovedOperation starts an approved operation and polls it to a
// terminal state. Direct upload mode requires the private local mapping saved
// by listing prepare on this computer; server mode needs no local files.
func (a *app) executeApprovedOperation(ctx context.Context, origin, token, workflowID, operationID string) error {
	m, mapErr := a.loadDelivery(origin, workflowID)
	if mapErr == nil {
		if e := verifyDeliveryFiles(m.Files); e != nil {
			return e
		}
	}
	report, e := mcpDeliveryTool(ctx, origin, token, "get_workflow", map[string]any{"operation_id": operationID})
	if e != nil {
		return e
	}
	var files map[string]mcpLocalArtifact
	if report["upload_mode"] == "direct" {
		if mapErr != nil {
			return mapErr
		}
		files = m.Files
	}
	if e = mcpExecuteWorkflow(ctx, origin, token, operationID, report); e != nil {
		return e
	}
	return a.finishDeliveryPoll(ctx, origin, token, operationID, files)
}

// mcpOperationInFlight reports states where the run already started or ended;
// confirmation can auto-start it before execute_workflow is called.
func mcpOperationInFlight(report map[string]any) bool {
	switch report["state"] {
	case "running", "queued", "upload_pending", "uploading", "succeeded", "completed":
		return true
	}
	return false
}

// mcpExecuteWorkflow starts an approved operation. An operation that
// confirmation already auto-started is in flight, so execute_workflow is only
// called while the report still shows an unstarted state; a rejection is
// tolerated when a fresh read shows the run already started or finished.
func mcpExecuteWorkflow(ctx context.Context, origin, token, operationID string, report map[string]any) error {
	if mcpOperationInFlight(report) {
		return nil
	}
	_, e := mcpDeliveryTool(ctx, origin, token, "execute_workflow", map[string]any{"operation_id": operationID})
	if e == nil {
		return nil
	}
	fresh, serr := mcpDeliveryTool(ctx, origin, token, "get_workflow", map[string]any{"operation_id": operationID})
	if serr != nil || !mcpOperationInFlight(fresh) {
		return e
	}
	return nil
}

// finishDeliveryPoll refreshes the saved grant while polling and prints the
// terminal operation state before returning the poll outcome.
func (a *app) finishDeliveryPoll(ctx context.Context, origin, token, operation string, files map[string]mcpLocalArtifact) error {
	saved, e := a.loadMCP(origin)
	if e != nil {
		return e
	}
	r, e := mcpPollDeliveryAuthorized(ctx, origin, token, operation, files, false, func(id string) error { return a.guardDeliveryAttempt(origin, operation, id) }, a.liveDeliveryAuth(origin, saved.ClientID))
	if r != nil {
		if oe := a.mcpResult(r, "Operation state: "+fmt.Sprint(r["state"])); oe != nil {
			return oe
		}
	}
	return e
}
