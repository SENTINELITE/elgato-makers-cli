package maker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

var ErrReadOnly = errors.New("maker client is read-only")

// UploadNotStartedError proves no package storage PUT or registration was sent.
type UploadNotStartedError struct{ Cause error }

func (e *UploadNotStartedError) Error() string {
	return "package upload was not started: " + e.Cause.Error()
}
func (e *UploadNotStartedError) Unwrap() error     { return e.Cause }
func (e *UploadNotStartedError) NotUploaded() bool { return true }

// NotSubmittedError proves createVersion was never dispatched. The preflight
// cause remains available through errors.Is/errors.As without exposing secrets.
type NotSubmittedError struct{ Cause error }

func (e *NotSubmittedError) Error() string {
	if category := e.FailureCategory(); category != "" {
		return "submission was not dispatched (" + category + "); complete preflight before retrying"
	}
	return "submission was not dispatched; complete preflight before retrying"
}
func (e *NotSubmittedError) Unwrap() error      { return e.Cause }
func (e *NotSubmittedError) NotSubmitted() bool { return true }

// FailureCategory describes only allowlisted local preflight failures. It never
// includes arbitrary transport errors, URLs, response payloads, or credentials.
func (e *NotSubmittedError) FailureCategory() string {
	for cause, depth := e.Cause, 0; cause != nil && depth < 32; cause, depth = errors.Unwrap(cause), depth+1 {
		switch cause.Error() {
		case "submission manifest identity mismatch":
			return "reviewed manifest identity mismatch"
		case "fresh submission manifest identity mismatch":
			return "uploaded manifest identity changed"
		case "uploaded manifest changed after review; review the upload before submission":
			return "uploaded manifest changed after review"
		case "unrecognized uploaded manifest":
			return "uploaded manifest schema not recognized"
		case "ambiguous uploaded manifest":
			return "uploaded manifest identity is ambiguous"
		case "current Maker action references not recognized":
			return "submission action references not recognized"
		case "conflicting Maker action references":
			return "submission action references conflict"
		case "too many Maker script references":
			return "submission action discovery exceeded its limit"
		case "Stream Deck minimum version validation schema is not recognized":
			return "minimum Stream Deck version validation returned an unrecognized schema"
		case "Stream Deck minimum version rejected":
			return "minimum Stream Deck version was rejected"
		case "unrecognized Maker action response", "missing Maker action result":
			return "submission preflight action response not recognized"
		case "Maker action rejected":
			return "submission preflight action was rejected"
		case "Maker authentication or access rejected":
			return "Maker authentication or access rejected"
		case "Maker transport failed":
			return "Maker transport failed"
		case "Maker response read failed":
			return "Maker response read failed"
		case "Maker response exceeds limit":
			return "Maker response exceeded its size limit"
		}
	}
	return ""
}

type AmbiguousSubmissionError struct{ Cause error }

func (e *AmbiguousSubmissionError) Error() string {
	return "submission outcome is ambiguous; reconcile versions before retrying"
}
func (e *AmbiguousSubmissionError) Unwrap() error { return e.Cause }

type Config struct {
	BaseURL        string
	UploadTransport http.RoundTripper
	HTTPClient     *http.Client
	Cookies        []*http.Cookie
	AllowLocalhost bool
	ReadOnly       bool
}
type Client struct {
	publicScripts   publicScriptCache
	base            *url.URL
	http            *http.Client
	uploadHTTP      *http.Client
	jar             *scopedJar
	local, readOnly bool
	stats           requestCounters
}
type Actions struct{ UploadAux, GetIsSDVersionValid, CreateVersion string }

func New(c Config) (*Client, error) {
	if c.BaseURL == "" {
		c.BaseURL = "https://maker.elgato.com"
	}
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("invalid Maker base URL")
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if !(u.Scheme == "https" && u.Host == "maker.elgato.com") && !(c.AllowLocalhost && local && (u.Scheme == "http" || u.Scheme == "https")) {
		return nil, errors.New("Maker origin is not allowed")
	}
	h := http.Client{Timeout: 30 * time.Second}
	if c.HTTPClient != nil {
		h = *c.HTTPClient
	}
	if h.Timeout == 0 {
		h.Timeout = 30 * time.Second
	}
	h.Jar = nil
	h.CheckRedirect = func(r *http.Request, v []*http.Request) error { return http.ErrUseLastResponse }
	u.Path = ""
	uploadHTTP := h
 if c.UploadTransport!=nil{uploadHTTP.Transport=c.UploadTransport;uploadHTTP.Timeout=2*time.Minute}
	jar := newScopedJar(u, c.Cookies)
	h.Jar = jar
	return &Client{base: u, http: &h, uploadHTTP: &uploadHTTP, jar: jar, local: c.AllowLocalhost, readOnly: c.ReadOnly}, nil
}
func (c *Client) endpoint(path string) string { return c.base.String() + path }
func safeID(s string) error {
	if s == "" || !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(s) {
		return errors.New("invalid Maker resource identifier")
	}
	return nil
}
func targetPath(t model.Target) (string, error) {
	if e := safeID(t.OrgID); e != nil {
		return "", e
	}
	if e := safeID(t.ProductID); e != nil {
		return "", e
	}
	return "/" + t.OrgID + "/products/" + t.ProductID + "/versions", nil
}
func (c *Client) request(ctx context.Context, method, raw, ct, action string, body io.Reader, maker bool) (data []byte, requestErr error) {
	req, e := http.NewRequestWithContext(ctx, method, raw, body)
	if e != nil {
		return nil, errors.New("cannot create request")
	}
	if maker {
		if req.URL.Scheme != c.base.Scheme || req.URL.Host != c.base.Host {
			return nil, errors.New("cross-origin Maker request blocked")
		}
	}
	if f, ok := body.(*os.File); ok {
		if st, err := f.Stat(); err == nil {
			req.ContentLength = st.Size()
		}
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if action != "" {
		req.Header.Set("Next-Action", action)
		req.Header.Set("Accept", "text/x-component")
		req.Header.Set("Origin", c.base.String())
	}
	h := c.http
	if !maker {
		h = c.uploadHTTP
	}
	if method != http.MethodGet {
		c.invalidateReadPhase(ctx)
		defer c.invalidateReadPhase(ctx)
	}
	started := time.Now()
	var responseBytes int64
	defer func() {
		c.recordRequest(maker, method, time.Since(started), responseBytes, requestErr != nil)
	}()
	resp, e := h.Do(req)
	if e != nil {
		return nil, errors.New("Maker transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return nil, ErrAuthentication
		}
		return nil, fmt.Errorf("Maker HTTP status %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024+1))
	responseBytes = int64(len(b))
	if e != nil {
		return nil, errors.New("Maker response read failed")
	}
	if len(b) > 16*1024*1024 {
		return nil, errors.New("Maker response exceeds limit")
	}
	return b, nil
}
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	return c.readPhaseGet(ctx, path, func() ([]byte, error) {
		return c.request(ctx, "GET", c.endpoint(path), "", "", nil, true)
	})
}
func (c *Client) CheckAuth(ctx context.Context, org string) error {
	_, e := c.Products(ctx, org)
	return e
}
func (c *Client) Products(ctx context.Context, org string) ([]model.Product, error) {
	if e := safeID(org); e != nil {
		return nil, e
	}
	b, e := c.get(ctx, "/"+org+"/products")
	if e != nil {
		return nil, e
	}
	p, _, e := parsePage(b)
	return p, e
}
func (c *Client) Product(ctx context.Context, t model.Target) (model.Product, error) {
	path, e := targetPath(t)
	if e != nil {
		return model.Product{}, e
	}
	b, e := c.get(ctx, path)
	if e != nil {
		return model.Product{}, e
	}
	ps, _, e := parsePage(b)
	if e != nil {
		return model.Product{}, e
	}
	for _, p := range ps {
		if p.ID == t.ProductID {
			return p, nil
		}
	}
	return model.Product{}, errors.New("product absent from recognized Maker response")
}
func (c *Client) Versions(ctx context.Context, t model.Target) ([]model.Version, error) {
	path, e := targetPath(t)
	if e != nil {
		return nil, e
	}
	b, e := c.get(ctx, path)
	if e != nil {
		return nil, e
	}
	ps, vs, e := parsePageMode(b, true)
	if e != nil {
		return nil, e
	}
	allowed := map[string]bool{}
	found := false
	for _, p := range ps {
		if p.ID == t.ProductID {
			found = true
			for _, variant := range p.Variants {
				allowed[variant.ID] = true
			}
		}
	}
	if !found {
		return nil, errors.New("product absent from recognized Maker response")
	}
	if t.VariantID != "" && !allowed[t.VariantID] {
		return nil, errors.New("variant absent from recognized Maker response")
	}
	out := []model.Version{}
	for _, v := range vs {
		if allowed[v.VariantID] && (t.VariantID == "" || v.VariantID == t.VariantID) {
			out = append(out, v)
		}
	}
	return out, nil
}

var scripts = regexp.MustCompile(`(?:src|href)=["']([^"']+\.js(?:\?[^"']*)?)["']`)
var actionPattern = serverReferencePattern(`(uploadAux|getIsSdVersionValid|createVersion)`)

func (c *Client) DiscoverActions(ctx context.Context, t model.Target) (Actions, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	path, e := targetPath(t)
	if e != nil {
		return Actions{}, e
	}
	b, e := c.get(ctx, path)
	if e != nil {
		return Actions{}, e
	}
	var a Actions
	scan := func(b []byte) error {
		for _, m := range actionPattern.FindAllSubmatch(b, -1) {
			var dst *string
			switch string(m[2]) {
			case "uploadAux":
				dst = &a.UploadAux
			case "getIsSdVersionValid":
				dst = &a.GetIsSDVersionValid
			case "createVersion":
				dst = &a.CreateVersion
			}
			if *dst != "" && *dst != string(m[1]) {
				return errors.New("conflicting Maker action references")
			}
			*dst = string(m[1])
		}
		return nil
	}
	if e = scan(b); e != nil {
		return a, e
	}
	seen := map[string]bool{}
	for _, m := range scripts.FindAllSubmatch(b, -1) {
		u, e := url.Parse(string(m[1]))
		if e != nil {
			continue
		}
		u = c.base.ResolveReference(u)
		if u.Host != c.base.Host || u.Scheme != c.base.Scheme || !strings.HasPrefix(u.Path, "/_next/static/") || seen[u.String()] {
			continue
		}
		if len(seen) >= 100 {
			return a, errors.New("too many Maker script references")
		}
		seen[u.String()] = true
		b, e = c.publicScript(ctx, u)
		if e != nil {
			return a, e
		}
		if e = scan(b); e != nil {
			return a, e
		}
	}
	if a.UploadAux == "" || a.GetIsSDVersionValid == "" || a.CreateVersion == "" {
		return a, errors.New("current Maker action references not recognized")
	}
	return a, nil
}
func actionResult(b []byte) (map[string]any, error) {
	for _, line := range strings.Split(string(b), "\n") {
		p := strings.SplitN(line, ":", 2)
		if len(p) != 2 {
			continue
		}
		if strings.HasPrefix(p[1], "E") {
			return nil, errors.New("Maker action rejected")
		}
		if p[0] == "1" {
			var m map[string]any
			if json.Unmarshal([]byte(p[1]), &m) != nil {
				return nil, errors.New("unrecognized Maker action response")
			}
			if _, ok := m["error"]; ok {
				return nil, errors.New("Maker action rejected")
			}
			return m, nil
		}
	}
	return nil, errors.New("missing Maker action result")
}
func (c *Client) multipart(ctx context.Context, path, action string, fields map[string]string) (map[string]any, error) {
	raw, err := c.multipartRaw(ctx, path, action, fields)
	if err != nil {
		return nil, err
	}
	return actionResult(raw)
}

func (c *Client) multipartRaw(ctx context.Context, path, action string, fields map[string]string) ([]byte, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	// Flight's streaming decoder snapshots referenced FormData when it reads
	// the root argument model. Emit every referenced field before root part 0,
	// as the browser encoder does; Go map iteration can otherwise omit fields.
	keys := make([]string, 0, len(fields))
	for key := range fields {
		if key != "0" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if _, ok := fields["0"]; ok {
		keys = append(keys, "0")
	}
	for _, key := range keys {
		if e := w.WriteField(key, fields[key]); e != nil {
			return nil, e
		}
	}
	if e := w.Close(); e != nil {
		return nil, e
	}
	raw, e := c.request(ctx, "POST", c.endpoint(path), w.FormDataContentType(), action, &b, true)
	if e != nil {
		return nil, e
	}
	return raw, nil
}
func (c *Client) ReadManifest(ctx context.Context, folder string) (model.Manifest, error) {
	var m model.Manifest
	if e := safeID(folder); e != nil {
		return m, e
	}
	b, e := c.get(ctx, "/api/upload-aux/manifest/"+folder)
	if e != nil {
		return m, e
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(b, &envelope) != nil {
		return m, errors.New("unrecognized uploaded manifest")
	}
	payload := b
	if wrapped, ok := envelope["manifest"]; ok {
		if string(wrapped) == "null" {
			return m, errors.New("unrecognized uploaded manifest")
		}
		if _, conflict := envelope["UUID"]; conflict {
			return m, errors.New("ambiguous uploaded manifest")
		}
		payload = wrapped
	}
	if json.Unmarshal(payload, &m) != nil || m.UUID == "" || m.Version == "" || m.Software.MinimumVersion == "" {
		return m, errors.New("unrecognized uploaded manifest")
	}
	return m, nil
}
func (c *Client) Upload(ctx context.Context, t model.Target, p model.Package) (out model.UploadResult, err error) {
	started := false
	defer func() {
		if err != nil && !started {
			err = &UploadNotStartedError{Cause: err}
		}
	}()
	if c.readOnly {
		return out, ErrReadOnly
	}
	path, e := targetPath(t)
	if e != nil {
		return out, e
	}
	a, e := c.DiscoverActions(ctx, t)
	if e != nil {
		return out, e
	}
	f, e := os.Open(p.Path)
	if e != nil {
		return out, errors.New("cannot open package")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() != p.Size {
		return out, errors.New("package size changed")
	}
	if p.SHA256 != "" {
		hash := sha256.New()
		if _, err := io.Copy(hash, f); err != nil {
			return out, errors.New("cannot hash package")
		}
		if hex.EncodeToString(hash.Sum(nil)) != p.SHA256 {
			return out, errors.New("package digest changed")
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return out, errors.New("cannot read package")
		}
	}
	out.FileID = t.OrgID + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10) + ".streamDeckPlugin"
	b, e := c.get(ctx, "/api/presigned-url?file="+url.QueryEscape(out.FileID))
	if e != nil {
		return out, e
	}
	var signed struct {
		URL string `json:"presignedUrl"`
	}
	if json.Unmarshal(b, &signed) != nil {
		return out, errors.New("unrecognized presigned URL response")
	}
	u, e := url.Parse(signed.URL)
	if e != nil || u.User != nil || u.Fragment != "" {
		return out, errors.New("invalid upload destination")
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if !(u.Scheme == "https" && u.Host == "s3.us-east-1.amazonaws.com" && u.Path == "/production.maker.elgato.com/"+out.FileID) && !(c.local && local && (u.Scheme == "http" || u.Scheme == "https")) {
		return out, errors.New("upload destination is not allowed")
	}
	started = true
	if _, e = c.request(ctx, "PUT", signed.URL, "", "", f, false); e != nil {
		return out, e
	}
	m, e := c.multipart(ctx, path, a.UploadAux, map[string]string{"_1_s3_link_id": out.FileID, "0": `[null,"$K1"]`})
	if e != nil {
		return out, e
	}
	out.FolderID, _ = m["id"].(string)
	if m["success"] != true || out.FolderID == "" {
		return out, errors.New("Maker upload action rejected")
	}
	out.Manifest, e = c.ReadManifest(ctx, out.FolderID)
	if e != nil {
		return out, e
	}
	if _, e = c.get(ctx, "/api/upload-aux/tags/"+out.FolderID); e != nil {
		return out, e
	}
	if out.Manifest.UUID != p.Manifest.UUID || out.Manifest.Version != p.Manifest.Version {
		return out, errors.New("uploaded manifest identity mismatch")
	}
	return out, nil
}
func (c *Client) Submit(ctx context.Context, t model.Target, s model.Submission) (out model.SubmitResult, err error) {
	dispatched := false
	defer func() {
		if err != nil && !dispatched {
			err = &NotSubmittedError{Cause: err}
		}
	}()
	if c.readOnly {
		return out, ErrReadOnly
	}
	path, e := targetPath(t)
	if e != nil {
		return out, e
	}
	if e = safeID(t.VariantID); e != nil {
		return out, e
	}
	if e = safeID(s.Upload.FolderID); e != nil {
		return out, e
	}
	if e = safeID(s.Upload.FileID); e != nil {
		return out, e
	}
	m := s.Upload.Manifest
	if m.UUID != t.UUID || m.UUID != s.Package.Manifest.UUID || m.Version != s.Package.Manifest.Version {
		return out, errors.New("submission manifest identity mismatch")
	}
	// Re-read the uploaded artifact immediately before server-action preflight.
	// A saved upload receipt cannot prove the provider still has that manifest.
	fresh, e := c.ReadManifest(ctx, s.Upload.FolderID)
	if e != nil {
		return out, e
	}
	if fresh.UUID != t.UUID || fresh.UUID != s.Package.Manifest.UUID || fresh.Version != s.Package.Manifest.Version {
		return out, errors.New("fresh submission manifest identity mismatch")
	}
	if !reflect.DeepEqual(fresh, m) {
		return out, errors.New("uploaded manifest changed after review; review the upload before submission")
	}
	m = fresh
	a, e := c.DiscoverActions(ctx, t)
	if e != nil {
		return out, e
	}
	args, _ := json.Marshal([]map[string]string{{"sdVersion": m.Software.MinimumVersion}})
	b, e := c.request(ctx, "POST", c.endpoint(path), "text/plain;charset=UTF-8", a.GetIsSDVersionValid, bytes.NewReader(args), true)
	if e != nil {
		return out, e
	}
	valid, e := actionResult(b)
	if e != nil {
		return out, e
	}
	versionValid, recognized := valid["is_valid"].(bool)
	if !recognized {
		return out, errors.New("Stream Deck minimum version validation schema is not recognized")
	}
	if !versionValid {
		return out, errors.New("Stream Deck minimum version rejected")
	}
	fields := map[string]string{"_1_version": m.Version, "_1_release_notes": s.Notes.Markdown, "_1_release_notesPlainTextLength": strconv.Itoa(s.Notes.PlainTextLength), "_1_is_ready_for_publish": strconv.FormatBool(s.AutoPublish), "_1_variant_id": t.VariantID, "_1_id": t.ProductID, "_1_org_id": t.OrgID, "_1_file_s3_id": s.Upload.FileID, "_1_folder_id": s.Upload.FolderID, "_1_min_app_version": m.Software.MinimumVersion, "_1_streamdeck_id": m.UUID, "0": `[{},"$K1"]`}
	for i, o := range m.OS {
		fields[fmt.Sprintf("_1_os_versions[%d][name]", i)] = o.Platform
		fields[fmt.Sprintf("_1_os_versions[%d][number]", i)] = o.MinimumVersion
	}
	dispatched = true
	raw, e := c.multipartRaw(ctx, path, a.CreateVersion, fields)
	if e != nil {
		return out, &AmbiguousSubmissionError{e}
	}
	r, e := actionResult(raw)
	if e != nil {
		return out, &AmbiguousSubmissionError{e}
	}
	receipt, _ := r["id"].(string)
	if r["success"] != true || safeID(receipt) != nil {
		return out, &AmbiguousSubmissionError{errors.New("Maker submission result did not confirm success")}
	}
	out.ID, out.ReceiptID = receipt, receipt
	out.Message, _ = r["message"].(string)
	canonical, snapshot, e := submissionVersionFromResponse(raw, t, m.Version)
	if e != nil {
		return out, &AmbiguousSubmissionError{e}
	}
	if snapshot {
		out.ID = canonical
	}
	return out, nil
}

// submissionVersionFromResponse binds an opaque successful action receipt to
// the canonical version only through that same response's owning product and
// variant. Without a product snapshot the legacy receipt-only result remains
// compatible; a present but conflicting snapshot is never silently ignored.
func submissionVersionFromResponse(raw []byte, target model.Target, expectedVersion string) (string, bool, error) {
	sawSnapshot, found := false, false
	canonical, status := "", ""
	invalid := func() error {
		return errors.New("acknowledged submission response has no unique matching scoped version")
	}
	err := walkListingPage(raw, func(product map[string]any, resolve func(any) any) error {
		id, _ := resolve(product["id"]).(string)
		name, _ := resolve(product["name"]).(string)
		rawVariants, hasVariants := product["variants"]
		if !hasVariants {
			if id == target.ProductID && name != "" {
				sawSnapshot = true
				return invalid()
			}
			return nil
		}
		// Ignore generic UI props named variants; product snapshots have an
		// identity and name, or the exact requested product identity.
		_, hasID := product["id"]
		_, hasName := product["name"]
		if id != target.ProductID && !hasID && !hasName {
			return nil
		}
		sawSnapshot = true
		if id != target.ProductID {
			return nil
		}
		if name == "" || (target.Name != "" && name != target.Name) {
			return invalid()
		}
		variants, ok := resolve(rawVariants).([]any)
		if !ok {
			return invalid()
		}
		matchingVariant := 0
		candidate, candidateStatus := "", ""
		for _, rawVariant := range variants {
			variant, ok := resolve(rawVariant).(map[string]any)
			if !ok {
				return invalid()
			}
			vid, _ := resolve(variant["id"]).(string)
			if safeID(vid) != nil {
				return invalid()
			}
			if vid != target.VariantID {
				continue
			}
			matchingVariant++
			uuid, _ := resolve(variant["streamdeckid"]).(string)
			if target.UUID == "" || uuid != target.UUID {
				return invalid()
			}
			versions, ok := resolve(variant["versions"]).([]any)
			if !ok {
				return invalid()
			}
			matches := 0
			for _, rawVersion := range versions {
				version, ok := resolve(rawVersion).(map[string]any)
				if !ok {
					return invalid()
				}
				for key, expected := range map[string]string{"product_id": target.ProductID, "variant_id": target.VariantID} {
					if rawOwner, exists := version[key]; exists {
						owner, ok := resolve(rawOwner).(string)
						if !ok || owner != expected {
							return invalid()
						}
					}
				}
				versionID, _ := resolve(version["id"]).(string)
				versionStatus, _ := resolve(version["status"]).(string)
				number := ""
				for _, key := range []string{"version_number", "number", "version"} {
					if rawNumber, exists := version[key]; exists {
						value, ok := resolve(rawNumber).(string)
						if !ok || !model.SameStreamDeckVersion(value, value) {
							return invalid()
						}
						if number != "" && !model.SameStreamDeckVersion(number, value) {
							return invalid()
						}
						number = value
					}
				}
				if safeID(versionID) != nil || strings.TrimSpace(versionStatus) == "" || !model.SameStreamDeckVersion(number, number) {
					return invalid()
				}
				if model.SameStreamDeckVersion(number, expectedVersion) {
					matches++
					candidate = versionID
					candidateStatus = strings.ToLower(versionStatus)
				}
			}
			if matches != 1 {
				return invalid()
			}
		}
		if matchingVariant != 1 {
			return invalid()
		}
		if found && (canonical != candidate || status != candidateStatus) {
			return invalid()
		}
		found = true
		canonical = candidate
		status = candidateStatus
		return nil
	})
	if err != nil {
		return "", sawSnapshot, err
	}
	if sawSnapshot && !found {
		return "", true, invalid()
	}
	return canonical, sawSnapshot, nil
}
