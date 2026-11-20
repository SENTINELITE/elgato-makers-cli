package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/filesec"
	"github.com/spf13/cobra"
)

const defaultMCPServer = "https://makers.sentinelite.com"

type mcpAuth struct {
	Server       string    `json:"server"`
	Scope        string    `json:"scope,omitempty"`
	ClientID     string    `json:"client_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (mcpAuth) String() string   { return "[redacted MCP OAuth credentials]" }
func (mcpAuth) GoString() string { return "[redacted MCP OAuth credentials]" }

type mcpMetadata struct {
	Issuer        string   `json:"issuer"`
	Authorization string   `json:"authorization_endpoint"`
	Token         string   `json:"token_endpoint"`
	Registration  string   `json:"registration_endpoint"`
	Revocation    string   `json:"revocation_endpoint"`
	Challenges    []string `json:"code_challenge_methods_supported"`
}
type mcpTokens struct {
	Scope   string `json:"scope"`
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Type    string `json:"token_type"`
	Expires int64  `json:"expires_in"`
}

// Normalize equivalent origin spellings before discovery or credential selection.
func mcpOrigin(raw string) (string, error) {
	origin, e := cloudOrigin(raw)
	if e != nil {
		return "", e
	}
	u, _ := url.Parse(origin)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host, nil
}

func mcpHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// mcpHTTPError reports a non-2xx response. OAuthError is set only to the
// whitelisted code "invalid_grant" from an HTTP 400; no other body text is kept.
type mcpHTTPError struct {
	Status       int
	OAuthError   string
	CIDiagnostic *ciRemoteDiagnostic
	CIRequest    bool
	CIRequestID  string
}

func (e *mcpHTTPError) Error() string {
	if e.CIDiagnostic != nil {
		return e.CIDiagnostic.Error()
	}
	if e.CIRequest {
		id := ""
		if e.CIRequestID != "" {
			id = " Request ID: " + e.CIRequestID + "."
		}
		return fmt.Sprintf("Hosted release API rejected the request (HTTP %d). Check service availability and CLI/policy configuration.%s Inspect any accepted operation before retrying a release.", e.Status, id)
	}
	return fmt.Sprintf("MCP request rejected (HTTP %d)", e.Status)
}

// Errors deliberately omit URLs, transport errors and server bodies: they can contain credentials.
func mcpRequest(ctx context.Context, client *http.Client, method, endpoint, contentType, token string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid MCP request")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("MCP-Protocol-Version", "2025-03-26")
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("MCP request failed; check the server and network")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rejected := &mcpHTTPError{Status: resp.StatusCode}
		if target, parseErr := url.Parse(endpoint); parseErr == nil && (strings.HasPrefix(target.Path, "/v1/ci/") || target.Path == "/v1/files" && strings.HasPrefix(token, "mci_")) {
			rejected.CIRequest = true
			if id := resp.Header.Get("X-Request-ID"); ciRequestID.MatchString(id) {
				rejected.CIRequestID = id
			}
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<10)+1))
		if readErr == nil && len(raw) <= 4<<10 {
			rejected.CIDiagnostic = parseCIRemoteDiagnostic(endpoint, raw)
			if resp.StatusCode == http.StatusBadRequest {
				var oauth struct {
					Error string `json:"error"`
				}
				if json.Unmarshal(raw, &oauth) == nil && oauth.Error == "invalid_grant" {
					rejected.OAuthError = oauth.Error
				}
			}
		}
		return rejected
	}
	if out == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return errors.New("invalid MCP response")
	}
	if json.Unmarshal(raw, out) != nil {
		return errors.New("invalid MCP JSON response")
	}
	return nil
}
func discoverMCP(ctx context.Context, c *http.Client, origin string) (mcpMetadata, error) {
	var resource struct {
		Resource string   `json:"resource"`
		Servers  []string `json:"authorization_servers"`
	}
	var m mcpMetadata
	if err := mcpRequest(ctx, c, "GET", origin+"/.well-known/oauth-protected-resource/mcp", "", "", nil, &resource); err != nil {
		return m, err
	}
	if resource.Resource != origin+"/mcp" || len(resource.Servers) != 1 || resource.Servers[0] != origin {
		return m, errors.New("MCP resource or authorization server does not match selected server")
	}
	if err := mcpRequest(ctx, c, "GET", origin+"/.well-known/oauth-authorization-server", "", "", nil, &m); err != nil {
		return m, err
	}
	if m.Issuer != origin {
		return m, errors.New("OAuth issuer does not match selected server")
	}
	for _, endpoint := range []string{m.Authorization, m.Token, m.Registration, m.Revocation} {
		u, e := url.Parse(endpoint)
		if e != nil || u.User != nil || u.Scheme+"://"+u.Host != origin || u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
			return m, errors.New("OAuth endpoint does not match selected server")
		}
	}
	for _, challenge := range m.Challenges {
		if challenge == "S256" {
			return m, nil
		}
	}
	return m, errors.New("OAuth server does not support S256 PKCE")
}
func mcpRandom() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", errors.New("cannot create OAuth nonce")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func mcpChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
func (a *app) mcpAuthPath(origin string) string {
	h := sha256.Sum256([]byte(origin))
	return filepath.Join(a.mcpDir(), fmt.Sprintf("mcp-%x.json", h))
}
func (a *app) mcpDir() string { return filepath.Join(filepath.Dir(a.configPath), "mcp") }
func (a *app) privateMCPDir(create bool) (string, error) {
	dir := a.mcpDir()
	if create {
		if e := filesec.CreatePrivateDir(dir); e != nil {
			return "", errors.New("cannot create MCP credential directory")
		}
	}
	info, e := os.Lstat(dir)
	if e != nil || filesec.VerifyPrivateDir(dir, info, false) != nil {
		return "", errors.New("MCP credential directory must be a real private directory (0700)")
	}
	return dir, nil
}
func syncMCPDir(dir string) error {
	if filesec.SyncDir(dir) != nil {
		return errors.New("cannot sync MCP credential directory")
	}
	return nil
}
func (a *app) loadMCP(origin string) (mcpAuth, error) {
	var auth mcpAuth
	if _, e := a.privateMCPDir(false); e != nil {
		return auth, e
	}
	p := a.mcpAuthPath(origin)
	info, e := os.Lstat(p)
	if e != nil {
		return auth, errors.New("no saved MCP login; run maker mcp login")
	}
	if filesec.VerifyPrivateFile(p, info, false) != nil {
		return auth, errors.New("MCP credential file must be a private regular file (0600)")
	}
	f, e := os.Open(p)
	if e != nil {
		return auth, errors.New("cannot read saved MCP login")
	}
	defer f.Close()
	actual, e := f.Stat()
	if e != nil || !os.SameFile(info, actual) {
		return auth, errors.New("MCP credential file changed while reading")
	}
	b, e := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if e != nil || len(b) > 64<<10 || json.Unmarshal(b, &auth) != nil || auth.Server != origin || auth.ClientID == "" || auth.AccessToken == "" {
		return auth, errors.New("invalid saved MCP login; run maker mcp login")
	}
	// Grants saved before explicit scope persistence were read-only. Normalize
	// their representation without granting the newly optional write capability.
	if auth.Scope == "" {
		auth.Scope = "maker:read"
	}
	return auth, nil
}
func (a *app) saveMCP(auth mcpAuth) error {
	dir, e := a.privateMCPDir(true)
	if e != nil {
		return e
	}
	f, e := filesec.CreateTemp(dir, ".mcp-auth-*")
	if e != nil {
		return errors.New("cannot save MCP login")
	}
	defer os.Remove(f.Name())
	b, e := json.Marshal(auth)
	if e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil {
		return errors.New("cannot save MCP login")
	}
	if os.Rename(f.Name(), a.mcpAuthPath(auth.Server)) != nil {
		return errors.New("cannot save MCP login")
	}
	return syncMCPDir(dir)
}
func (a *app) mcpRefreshGuard(origin string) string { return a.mcpAuthPath(origin) + ".refreshing" }
func (a *app) checkMCPRefreshGuard(origin string) error {
	if _, e := os.Lstat(a.mcpRefreshGuard(origin)); !os.IsNotExist(e) {
		return errors.New("MCP refresh outcome is uncertain; run maker mcp logout, then login again; do not remove the refresh guard")
	}
	return nil
}
func (a *app) beginMCPRefresh(origin string) error {
	dir, e := a.privateMCPDir(true)
	if e != nil {
		return e
	}
	f, e := filesec.OpenPrivateFile(a.mcpRefreshGuard(origin), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if e != nil {
		return errors.New("MCP refresh is guarded; run maker mcp logout, then login again")
	}
	_, e = f.WriteString("Refresh outcome may be uncertain. Logout and login to recover.\n")
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil {
		return errors.New("cannot persist MCP refresh guard")
	}
	return syncMCPDir(dir)
}
func (a *app) clearMCPRefreshGuard(origin string) error {
	if e := os.Remove(a.mcpRefreshGuard(origin)); e != nil && !os.IsNotExist(e) {
		return errors.New("cannot remove MCP refresh guard")
	}
	return syncMCPDir(a.mcpDir())
}
func revokeMCP(ctx context.Context, c *http.Client, m mcpMetadata, auth mcpAuth) error {
	token := auth.RefreshToken
	hint := "refresh_token"
	if token == "" {
		token = auth.AccessToken
		hint = "access_token"
	}
	return mcpRequest(ctx, c, "POST", m.Revocation, "application/x-www-form-urlencoded", "", []byte(url.Values{"client_id": {auth.ClientID}, "token": {token}, "token_type_hint": {hint}}.Encode()), nil)
}
func mcpToken(ctx context.Context, c *http.Client, m mcpMetadata, form url.Values) (mcpTokens, error) {
	var tokens mcpTokens
	e := mcpRequest(ctx, c, "POST", m.Token, "application/x-www-form-urlencoded", "", []byte(form.Encode()), &tokens)
	if e != nil {
		return tokens, e
	}
	if tokens.Access == "" || !strings.EqualFold(tokens.Type, "Bearer") || tokens.Expires < 1 || tokens.Expires > 31536000 {
		return tokens, errors.New("invalid OAuth token response")
	}
	return tokens, nil
}
func (a *app) refreshMCP(ctx context.Context, c *http.Client, m mcpMetadata, auth *mcpAuth) error {
	if auth.RefreshToken == "" {
		return errors.New("MCP login expired; run maker mcp login")
	}
	if e := a.checkMCPRefreshGuard(auth.Server); e != nil {
		return e
	}
	if e := a.beginMCPRefresh(auth.Server); e != nil {
		return e
	}
	tokens, e := mcpToken(ctx, c, m, url.Values{"grant_type": {"refresh_token"}, "client_id": {auth.ClientID}, "refresh_token": {auth.RefreshToken}, "resource": {auth.Server + "/mcp"}})
	if e != nil {
		// A definitive invalid_grant means the server rejected this token without
		// rotating it, so the guard set above is safe to clear. Any other failure
		// may have rotated the token and keeps the guard.
		var rejected *mcpHTTPError
		if errors.As(e, &rejected) && rejected.OAuthError == "invalid_grant" {
			if a.clearMCPRefreshGuard(auth.Server) != nil {
				return errors.New("MCP login is no longer valid and the refresh guard could not be removed; run maker mcp logout, then maker mcp login again")
			}
			return errors.New("MCP login is no longer valid (the grant was revoked, for example after your Maker connection was replaced); run maker mcp logout, then maker mcp login again")
		}
		return e
	}
	if tokens.Refresh == "" {
		return errors.New("OAuth refresh returned no rotated refresh token; run maker mcp logout, then login again")
	}
	if tokens.Scope != "" {
		for _, scope := range strings.Fields(tokens.Scope) {
			if !mcpScopeContains(auth.Scope, scope) {
				return errors.New("OAuth refresh changed granted scope; login again")
			}
		}
		auth.Scope = tokens.Scope
	}
	auth.AccessToken = tokens.Access
	auth.RefreshToken = tokens.Refresh
	auth.ExpiresAt = a.o.Now().Add(time.Duration(tokens.Expires) * time.Second)
	if e := a.saveMCP(*auth); e != nil {
		return errors.New("MCP refresh completed but credential persistence failed; run maker mcp logout, then login again")
	}
	return a.clearMCPRefreshGuard(auth.Server)
}
func mcpScopeContains(scope, want string) bool {
	if scope == "" {
		scope = "maker:read"
	}
	for _, s := range strings.Fields(scope) {
		if s == want {
			return true
		}
	}
	return false
}
func (a *app) mcpLogin(ctx context.Context, origin string, noBrowser bool) error {
	return a.mcpLoginScope(ctx, origin, noBrowser, false)
}
func (a *app) mcpLoginScope(ctx context.Context, origin string, noBrowser, write bool) error {
	scope := "maker:read"
	if write {
		scope += " maker:write"
	}
	if _, e := a.privateMCPDir(true); e != nil {
		return e
	}
	if e := a.checkMCPRefreshGuard(origin); e != nil {
		return e
	}
	c := mcpHTTPClient()
	m, e := discoverMCP(ctx, c, origin)
	if e != nil {
		return e
	}
	// Replacing a grant would strand the prior refresh token and lose revocation authority.
	if _, e := os.Lstat(a.mcpAuthPath(origin)); e == nil {
		return errors.New("MCP login already saved; run maker mcp logout before logging in again")
	} else if !os.IsNotExist(e) {
		return errors.New("cannot inspect saved MCP login")
	}
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return errors.New("cannot listen for OAuth callback")
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	state, e := mcpRandom()
	if e != nil {
		return e
	}
	verifier, e := mcpRandom()
	if e != nil {
		return e
	}
	var registration struct {
		ID string `json:"client_id"`
	}
	body, _ := json.Marshal(map[string]any{"client_name": "Maker CLI", "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "scope": scope})
	if e = mcpRequest(ctx, c, "POST", m.Registration, "application/json", "", body, &registration); e != nil {
		return e
	}
	if registration.ID == "" {
		return errors.New("OAuth registration returned no client identifier")
	}
	authURL, _ := url.Parse(m.Authorization)
	q := url.Values{"client_id": {registration.ID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {scope}, "resource": {origin + "/mcp"}, "state": {state}, "code_challenge": {mcpChallenge(verifier)}, "code_challenge_method": {"S256"}}
	authURL.RawQuery = q.Encode()
	codes := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		values, err := url.ParseQuery(r.URL.RawQuery)
		if r.Method != "GET" || r.Host != listener.Addr().String() || err != nil || len(values["state"]) != 1 || subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid OAuth callback.", http.StatusBadRequest)
			return
		}
		if values.Get("error") != "" {
			select {
			case codes <- "":
			default:
			}
			http.Error(w, "Authorization declined. Return to Maker CLI.", 400)
			return
		}
		if len(values["code"]) != 1 || values.Get("code") == "" || len(values.Get("code")) > 4096 {
			http.Error(w, "Invalid OAuth callback.", 400)
			return
		}
		select {
		case codes <- values.Get("code"):
			fmt.Fprintln(w, "Authorization received. Return to Maker CLI.")
		default:
			http.Error(w, "Callback already received.", 409)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
	defer srv.Close()
	go func() { _ = srv.Serve(listener) }()
	// Authorization URLs contain public client/PKCE parameters only, never a code or token.
	fmt.Fprintln(a.o.Err, "Open this URL to authorize Maker CLI:")
	fmt.Fprintln(a.o.Err, authURL.String())
	if !noBrowser {
		if e := openMCPBrowser(authURL.String()); e != nil {
			fmt.Fprintln(a.o.Err, "Browser could not be opened. Open the URL above manually.")
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var code string
	select {
	case code = <-codes:
		if code == "" {
			return errors.New("MCP authorization declined")
		}
	case <-waitCtx.Done():
		return errors.New("MCP login timed out or cancelled")
	}
	tokens, e := mcpToken(ctx, c, m, url.Values{"grant_type": {"authorization_code"}, "client_id": {registration.ID}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {origin + "/mcp"}})
	if e != nil {
		return e
	}
	if tokens.Refresh == "" {
		return errors.New("OAuth server returned no refresh token")
	}
	if tokens.Scope != "" {
		for _, v := range strings.Fields(tokens.Scope) {
			if !mcpScopeContains(scope, v) {
				return errors.New("OAuth grant exceeded requested scope")
			}
		}
		scope = tokens.Scope
	}
	auth := mcpAuth{Scope: scope, Server: origin, ClientID: registration.ID, AccessToken: tokens.Access, RefreshToken: tokens.Refresh, ExpiresAt: a.o.Now().Add(time.Duration(tokens.Expires) * time.Second)}
	if e = a.saveMCP(auth); e != nil {
		revokeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if revokeMCP(revokeCtx, c, m, auth) != nil {
			return errors.New("MCP login could not be saved and grant revocation failed; revoke Maker CLI access in the hosted account page")
		}
		return errors.New("MCP login could not be saved; the issued grant was revoked")
	}
	return a.mcpResult(map[string]any{"authenticated": true, "server": origin}, "MCP login saved. Run maker mcp status to check your Maker connection.")
}
func openMCPBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Run()
}
func (a *app) mcpResult(v any, message string) error {
	if a.json {
		return a.output(v)
	}
	_, e := fmt.Fprintln(a.o.Out, message)
	return e
}
func (a *app) mcpStatus(ctx context.Context, origin string) (resultErr error) {
	defer func() {
		if resultErr != nil && a.json {
			_ = a.output(map[string]any{"authenticated": nil, "server": origin, "state": "unavailable", "error": resultErr.Error()})
		}
	}()
	auth, e := a.loadMCP(origin)
	if e != nil {
		return e
	}
	if e = a.checkMCPRefreshGuard(origin); e != nil {
		return e
	}
	c := mcpHTTPClient()
	m, e := discoverMCP(ctx, c, origin)
	if e != nil {
		return e
	}
	if !auth.ExpiresAt.After(a.o.Now().Add(30 * time.Second)) {
		if e = a.refreshMCP(ctx, c, m, &auth); e != nil {
			return e
		}
	}
	result, e := mcpDeliveryTool(ctx, origin, auth.AccessToken, "get_account_status", map[string]any{})
	if errors.Is(e, errMCPUnknownTool) {
		result, e = mcpDeliveryTool(ctx, origin, auth.AccessToken, "get_connection_status", map[string]any{})
	}
	if e != nil {
		return errors.New("MCP connection status failed; reconnect in the hosted service")
	}
	if connection, ok := result["connection"].(map[string]any); ok {
		result = connection
	}
	state, _ := result["state"].(string)
	org, _ := result["org_id"].(string)
	demo, _ := result["demo"].(bool)
	switch state {
	case "connected", "disconnected", "reconnect_required":
	default:
		return errors.New("MCP server returned an unknown connection state")
	}
	// Whitelist status fields. Raw MCP tool text/errors can contain upstream private data.
	status := map[string]any{"authenticated": true, "server": origin, "state": state, "demo": demo}
	if org != "" {
		status["org_id"] = org
	}
	return a.mcpResult(status, fmt.Sprintf("MCP login: authenticated\nServer: %s\nMaker connection: %s", origin, state))
}
func (a *app) mcpLogout(ctx context.Context, origin string) error {
	auth, e := a.loadMCP(origin)
	if e != nil {
		if _, statErr := os.Lstat(a.mcpAuthPath(origin)); os.IsNotExist(statErr) {
			return a.mcpResult(map[string]any{"logged_out": true, "server": origin}, "No saved MCP login for this server.")
		}
		return e
	}
	c := mcpHTTPClient()
	m, e := discoverMCP(ctx, c, origin)
	if e != nil {
		return e
	}
	if e = revokeMCP(ctx, c, m, auth); e != nil {
		return errors.New("MCP grant revocation failed; saved login retained so you can retry logout")
	}
	if e = a.clearMCPRefreshGuard(origin); e != nil {
		return e
	}
	if os.Remove(a.mcpAuthPath(origin)) != nil {
		return errors.New("MCP grant revoked but local credential removal failed")
	}
	if e = syncMCPDir(a.mcpDir()); e != nil {
		return e
	}
	return a.mcpResult(map[string]any{"logged_out": true, "server": origin, "maker_disconnected": false}, "MCP grant revoked and local login removed. Your Maker connection remains saved remotely; disconnect it in the hosted service if desired.")
}
func (a *app) lockMCP(origin string) (func(), error) {
	p := a.mcpAuthPath(origin) + ".lock"
	if _, e := a.privateMCPDir(true); e != nil {
		return nil, e
	}
	f, e := filesec.OpenPrivateFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if e != nil {
		return nil, fmt.Errorf("MCP login is locked; verify another CLI process has stopped before removing %s", p)
	}
	_, e = fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	ce := f.Close()
	if e != nil || ce != nil {
		_ = os.Remove(p)
		return nil, errors.New("cannot create MCP credential lock")
	}
	return func() { _ = os.Remove(p) }, nil
}

func (a *app) mcpCommands() *cobra.Command {
	root := &cobra.Command{Use: "mcp", Short: "Authorize and check your individual hosted MCP connection", Long: "Authorize Maker CLI to a hosted MCP service. Hosted MCP needs no local daemon or start/stop commands. Maker cookies remain separate; login does not transfer them. Use connect only with a one-time link from the service's account page."}
	a.addHostedAuthCommands(root, false)
	cloud := a.cloudCommands()
	for _, child := range cloud.Commands() {
		if child.Name() == "connect" {
			cloud.RemoveCommand(child)
			root.AddCommand(child)
		}
	}
	a.addMCPDeliveryCommands(root)
	return root
}

// Shared OAuth client; cloud setup uses REST and never invokes MCP tools.
func (a *app) addHostedAuthCommands(root *cobra.Command, cloud bool) {
	for _, name := range []string{"login", "status", "logout"} {
		var server string
		var noBrowser, write bool
		name := name
		cmd := &cobra.Command{Use: name, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			origin, e := mcpOrigin(server)
			if e != nil {
				return e
			}
			unlock, e := a.lockMCP(origin)
			if e != nil {
				return e
			}
			defer unlock()
			switch name {
			case "login":
				return a.mcpLoginScope(cmd.Context(), origin, noBrowser, write)
			case "status":
				if cloud {
					return a.ciPolicyList(cmd.Context(), origin)
				}
				return a.mcpStatus(cmd.Context(), origin)
			default:
				return a.mcpLogout(cmd.Context(), origin)
			}
		}}
		switch name {
		case "login":
			cmd.Short = "Authorize with browser consent and a private loopback callback"
			cmd.Flags().BoolVar(&write, "write", false, "Request maker:write in addition to read access; requires explicit browser consent")
			cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Print authorization URL without opening your browser; still waits for callback")
		case "status":
			cmd.Short = "Read your hosted Maker connection status (refreshes OAuth if needed)"
		default:
			cmd.Short = "Revoke this CLI grant and remove its local OAuth credentials"
		}
		cmd.Flags().StringVar(&server, "server", defaultMCPServer, "Hosted MCP HTTPS origin (exact loopback HTTP allowed for tests)")
		root.AddCommand(cmd)
	}
}
