package maker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestReadUploadTagsScalarAndLegacyShapes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       map[string][]string
		valid      bool
	}{
		{"observed scalar IDs", `{"tags":{"Dial support":"dial-yes","Neo Infobar support":"neo-no","Profiles":"profiles-yes"}}`, map[string][]string{"Dial support": {"dial-yes"}, "Neo Infobar support": {"neo-no"}, "Profiles": {"profiles-yes"}}, true},
		{"legacy arrays", `{"tags":{"OS":["Mac","Windows"]}}`, map[string][]string{"OS": {"Mac", "Windows"}}, true},
		{"mixed shapes", `{"tags":{"OS":["Mac"],"Dial support":"dial-yes"}}`, map[string][]string{"OS": {"Mac"}, "Dial support": {"dial-yes"}}, true},
		{"empty tags", `{"tags":{}}`, map[string][]string{}, true},
		{"empty array", `{"tags":{"OS":[]}}`, map[string][]string{"OS": {}}, true},
		{"null tags", `{"tags":null}`, nil, false},
		{"missing tags", `{}`, nil, false},
		{"tags array", `{"tags":[]}`, nil, false},
		{"null value", `{"tags":{"OS":null}}`, nil, false},
		{"numeric value", `{"tags":{"OS":1}}`, nil, false},
		{"boolean value", `{"tags":{"OS":false}}`, nil, false},
		{"object value", `{"tags":{"OS":{"id":"os-mac"}}}`, nil, false},
		{"null array item", `{"tags":{"OS":[null]}}`, nil, false},
		{"number array item", `{"tags":{"OS":["Mac",1]}}`, nil, false},
		{"empty scalar", `{"tags":{"OS":""}}`, nil, false},
		{"blank scalar", `{"tags":{"OS":" \n"}}`, nil, false},
		{"empty array item", `{"tags":{"OS":[""]}}`, nil, false},
		{"blank group", `{"tags":{" ":["Mac"]}}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || r.URL.Path != "/api/upload-aux/tags/synthetic-folder" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, AllowLocalhost: true, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.ReadUploadTags(context.Background(), "synthetic-folder")
			if (err == nil) != tc.valid || requests != 1 {
				t.Fatalf("valid=%v error=%v requests=%d", tc.valid, err, requests)
			}
			if tc.valid && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tags=%v want %v", got, tc.want)
			}
		})
	}
}

func uploadTagsMetadata() ListingMetadata {
	return ListingMetadata{Tagsets: []ListingTagset{
		{ID: "group-dial", Name: "Dials", Label: "Dial support", Slug: "dial", Tags: []ListingOption{{ID: "dial-yes", Name: "Yes"}, {ID: "dial-no", Name: "No"}}},
		{ID: "group-neo", Name: "Neo", Label: "Neo Infobar support", Slug: "neo", Tags: []ListingOption{{ID: "neo-yes", Name: "Yes"}, {ID: "neo-no", Name: "No"}}},
		{ID: "group-profiles", Name: "Profiles", Label: "Profiles", Slug: "profiles", Tags: []ListingOption{{ID: "profiles-yes", Name: "Yes"}}},
		{ID: "group-language", Name: "Languages", Label: "Languages", Slug: "language", Tags: []ListingOption{{ID: "lang-en", Name: "English"}}},
		{ID: "group-type", Name: "Type", Label: "Type", Slug: "type", Tags: []ListingOption{{ID: "type-tools", Name: "Tools"}}},
	}}
}
func TestResolveUploadTagsIDsNamesAndScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tags   map[string][]string
		mutate func(*ListingMetadata)
		want   []string
		valid  bool
	}{
		{"observed scalar IDs normalized", map[string][]string{"Dial support": {"dial-yes"}, "Neo Infobar support": {"neo-no"}, "Profiles": {"profiles-yes"}}, nil, []string{"dial-yes", "neo-no", "profiles-yes"}, true},
		{"legacy exact names scoped", map[string][]string{"Dial support": {"Yes"}, "Neo Infobar support": {"No"}}, nil, []string{"dial-yes", "neo-no"}, true},
		{"group name alias", map[string][]string{"Dials": {"dial-yes"}}, nil, []string{"dial-yes"}, true},
		{"mixed duplicates sorted", map[string][]string{"Profiles": {"Yes", "profiles-yes"}, "Dial support": {"dial-yes", "Yes", "dial-yes"}}, nil, []string{"dial-yes", "profiles-yes"}, true},
		{"user taxonomy excluded after validation", map[string][]string{"Languages": {"lang-en"}, "Type": {"Tools"}, "Dial support": {"dial-yes"}}, nil, []string{"dial-yes"}, true},
		{"wrong group ID", map[string][]string{"Dial support": {"neo-yes"}}, nil, nil, false},
		{"unknown ID", map[string][]string{"Dial support": {"unknown-id"}}, nil, nil, false},
		{"nonexact name", map[string][]string{"Dial support": {"yes"}}, nil, nil, false},
		{"unknown group", map[string][]string{"Unknown": {"dial-yes"}}, nil, nil, false},
		{"empty token", map[string][]string{"Dial support": {""}}, nil, nil, false},
		{"excluded group wrong ID", map[string][]string{"Languages": {"dial-yes"}}, nil, nil, false},
		{"ambiguous group", map[string][]string{"Dial support": {"Yes"}}, func(m *ListingMetadata) {
			m.Tagsets = append(m.Tagsets, ListingTagset{ID: "other-group", Label: "Dial support", Tags: []ListingOption{{ID: "other-yes", Name: "Yes"}}})
		}, nil, false},
		{"ambiguous option name", map[string][]string{"Dial support": {"Yes"}}, func(m *ListingMetadata) {
			m.Tagsets[0].Tags = append(m.Tagsets[0].Tags, ListingOption{ID: "other-yes", Name: "Yes"})
		}, nil, false},
		{"ID and name collision", map[string][]string{"Dial support": {"dial-yes"}}, func(m *ListingMetadata) {
			m.Tagsets[0].Tags = append(m.Tagsets[0].Tags, ListingOption{ID: "other-yes", Name: "dial-yes"})
		}, nil, false},
		{"empty selected ID", map[string][]string{"Dial support": {"Yes"}}, func(m *ListingMetadata) { m.Tagsets[0].Tags[0].ID = "" }, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := uploadTagsMetadata()
			if tc.mutate != nil {
				tc.mutate(&metadata)
			}
			got, err := metadata.ResolveUploadTags(tc.tags)
			if (err == nil) != tc.valid {
				t.Fatalf("error=%v valid=%v", err, tc.valid)
			}
			if tc.valid && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ids=%v want %v", got, tc.want)
			}
		})
	}
}
