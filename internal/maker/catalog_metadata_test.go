package maker

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestCatalogPricingFlightReferencesAndUnknown(t *testing.T) {
	stream := "1:" + `{"id":"$2","name":"Synthetic","variants":"$3"}` + "\n2:\"p\"\n3:" + `[{"id":"a","is_free":"$4"},{"id":"b"},{"id":"c","is_free":"false"}]` + "\n4:false\n"
	frame, _ := json.Marshal([]any{1, stream})
	page := []byte(fmt.Sprintf(`<script>self.__next_f.push(%s)</script>`, frame))
	rows, err := parseCatalogPricing(page)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	values := map[string]*bool{}
	for _, v := range rows[0].Variants {
		values[v.ID] = v.IsFree
	}
	if values["a"] == nil || *values["a"] || values["b"] != nil || values["c"] != nil {
		t.Fatal(values)
	}
}
func TestCatalogPricingConflictingAnchoredEvidence(t *testing.T) {
	raw := []byte(`[{"id":"p","name":"Synthetic","variants":[{"id":"a","is_free":true}]},{"id":"p","name":"Synthetic","variants":[{"id":"a","is_free":false}]},{"id":"orphan","is_free":true}]`)
	rows, err := parseCatalogPricing(raw)
	if err != nil || len(rows) != 1 || len(rows[0].Variants) != 1 || rows[0].Variants[0].IsFree != nil {
		t.Fatal(rows, err)
	}
}
