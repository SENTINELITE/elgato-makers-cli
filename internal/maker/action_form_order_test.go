package maker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// The deployed Flight decoder snapshots the already received _1_ fields as
// soon as root part 0 references $K1. Parsing the entire multipart request first
// hides this protocol constraint and previously let random map order pass.
func TestActionMultipartRootFollowsReferencedFormData(t *testing.T) {
	for _, root := range []string{`[null,"$K1"]`, `[{},"$K1"]`} {
		t.Run(root, func(t *testing.T) {
			fields := map[string]string{
				"0": root, "_1_name": "Synthetic Product", "_1_description": "Synthetic\nmultiline description",
				"_1_price[unit_amount]": "6.99", "_1_tags[0]": "synthetic-language", "_1_tags[1]": "synthetic-category",
				"_1_os_versions[0][name]": "windows", "_1_s3_link_id": "synthetic-file",
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reader, err := r.MultipartReader()
				if err != nil {
					t.Error(err)
					http.Error(w, "invalid synthetic form", http.StatusBadRequest)
					return
				}
				received := map[string]string{}
				var order []string
				var snapshot map[string]string
				for {
					part, err := reader.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Error(err)
						return
					}
					value, err := io.ReadAll(part)
					if err != nil {
						t.Error(err)
						return
					}
					name := part.FormName()
					order = append(order, name)
					received[name] = string(value)
					if name == "0" {
						snapshot = make(map[string]string, len(received))
						for key, value := range received {
							snapshot[key] = value
						}
					}
				}
				if !reflect.DeepEqual(received, fields) || !reflect.DeepEqual(snapshot, fields) || len(order) == 0 || order[len(order)-1] != "0" {
					t.Errorf("referenced fields were not available at root: order=%v snapshot=%v", order, snapshot)
					fmt.Fprint(w, "1:{\"success\":false,\"message\":\"Required\"}\n")
					return
				}
				fmt.Fprint(w, "1:{\"success\":true}\n")
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			if err != nil {
				t.Fatal(err)
			}
			// Multiple requests exercise map iteration without accepting one lucky
			// root-last request as evidence that the encoder preserves order.
			for attempt := 0; attempt < 20; attempt++ {
				result, err := client.multipart(context.Background(), "/synthetic", "synthetic-action", fields)
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(result)
				if result["success"] != true {
					t.Fatalf("streamed form was incomplete: %s", encoded)
				}
			}
		})
	}
}
