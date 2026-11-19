package maker

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func versionDetailPage(detail map[string]any, extras ...map[string]any) []byte {
	product := map[string]any{"id": "product-a", "name": "Synthetic", "variants": []any{map[string]any{"id": "variant-a", "versions": []any{map[string]any{"id": "version-a", "number": "1", "status": "review"}}}}}
	rows := []any{product}
	if detail != nil {
		rows = append(rows, detail)
	}
	for _, extra := range extras {
		rows = append(rows, extra)
	}
	b, _ := json.Marshal(rows)
	return b
}

func versionDetail() map[string]any {
	return map[string]any{"id": "version-a", "number": "1", "version_number": "1.0.0.0", "status": "review", "release_notes": "Initial release\n", "is_ready_for_publish": true}
}

func TestParserEnrichesAnchoredVersionFromStandaloneFlightDetailRefs(t *testing.T) {
	records := []any{
		map[string]any{"product": "$2", "detail": "$3"},
		map[string]any{"id": "product-a", "name": "Synthetic", "variants": []any{map[string]any{"id": "variant-a", "versions": []any{map[string]any{"id": "version-a", "number": "1", "status": "review"}}}}},
		map[string]any{"id": "$4", "number": "$5", "version_number": "$6", "status": "$7", "release_notes": "$8", "is_ready_for_publish": "$9"},
		"version-a", "1", "1.0.0.0", "review", "Initial release\n", true,
	}
	var stream strings.Builder
	for i, record := range records {
		b, _ := json.Marshal(record)
		fmt.Fprintf(&stream, "%x:%s\n", i+1, b)
	}
	frame, _ := json.Marshal([]any{1, stream.String()})
	page := []byte(fmt.Sprintf(`<script>self.__next_f.push(%s)</script>`, frame))
	products, versions, err := parsePageMode(page, true)
	if err != nil || len(products) != 1 || len(versions) != 1 {
		t.Fatalf("detail parse failed: %v %v %v", err, products, versions)
	}
	version := versions[0]
	if version.ID != "version-a" || version.VariantID != "variant-a" || version.Version != "1" || version.Status != "review" || version.ReleaseNotes != "Initial release\n" || version.AutoPublish == nil || !*version.AutoPublish {
		t.Fatalf("enrichment lost scoped evidence: %+v", version)
	}
}

func TestParserEnrichmentPreservesUnknownAndFalseAutoPublish(t *testing.T) {
	for _, state := range []string{"summary only", "null", "false"} {
		t.Run(state, func(t *testing.T) {
			var detail map[string]any
			if state != "summary only" {
				detail = versionDetail()
				detail["is_ready_for_publish"] = nil
			}
			if state == "false" {
				detail["is_ready_for_publish"] = false
			}
			_, versions, err := parsePageMode(versionDetailPage(detail), true)
			if err != nil || len(versions) != 1 {
				t.Fatal(err, versions)
			}
			if state == "false" {
				if versions[0].AutoPublish == nil || *versions[0].AutoPublish {
					t.Fatal("explicit false became unknown/true")
				}
			} else if versions[0].AutoPublish != nil {
				t.Fatal("unknown auto-publish became explicit")
			}
			if state == "summary only" && versions[0].ReleaseNotes != "" {
				t.Fatal("summary invented release notes")
			}
		})
	}
}

func TestParserEnrichmentNeverAddsUnanchoredVersions(t *testing.T) {
	wrong := versionDetail()
	wrong["id"] = "version-other"
	wrong["release_notes"] = 42 // Unowned rows are not evidence, even when malformed.
	_, versions, err := parsePageMode(versionDetailPage(wrong), true)
	if err != nil || len(versions) != 1 || versions[0].ID != "version-a" || versions[0].AutoPublish != nil || versions[0].ReleaseNotes != "" {
		t.Fatalf("unowned version changed results: %v %+v", err, versions)
	}
}

func TestParserEnrichmentRejectsConflictsAndMalformedDetails(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value any
	}{
		{"number conflict", "number", "2"}, {"version alias conflict", "version_number", "2"}, {"status conflict", "status", "published"},
		{"number type", "number", 1}, {"status type", "status", false}, {"notes type", "release_notes", 23}, {"notes null", "release_notes", nil}, {"publish type", "is_ready_for_publish", "false"},
		{"variant ownership", "variant_id", "variant-b"}, {"product ownership", "product_id", "product-b"}, {"ownership type", "variant_id", 12},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail := versionDetail()
			detail[test.key] = test.value
			if _, _, err := parsePageMode(versionDetailPage(detail), true); err == nil {
				t.Fatal("conflicting or malformed detail accepted")
			}
		})
	}
	for _, field := range []string{"release_notes", "is_ready_for_publish"} {
		t.Run("duplicate "+field, func(t *testing.T) {
			first, second := versionDetail(), versionDetail()
			if field == "release_notes" {
				second[field] = "Other notes"
			} else {
				second[field] = false
			}
			if _, _, err := parsePageMode(versionDetailPage(first, second), true); err == nil {
				t.Fatal("conflicting duplicate detail accepted")
			}
		})
	}
}

func TestParserEnrichmentRejectsVersionSharedAcrossVariantMembership(t *testing.T) {
	otherProduct := map[string]any{"id": "product-b", "name": "Other", "variants": []any{map[string]any{"id": "variant-b", "versions": []any{map[string]any{"id": "version-a", "number": "1", "status": "review"}}}}}
	if _, _, err := parsePageMode(versionDetailPage(versionDetail(), otherProduct), true); err == nil {
		t.Fatal("one version ID claimed by different variants")
	}
}
