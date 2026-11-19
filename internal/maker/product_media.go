package maker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/media"
)

type ProductMediaAsset struct {
	ID                    string  `json:"id"`
	VariantID             string  `json:"variant_id,omitempty"`
	Role                  string  `json:"role"`
	Order                 int     `json:"order"`
	Kind                  string  `json:"kind"`
	PublicURL             string  `json:"public_url,omitempty"`
	URLStatus             string  `json:"url_status"`
	SourceKind            string  `json:"source_kind"`
	MIME                  *string `json:"mime_type"`
	Width                 *int    `json:"width"`
	Height                *int    `json:"height"`
	ByteSize              *int64  `json:"byte_size"`
	MetadataSource        string  `json:"metadata_source"`
	UnknownReason         string  `json:"unknown_reason,omitempty"`
	OriginalMetadataKnown bool    `json:"original_metadata_known"`
	ObservedAt            string  `json:"observed_at"`
}
type ProductMedia struct {
	ProductID           string              `json:"product_id"`
	Icon                *ProductMediaAsset  `json:"icon"`
	Thumbnail           *ProductMediaAsset  `json:"thumbnail"`
	Gallery             []ProductMediaAsset `json:"gallery"`
	SourceKind          string              `json:"source_kind"`
	ObservedAt          string              `json:"observed_at"`
	InspectionRequested bool                `json:"inspection_requested"`
	InspectionRequests  int                 `json:"inspection_requests"`
	Warnings            []string            `json:"warnings"`
}

// publicMediaURL only admits the reviewed public CDN origin and media prefix.
// No query is returned, even on the allowed host: it may contain storage secrets.
func publicMediaURL(raw string) string {
	u, e := url.Parse(raw)
	if e != nil || len(raw) > 4096 || u.Scheme != "https" || u.Host != "mp-cdn.elgato.com" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, "/media/") || strings.Contains(u.Path, "\\") {
		return ""
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return ""
		}
	}
	return u.String()
}

// ProductMedia reads the same owned product page as existing media reconciliation.
// It never reads presigned storage URLs and never uploads or changes a listing.
func (c *Client) ProductMedia(ctx context.Context, org, product string, inspect bool) (ProductMedia, error) {
	out := ProductMedia{ProductID: product, Gallery: []ProductMediaAsset{}, Warnings: []string{}, SourceKind: "authenticated_maker_product_page", InspectionRequested: inspect, ObservedAt: time.Now().UTC().Format(time.RFC3339)}
	if safeID(org) != nil || safeID(product) != nil || len(org) > 128 || len(product) > 128 || org == "." || org == ".." || product == "." || product == ".." {
		return out, errors.New("invalid media resource identifier")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	page, e := c.get(ctx, "/"+org+"/products/"+product)
	if e != nil {
		return out, e
	}
	found := false
	var productRecord string
	e = walkListingPage(page, func(m map[string]any, resolve func(any) any) error {
		id, _ := resolve(m["id"]).(string)
		variants, ok := resolve(m["variants"]).([]any)
		if id != product || !ok {
			return nil
		}
		record, _ := json.Marshal(m)
		if found {
			if string(record) != productRecord {
				return errors.New("ambiguous product media response")
			}
			return nil
		}
		found = true
		productRecord = string(record)
		if len(variants) > 32 {
			return errors.New("media variant limit exceeded")
		}
		if v, exists := m["organization_id"]; exists && resolve(v) != org {
			return errors.New("media product ownership mismatch")
		}
		asset := func(row map[string]any, role, variant string, order int) (ProductMediaAsset, error) {
			mid, _ := resolve(row["id"]).(string)
			if safeID(mid) != nil || len(mid) > 256 {
				return ProductMediaAsset{}, errors.New("invalid media identity")
			}
			a := ProductMediaAsset{ID: mid, VariantID: variant, Role: role, Order: order, Kind: "image", SourceKind: "provider_media_record", MetadataSource: "not_inspected", UnknownReason: "Original upload metadata is not supplied by the verified page contract; rendition bytes were not inspected.", ObservedAt: out.ObservedAt, URLStatus: "unavailable_or_not_allowlisted"}
			raw, _ := resolve(row["file"]).(string)
			if role == "gallery" {
				kind, err := galleryMediaKind(row, resolve)
				if err != nil {
					return a, err
				}
				a.Kind = kind
				if kind == "video" {
					details, _ := resolve(row["video"]).(map[string]any)
					raw, _ = resolve(details["file_cdn"]).(string)
				}
			}
			a.PublicURL = publicMediaURL(raw)
			if a.PublicURL != "" {
				a.SourceKind = "cdn_rendition"
				a.URLStatus = "safe_public_cdn_url"
			}
			return a, nil
		}
		rows, ok := resolve(m["media"]).([]any)
		if !ok || len(rows) > 2 {
			return errors.New("product media schema is not recognized")
		}
		for _, raw := range rows {
			row, ok := resolve(raw).(map[string]any)
			if !ok {
				return errors.New("invalid product media record")
			}
			kind, _ := resolve(row["media_type"]).(string)
			role := ""
			switch kind {
			case "app-icon":
				role = "icon"
			case "thumbnail":
				role = "thumbnail"
			default:
				return errors.New("unsupported product media kind")
			}
			a, err := asset(row, role, "", 1)
			if err != nil {
				return err
			}
			if role == "icon" {
				if out.Icon != nil {
					return errors.New("duplicate icon")
				}
				out.Icon = &a
			} else {
				if out.Thumbnail != nil {
					return errors.New("duplicate thumbnail")
				}
				out.Thumbnail = &a
			}
		}
		seenVariants := map[string]bool{}
		for _, raw := range variants {
			v, ok := resolve(raw).(map[string]any)
			if !ok {
				return errors.New("invalid media variant")
			}
			vid, _ := resolve(v["id"]).(string)
			if safeID(vid) != nil || len(vid) > 256 || seenVariants[vid] {
				return errors.New("invalid media variant identity")
			}
			seenVariants[vid] = true
			if pid, exists := v["product_id"]; exists && resolve(pid) != product {
				return errors.New("media variant ownership mismatch")
			}
			previews, ok := resolve(v["previews"]).([]any)
			if !ok || len(previews) > 10 {
				return errors.New("gallery schema is not recognized")
			}
			seen := map[int]bool{}
			for _, raw := range previews {
				row, ok := resolve(raw).(map[string]any)
				if !ok {
					return errors.New("invalid gallery record")
				}
				order, ok := resolve(row["order"]).(float64)
				if !ok || order < 1 || order > 10 || order != float64(int(order)) || seen[int(order)] {
					return errors.New("invalid gallery order")
				}
				seen[int(order)] = true
				a, err := asset(row, "gallery", vid, int(order))
				if err != nil {
					return err
				}
				out.Gallery = append(out.Gallery, a)
			}
		}
		return nil
	})
	if e != nil {
		return ProductMedia{}, e
	}
	if !found {
		return ProductMedia{}, errors.New("product media absent from recognized Maker response")
	}
	sort.SliceStable(out.Gallery, func(i, j int) bool {
		a, b := out.Gallery[i], out.Gallery[j]
		if a.VariantID != b.VariantID {
			return a.VariantID < b.VariantID
		}
		return a.Order < b.Order
	})
	out.Warnings = append(out.Warnings, "URLs and measured bytes describe public CDN renditions, not original uploads. Missing metadata is unknown; this does not validate new listing files.")
	if inspect {
		c.inspectProductMedia(ctx, &out)
	}
	return out, nil
}

func (c *Client) inspectProductMedia(ctx context.Context, out *ProductMedia) {
	// The isolated client sends no Maker cookies, credentials or Referer. Redirects
	// are not followed, so an allowed CDN cannot redirect into a private network.
	h := &http.Client{Timeout: 5 * time.Second, Transport: c.http.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	budget := int64(24 << 20)
	assets := []*ProductMediaAsset{out.Icon, out.Thumbnail}
	for i := range out.Gallery {
		assets = append(assets, &out.Gallery[i])
	}
	for _, a := range assets {
		if a == nil {
			continue
		}
		if a.PublicURL == "" {
			a.UnknownReason = "The provider URL is absent or outside the reviewed public CDN allowlist; no bytes were fetched."
			continue
		}
		if out.InspectionRequests >= 12 || budget <= 0 || ctx.Err() != nil {
			a.UnknownReason = "Rendition inspection request, byte or time budget exhausted."
			continue
		}
		limit := int64(10 << 20)
		if a.Role == "icon" {
			limit = 2 << 20
		}
		if a.Role == "thumbnail" {
			limit = 5 << 20
		}
		if limit > budget {
			limit = budget
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, a.PublicURL, nil)
		if e != nil {
			continue
		}
		req.Header.Set("Accept", "image/png,image/jpeg,video/mp4")
		req.Header.Set("Accept-Encoding", "identity")
		out.InspectionRequests++
		resp, e := h.Do(req)
		if e != nil {
			a.UnknownReason = "Public rendition request failed."
			continue
		}
		if resp.StatusCode != http.StatusOK || resp.ContentLength > limit || resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
			resp.Body.Close()
			a.UnknownReason = "Public rendition response was redirected, unsupported or exceeded the inspection byte limit."
			continue
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		budget -= int64(len(b))
		if e != nil || int64(len(b)) > limit {
			a.UnknownReason = "Public rendition response exceeded the inspection byte limit or could not be read."
			continue
		}
		mime, w, height, e := media.InspectRendition(b, a.Kind == "video")
		if e != nil {
			a.UnknownReason = "Unsupported or malformed rendition; metadata remains unknown."
			continue
		}
		size := int64(len(b))
		a.MIME = &mime
		a.Width = &w
		a.Height = &height
		a.ByteSize = &size
		a.MetadataSource = "bounded_public_cdn_bytes"
		a.UnknownReason = "Original upload metadata remains unknown. MP4 inspection validates container metadata, not codec playback."
	}
}
