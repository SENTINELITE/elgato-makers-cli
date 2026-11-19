package maker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/media"
	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

// ListingDetailsInput contains identifiers resolved from current Maker taxonomy.
// Price is a decimal USD amount, rather than an integer number of cents.
type ListingDetailsInput struct {
	Target      model.Target      `json:"target"`
	LanguageIDs []string          `json:"language_ids"`
	CategoryIDs []string          `json:"category_ids"`
	ExtraTagIDs []string          `json:"extra_tag_ids,omitempty"`
	Price       string            `json:"price"`
	IsFree      bool              `json:"is_free"`
	Links       map[string]string `json:"links"`
}
type ListingOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type ListingTagset struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Label     string          `json:"label"`
	Slug      string          `json:"slug"`
	Mandatory bool            `json:"mandatory"`
	Selection string          `json:"selection"`
	Tags      []ListingOption `json:"tags"`
}
type ListingMetadata struct {
	Tagsets        []ListingTagset `json:"tagsets"`
	LinkCategories []string        `json:"link_categories"`
	Languages      []ListingOption `json:"languages"`
	Categories     []ListingOption `json:"categories"`
}
type MediaInput struct {
	Target model.Target `json:"target"`
	Kind   string       `json:"kind"`
	Order  int          `json:"order"`
	Path   string       `json:"path"`
	SHA256 string       `json:"sha256"`
	Size   int64        `json:"size"`
}
type MediaResult struct {
	FileID       string `json:"file_id"`
	MediaID      string `json:"media_id"`
	Reconciled   bool   `json:"reconciled,omitempty"`
	Acknowledged bool   `json:"acknowledged"`
}

// ListingNotUpdatedError proves the listing mutation was never dispatched.
type ListingNotUpdatedError struct{ Cause error }

func (e *ListingNotUpdatedError) Error() string {
	return "listing update was not dispatched; complete preflight before retrying"
}
func (e *ListingNotUpdatedError) Unwrap() error    { return e.Cause }
func (e *ListingNotUpdatedError) NotUpdated() bool { return true }

type AmbiguousListingError struct{ Cause error }

func (e *AmbiguousListingError) Error() string {
	if errors.Is(e.Cause, errListingPriceCreation) {
		return "Marketplace could not create a price; details may already be saved; run listing status"
	}
	return "listing update outcome is ambiguous; reconcile listing before retrying"
}
func (e *AmbiguousListingError) Unwrap() error { return e.Cause }

var errListingPriceCreation = errors.New("Maker listing price creation failed")

// MediaNotUploadedError proves media association was never dispatched. An
// unassociated object may have been uploaded to storage and is safe to reuse.
type MediaNotUploadedError struct{ Cause error }

func (e *MediaNotUploadedError) Error() string {
	return "media association was not dispatched; complete preflight before retrying"
}
func (e *MediaNotUploadedError) Unwrap() error     { return e.Cause }
func (e *MediaNotUploadedError) NotUploaded() bool { return true }

type AmbiguousMediaError struct{ Cause error }

func (e *AmbiguousMediaError) Error() string {
	return "media association outcome is ambiguous; reconcile remote media before retrying"
}
func (e *AmbiguousMediaError) Unwrap() error { return e.Cause }

var listingPricePattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})(\.[0-9]{1,2})?$`)

func listingFields(in ListingDetailsInput) (map[string]string, error) {
	if safeID(in.Target.OrgID) != nil || safeID(in.Target.ProductID) != nil || safeID(in.Target.VariantID) != nil {
		return nil, errors.New("invalid listing target")
	}
	for label, raw := range in.Links {
		u, err := url.Parse(raw)
		if strings.TrimSpace(label) == "" || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return nil, errors.New("invalid listing additional link")
		}
	}
	price := in.Price
	if in.IsFree {
		if price != "" && price != "0" && price != "0.00" {
			return nil, errors.New("free listing cannot have a paid price")
		}
		price = "0"
	}
	if !listingPricePattern.MatchString(price) {
		return nil, errors.New("invalid decimal USD listing price")
	}
	if len(in.LanguageIDs) == 0 || len(in.CategoryIDs) == 0 {
		return nil, errors.New("listing requires languages and categories")
	}
	fields := map[string]string{"_1_price[unit_amount]": price, "_1_price[currency]": "USD", "_1_price[plan]": "once", "_1_product": in.Target.ProductID, "_1_variant": in.Target.VariantID, "_1_organization_id": in.Target.OrgID, "0": `[null,"$K1"]`}
	seen := map[string]bool{}
	for _, id := range append(append(append([]string{}, in.LanguageIDs...), in.CategoryIDs...), in.ExtraTagIDs...) {
		if safeID(id) != nil || seen[id] {
			return nil, errors.New("invalid or duplicate listing taxonomy identifier")
		}
		seen[id] = true
		fields[fmt.Sprintf("_1_tags[%d]", len(seen)-1)] = id
	}
	labels := make([]string, 0, len(in.Links))
	for label := range in.Links {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for i, label := range labels {
		prefix := fmt.Sprintf("_1_additional_links[%d]", i)
		fields[prefix+"[display_text]"] = label
		fields[prefix+"[url]"] = in.Links[label]
		fields[prefix+"[order]"] = fmt.Sprint(i)
		fields[prefix+"[id]"] = label
	}
	return fields, nil
}

// Resolve selects a current taxonomy identifier by its exact name or ID.
func (m ListingMetadata) Resolve(kind, value string) (string, error) {
	var options []ListingOption
	switch kind {
	case "language":
		options = m.Languages
	case "category":
		options = m.Categories
	default:
		return "", errors.New("invalid listing taxonomy kind")
	}
	found := ""
	for _, option := range options {
		if option.ID == value || strings.EqualFold(strings.TrimSpace(option.Name), strings.TrimSpace(value)) {
			if found != "" && found != option.ID {
				return "", errors.New("ambiguous listing taxonomy name")
			}
			found = option.ID
		}
	}
	if found == "" {
		return "", errors.New("listing taxonomy value is not available")
	}
	return found, nil
}

// discoverListingAction discovers the current deployment reference from the
// page and its same-origin Next.js scripts. References are never persisted.
func (c *Client) discoverListingAction(ctx context.Context, path, name string) (string, error) {
	actions, err := c.discoverListingCapabilities(ctx, path, []string{name}, false)
	return actions[name], err
}
func (c *Client) discoverListingCapabilities(ctx context.Context, path string, names []string, media bool) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	actions := map[string]string{}
	page, err := c.get(ctx, path)
	if err != nil {
		return actions, err
	}
	patterns := map[string]*regexp.Regexp{}
	for _, name := range names {
		patterns[name] = serverReferencePattern(regexp.QuoteMeta(name))
	}
	tokens := map[string]bool{"/api/presigned-url?file=": false, "/api/organizations/": false, "/variants/": false, "s3_file_id": false, "app-icon": false, "thumbnail": false}
	scan := func(body []byte) error {
		for name, pattern := range patterns {
			for _, match := range pattern.FindAllSubmatch(body, -1) {
				candidate := string(match[1])
				if actions[name] != "" && actions[name] != candidate {
					return errors.New("conflicting Maker listing action references")
				}
				actions[name] = candidate
			}
		}
		for token := range tokens {
			if bytes.Contains(body, []byte(token)) {
				tokens[token] = true
			}
		}
		return nil
	}
	if err = scan(page); err != nil {
		return actions, err
	}
	seen := map[string]bool{}
	for _, match := range scripts.FindAllSubmatch(page, -1) {
		u, e := url.Parse(html.UnescapeString(string(match[1])))
		if e != nil {
			continue
		}
		u = c.base.ResolveReference(u)
		if u.User != nil || u.Fragment != "" || u.Host != c.base.Host || u.Scheme != c.base.Scheme || !strings.HasPrefix(u.Path, "/_next/static/") || seen[u.String()] {
			continue
		}
		if len(seen) >= 100 {
			return actions, errors.New("too many Maker script references")
		}
		seen[u.String()] = true
		body, e := c.publicScript(ctx, u)
		if e != nil {
			return actions, e
		}
		if e = scan(body); e != nil {
			return actions, e
		}
	}
	for _, name := range names {
		if actions[name] == "" {
			return actions, errors.New("current Maker listing action reference not recognized")
		}
	}
	if media {
		for _, found := range tokens {
			if !found {
				return actions, errors.New("current Maker media upload capability is not recognized")
			}
		}
	}
	return actions, nil
}

// ValidateListingDetails performs local validation without making requests.
func ValidateListingDetails(input ListingDetailsInput) error {
	_, err := listingFields(input)
	return err
}

// walkListingPage reads structured Next Flight values, including split frames
// and references, without searching arbitrary strings for account identifiers.
func walkListingPage(page []byte, visit func(map[string]any, func(any) any) error) error {
	return walkListingPageValues(page, visit, nil)
}

func walkListingPageValues(page []byte, visit func(map[string]any, func(any) any) error, visitArray func([]any, func(any) any) error) error {
	return walkFlightPage(page, visit, visitArray, json.Unmarshal)
}

func walkFlightPage(page []byte, visit func(map[string]any, func(any) any) error, visitArray func([]any, func(any) any) error, unmarshal func([]byte, any) error) error {
	records := map[string]any{}
	var roots []any
	var plain any
	if unmarshal(page, &plain) == nil {
		roots = append(roots, plain)
	}
	var stream strings.Builder
	for _, frame := range framePattern.FindAllSubmatch(page, -1) {
		var parts []json.RawMessage
		if json.Unmarshal(frame[1], &parts) != nil || len(parts) < 2 {
			continue
		}
		var fragment string
		if json.Unmarshal(parts[1], &fragment) == nil {
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
		var value any
		if unmarshal([]byte(payload[:end]), &value) == nil {
			records[id] = value
			roots = append(roots, value)
		}
		remaining = strings.TrimPrefix(payload[end:], "\n")
	}
	resolve := func(value any) any {
		for depth := 0; depth < 30; depth++ {
			s, ok := value.(string)
			if !ok || !strings.HasPrefix(s, "$") {
				return value
			}
			ref, ok := records[strings.TrimPrefix(strings.TrimPrefix(s, "$"), "L")]
			if !ok {
				return value
			}
			value = ref
		}
		return nil
	}
	visits := 0
	var walk func(any, int) error
	walk = func(value any, depth int) error {
		visits++
		if visits > 100000 || depth > 40 {
			return errors.New("Maker listing page exceeds structure limit")
		}
		switch value := resolve(value).(type) {
		case []any:
			if visitArray != nil {
				if err := visitArray(value, resolve); err != nil {
					return err
				}
			}
			for _, child := range value {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case map[string]any:
			if err := visit(value, resolve); err != nil {
				return err
			}
			for _, child := range value {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, root := range roots {
		if err := walk(root, 0); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) Metadata(ctx context.Context, org string) (ListingMetadata, error) {
	var out ListingMetadata
	if safeID(org) != nil {
		return out, errors.New("invalid Maker organization")
	}
	page, err := c.get(ctx, "/"+org+"/create/plugins")
	if err != nil {
		return out, err
	}
	seen := map[string]string{}
	err = walkListingPage(page, func(m map[string]any, resolve func(any) any) error {
		if raw, ok := m["additionalLinkCategories"]; ok {
			values, ok := resolve(raw).([]any)
			if !ok {
				return errors.New("unrecognized additional link categories")
			}
			for _, v := range values {
				label, ok := resolve(v).(string)
				if !ok || label == "" {
					return errors.New("invalid additional link category")
				}
				if !containsString(out.LinkCategories, label) {
					out.LinkCategories = append(out.LinkCategories, label)
				}
			}
		}
		raw, ok := m["tagsets"]
		if !ok {
			return nil
		}
		groups, ok := resolve(raw).([]any)
		if !ok {
			return errors.New("unrecognized Maker taxonomy")
		}
		for _, rawGroup := range groups {
			g, ok := resolve(rawGroup).(map[string]any)
			if !ok {
				return errors.New("invalid Maker taxonomy group")
			}
			str := func(k string) string { v, _ := resolve(g[k]).(string); return v }
			group := ListingTagset{ID: str("id"), Name: str("name"), Label: str("label"), Slug: str("slug"), Selection: str("selection")}
			group.Mandatory, _ = resolve(g["mandatory"]).(bool)
			if safeID(group.ID) != nil || group.Name == "" || group.Slug == "" {
				return errors.New("invalid Maker taxonomy identity")
			}
			tags, ok := resolve(g["tags"]).([]any)
			if !ok {
				return errors.New("invalid Maker taxonomy tags")
			}
			for _, rawTag := range tags {
				tag, ok := resolve(rawTag).(map[string]any)
				if !ok {
					return errors.New("invalid Maker taxonomy tag")
				}
				id, _ := resolve(tag["id"]).(string)
				name, _ := resolve(tag["name"]).(string)
				if safeID(id) != nil || name == "" {
					return errors.New("invalid Maker taxonomy option")
				}
				group.Tags = append(group.Tags, ListingOption{ID: id, Name: name})
			}
			fingerprint, _ := json.Marshal(group)
			if prior, exists := seen[group.ID]; exists {
				if prior != string(fingerprint) {
					return errors.New("conflicting Maker taxonomy group")
				}
				continue
			}
			seen[group.ID] = string(fingerprint)
			out.Tagsets = append(out.Tagsets, group)
			switch group.Slug {
			case "language":
				out.Languages = append(out.Languages, group.Tags...)
			case "type":
				out.Categories = append(out.Categories, group.Tags...)
			}
		}
		return nil
	})
	if err != nil {
		return ListingMetadata{}, err
	}
	if len(out.Languages) == 0 || len(out.Categories) == 0 {
		return ListingMetadata{}, errors.New("current Maker listing taxonomy is not recognized")
	}
	return out, nil
}
func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func (c *Client) PreflightListing(ctx context.Context, org string) error {
	if safeID(org) != nil {
		return errors.New("invalid Maker organization")
	}
	if _, err := c.Metadata(ctx, org); err != nil {
		return err
	}
	if _, _, err := c.discoverDraft(ctx, "/"+org+"/create/plugins", ""); err != nil {
		return err
	}
	_, err := c.discoverListingCapabilities(ctx, "/"+org+"/create/plugins", []string{"updateProductDetails", "uploadAux", "createVersion", "getIsSdVersionValid"}, true)
	return err
}

func (c *Client) UpdateListing(ctx context.Context, input ListingDetailsInput) (err error) {
	dispatched := false
	defer func() {
		if err != nil && !dispatched {
			err = &ListingNotUpdatedError{Cause: err}
		}
	}()
	if c.readOnly {
		return ErrReadOnly
	}
	if _, err = listingFields(input); err != nil {
		return err
	}
	metadata, err := c.Metadata(ctx, input.Target.OrgID)
	if err != nil {
		return err
	}
	for _, group := range []struct {
		ids     []string
		options []ListingOption
	}{{input.LanguageIDs, metadata.Languages}, {input.CategoryIDs, metadata.Categories}} {
		for _, id := range group.ids {
			found := false
			for _, option := range group.options {
				if option.ID == id {
					found = true
					break
				}
			}
			if !found {
				return errors.New("listing taxonomy identifier is not available")
			}
		}
	}
	availableExtra := map[string]bool{}
	for _, group := range metadata.Tagsets {
		if group.Slug != "language" && group.Slug != "type" {
			for _, option := range group.Tags {
				availableExtra[option.ID] = true
			}
		}
	}
	for _, id := range input.ExtraTagIDs {
		if !availableExtra[id] {
			return errors.New("listing compatibility tag is not available")
		}
	}
	for label := range input.Links {
		if !containsString(metadata.LinkCategories, label) {
			return errors.New("listing additional link category is not available")
		}
	}
	// Preserve feature and OS tags that are outside the requested language/type
	// groups; changing these would silently remove existing compatibility claims.
	page, err := c.get(ctx, "/"+input.Target.OrgID+"/products/"+input.Target.ProductID)
	if err != nil {
		return err
	}
	initializePrice, err := validateListingPricePage(ctx, page, input)
	if err != nil {
		return err
	}
	productFound := false
	variantFound := false
	preservedLinks := map[string]string{}
	replaceable := map[string]bool{}
	for _, option := range append(append([]ListingOption{}, metadata.Languages...), metadata.Categories...) {
		replaceable[option.ID] = true
	}
	err = walkListingPage(page, func(m map[string]any, resolve func(any) any) error {
		id, _ := resolve(m["id"]).(string)
		if id != input.Target.ProductID {
			return nil
		}
		rawVariants, ok := m["variants"]
		if !ok {
			return nil
		}
		variants, ok := resolve(rawVariants).([]any)
		if !ok {
			return errors.New("invalid listing product variants")
		}
		productFound = true
		if input.Links == nil {
			if raw, exists := m["additional_links"]; exists {
				links, ok := resolve(raw).([]any)
				if !ok {
					return errors.New("listing additional links schema is not recognized")
				}
				for i, rawLink := range links {
					link, ok := resolve(rawLink).(map[string]any)
					if !ok {
						return errors.New("invalid listing additional link")
					}
					prefix := fmt.Sprintf("_1_additional_links[%d]", i)
					for _, key := range []string{"id", "display_text", "url"} {
						if rawValue, exists := link[key]; exists && resolve(rawValue) != nil {
							value, ok := resolve(rawValue).(string)
							if !ok {
								return errors.New("invalid listing additional link field")
							}
							preservedLinks[prefix+"["+key+"]"] = value
						}
					}
					if rawOrder, exists := link["order"]; exists {
						order, ok := resolve(rawOrder).(float64)
						if !ok || order < 0 || order != float64(int(order)) {
							return errors.New("invalid listing link order")
						}
						preservedLinks[prefix+"[order]"] = strconv.Itoa(int(order))
					}
				}
			}
		}
		for _, rv := range variants {
			v, ok := resolve(rv).(map[string]any)
			if !ok {
				return errors.New("invalid listing variant")
			}
			vid, _ := resolve(v["id"]).(string)
			if vid == input.Target.VariantID {
				remoteFree, recognized := resolve(v["is_free"]).(bool)
				if !recognized {
					return errors.New("existing listing pricing is not recognized")
				}
				if remoteFree != input.IsFree {
					versions, recognized := resolve(v["versions"]).([]any)
					status, _ := resolve(m["status"]).(string)
					if !recognized || len(versions) > 0 || !strings.EqualFold(status, "DRAFT") {
						return errors.New("cannot switch between paid and free after product submission")
					}
				}
				if err := listingVariantIdentity(ctx, input.Target, m, v, resolve); err != nil {
					return err
				}
				variantFound = true
			}
		}
		tags, ok := resolve(m["tags"]).([]any)
		if !ok {
			return errors.New("listing product tags are not recognized")
		}
		for _, raw := range tags {
			tag, ok := resolve(raw).(string)
			if !ok || safeID(tag) != nil {
				return errors.New("invalid listing product tag")
			}
			if !replaceable[tag] && !containsString(input.ExtraTagIDs, tag) {
				input.ExtraTagIDs = append(input.ExtraTagIDs, tag)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !productFound || !variantFound {
		return errors.New("listing target absent from recognized Maker response")
	}
	fields, err := listingFields(input)
	if err != nil {
		return err
	}
	for key, value := range preservedLinks {
		fields[key] = value
	}
	path := "/" + input.Target.OrgID + "/create/plugins"
	var action string
	if initializePrice {
		action, err = c.discoverListingAction(ctx, path, "updateProductDetails")
	} else {
		path = "/" + input.Target.OrgID + "/products/" + input.Target.ProductID
		action, err = c.discoverMetadataListingAction(ctx, path)
		for key := range fields {
			if strings.HasPrefix(key, "_1_price[") || key == "_1_product" || key == "_1_variant" || key == "_1_organization_id" {
				delete(fields, key)
			}
		}
		fields["_1_id"] = input.Target.ProductID
		fields["_1_organization"] = input.Target.OrgID
	}
	if err != nil {
		return err
	}
	fresh, err := c.get(ctx, "/"+input.Target.OrgID+"/products/"+input.Target.ProductID)
	if err != nil {
		return err
	}
	freshInitialize, err := validateListingPricePage(ctx, fresh, input)
	if err != nil {
		return err
	}
	if freshInitialize != initializePrice {
		return errors.New("listing pricing changed during preflight; reconcile before retrying")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	dispatched = true
	result, err := c.multipart(ctx, path, action, fields)
	if err != nil {
		return &AmbiguousListingError{Cause: err}
	}
	if result["success"] != true {
		if result["message"] == "There was an error creating a price for this product." {
			return &AmbiguousListingError{Cause: errListingPriceCreation}
		}
		return &AmbiguousListingError{Cause: errors.New("Maker listing result did not confirm success")}
	}
	return nil
}

func (c *Client) ReadUploadTags(ctx context.Context, folderID string) (map[string][]string, error) {
	if safeID(folderID) != nil {
		return nil, errors.New("invalid Maker upload folder")
	}
	b, err := c.get(ctx, "/api/upload-aux/tags/"+folderID)
	if err != nil {
		return nil, err
	}
	var response struct {
		Tags map[string]json.RawMessage `json:"tags"`
	}
	if json.Unmarshal(b, &response) != nil || response.Tags == nil {
		return nil, errors.New("unrecognized uploaded package tags")
	}
	tags := make(map[string][]string, len(response.Tags))
	for label, raw := range response.Tags {
		if strings.TrimSpace(label) == "" {
			return nil, errors.New("invalid uploaded package tag group")
		}
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return nil, errors.New("unrecognized uploaded package tags")
		}
		var tokens []string
		switch value := value.(type) {
		case string:
			tokens = []string{value}
		case []any:
			tokens = make([]string, 0, len(value))
			for _, rawToken := range value {
				token, ok := rawToken.(string)
				if !ok {
					return nil, errors.New("invalid uploaded package tag value")
				}
				tokens = append(tokens, token)
			}
		default:
			return nil, errors.New("unrecognized uploaded package tag value")
		}
		for _, token := range tokens {
			if strings.TrimSpace(token) == "" {
				return nil, errors.New("invalid uploaded package tag value")
			}
		}
		tags[label] = tokens
	}
	return tags, nil
}
func (m ListingMetadata) ResolveUploadTags(tags map[string][]string) ([]string, error) {
	var ids []string
	for label, tokens := range tags {
		var group *ListingTagset
		for i := range m.Tagsets {
			if m.Tagsets[i].Label == label || m.Tagsets[i].Name == label {
				if group != nil && group.ID != m.Tagsets[i].ID {
					return nil, errors.New("ambiguous uploaded tag group")
				}
				group = &m.Tagsets[i]
			}
		}
		if group == nil {
			return nil, errors.New("uploaded package tag group is unavailable")
		}
		for _, token := range tokens {
			if strings.TrimSpace(token) == "" {
				return nil, errors.New("invalid uploaded package tag value")
			}
			found := ""
			for _, option := range group.Tags {
				if option.ID == token || option.Name == token {
					if safeID(option.ID) != nil {
						return nil, errors.New("invalid uploaded package tag identifier")
					}
					if found != "" && found != option.ID {
						return nil, errors.New("ambiguous uploaded tag")
					}
					found = option.ID
				}
			}
			if found == "" {
				return nil, errors.New("uploaded package tag is unavailable")
			}
			if group.Slug == "language" || group.Slug == "type" {
				continue
			}
			if !containsString(ids, found) {
				ids = append(ids, found)
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func mediaIdentity(input MediaInput) (string, string, error) {
	if safeID(input.Target.OrgID) != nil || safeID(input.Target.ProductID) != nil || safeID(input.Target.VariantID) != nil {
		return "", "", errors.New("invalid media target")
	}
	if input.Kind != "icon" && input.Kind != "thumbnail" && input.Kind != "gallery" {
		return "", "", errors.New("invalid listing media kind")
	}
	if input.Order < 1 || input.Order > 10 || (input.Kind != "gallery" && input.Order != 1) {
		return "", "", errors.New("invalid listing media order")
	}
	digest, err := hex.DecodeString(input.SHA256)
	if err != nil || len(digest) != sha256.Size || input.Size < 1 {
		return "", "", errors.New("invalid listing media fingerprint")
	}
	ext := strings.ToLower(filepath.Ext(input.Path))
	switch ext {
	case ".png", ".jpg", ".jpeg":
	case ".mp4":
		if input.Kind != "gallery" {
			return "", "", errors.New("MP4 is supported only in the gallery")
		}
	default:
		return "", "", errors.New("unsupported listing image format")
	}
	sum := sha256.Sum256([]byte(input.Kind + ":" + strconv.Itoa(input.Order) + ":" + strings.ToLower(input.SHA256)))
	sum[6] = (sum[6] & 15) | 0x50
	sum[8] = (sum[8] & 63) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
	file := input.Target.OrgID + "/" + input.Target.ProductID + "/"
	path := "/api/organizations/" + input.Target.OrgID + "/products/" + input.Target.ProductID
	if input.Kind == "gallery" {
		file += input.Target.VariantID + "/"
		path += "/variants/" + input.Target.VariantID
	}
	file += id + ext
	return file, path + "/media", nil
}

func (c *Client) UploadMedia(ctx context.Context, input MediaInput) (out MediaResult, err error) {
	dispatched := false
	defer func() {
		if err != nil && !dispatched {
			err = &MediaNotUploadedError{Cause: err}
		}
	}()
	if c.readOnly {
		return out, ErrReadOnly
	}
	fileID, path, err := mediaIdentity(input)
	if err != nil {
		return out, err
	}
	out.FileID = fileID
	asset, err := media.Inspect(input.Path, input.Kind)
	if err != nil {
		return out, err
	}
	if asset.Size != input.Size || asset.SHA256 != strings.ToLower(input.SHA256) {
		return out, errors.New("listing media fingerprint changed")
	}
	f, err := os.Open(input.Path)
	if err != nil {
		return out, errors.New("cannot open listing image")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != input.Size {
		return out, errors.New("listing image size changed")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return out, errors.New("cannot hash listing image")
	}
	if hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(input.SHA256) {
		return out, errors.New("listing image digest changed")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return out, errors.New("cannot read listing image")
	}
	slots, err := c.mediaSlots(ctx, input)
	if err != nil {
		return out, err
	}
	method := "POST"
	if len(slots) == 1 {
		method = "PATCH"
		path += "/" + slots[0]
	}
	b, err := c.get(ctx, "/api/presigned-url?file="+url.QueryEscape("media/"+fileID))
	if err != nil {
		return out, err
	}
	var signed struct {
		URL string `json:"presignedUrl"`
	}
	if json.Unmarshal(b, &signed) != nil {
		return out, errors.New("unrecognized presigned media response")
	}
	u, err := url.Parse(signed.URL)
	if err != nil || u.User != nil || u.Fragment != "" {
		return out, errors.New("invalid media upload destination")
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if !(u.Scheme == "https" && u.Host == "s3.us-east-1.amazonaws.com" && u.Path == "/production.maker.elgato.com/media/"+fileID) && !(c.local && local && (u.Scheme == "http" || u.Scheme == "https")) {
		return out, errors.New("media upload destination is not allowed")
	}
	if _, err = c.request(ctx, "PUT", signed.URL, asset.MIME, "", f, false); err != nil {
		return out, err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{"s3_file_id": fileID, "file_type": "media", "order": strconv.Itoa(input.Order)}
	if asset.MIME == "video/mp4" {
		fields["file_type"] = "video"
	}
	if input.Kind != "gallery" {
		fields["media_type"] = input.Kind
		if input.Kind == "icon" {
			fields["media_type"] = "app-icon"
		}
	}
	for k, v := range fields {
		if err = writer.WriteField(k, v); err != nil {
			return out, errors.New("cannot encode media association")
		}
	}
	if err = writer.Close(); err != nil {
		return out, errors.New("cannot encode media association")
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	dispatched = true
	b, err = c.request(ctx, method, c.endpoint(path), writer.FormDataContentType(), "", &body, true)
	if err != nil {
		return out, &AmbiguousMediaError{Cause: err}
	}
	var result struct {
		ID      string `json:"id"`
		MediaID string `json:"media_id"`
	}
	if json.Unmarshal(b, &result) != nil {
		return out, &AmbiguousMediaError{Cause: errors.New("unrecognized media association response")}
	}
	out.MediaID = result.ID
	if out.MediaID == "" {
		out.MediaID = result.MediaID
	}
	if safeID(out.MediaID) != nil {
		return out, &AmbiguousMediaError{Cause: errors.New("media association did not confirm identity")}
	}
	out.Acknowledged = true
	return out, nil
}

func ValidateMediaInput(input MediaInput) error { _, _, err := mediaIdentity(input); return err }

// mediaSlots reads only identifiers and positions. Maker exposes transformed
// CDN URLs, which cannot prove that an unacknowledged upload has been applied.
func (c *Client) mediaSlots(ctx context.Context, input MediaInput) ([]string, error) {
	page, err := c.get(ctx, "/"+input.Target.OrgID+"/products/"+input.Target.ProductID)
	if err != nil {
		return nil, err
	}
	var slots []string
	productFound, variantFound := false, false
	err = walkListingPage(page, func(m map[string]any, resolve func(any) any) error {
		id, _ := resolve(m["id"]).(string)
		if id != input.Target.ProductID {
			return nil
		}
		variants, ok := resolve(m["variants"]).([]any)
		if !ok {
			return nil
		}
		productFound = true
		var rows []any
		for _, raw := range variants {
			v, ok := resolve(raw).(map[string]any)
			if !ok {
				return errors.New("invalid media variant")
			}
			vid, _ := resolve(v["id"]).(string)
			if vid != input.Target.VariantID {
				continue
			}
			variantFound = true
			if err := listingVariantIdentity(ctx, input.Target, m, v, resolve); err != nil {
				return err
			}
			if input.Kind == "gallery" {
				var ok bool
				rows, ok = resolve(v["previews"]).([]any)
				if !ok {
					return errors.New("gallery media schema is not recognized")
				}
			}
		}
		if input.Kind != "gallery" {
			var ok bool
			rows, ok = resolve(m["media"]).([]any)
			if !ok {
				return errors.New("product media schema is not recognized")
			}
		}
		kind := input.Kind
		if kind == "icon" {
			kind = "app-icon"
		}
		if kind == "gallery" {
			kind = "preview"
		}
		for _, raw := range rows {
			row, ok := resolve(raw).(map[string]any)
			if !ok {
				return errors.New("invalid listing media record")
			}
			mediaKind, _ := resolve(row["media_type"]).(string)
			if input.Kind == "gallery" {
				order, ok := resolve(row["order"]).(float64)
				if !ok || order < 1 || order != float64(int(order)) {
					return errors.New("invalid gallery media order")
				}
				if order != float64(input.Order) {
					continue
				}
				existingKind, err := galleryMediaKind(row, resolve)
				if err != nil {
					return err
				}
				desiredKind := "image"
				if strings.EqualFold(filepath.Ext(input.Path), ".mp4") {
					desiredKind = "video"
				}
				if existingKind != desiredKind {
					return errors.New("gallery image/video type change requires explicit remote replacement; resolve remote artwork before retrying")
				}
			} else if mediaKind != kind {
				continue
			}
			mediaID, _ := resolve(row["id"]).(string)
			if safeID(mediaID) != nil {
				return errors.New("invalid listing media identity")
			}
			if !containsString(slots, mediaID) {
				slots = append(slots, mediaID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !productFound || !variantFound {
		return nil, errors.New("media target absent from recognized Maker response")
	}
	if len(slots) > 1 {
		return nil, errors.New("multiple remote images occupy the requested media slot; resolve conflicting media before retrying")
	}
	return slots, nil
}
func (c *Client) ReconcileMedia(ctx context.Context, input MediaInput, prior MediaResult) (MediaResult, error) {
	fileID, _, err := mediaIdentity(input)
	if err != nil {
		return MediaResult{}, err
	}
	if prior.FileID != "" && prior.FileID != fileID {
		return MediaResult{}, errors.New("media journal fingerprint differs from current image")
	}
	slots, err := c.mediaSlots(ctx, input)
	if err != nil {
		return MediaResult{}, err
	}
	out := MediaResult{FileID: fileID}
	if prior.MediaID == "" || !prior.Acknowledged {
		return out, nil
	}
	if len(slots) == 1 && slots[0] == prior.MediaID {
		out.MediaID = prior.MediaID
		out.Reconciled = true
		out.Acknowledged = true
		return out, nil
	}
	return out, errors.New("acknowledged media is absent from its remote slot")
}

// ValidateExistingListingPlan detects incompatible pricing and gallery layouts
// before a workflow starts writing. It never deletes surplus remote artwork.
func (c *Client) ValidateExistingListingPlan(ctx context.Context, target model.Target, isFree bool, galleryCount int) error {
	if safeID(target.OrgID) != nil || safeID(target.ProductID) != nil || safeID(target.VariantID) != nil || galleryCount < 3 || galleryCount > 10 {
		return errors.New("invalid existing listing plan")
	}
	page, err := c.get(ctx, "/"+target.OrgID+"/products/"+target.ProductID)
	if err != nil {
		return err
	}
	found := false
	err = walkListingPage(page, func(m map[string]any, resolve func(any) any) error {
		id, _ := resolve(m["id"]).(string)
		if id != target.ProductID {
			return nil
		}
		variants, ok := resolve(m["variants"]).([]any)
		if !ok {
			return nil
		}
		for _, raw := range variants {
			v, ok := resolve(raw).(map[string]any)
			if !ok {
				return errors.New("invalid listing plan variant")
			}
			vid, _ := resolve(v["id"]).(string)
			if vid != target.VariantID {
				continue
			}
			found = true
			if err := listingVariantIdentity(ctx, target, m, v, resolve); err != nil {
				return err
			}
			remoteFree, ok := resolve(v["is_free"]).(bool)
			if !ok {
				return errors.New("existing listing pricing is not recognized")
			}
			versions, ok := resolve(v["versions"]).([]any)
			if !ok {
				return errors.New("existing listing version state is not recognized")
			}
			status, _ := resolve(m["status"]).(string)
			if remoteFree != isFree && (len(versions) > 0 || !strings.EqualFold(status, "DRAFT")) {
				return errors.New("cannot switch between paid and free after product submission")
			}
			previews, ok := resolve(v["previews"]).([]any)
			if !ok {
				return errors.New("existing gallery schema is not recognized")
			}
			orders := map[int]bool{}
			for _, rawPreview := range previews {
				preview, ok := resolve(rawPreview).(map[string]any)
				if !ok {
					return errors.New("invalid existing gallery item")
				}
				if _, err := galleryMediaKind(preview, resolve); err != nil {
					return err
				}
				order, ok := resolve(preview["order"]).(float64)
				if !ok || order < 1 || order != float64(int(order)) || order > float64(galleryCount) {
					return errors.New("existing gallery has artwork outside the configured plan; resolve remote artwork before retrying")
				}
				if orders[int(order)] {
					return errors.New("existing gallery has conflicting artwork orders; resolve remote artwork before retrying")
				}
				orders[int(order)] = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("listing plan target absent from recognized Maker response")
	}
	return nil
}

// galleryMediaKind recognizes the fields consumed by Maker's media page.
// CDN URLs are inspected as metadata only and are never fetched.
func galleryMediaKind(row map[string]any, resolve func(any) any) (string, error) {
	id, _ := resolve(row["id"]).(string)
	kind, _ := resolve(row["media_type"]).(string)
	rawVideo, recognized := row["video"]
	if safeID(id) != nil || kind != "preview" || !recognized {
		return "", errors.New("existing gallery contains unsupported media; resolve remote artwork before retrying")
	}
	video := resolve(rawVideo)
	if video == nil {
		return "image", nil
	}
	details, ok := video.(map[string]any)
	if !ok {
		return "", errors.New("existing gallery video schema is not recognized")
	}
	file, _ := resolve(details["file_cdn"]).(string)
	u, err := url.Parse(file)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Path == "" || u.Path == "/" {
		return "", errors.New("existing gallery video URL is not recognized")
	}
	return "video", nil
}

// ValidateGallery checks the entire desired gallery before staging any writes.
// Type changes need explicit remote replacement because this client never
// silently deletes an existing image or video.
func (c *Client) ValidateGallery(ctx context.Context, target model.Target, inputs []MediaInput) error {
	if len(inputs) < 3 || len(inputs) > 10 {
		return errors.New("gallery requires 3 to 10 media assets")
	}
	for i, input := range inputs {
		if input.Kind != "gallery" || input.Order != i+1 || input.Target.OrgID != target.OrgID || input.Target.ProductID != target.ProductID || input.Target.VariantID != target.VariantID || input.Target.UUID != target.UUID {
			return errors.New("gallery inputs do not match the sequential target plan")
		}
		if err := ValidateMediaInput(input); err != nil {
			return err
		}
		asset, err := media.Inspect(input.Path, input.Kind)
		if err != nil {
			return err
		}
		if asset.Size != input.Size || asset.SHA256 != strings.ToLower(input.SHA256) {
			return errors.New("listing media fingerprint changed")
		}
		if _, err := c.mediaSlots(ctx, input); err != nil {
			return err
		}
	}
	return nil
}
