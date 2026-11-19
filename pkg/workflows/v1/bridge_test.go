package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SENTINELITE/elgato-makers-cli/pkg/connect"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// This traverses the public constructor and bridge into the existing concrete
// provider implementation, with all traffic restricted to synthetic localhost.
func TestWriteConstructorBridgeAndDisabledReconciliation(t *testing.T) {
	for _, loseResponse := range []bool{false, true} {
		t.Run(fmt.Sprint("lost-response-", loseResponse), func(t *testing.T) {
			disabled := false
			posts := 0
			record := func() map[string]any {
				enabled, state := true, "active"
				if disabled {
					enabled, state = false, "disabled"
				}
				return map[string]any{"id": "one", "name": "Synthetic", "discount_type": "makers", "mode": "percentage", "percentage_off": "10", "makers": []any{"synthetic-org"}, "valid_from": "2030-01-01T00:00:00Z", "expires_at": nil, "enabled": enabled, "valid": true, "state": state, "items": []any{}, "code": map[string]any{"maker_code": "SAVE10", "code": "synthetic-SAVE10", "max_unique_users": nil, "max_uses_per_user": nil, "total_uses_count": 0}}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
					if r.Header.Get("Next-Action") != strings.Repeat("a", 40) {
						t.Error("missing discovered action")
					}
					disabled = true
					if loseResponse {
						fmt.Fprint(w, `1:{"success":false}`)
					} else {
						fmt.Fprint(w, `1:{"success":true}`)
					}
					return
				}
				if strings.HasPrefix(r.URL.Path, "/_next/static/") {
					fmt.Fprintf(w, `(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"toggleDiscountsEnabledAction");`, strings.Repeat("a", 40))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/one") {
					json.NewEncoder(w).Encode(map[string]any{"discount": record()})
					return
				}
				fmt.Fprint(w, `<script src="/_next/static/discount.js"></script>`)
			}))
			defer srv.Close()
			client, e := connect.NewWorkflowClient(connect.Credentials{}, connect.Config{BaseURL: srv.URL, AllowLocalhost: true})
			if e != nil {
				t.Fatal(e)
			}
			svc, e := New(t.TempDir(), "synthetic-org", client)
			if e != nil {
				t.Fatal(e)
			}
			ctx := context.Background()
			prepared, e := svc.PrepareDeactivation(ctx, "one", "SAVE10")
			if e != nil {
				t.Fatal(e)
			}
			result, e := svc.Deactivate(ctx, prepared.Ref)
			if loseResponse && e == nil {
				t.Fatal("ambiguous response accepted")
			}
			if !loseResponse && e != nil {
				t.Fatal(e)
			}
			if posts != 1 || !disabled {
				t.Fatalf("write bridge did not dispatch: %d", posts)
			}
			if len(result.Plan) == 0 {
				t.Fatal("lost uncertain checkpoint")
			}
			status, e := svc.DeactivationStatus(ctx, prepared.Ref.ID)
			if e != nil {
				t.Fatal(e)
			}
			var out struct {
				Local Review `json:"local"`
			}
			if e = json.Unmarshal(status, &out); e != nil {
				t.Fatal(e)
			}
			var plan deactivationPlan
			json.Unmarshal(out.Local.Plan, &plan)
			if plan.State != "verified" {
				t.Fatalf("status not reconciled: %s", plan.State)
			}
			if loseResponse && plan.Receipt != nil {
				t.Fatal("status fabricated acknowledgement")
			}
			loaded, e := svc.Show(Deactivation, prepared.Ref.ID)
			if e != nil || loaded.Ref != out.Local.Ref {
				t.Fatal("terminal status not persisted", e)
			}
			if _, e = svc.Deactivate(ctx, loaded.Ref); e == nil {
				t.Fatal("replayed mutation")
			}
			if posts != 1 {
				t.Fatal("status or retry wrote again")
			}
			path, _ := svc.deactivationPath(prepared.Ref.ID)
			if _, e = os.Stat(path + ".attempt"); e != nil {
				t.Fatal("attempt guard removed")
			}
		})
	}
}
