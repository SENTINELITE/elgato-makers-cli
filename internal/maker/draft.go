package maker

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// DraftInput describes the initial plugin record. Creating it neither uploads a
// package nor creates a version or submits a review request.
type DraftInput struct {
	OrgID                      string `json:"org_id"`
	Name                       string `json:"name"`
	Description                string `json:"description"`
	DescriptionPlainTextLength int    `json:"description_plain_text_length"`
	UUID                       string `json:"uuid"`
	ExtensionID                string `json:"extension_id,omitempty"`
}
type DraftResult struct {
	ProductID string `json:"product_id"`
	VariantID string `json:"variant_id"`
}

// DraftNotCreatedError proves the createProduct request was never dispatched.
type DraftNotCreatedError struct{ Cause error }

func (e *DraftNotCreatedError) Error() string {
	return "draft creation was not dispatched; complete preflight before retrying"
}
func (e *DraftNotCreatedError) Unwrap() error    { return e.Cause }
func (e *DraftNotCreatedError) NotCreated() bool { return true }

// AmbiguousDraftError requires reconciliation before another creation attempt.
// Error text intentionally excludes server payloads and transport URLs.
type AmbiguousDraftError struct{ Cause error }

func (e *AmbiguousDraftError) Error() string {
	return "draft creation outcome is ambiguous (" + e.FailureCategory() + "); reconcile products before retrying"
}
func (e *AmbiguousDraftError) Unwrap() error { return e.Cause }

// FailureCategory returns only fixed diagnostics or an exact bounded HTTP
// status. Unknown causes, response bodies, URLs, and credentials stay private.
func (e *AmbiguousDraftError) FailureCategory() string {
	if errors.Is(e.Cause, context.DeadlineExceeded) {
		return "request deadline exceeded"
	}
	if errors.Is(e.Cause, context.Canceled) {
		return "request canceled"
	}
	for cause := e.Cause; cause != nil; cause = errors.Unwrap(cause) {
		switch cause.Error() {
		case "Maker transport failed":
			return "transport failure"
		case "Maker authentication or access rejected":
			return "authentication or access rejected"
		case "Maker response read failed":
			return "response read failure"
		case "Maker response exceeds limit":
			return "response exceeded size limit"
		case "Maker action rejected":
			return "action rejected"
		case "unrecognized Maker action response":
			return "unrecognized action response"
		case "missing Maker action result":
			return "action acknowledgement missing"
		case "Maker draft result reported unsuccessful creation":
			return "server reported unsuccessful creation"
		case "Maker draft result did not confirm complete identifiers":
			return "creation acknowledgement missing or incomplete"
		}
		if match := draftHTTPStatusPattern.FindStringSubmatch(cause.Error()); match != nil {
			return "HTTP status " + match[1]
		}
	}
	return "unknown failure"
}

var draftHTTPStatusPattern = regexp.MustCompile(`^Maker HTTP status ([1-5][0-9]{2})$`)

var errDraftExtensionMissing = errors.New("current Maker plugin extension not recognized")

var draftActionPattern = serverReferencePattern(`createProduct`)

func (c *Client) CreateDraft(ctx context.Context, input DraftInput) (out DraftResult, err error) {
	dispatched := false
	defer func() {
		if err != nil && !dispatched {
			err = &DraftNotCreatedError{Cause: err}
		}
	}()
	if c.readOnly {
		return out, ErrReadOnly
	}
	if input.ExtensionID != "" && safeID(input.ExtensionID) != nil {
		return out, errors.New("invalid Maker extension identifier")
	}
	if safeID(input.OrgID) != nil || safeID(input.UUID) != nil || strings.TrimSpace(input.Name) == "" || len(utf16.Encode([]rune(input.Name))) > 50 || strings.TrimSpace(input.Description) == "" || input.DescriptionPlainTextLength < 1 || input.DescriptionPlainTextLength > 4000 {
		return out, errors.New("invalid Maker draft input")
	}
	path := "/" + input.OrgID + "/create/plugins"
	action, extension, err := c.discoverDraft(ctx, path, input.ExtensionID)
	if err != nil {
		return out, err
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	fields := map[string]string{
		"_1_name": input.Name, "_1_description": input.Description,
		"_1_descriptionPlainTextLength": strconv.Itoa(input.DescriptionPlainTextLength),
		"_1_organization_id":            input.OrgID, "_1_extension": extension,
		"_1_extension_slug": "plugins", "_1_streamDeckId": input.UUID,
		"0": `[null,"$K1"]`,
	}
	dispatched = true
	result, err := c.multipart(ctx, path, action, fields)
	if err != nil {
		return out, &AmbiguousDraftError{Cause: err}
	}
	if result["success"] == false {
		return out, &AmbiguousDraftError{Cause: errors.New("Maker draft result reported unsuccessful creation")}
	}
	product, _ := result["productId"].(string)
	variant, _ := result["variantId"].(string)
	if result["success"] != true || safeID(product) != nil || safeID(variant) != nil {
		return out, &AmbiguousDraftError{Cause: errors.New("Maker draft result did not confirm complete identifiers")}
	}
	return DraftResult{ProductID: product, VariantID: variant}, nil
}

func (c *Client) discoverDraft(ctx context.Context, path, explicitExtension string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	page, err := c.get(ctx, path)
	if err != nil {
		return "", "", err
	}
	extension, err := draftExtension(page)
	if err != nil {
		if !errors.Is(err, errDraftExtensionMissing) || explicitExtension == "" {
			return "", "", err
		}
		extension = explicitExtension
	} else if explicitExtension != "" && explicitExtension != extension {
		return "", "", errors.New("explicit Maker extension conflicts with current page")
	}
	action := ""
	scan := func(body []byte) error {
		for _, m := range draftActionPattern.FindAllSubmatch(body, -1) {
			candidate := string(m[1])
			if action != "" && action != candidate {
				return errors.New("conflicting Maker draft action references")
			}
			action = candidate
		}
		return nil
	}
	if err = scan(page); err != nil {
		return "", "", err
	}
	seen := map[string]bool{}
	for _, m := range scripts.FindAllSubmatch(page, -1) {
		u, err := url.Parse(html.UnescapeString(string(m[1])))
		if err != nil {
			continue
		}
		u = c.base.ResolveReference(u)
		if u.Scheme != c.base.Scheme || u.Host != c.base.Host || u.User != nil || !strings.HasPrefix(u.Path, "/_next/static/") || seen[u.String()] {
			continue
		}
		if len(seen) >= 100 {
			return "", "", errors.New("too many Maker script references")
		}
		seen[u.String()] = true
		body, err := c.publicScript(ctx, u)
		if err != nil {
			return "", "", err
		}
		if err = scan(body); err != nil {
			return "", "", err
		}
	}
	if action == "" {
		return "", "", errors.New("current Maker draft action reference not recognized")
	}
	return action, extension, nil
}

// draftExtension reads structured page data, including split Next Flight frames
// and references. It never infers an extension from a bundle constant or URL.
func draftExtension(page []byte) (string, error) {
	var roots []any
	records := map[string]any{}
	var value any
	if json.Unmarshal(page, &value) == nil {
		roots = append(roots, value)
	}
	var stream strings.Builder
	for _, frame := range framePattern.FindAllSubmatch(page, -1) {
		var a []json.RawMessage
		if json.Unmarshal(frame[1], &a) != nil || len(a) < 2 {
			continue
		}
		var fragment string
		if json.Unmarshal(a[1], &fragment) == nil {
			stream.WriteString(fragment)
		}
	}
	if stream.Len() == 0 && len(roots) == 0 {
		stream.Write(page)
	}
	remaining := stream.String()
	for len(remaining) > 0 {
		colon := strings.IndexByte(remaining, ':')
		newline := strings.IndexByte(remaining, '\n')
		if colon < 0 {
			break
		}
		if newline >= 0 && newline < colon {
			remaining = remaining[newline+1:]
			continue
		}
		id, payload := remaining[:colon], remaining[colon+1:]
		if strings.HasPrefix(payload, "T") {
			comma := strings.IndexByte(payload, ',')
			if comma > 1 && comma < 20 {
				size, e := strconv.ParseUint(payload[1:comma], 16, 32)
				if e == nil && int(size) <= len(payload)-comma-1 {
					records[id] = payload[comma+1 : comma+1+int(size)]
					remaining = payload[comma+1+int(size):]
					continue
				}
			}
		}
		end := strings.IndexByte(payload, '\n')
		if end < 0 {
			end = len(payload)
		}
		var v any
		if json.Unmarshal([]byte(payload[:end]), &v) == nil {
			records[id] = v
			roots = append(roots, v)
		}
		remaining = strings.TrimPrefix(payload[end:], "\n")
	}
	var resolve func(any, int) any
	resolve = func(v any, depth int) any {
		if depth > 30 {
			return nil
		}
		if s, ok := v.(string); ok && strings.HasPrefix(s, "$") {
			id := strings.TrimPrefix(strings.TrimPrefix(s, "$"), "L")
			if ref, ok := records[id]; ok {
				return resolve(ref, depth+1)
			}
		}
		return v
	}
	ids := map[string]bool{}
	malformed := false
	visits := 0
	var walk func(any, int)
	walk = func(v any, depth int) {
		visits++
		if visits > 100000 {
			malformed = true
			return
		}
		if depth > 40 {
			malformed = true
			return
		}
		switch v := resolve(v, 0).(type) {
		case []any:
			for _, child := range v {
				walk(child, depth+1)
			}
		case map[string]any:
			if slug, ok := resolve(v["slug"], 0).(string); ok && slug == "plugins" {
				id, ok := resolve(v["id"], 0).(string)
				if !ok || safeID(id) != nil {
					malformed = true
				} else {
					ids[id] = true
				}
			}
			for _, child := range v {
				walk(child, depth+1)
			}
		}
	}
	for _, root := range roots {
		walk(root, 0)
	}
	if !malformed && len(ids) == 0 {
		return "", errDraftExtensionMissing
	}
	if malformed || len(ids) != 1 {
		return "", errors.New("current Maker plugin extension schema missing or conflicting")
	}
	for id := range ids {
		return id, nil
	}
	return "", errDraftExtensionMissing
}
