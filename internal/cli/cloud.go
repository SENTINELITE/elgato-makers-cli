package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/pkg/connect"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var cloudLinkID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var cloudCode = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)

func cloudOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return "", errors.New("cloud server must be an HTTPS origin")
	}
	loop := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loop) {
		return "", errors.New("cloud server requires HTTPS, except exact loopback for local testing")
	}
	return u.Scheme + "://" + u.Host, nil
}

func (a *app) cloudCommands() *cobra.Command {
	var server, link, codeFile string
	var yes bool
	root := &cobra.Command{Use: "cloud", Short: "Sign in to a hosted service or link a saved Maker session"}
	c := &cobra.Command{Use: "connect", Short: "Transfer saved credentials with a one-time link code", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		origin, err := cloudOrigin(server)
		if err != nil {
			return err
		}
		if !cloudLinkID.MatchString(link) {
			return errors.New("invalid cloud link identifier")
		}
		config, err := a.config()
		if err != nil {
			return errors.New("cannot read local configuration")
		}
		org, err := a.organization(config)
		if err != nil {
			return err
		}
		if !yes {
			if !a.o.IsTerminal() {
				return errors.New("cloud linking requires terminal consent or explicit --yes")
			}
			fmt.Fprintf(a.o.Err, "Transfer your saved Maker session for organization %s to %s? The service can renew and use this session. [y/N] ", org, origin)
			answer, readErr := bufio.NewReader(a.o.In).ReadString('\n')
			if readErr != nil || strings.ToLower(strings.TrimSpace(answer)) != "y" {
				return errors.New("cloud linking cancelled")
			}
		}
		var b []byte
		if codeFile != "" {
			f, e := os.Open(codeFile)
			if e != nil {
				return errors.New("cannot read one-time code file")
			}
			defer f.Close()
			b, err = io.ReadAll(io.LimitReader(f, 258))
		} else {
			if !a.o.IsTerminal() {
				return errors.New("use --code-file for noninteractive one-time codes")
			}
			f, ok := a.o.In.(*os.File)
			if !ok || !term.IsTerminal(int(f.Fd())) {
				return errors.New("one-time code requires a terminal or --code-file")
			}
			fmt.Fprint(a.o.Err, "One-time code: ")
			b, err = term.ReadPassword(int(f.Fd()))
			fmt.Fprintln(a.o.Err)
		}
		if err != nil {
			return errors.New("cannot read one-time code")
		}
		code := strings.TrimSpace(string(b))
		if !cloudCode.MatchString(code) {
			return errors.New("invalid one-time code")
		}
		client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		endpoint := origin + "/v1/links/" + link
		var pending struct {
			ID        string    `json:"id"`
			OrgID     string    `json:"org_id"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err = cloudRequest(cmd, client, http.MethodGet, endpoint, code, nil, &pending); err != nil {
			return err
		}
		if pending.ID != link || pending.OrgID != org || !pending.ExpiresAt.After(a.o.Now()) {
			return errors.New("cloud link is expired or does not match the selected organization")
		}
		// Load under the same lock as local session rotation, after consent and link
		// validation. Do not refresh here: renewal belongs to the receiving service.
		if err = a.lockSession(); err != nil {
			return errors.New("cannot lock saved Maker session")
		}
		creds, err := a.store().Load()
		if err != nil {
			return errors.New("cannot load saved Maker session")
		}
		if !pending.ExpiresAt.After(a.o.Now()) {
			return errors.New("cloud link expired before credential transfer")
		}
		cookies, err := connect.Encode(creds)
		if err != nil {
			return errors.New("cannot encode saved Maker session")
		}
		payload := append([]byte(`{"cookies":`), cookies...)
		payload = append(payload, '}')
		var result struct {
			ConnectionID string `json:"connection_id"`
			OrgID        string `json:"org_id"`
			State        string `json:"state"`
			// Additive: absent from older servers, which decode as 0 and print no notice.
			GrantsRevoked int `json:"grants_revoked,omitempty"`
		}
		if err = cloudRequest(cmd, client, http.MethodPost, endpoint+"/complete", code, payload, &result); err != nil {
			return err
		}
		if !cloudLinkID.MatchString(result.ConnectionID) || result.OrgID != org || (result.State != "connected" && result.State != "active") {
			return errors.New("cloud link completion response is invalid")
		}
		// Treat completion identifiers as untrusted too: a server must not cause
		// a one-time code or a saved cookie value to be printed as an identifier.
		if strings.Contains(result.ConnectionID, code) {
			return errors.New("cloud link completion response is invalid")
		}
		for _, cookie := range creds.Cookies() {
			if cookie.Value != "" && strings.Contains(result.ConnectionID, cookie.Value) {
				return errors.New("cloud link completion response is invalid")
			}
		}
		// Emit only known fields with validated identifiers, never the server body.
		if a.json {
			if err = a.output(result); err != nil {
				return err
			}
		}
		a.completion = "Maker cloud connection saved."
		if result.GrantsRevoked > 0 {
			a.completion += "\nReplacing the Maker connection revoked connected app access; re-approve your connected apps."
		}
		return nil
	}}
	c.Flags().StringVar(&server, "server", "", "Hosted service origin")
	c.Flags().StringVar(&link, "link", "", "Pending one-time link identifier")
	c.Flags().StringVar(&codeFile, "code-file", "", "Read the one-time code from a file")
	c.Flags().BoolVar(&yes, "yes", false, "Consent to sending the saved session to this service")
	a.addHostedAuthCommands(root, true)
	root.AddCommand(c)
	return root
}

func cloudRequest(cmd *cobra.Command, client *http.Client, method, endpoint, code string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(cmd.Context(), method, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot create cloud link request")
	}
	req.Header.Set("Authorization", "Bearer "+code)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("cloud link transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("cloud link rejected; create a new link if the code expired or was already used")
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(b) > 65536 {
		return errors.New("cloud link response exceeds limits or could not be read")
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("cloud link response is invalid")
	}
	return nil
}
