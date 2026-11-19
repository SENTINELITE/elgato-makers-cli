package listing

import (
	"reflect"
	"testing"
)

func TestReorderCompletePermutation(t *testing.T) {
	no := false
	c := Config{SchemaVersion: 1, Name: "Listing", OrganizationID: "org", ProductID: "product", VariantID: "variant", PackagePath: "package", DescriptionFile: "description", ReleaseNotesFile: "notes", Pricing: Pricing{Type: "paid", Amount: "1.25", Currency: "USD"}, Languages: []string{"en"}, Categories: []string{"Gaming"}, Links: map[string]string{"Support": "https://example.com"}, AutoPublish: &no, GalleryUploadMode: "sequential", Assets: Assets{Icon: "icon", Thumbnail: "thumb", Gallery: []string{"one.png", "two.jpg", "three.mp4"}}}
	result, err := Reorder(c, []int{3, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	expected := c
	expected.Assets.Gallery = []string{"three.mp4", "one.png", "two.jpg"}
	if !reflect.DeepEqual(result, expected) {
		t.Fatalf("reorder changed other config fields: %#v", result)
	}
	result.Assets.Gallery[0] = "changed"
	result.Languages[0] = "changed"
	result.Categories[0] = "changed"
	result.Links["Support"] = "changed"
	*result.AutoPublish = true
	if c.Assets.Gallery[0] != "one.png" || c.Languages[0] != "en" || c.Categories[0] != "Gaming" || c.Links["Support"] != "https://example.com" || *c.AutoPublish {
		t.Fatal("reorder result aliases original config")
	}
}

func TestReorderRejectsIncompleteOrInvalidOrder(t *testing.T) {
	c := Config{Assets: Assets{Gallery: []string{"one", "two", "three"}}}
	for _, order := range [][]int{nil, {1}, {1, 2}, {1, 2, 3, 4}, {1, 1, 2}, {0, 1, 2}, {1, 2, 4}, {-1, 1, 2}} {
		if _, err := Reorder(c, order); err == nil {
			t.Errorf("accepted invalid order %v", order)
		}
	}
	for _, count := range []int{0, 1, 2, 11} {
		c.Assets.Gallery = make([]string, count)
		order := make([]int, count)
		for i := range order {
			order[i] = i + 1
		}
		if _, err := Reorder(c, order); err == nil {
			t.Errorf("accepted gallery count %d", count)
		}
	}
}

func TestReorderIdentityAndEmptyLinks(t *testing.T) {
	for _, empty := range []bool{false, true} {
		c := Config{Assets: Assets{Gallery: []string{"one", "two", "three"}}}
		if empty {
			c.Links = map[string]string{}
		}
		result, err := Reorder(c, []int{1, 2, 3})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result, c) {
			t.Fatalf("identity reorder changed fields: %#v", result)
		}
	}
}

func TestReorderTenItems(t *testing.T) {
	c := Config{Assets: Assets{Gallery: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}}}
	result, err := Reorder(c, []int{10, 9, 8, 7, 6, 5, 4, 3, 2, 1})
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range result.Assets.Gallery {
		if path != c.Assets.Gallery[9-i] {
			t.Fatalf("position%d: %s", i+1, path)
		}
	}
}
