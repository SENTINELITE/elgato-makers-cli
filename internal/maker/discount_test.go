package maker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SENTINELITE/elgato-makers-cli/pkg/discountplan"
)

func discountFixture(id string) map[string]any {
	return map[string]any{"id": id, "name": "Synthetic", "discount_type": "makers", "mode": "percentage", "percentage_off": "10", "makers": []any{"synthetic-org"}, "valid_from": "2030-01-01T00:00:00Z", "expires_at": nil, "enabled": true, "valid": true, "state": "active", "items": []any{}, "code": map[string]any{"maker_code": "SAVE10", "code": "synthetic-SAVE10", "max_unique_users": nil, "max_uses_per_user": nil, "total_uses_count": 0}}
}

func TestCapturedDiscountCreation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		targeted   bool
		productOrg string
		changeRow  func(map[string]any)
	}{
		{name: "fixed USD"},
		{name: "fixed incorrect currency", changeRow: func(row map[string]any) { row["currency"] = "EUR" }},
		{name: "fixed wrong minor units", changeRow: func(row map[string]any) { row["amount"] = 5 }},
		{name: "fixed fractional value that rounds to 500", changeRow: func(row map[string]any) { row["amount"] = json.RawMessage("500.00000000000000001") }},
		{name: "owned targeted percentage", targeted: true, productOrg: "synthetic-org"},
		{name: "foreign product", targeted: true, productOrg: "foreign-org"},
		{name: "wrong readback target", targeted: true, productOrg: "synthetic-org", changeRow: func(row map[string]any) { row["items"] = []string{"wrong-product"} }},
		{name: "extra readback target", targeted: true, productOrg: "synthetic-org", changeRow: func(row map[string]any) { row["items"] = []string{"synthetic-product", "extra-product"} }},
		{name: "widened readback scope", targeted: true, productOrg: "synthetic-org", changeRow: func(row map[string]any) { row["discount_type"] = "makers" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const org, product = "synthetic-org", "synthetic-product"
			end, limit := "2030-01-03T23:59:00Z", 1
			input := discountplan.Input{
				OrganizationID: org, Name: "Synthetic", MakerCode: "SAVE10",
				Mode: "fixed", Scope: "all_products", AmountMinor: 500, Currency: "USD",
				StartAt: "2030-01-01T00:00:00Z", EndAt: &end, MaxUniqueUsers: &limit, MaxUsesPerUser: &limit,
			}
			row := discountFixture("one")
			row["state"], row["valid"], row["expires_at"] = "upcoming", false, end
			code := row["code"].(map[string]any)
			code["max_unique_users"], code["max_uses_per_user"] = limit, limit
			want := [][2]string{
				{"_1_method", "code"}, {"_1_name", "Synthetic"}, {"_1_makerCode", "SAVE10"},
				{"_1_startDate", "2030-01-01"}, {"_1_startTime", "00:00"}, {"_1_hasEndDate", "true"},
				{"_1_endDate", "2030-01-03"}, {"_1_endTime", "23:59"},
			}
			if tc.targeted {
				input.Mode, input.Scope, input.ProductIDs = "percentage", "specific_products", []string{product}
				input.PercentageOff, input.AmountMinor, input.Currency = 15, 0, ""
				row["discount_type"], row["items"], row["percentage_off"] = "products", []string{product}, "15"
				row["amount"], row["currency"] = nil, nil
				want = append(want, [2]string{"_1_mode", "percentage"}, [2]string{"_1_percentageOff", "15"}, [2]string{"_1_appliesTo", "products"}, [2]string{"_1_items[0]", product})
			} else {
				row["mode"], row["amount"], row["currency"], row["percentage_off"] = "absolute", 500, "USD", nil
				want = append(want, [2]string{"_1_mode", "absolute"}, [2]string{"_1_amount", "5"}, [2]string{"_1_appliesTo", "makers"})
			}
			want = append(want,
				[2]string{"_1_limitUniqueUsers", "true"}, [2]string{"_1_maxUniqueUsers", "1"},
				[2]string{"_1_limitUsesPerUser", "true"}, [2]string{"_1_maxUsesPerUser", "1"},
				[2]string{"_1_organizationId", org}, [2]string{"_1_currency", "USD"}, [2]string{"0", `[null,"$K1"]`},
			)
			if tc.changeRow != nil {
				tc.changeRow(row)
			}
			plan, err := discountplan.Prepare(input)
			if err != nil {
				t.Fatal(err)
			}

			var productRead, checkpoint atomic.Bool
			var posts atomic.Int32
			action := strings.Repeat("c", 40)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "POST" && r.URL.Path == "/"+org+"/discounts/create":
					posts.Add(1)
					if r.Header.Get("Next-Action") != action || (tc.targeted && !productRead.Load()) {
						t.Error("creation dispatch lacked current action or owned-product read")
					}
					reader, err := r.MultipartReader()
					if err != nil {
						t.Error(err)
						return
					}
					var fields [][2]string
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
						fields = append(fields, [2]string{part.FormName(), string(value)})
					}
					if !slices.Equal(fields, want) {
						t.Errorf("multipart fields in wire order = %q, want %q", fields, want)
					}
					fmt.Fprint(w, `1:{"success":true,"discountId":"one"}`)
				case r.Method == "GET" && r.URL.Path == "/"+org+"/discounts/create":
					fmt.Fprint(w, `<script src="/_next/static/discount.js"></script>`)
				case r.Method == "GET" && r.URL.Path == "/_next/static/discount.js":
					fmt.Fprintf(w, `(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"createDiscount");`, action)
				case r.Method == "GET" && r.URL.Path == "/"+org+"/products/"+product && tc.targeted:
					productRead.Store(true)
					fmt.Fprintf(w, `{"product":{"id":%q,"organization_id":%q,"name":"Synthetic Product","variants":[]}}`, product, tc.productOrg)
				case r.Method == "GET" && r.URL.Path == "/"+org+"/discounts/one":
					if !checkpoint.Load() {
						t.Error("readback preceded durable receipt")
					}
					b, _ := json.Marshal(map[string]any{"discount": row})
					stream, _ := json.Marshal("1:" + string(b) + "\n")
					fmt.Fprintf(w, `<script>self.__next_f.push([1,%s])</script>`, stream)
				case r.Method == "GET" && r.URL.Path == "/"+org+"/discounts":
					listFixture(w, []any{}, 0, 1, 1)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer s.Close()
			c, err := New(Config{BaseURL: s.URL, AllowLocalhost: true})
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := c.CreateDiscount(context.Background(), plan, func(r DiscountReceipt) error {
				if r.DiscountID != "one" || !r.Success {
					t.Errorf("unexpected receipt %+v", r)
				}
				checkpoint.Store(true)
				return nil
			})
			if tc.targeted && tc.productOrg != org {
				if err == nil || !productRead.Load() || posts.Load() != 0 || checkpoint.Load() {
					t.Fatalf("foreign product reached dispatch or receipt, got %+v, %v", receipt, err)
				}
				return
			}
			if !checkpoint.Load() || posts.Load() != 1 || receipt.DiscountID != "one" {
				t.Fatalf("expected one acknowledged dispatch, got %+v, %v", receipt, err)
			}
			var ambiguous *AmbiguousDiscountError
			if (tc.changeRow == nil && err != nil) || (tc.changeRow != nil && !errors.As(err, &ambiguous)) {
				t.Fatalf("unexpected result for provider readback %+v, %v", row, err)
			}
		})
	}
}
func listFixture(w http.ResponseWriter, rows []any, total, pages, page int) {
	b, _ := json.Marshal(map[string]any{"discounts": rows, "totalDiscounts": total, "pagination": map[string]int{"pageCount": pages, "selectedPage": page}})
	stream, _ := json.Marshal("1:" + string(b) + "\n")
	fmt.Fprintf(w, `<script>self.__next_f.push([1,%s])</script>`, stream)
}
func TestDiscountPaginationAndOrganization(t *testing.T) {
	duplicate := false
	foreign := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		id := "one"
		if r.URL.Query().Get("page") == "2" {
			page = 2
			if !duplicate {
				id = "two"
			}
		}
		row := discountFixture(id)
		if foreign {
			row["makers"] = []any{"other-org"}
		}
		listFixture(w, []any{row}, 2, 2, page)
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, AllowLocalhost: true})
	rows, e := c.Discounts(context.Background(), "synthetic-org", "")
	if e != nil || len(rows) != 2 {
		t.Fatal(rows, e)
	}
	duplicate = true
	if _, e = c.Discounts(context.Background(), "synthetic-org", ""); e == nil {
		t.Fatal("duplicate pagination accepted")
	}
	duplicate = false
	foreign = true
	if _, e = c.Discounts(context.Background(), "synthetic-org", ""); e == nil {
		t.Fatal("foreign org accepted")
	}
}
func TestDiscountScheduledAndDisabledRawState(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "scheduled" {
			t.Error("wrong mapped filter")
		}
		d := discountFixture("one")
		d["enabled"], d["state"], d["valid"] = false, "disabled", true
		listFixture(w, []any{d}, 1, 1, 1)
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, AllowLocalhost: true})
	rows, e := c.Discounts(context.Background(), "synthetic-org", "upcoming")
	if e != nil || rows[0]["valid"] != true || rows[0]["enabled"] != false {
		t.Fatal(rows, e)
	}
	if _, e = c.Discounts(context.Background(), "synthetic-org", "active"); e == nil {
		t.Fatal("unverified query accepted")
	}
}
func TestDiscountCreateReceiptBeforeReadbackAndDeactivate(t *testing.T) {
	created := false
	disabled := false
	checkpoint := false
	posts := 0
	actionHash := strings.Repeat("a", 40)
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
			mr, e := r.MultipartReader()
			if e != nil {
				t.Fatal(e)
			}
			keys := []string{}
			fields := map[string]string{}
			for {
				p, e := mr.NextPart()
				if e != nil {
					break
				}
				var text strings.Builder
				_, e = io.Copy(&text, p)
				_ = e
				keys = append(keys, p.FormName())
				fields[p.FormName()] = text.String()
			}
			if keys[len(keys)-1] != "0" {
				t.Error("root part not last")
			}
			if r.Header.Get("Next-Action") != actionHash {
				t.Error("wrong dynamic hash")
			}
			if strings.HasSuffix(r.URL.Path, "create") {
				if fields["_1_startDate"] != "2030-01-01" || fields["_1_startTime"] != "00:00" || fields["_1_hasEndDate"] != "false" || fields["_1_limitUniqueUsers"] != "false" || fields["_1_appliesTo"] != "makers" {
					t.Error("wrong create fields", fields)
				}
				created = true
				fmt.Fprint(w, `1:{"success":true,"discountId":"one"}`)
			} else {
				if fields["_1_discount_ids"] != `["one"]` || fields["_1_enabled"] != "false" {
					t.Error("wrong deactivate fields")
				}
				disabled = true
				fmt.Fprint(w, `1:{"success":true}`)
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_next/static/") {
			fmt.Fprintf(w, `(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"createDiscount");(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"toggleDiscountsEnabledAction");`, actionHash, actionHash)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/create") {
			fmt.Fprint(w, `<script src="/_next/static/discount.js"></script>`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/one") {
			if created && !checkpoint {
				t.Error("readback before durable receipt")
			}
			d := discountFixture("one")
			if disabled {
				d["enabled"], d["state"] = false, "disabled"
			}
			json.NewEncoder(w).Encode(map[string]any{"discount": d})
			return
		}
		fmt.Fprint(w, `<script src="/_next/static/discount.js"></script>`)
		rows := []any{}
		if created {
			rows = append(rows, discountFixture("one"))
		}
		listFixture(w, rows, len(rows), 1, 1)
	}))
	defer s.Close()
	c, _ := New(Config{BaseURL: s.URL, AllowLocalhost: true})
	p, e := discountplan.PrepareAt(discountplan.Input{OrganizationID: "synthetic-org", Name: "Synthetic", MakerCode: "SAVE10", Mode: "percentage", Scope: "all_products", PercentageOff: 10, StartAt: "2030-01-01T00:00:00Z"}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	r, e := c.CreateDiscount(context.Background(), p, func(DiscountReceipt) error { checkpoint = true; return nil })
	if e != nil || r.DiscountID != "one" {
		t.Fatal(r, e)
	}
	_, e = c.DeactivateDiscount(context.Background(), "synthetic-org", "one", "SAVE10", func(DiscountReceipt) error { checkpoint = true; return nil })
	if e != nil || posts != 2 {
		t.Fatal(e, posts)
	}
	ro, _ := New(Config{ReadOnly: true})
	if _, e = ro.CreateDiscount(context.Background(), p, func(DiscountReceipt) error { return nil }); e != ErrReadOnly {
		t.Fatal(e)
	}
	if _, e = ro.DeactivateDiscount(context.Background(), "synthetic-org", "one", "SAVE10", func(DiscountReceipt) error { return nil }); e != ErrReadOnly {
		t.Fatal(e)
	}
}
