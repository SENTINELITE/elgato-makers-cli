package maker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

type DeleteResult struct {
	Deleted   bool   `json:"deleted"`
	ProductID string `json:"product_id"`
}

// NotDeletedError means no deletion request was dispatched.
type NotDeletedError struct{ Cause error }

func (e *NotDeletedError) Error() string {
	return "deletion was not dispatched; complete preflight before retrying"
}
func (e *NotDeletedError) Unwrap() error    { return e.Cause }
func (e *NotDeletedError) NotDeleted() bool { return true }

// AmbiguousDeleteError requires read-only reconciliation before any manual retry.
type AmbiguousDeleteError struct{ Cause error }

func (e *AmbiguousDeleteError) Error() string {
	return "deletion outcome is ambiguous; reconcile the product catalog before retrying"
}
func (e *AmbiguousDeleteError) Unwrap() error { return e.Cause }

var deleteActionPattern = serverReferencePattern(`deleteProductAction`)

func (c *Client) discoverDeleteAction(ctx context.Context, t model.Target) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	action := ""
	seen := map[string]bool{}
	queue := []string{c.endpoint("/" + t.OrgID + "/products/" + t.ProductID), c.endpoint("/" + t.OrgID + "/products")}
	for len(queue) > 0 {
		raw := queue[0]
		queue = queue[1:]
		if seen[raw] {
			continue
		}
		if len(seen) >= 102 {
			return "", errors.New("too many Maker script references")
		}
		seen[raw] = true
		u, _ := url.Parse(raw)
		var b []byte
		var e error
		if u != nil && strings.HasPrefix(u.Path, "/_next/static/") {
			b, e = c.publicScript(ctx, u)
		} else {
			b, e = c.request(ctx, http.MethodGet, raw, "", "", nil, true)
		}
		if e != nil {
			return "", e
		}
		for _, m := range deleteActionPattern.FindAllSubmatch(b, -1) {
			found := string(m[1])
			if action != "" && action != found {
				return "", errors.New("conflicting Maker deletion action references")
			}
			action = found
		}
		for _, m := range scripts.FindAllSubmatch(b, -1) {
			u, e := url.Parse(string(m[1]))
			if e != nil {
				continue
			}
			base, _ := url.Parse(raw)
			u = base.ResolveReference(u)
			if u.User != nil || u.Fragment != "" || u.Scheme != c.base.Scheme || u.Host != c.base.Host || !strings.HasPrefix(u.Path, "/_next/static/") {
				continue
			}
			if !seen[u.String()] {
				queue = append(queue, u.String())
			}
		}
	}
	if action == "" {
		return "", errors.New("current Maker deletion action reference not recognized")
	}
	return action, nil
}

// DeleteProduct only supports drafts. The caller owns confirmation and durable
// uncertainty checkpoints. A redirect is an acknowledgement, never deletion proof.
func (c *Client) DeleteProduct(ctx context.Context, t model.Target) (out DeleteResult, err error) {
	dispatched := false
	defer func() {
		if err != nil && !dispatched {
			err = &NotDeletedError{Cause: err}
		}
	}()
	out.ProductID = t.ProductID
	if c.readOnly {
		return out, ErrReadOnly
	}
	if _, e := targetPath(t); e != nil {
		return out, e
	}
	if strings.TrimSpace(t.Name) == "" {
		return out, errors.New("exact product name is required for deletion")
	}
	p, e := c.Product(ctx, t)
	if e != nil {
		return out, e
	}
	if p.ID != t.ProductID || p.Name != t.Name || (t.UUID != "" && p.UUID != t.UUID) {
		return out, errors.New("deletion product identity mismatch")
	}
	if !strings.EqualFold(p.Status, "DRAFT") {
		return out, errors.New("only draft products can be deleted")
	}
	action, e := c.discoverDeleteAction(ctx, t)
	if e != nil {
		return out, e
	}
	before, e := c.DeletionProducts(ctx, t.OrgID)
	if e != nil {
		return out, e
	}
	present := false
	for _, product := range before {
		if product.ID == t.ProductID && product.Name == t.Name {
			present = true
			break
		}
	}
	if !present {
		return out, errors.New("deletion target absent from strict Maker catalog preflight")
	}
	// Discovery may fetch many bundles. Recheck the live identity and draft
	// state after it completes, immediately before preparing the mutation.
	p, e = c.Product(ctx, t)
	if e != nil {
		return out, e
	}
	if p.ID != t.ProductID || p.Name != t.Name || (t.UUID != "" && p.UUID != t.UUID) {
		return out, errors.New("deletion product identity changed during preflight")
	}
	if !strings.EqualFold(p.Status, "DRAFT") {
		return out, errors.New("deletion product is no longer a draft")
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, field := range [][2]string{{"_1_organization_id", t.OrgID}, {"_1_product_id", t.ProductID}, {"0", `[{"success":false},"$K1"]`}} {
		if e = w.WriteField(field[0], field[1]); e != nil {
			return out, e
		}
	}
	if e = w.Close(); e != nil {
		return out, e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/"+t.OrgID+"/products/"+t.ProductID), &body)
	if e != nil {
		return out, errors.New("cannot create deletion request")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Next-Action", action)
	req.Header.Set("Accept", "text/x-component")
	req.Header.Set("Origin", c.base.String())
	if e = ctx.Err(); e != nil {
		return out, e
	}
	// Copy the configured client, preserving transport and scoped cookies, but
	// never follow a redirect or replay this non-idempotent POST.
	h := *c.http
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.invalidateReadPhase(ctx)
	defer c.invalidateReadPhase(ctx)
	dispatched = true
	ambiguous := func(cause error) (DeleteResult, error) { return out, &AmbiguousDeleteError{Cause: cause} }
	var resp *http.Response
	var b []byte
	e = func() error {
		started := time.Now()
		var responseBytes int64
		failed := true
		defer func() { c.recordRequest(true, http.MethodPost, time.Since(started), responseBytes, failed) }()
		var err error
		resp, err = h.Do(req)
		if err != nil {
			return errors.New("Maker deletion transport failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusOK {
			return errors.New("Maker deletion status did not acknowledge dispatch")
		}
		failed = false
		// Even a 303 must not be accepted when it attempts to move credentials or
		// navigation outside the Maker origin. We never follow its Location.
		if loc := resp.Header.Get("Location"); loc != "" {
			u, e := url.Parse(loc)
			if e != nil {
				return errors.New("invalid Maker deletion redirect")
			}
			u = req.URL.ResolveReference(u)
			if u.User != nil || u.Scheme != c.base.Scheme || u.Host != c.base.Host {
				return errors.New("cross-origin Maker deletion redirect blocked")
			}
		}
		if resp.StatusCode == http.StatusOK {
			b, err = io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024+1))
			responseBytes = int64(len(b))
			if err != nil || len(b) > 16*1024*1024 {
				failed = true
				return errors.New("Maker deletion response could not be read")
			}
		}
		failed = false
		return nil
	}()
	if e != nil {
		return ambiguous(e)
	}

	if resp.StatusCode == http.StatusOK {
		result, e := actionResult(b)
		if e != nil || result["success"] != true {
			return ambiguous(errors.New("Maker deletion result did not confirm success"))
		}
	}
	products, e := c.DeletionProducts(ctx, t.OrgID)
	if e != nil {
		return ambiguous(e)
	}
	for _, product := range products {
		if product.ID == t.ProductID {
			return ambiguous(errors.New("deleted product remains in Maker catalog"))
		}
	}
	out.Deleted = true
	return out, nil
}

// DeletionProducts reads the catalog with the stricter schema required to prove
// absence when reconciling a pending deletion. It never mutates Maker state.
func (c *Client) DeletionProducts(ctx context.Context, org string) ([]model.Product, error) {
	if err := safeID(org); err != nil {
		return nil, err
	}
	catalog, err := c.get(ctx, "/"+org+"/products")
	if err != nil {
		return nil, err
	}
	return parseDeleteCatalogForOrg(catalog, org)
}

// parseDeleteCatalog accepts only an explicit products collection or Maker's
// recognized complete grid with adjacent single-page pagination. Standalone
// product details cannot prove catalog absence.
func parseDeleteCatalog(b []byte) ([]model.Product, error) {
	return parseDeleteCatalogForOrg(b, "")
}

const deletionCatalogGridClass = "grid grid-cols-1 gap-x-m gap-y-2xl m:grid-cols-2 l:grid-cols-3 xl:grid-cols-4"

func parseDeleteCatalogForOrg(b []byte, org string) ([]model.Product, error) {
	var catalog []model.Product
	found := false
	rendered := false
	explicit := false
	cards := map[string]model.Product{}
	schemaError := func() error { return errors.New("Maker deletion verification catalog schema missing or malformed") }
	// Multiple serialized references may repeat the same complete collection.
	// Distinct collections are not evidence of a complete catalog.
	accept := func(list []model.Product) error {
		if found {
			if len(catalog) != len(list) {
				return schemaError()
			}
			for i := range list {
				if !reflect.DeepEqual(catalog[i], list[i]) {
					return schemaError()
				}
			}
		} else {
			catalog = list
			found = true
		}
		return nil
	}
	readRow := func(raw any, resolve func(any) any) (model.Product, error) {
		row, ok := resolve(raw).(map[string]any)
		if !ok {
			return model.Product{}, schemaError()
		}
		id, _ := resolve(row["id"]).(string)
		name, _ := resolve(row["name"]).(string)
		_, variantsOK := resolve(row["variants"]).([]any)
		if safeID(id) != nil || name == "" || !variantsOK {
			return model.Product{}, schemaError()
		}
		str := func(m map[string]any, key string) (string, error) {
			raw, exists := m[key]
			if !exists || resolve(raw) == nil {
				return "", nil
			}
			value, ok := resolve(raw).(string)
			if !ok {
				return "", schemaError()
			}
			return value, nil
		}
		product := model.Product{ID: id, Name: name}
		var err error
		if product.Status, err = str(row, "status"); err != nil {
			return model.Product{}, err
		}
		if product.Description, err = str(row, "description"); err != nil {
			return model.Product{}, err
		}
		if product.Type, err = str(row, "type"); err != nil {
			return model.Product{}, err
		}
		if product.Type == "" {
			if product.Type, err = str(row, "extension_name"); err != nil {
				return model.Product{}, err
			}
		}
		variants := resolve(row["variants"]).([]any)
		seenVariants := map[string]bool{}
		for _, rawVariant := range variants {
			variant, ok := resolve(rawVariant).(map[string]any)
			if !ok {
				return model.Product{}, schemaError()
			}
			vid, _ := resolve(variant["id"]).(string)
			if safeID(vid) != nil || seenVariants[vid] {
				return model.Product{}, schemaError()
			}
			seenVariants[vid] = true
			uuid, err := str(variant, "streamdeckid")
			if err != nil {
				return model.Product{}, err
			}
			variantName, err := str(variant, "name")
			if err != nil {
				return model.Product{}, err
			}
			product.Variants = append(product.Variants, model.Variant{ID: vid, UUID: uuid, Name: variantName})
			if product.UUID == "" {
				product.UUID = uuid
			}
			if rawVersions, exists := variant["versions"]; exists {
				versions, ok := resolve(rawVersions).([]any)
				if !ok {
					return model.Product{}, schemaError()
				}
				for _, rawVersion := range versions {
					version, ok := resolve(rawVersion).(map[string]any)
					if !ok {
						return model.Product{}, schemaError()
					}
					versionID, _ := resolve(version["id"]).(string)
					status, _ := resolve(version["status"]).(string)
					number, _ := resolve(version["version_number"]).(string)
					if number == "" {
						number, _ = resolve(version["number"]).(string)
					}
					if number == "" {
						number, _ = resolve(version["version"]).(string)
					}
					if safeID(versionID) != nil || number == "" || status == "" {
						return model.Product{}, schemaError()
					}
				}
			}
		}
		return product, nil
	}
	err := walkListingPageValues(b, func(m map[string]any, resolve func(any) any) error {
		if _, card := m["item"]; card {
			_, hasOrg := m["orgId"]
			_, hasStripe := m["stripeStatus"]
			if hasOrg || hasStripe {
				product, err := readRow(m["item"], resolve)
				if err != nil {
					return err
				}
				cardOrg, ok := resolve(m["orgId"]).(string)
				if !ok || safeID(cardOrg) != nil || (org != "" && cardOrg != org) || !hasStripe {
					return schemaError()
				}
				if prior, exists := cards[product.ID]; exists && !reflect.DeepEqual(prior, product) {
					return schemaError()
				}
				cards[product.ID] = product
			}
		}
		raw, ok := m["products"]
		if !ok {
			return nil
		}
		explicit = true
		rows, ok := resolve(raw).([]any)
		if !ok {
			return schemaError()
		}
		list := []model.Product{}
		seen := map[string]bool{}
		for _, raw := range rows {
			product, err := readRow(raw, resolve)
			if err != nil {
				return err
			}
			if seen[product.ID] {
				return schemaError()
			}
			seen[product.ID] = true
			list = append(list, product)
		}
		return accept(list)
	}, func(values []any, resolve func(any) any) error {
		// The current Maker catalog serializes an adjacent full grid and
		// pagination component instead of a products-array property.
		if len(values) == 0 {
			return nil
		}
		grid, ok := resolve(values[0]).([]any)
		if !ok || len(grid) != 4 {
			return nil
		}
		props, ok := resolve(grid[3]).(map[string]any)
		if !ok {
			return nil
		}
		class, _ := resolve(props["className"]).(string)
		if class != deletionCatalogGridClass {
			return nil
		}
		if len(values) != 2 || resolve(grid[0]) != "$" || resolve(grid[1]) != "div" || resolve(grid[2]) != nil || len(props) != 2 {
			return schemaError()
		}
		pagination, ok := resolve(values[1]).([]any)
		if !ok || len(pagination) != 4 || resolve(pagination[0]) != "$" {
			return schemaError()
		}
		paging, ok := resolve(pagination[3]).(map[string]any)
		if !ok || len(paging) != 2 || resolve(paging["pageCount"]) != float64(1) || resolve(paging["selectedPage"]) != float64(1) {
			return errors.New("Maker deletion verification requires the complete unfiltered single-page catalog")
		}
		rows, ok := resolve(props["children"]).([]any)
		if !ok {
			return schemaError()
		}
		list := []model.Product{}
		seen := map[string]bool{}
		for _, raw := range rows {
			link, ok := resolve(raw).([]any)
			if !ok || len(link) != 4 || resolve(link[0]) != "$" {
				return schemaError()
			}
			linkProps, ok := resolve(link[3]).(map[string]any)
			if !ok {
				return schemaError()
			}
			card, ok := resolve(linkProps["children"]).([]any)
			if !ok || len(card) != 4 || resolve(card[0]) != "$" {
				return schemaError()
			}
			cardProps, ok := resolve(card[3]).(map[string]any)
			if !ok || len(cardProps) != 3 {
				return schemaError()
			}
			if _, ok := cardProps["stripeStatus"]; !ok {
				return schemaError()
			}
			cardOrg, ok := resolve(cardProps["orgId"]).(string)
			if !ok || safeID(cardOrg) != nil || (org != "" && cardOrg != org) {
				return schemaError()
			}
			product, err := readRow(cardProps["item"], resolve)
			if err != nil {
				return err
			}
			href, ok := resolve(linkProps["href"]).(string)
			if !ok || href != "/"+cardOrg+"/products/"+product.ID || resolve(linkProps["prefetch"]) != false {
				return schemaError()
			}
			if seen[product.ID] {
				return schemaError()
			}
			seen[product.ID] = true
			list = append(list, product)
		}
		rendered = true
		return accept(list)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, schemaError()
	}
	if rendered {
		if len(cards) != len(catalog) {
			return nil, schemaError()
		}
		for _, product := range catalog {
			if card, exists := cards[product.ID]; !exists || !reflect.DeepEqual(card, product) {
				return nil, schemaError()
			}
		}
	}
	if explicit {
		if err := validateDeleteCatalogPagination(b); err != nil {
			return nil, err
		}
	}
	return catalog, nil
}

// validateDeleteCatalogPagination checks paging evidence in the explicit
// collection's envelope. Legacy unpaged collections need no invented metadata;
// once a paging representation appears, only a complete known page is proof.
func validateDeleteCatalogPagination(page []byte) error {
	incomplete := func() error {
		return errors.New("Maker deletion verification requires the complete unfiltered single-page catalog")
	}
	normalize := func(key string) string { return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key)) }
	metadata := func(key string) bool {
		switch key {
		case "pagination", "paging", "pager", "meta", "metadata", "pagemetadata":
			return true
		}
		return false
	}
	unsupported := func(key string) bool {
		switch key {
		case "pageinfo", "paginationinfo", "paginginfo", "paginationdata", "pagingdata", "paginationmetadata", "next", "previous", "prev", "after", "before", "page", "pagenumber", "currentpage", "pages", "total", "totals", "totalcount", "totalpages", "totalitems", "totalproducts", "count", "cursor", "nextcursor", "previouscursor", "prevcursor", "startcursor", "endcursor", "hasmore", "hasnext", "hasnextpage", "haspreviouspage", "hasprevpage", "nextpage", "previouspage", "prevpage", "offset", "limit", "skip", "take", "perpage", "pagesize":
			return true
		}
		return false
	}
	var inspect func(map[string]any, func(any) any, int, bool) (bool, error)
	inspect = func(m map[string]any, resolve func(any) any, depth int, strict bool) (bool, error) {
		if depth > 20 {
			return false, incomplete()
		}
		indicators := false
		count, pageNumber := false, false
		for key, raw := range m {
			key = normalize(key)
			switch key {
			case "pagecount":
				indicators = true
				count = true
				if resolve(raw) != float64(1) {
					return true, incomplete()
				}
			case "selectedpage":
				indicators = true
				pageNumber = true
				if resolve(raw) != float64(1) {
					return true, incomplete()
				}
			default:
				if strict {
					return true, incomplete()
				}
				if unsupported(key) {
					return true, incomplete()
				}
				if metadata(key) {
					nested, ok := resolve(raw).(map[string]any)
					if !ok {
						if key == "meta" || key == "metadata" {
							continue
						}
						return true, incomplete()
					}
					found, err := inspect(nested, resolve, depth+1, key != "meta" && key != "metadata")
					if err != nil {
						return true, err
					}
					// An explicitly named paging object must itself contain recognized proof.
					if key != "meta" && key != "metadata" && !found {
						return true, incomplete()
					}
					indicators = indicators || found
				}
			}
		}
		if count != pageNumber {
			return true, incomplete()
		}
		return indicators, nil
	}
	// Descendant search is bounded separately from Flight decoding. Only direct
	// fields and named metadata on catalog ancestors are inspected, so an unrelated
	// sidebar's own counters cannot invalidate an unpaged catalog.
	searches := 0
	var containsCatalog func(any, func(any) any, int) (bool, error)
	containsCatalog = func(value any, resolve func(any) any, depth int) (bool, error) {
		searches++
		if searches > 100000 || depth > 40 {
			return false, incomplete()
		}
		switch value := resolve(value).(type) {
		case map[string]any:
			if _, exists := value["products"]; exists {
				return true, nil
			}
			for _, child := range value {
				found, err := containsCatalog(child, resolve, depth+1)
				if err != nil || found {
					return found, err
				}
			}
		case []any:
			for _, child := range value {
				found, err := containsCatalog(child, resolve, depth+1)
				if err != nil || found {
					return found, err
				}
			}
		}
		return false, nil
	}
	return walkListingPageValues(page, func(m map[string]any, resolve func(any) any) error {
		found, err := containsCatalog(m, resolve, 0)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		_, err = inspect(m, resolve, 0, false)
		return err
	}, func(values []any, resolve func(any) any) error {
		// A serialized pagination component carries paging proof even if its Flight
		// record is separate from the collection component's record.
		if len(values) == 4 && resolve(values[0]) == "$" {
			if props, ok := resolve(values[3]).(map[string]any); ok {
				count, selected, pagingMarker := false, false, false
				for key := range props {
					key = normalize(key)
					count = count || key == "pagecount"
					selected = selected || key == "selectedpage"
					switch key {
					case "pagination", "paging", "pager", "pageinfo", "nextcursor", "cursor", "hasmore", "hasnextpage":
						pagingMarker = true
					}
				}
				if count || selected || pagingMarker {
					_, err := inspect(props, resolve, 0, !pagingMarker)
					if err != nil {
						return err
					}
				}
			}
		}
		found, err := containsCatalog(values, resolve, 0)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		for _, raw := range values {
			child := resolve(raw)
			if component, ok := child.([]any); ok && len(component) == 4 && resolve(component[0]) == "$" {
				child = resolve(component[3])
			}
			if props, ok := child.(map[string]any); ok {
				if _, err := inspect(props, resolve, 0, false); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
