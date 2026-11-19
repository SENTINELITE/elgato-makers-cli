package maker

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
)

var framePattern = regexp.MustCompile(`self\.__next_f\.push\((\[.*?\])\)</script>`)

func parsePage(b []byte) ([]model.Product, []model.Version, error) { return parsePageMode(b, false) }
func parsePageMode(b []byte, requireVersions bool) ([]model.Product, []model.Version, error) {
	records := map[string]any{}
	var roots []any
	var stream strings.Builder
	var plain any
	if json.Unmarshal(b, &plain) == nil {
		roots = append(roots, plain)
	}
	for _, f := range framePattern.FindAllSubmatch(b, -1) {
		var a []json.RawMessage
		if json.Unmarshal(f[1], &a) != nil || len(a) < 2 {
			continue
		}
		var s string
		if json.Unmarshal(a[1], &s) == nil {
			stream.WriteString(s)
		}
	}
	if stream.Len() == 0 && len(roots) == 0 {
		stream.Write(b)
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
		id := remaining[:colon]
		payload := remaining[colon+1:]
		if len(payload) > 0 && payload[0] == 'T' {
			comma := strings.IndexByte(payload, ',')
			if comma > 1 && comma < 20 {
				size, err := strconv.ParseUint(payload[1:comma], 16, 32)
				if err == nil && int(size) <= len(payload)-comma-1 {
					value := payload[comma+1 : comma+1+int(size)]
					records[id] = value
					roots = append(roots, value)
					remaining = payload[comma+1+int(size):]
					continue
				}
			}
		}
		lineEnd := strings.IndexByte(payload, '\n')
		if lineEnd < 0 {
			lineEnd = len(payload)
		}
		var value any
		if json.Unmarshal([]byte(payload[:lineEnd]), &value) == nil {
			records[id] = value
			roots = append(roots, value)
		}
		remaining = payload[lineEnd:]
		remaining = strings.TrimPrefix(remaining, "\n")
	}
	var resolve func(any, int) any
	resolve = func(x any, depth int) any {
		if depth > 30 {
			return nil
		}
		if s, ok := x.(string); ok && strings.HasPrefix(s, "$") {
			id := strings.TrimPrefix(s, "$")
			id = strings.TrimPrefix(id, "L")
			if r, ok := records[id]; ok {
				return resolve(r, depth+1)
			}
		}
		return x
	}
	type versionEvidence struct {
		version    model.Version
		productID  string
		notesKnown bool
	}
	versionEvidenceByID := map[string]*versionEvidence{}
	var versionRows []map[string]any
	// Both summaries and detail rows must identify the same numeric version and
	// status. Optional fields remain unknown until explicitly supplied.
	mergeVersion := func(row map[string]any, evidence *versionEvidence) error {
		readString := func(key string) (string, bool, error) {
			raw, exists := row[key]
			if !exists {
				return "", false, nil
			}
			value, ok := resolve(raw, 0).(string)
			if !ok {
				return "", true, errors.New("invalid Maker version field")
			}
			return value, true, nil
		}
		var number string
		for _, key := range []string{"version_number", "number", "version"} {
			value, exists, err := readString(key)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
			if value == "" || number != "" && number != value && !model.SameStreamDeckVersion(number, value) {
				return errors.New("conflicting Maker version numbers")
			}
			if number == "" {
				number = value
			}
		}
		status, _, err := readString("status")
		if err != nil {
			return err
		}
		if number == "" || status == "" {
			return errors.New("missing Maker version identity")
		}
		if evidence.version.Version != "" && evidence.version.Version != number && !model.SameStreamDeckVersion(evidence.version.Version, number) {
			return errors.New("conflicting Maker version numbers")
		}
		if evidence.version.Status != "" && !strings.EqualFold(evidence.version.Status, status) {
			return errors.New("conflicting Maker version status")
		}
		for _, scope := range []struct{ key, expected string }{{"variant_id", evidence.version.VariantID}, {"product_id", evidence.productID}} {
			value, exists, err := readString(scope.key)
			if err != nil {
				return err
			}
			if exists && value != scope.expected {
				return errors.New("conflicting Maker version ownership")
			}
		}
		if evidence.version.Version == "" {
			evidence.version.Version = number
		}
		if evidence.version.Status == "" {
			evidence.version.Status = status
		}
		notes, hasNotes, err := readString("release_notes")
		if err != nil {
			return err
		}
		if hasNotes {
			if evidence.notesKnown && evidence.version.ReleaseNotes != notes {
				return errors.New("conflicting Maker version release notes")
			}
			evidence.version.ReleaseNotes, evidence.notesKnown = notes, true
		}
		if raw, exists := row["is_ready_for_publish"]; exists {
			value := resolve(raw, 0)
			if value != nil {
				ap, ok := value.(bool)
				if !ok {
					return errors.New("invalid Maker version auto-publish field")
				}
				if evidence.version.AutoPublish != nil && *evidence.version.AutoPublish != ap {
					return errors.New("conflicting Maker version auto-publish fields")
				}
				evidence.version.AutoPublish = &ap
			}
		}
		return nil
	}
	var products []model.Product
	var versions []model.Version
	seen := map[string]bool{}
	recognized := false
	malformed := false
	versionsRecognized := false
	str := func(m map[string]any, k string) string { s, _ := resolve(m[k], 0).(string); return s }
	visits := 0
	var walk func(any, int)
	walk = func(x any, depth int) {
		visits++
		if visits > 100000 {
			malformed = true
			return
		}
		if depth > 40 {
			return
		}
		x = resolve(x, 0)
		switch v := x.(type) {
		case []any:
			for _, a := range v {
				walk(a, depth+1)
			}
		case map[string]any:
			versionRows = append(versionRows, v)
			if raw, ok := v["products"]; ok {
				if _, ok = resolve(raw, 0).([]any); ok {
					recognized = true
				}
			}
			if variants, ok := resolve(v["variants"], 0).([]any); ok && str(v, "id") != "" && str(v, "name") != "" {
				recognized = true
				p := model.Product{ID: str(v, "id"), Name: str(v, "name"), Type: str(v, "type"), Status: str(v, "status"), Description: str(v, "description")}
				if p.Type == "" {
					p.Type = str(v, "extension_name")
				}
				for _, rv := range variants {
					vv, ok := resolve(rv, 0).(map[string]any)
					if !ok {
						malformed = true
						continue
					}
					if str(vv, "id") == "" {
						malformed = true
					}
					variant := model.Variant{ID: str(vv, "id"), UUID: str(vv, "streamdeckid"), Name: str(vv, "name")}
					p.Variants = append(p.Variants, variant)
					if p.UUID == "" {
						p.UUID = variant.UUID
					}
					if raw, exists := vv["versions"]; exists {
						if _, ok := resolve(raw, 0).([]any); !ok {
							malformed = true
						}
					}
					if vs, ok := resolve(vv["versions"], 0).([]any); ok {
						versionsRecognized = true
						for _, rver := range vs {
							vm, ok := resolve(rver, 0).(map[string]any)
							if !ok {
								malformed = true
								continue
							}
							id := str(vm, "id")
							if id == "" {
								malformed = true
								continue
							}
							evidence, exists := versionEvidenceByID[id]
							if !exists {
								evidence = &versionEvidence{version: model.Version{ID: id, VariantID: variant.ID}, productID: p.ID}
								versionEvidenceByID[id] = evidence
							} else if evidence.version.VariantID != variant.ID || evidence.productID != p.ID {
								malformed = true
								continue
							}
							if mergeVersion(vm, evidence) != nil {
								malformed = true
							}
							if !seen["v"+id] {
								seen["v"+id] = true
								versions = append(versions, evidence.version)
							}
						}
					}
				}
				if !seen["p"+p.ID] {
					seen["p"+p.ID] = true
					products = append(products, p)
				}
			}
			for _, a := range v {
				walk(a, depth+1)
			}
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	// Detail objects elsewhere in the Flight page may enrich an anchored
	// summary, but can never introduce another variant's unscoped version.
	for _, row := range versionRows {
		id := str(row, "id")
		evidence, anchored := versionEvidenceByID[id]
		if !anchored {
			continue
		}
		candidate := false
		for _, key := range []string{"version_number", "number", "version", "release_notes", "is_ready_for_publish"} {
			if _, exists := row[key]; exists {
				candidate = true
				break
			}
		}
		if candidate && mergeVersion(row, evidence) != nil {
			malformed = true
		}
	}
	for i := range versions {
		versions[i] = versionEvidenceByID[versions[i].ID].version
	}
	if requireVersions && !versionsRecognized {
		return nil, nil, errors.New("Maker versions schema missing")
	}
	if malformed {
		return nil, nil, errors.New("unrecognized Maker variant or version schema")
	}
	if !recognized {
		return nil, nil, errors.New("unrecognized Maker product schema or unauthenticated page")
	}
	if products == nil {
		products = []model.Product{}
	}
	if versions == nil {
		versions = []model.Version{}
	}
	return products, versions, nil
}
