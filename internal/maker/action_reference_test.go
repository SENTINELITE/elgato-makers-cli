package maker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func syntheticBoundAction(hash, name string) string {
	return fmt.Sprintf(`(0,c.createServerReference)("%s",c.callServer,void 0,c.findSourceMapURL,"%s")`, hash, name)
}
func TestServerReferencePatternsNeverCrossChainedCalls(t *testing.T) {
	for _, name := range []string{"uploadAux", "getIsSdVersionValid", "createVersion", "createProduct", "updateProductDetails", "updateProduct", "deleteProductAction"} {
		t.Run(name, func(t *testing.T) {
			unrelated := strings.Repeat("a", 40)
			wanted := strings.Repeat("b", 40)
			source := `let first=` + syntheticBoundAction(unrelated, "getIsIcueIdAvailable") + `,second=` + syntheticBoundAction(wanted, name) + `;`
			patterns := []*regexp.Regexp{serverReferencePattern(regexp.QuoteMeta(name))}
			switch name {
			case "uploadAux", "getIsSdVersionValid", "createVersion":
				patterns = append(patterns, actionPattern)
			case "createProduct":
				patterns = append(patterns, draftActionPattern)
			case "updateProduct":
				patterns = append(patterns, metadataAction)
			case "deleteProductAction":
				patterns = append(patterns, deleteActionPattern)
			}
			for _, pattern := range patterns {
				matches := pattern.FindAllStringSubmatch(source, -1)
				if len(matches) != 1 || matches[0][1] != wanted {
					t.Fatalf("incorrect same-call binding: matches=%v", matches)
				}
			}
		})
	}
}
func TestServerReferencePatternRejectsUnsupportedArgumentBoundaries(t *testing.T) {
	hash := strings.Repeat("a", 40)
	pattern := serverReferencePattern(`getIsSdVersionValid`)
	for _, arguments := range []string{`c.callServer()`, `choose(c.callServer)`, `"getIsIcueIdAvailable"),next=(0,c.createServerReference)("` + strings.Repeat("b", 40) + `",c.callServer`, `{server:c.callServer}`, `(c.callServer)`, `c.callServer,"literal",c.findSourceMapURL`, `c.callServer,void 0,c.findSourceMapURL,c.extra`} {
		source := `(0,c.createServerReference)("` + hash + `",` + arguments + `,"getIsSdVersionValid")`
		// A real subsequent complete call is allowed; no match may bind the first
		// hash across the malformed or completed preceding argument list.
		for _, match := range pattern.FindAllStringSubmatch(source, -1) {
			if match[1] == hash {
				t.Fatalf("accepted unsupported cross-call arguments: %s", arguments)
			}
		}
	}
	for _, arguments := range []string{`x`, `c.callServer,void 0,c.findSourceMapURL`, `c.callServer,undefined,c.findSourceMapURL`} {
		source := `(0,c.createServerReference)("` + hash + `",` + arguments + `,"getIsSdVersionValid")`
		if !pattern.MatchString(source) {
			t.Fatalf("rejected supported arguments: %s", arguments)
		}
	}
}
func TestSubmitChainedActionBindingAndValidationSchema(t *testing.T) {
	for _, tc := range []struct {
		name, body, category string
		submit               bool
	}{
		{"recognized true", `{"is_valid":true}`, "", true},
		{"explicit false", `{"is_valid":false}`, "minimum Stream Deck version was rejected", false},
		{"unrelated available result", `{"available":false}`, "minimum Stream Deck version validation returned an unrecognized schema", false},
		{"string true", `{"is_valid":"true"}`, "minimum Stream Deck version validation returned an unrecognized schema", false},
		{"null validity", `{"is_valid":null}`, "minimum Stream Deck version validation returned an unrecognized schema", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, submission := syntheticSubmission()
			validations, creates := 0, 0
			validator, unrelated, create, upload := strings.Repeat("b", 40), strings.Repeat("a", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
			source := `let a=` + syntheticBoundAction(unrelated, "getIsIcueIdAvailable") + `,b=` + syntheticBoundAction(validator, "getIsSdVersionValid") + `,c=` + syntheticBoundAction(unrelated, "unrelatedCreate") + `,d=` + syntheticBoundAction(create, "createVersion") + `,e=` + syntheticBoundAction(upload, "uploadAux") + `;`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					switch r.URL.Path {
					case "/api/upload-aux/manifest/folder":
						fmt.Fprintf(w, `{"UUID":%q,"Version":%q,"Software":{"MinimumVersion":%q}}`, submission.Upload.Manifest.UUID, submission.Upload.Manifest.Version, submission.Upload.Manifest.Software.MinimumVersion)
					case "/_next/static/chunk.js":
						fmt.Fprint(w, source)
					default:
						fmt.Fprint(w, fixturePage())
					}
					return
				}
				switch r.Header.Get("Next-Action") {
				case validator:
					validations++
					fmt.Fprintf(w, "1:%s\n", tc.body)
				case create:
					creates++
					fmt.Fprint(w, "1:{\"success\":true,\"id\":\"synthetic-version\"}\n")
				default:
					t.Error("selected unrelated server-action reference")
					fmt.Fprint(w, "1:{\"available\":false}\n")
				}
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			_, err := client.Submit(context.Background(), target, submission)
			if validations != 1 {
				t.Fatalf("validator calls=%d", validations)
			}
			if tc.submit {
				if err != nil || creates != 1 {
					t.Fatalf("matching submission: error=%v creates=%d", err, creates)
				}
			} else {
				var notSubmitted *NotSubmittedError
				if !errors.As(err, &notSubmitted) || !strings.Contains(err.Error(), tc.category) || creates != 0 {
					t.Fatalf("preflight: error=%v creates=%d", err, creates)
				}
			}
		})
	}
}
func TestNotSubmittedDiagnosticsAllowlistAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{errors.New("submission manifest identity mismatch"), "reviewed manifest identity mismatch"},
		{errors.New("fresh submission manifest identity mismatch"), "uploaded manifest identity changed"},
		{errors.New("uploaded manifest changed after review; review the upload before submission"), "uploaded manifest changed after review"},
		{errors.New("unrecognized uploaded manifest"), "uploaded manifest schema not recognized"},
		{errors.New("conflicting Maker action references"), "submission action references conflict"},
		{errors.New("current Maker action references not recognized"), "submission action references not recognized"},
		{fmt.Errorf("synthetic-secret https://example.test/?token=synthetic-secret: %w", errors.New("missing Maker action result")), "submission preflight action response not recognized"},
		{errors.New("https://example.test/?token=synthetic-secret"), ""},
		{errors.New("Stream Deck minimum version rejected synthetic-secret"), ""},
	} {
		err := &NotSubmittedError{Cause: tc.cause}
		if got := err.FailureCategory(); got != tc.want {
			t.Errorf("category=%q want %q", got, tc.want)
		}
		if strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "example.test") {
			t.Fatal("unsafe preflight diagnostics")
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Fatal("known diagnostic missing")
		}
		if !errors.Is(err, tc.cause) || !err.NotSubmitted() {
			t.Fatal("not-submitted identity changed")
		}
	}
}

func TestDiscoverActionsRejectsConflictingChainedReferences(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict=%v", conflict), func(t *testing.T) {
			validator := strings.Repeat("b", 40)
			second := validator
			if conflict {
				second = strings.Repeat("c", 40)
			}
			source := `let a=` + syntheticBoundAction(strings.Repeat("d", 40), "uploadAux") + `,b=` + syntheticBoundAction(validator, "getIsSdVersionValid") + `,c=` + syntheticBoundAction(second, "getIsSdVersionValid") + `,d=` + syntheticBoundAction(strings.Repeat("e", 40), "createVersion") + `;`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("discovery attempted mutation: %s", r.Method)
				}
				if r.URL.Path == "/_next/static/chunk.js" {
					fmt.Fprint(w, source)
				} else {
					fmt.Fprint(w, fixturePage())
				}
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, AllowLocalhost: true})
			target, _ := syntheticSubmission()
			actions, err := client.DiscoverActions(context.Background(), target)
			if conflict {
				if err == nil || err.Error() != "conflicting Maker action references" {
					t.Fatalf("conflict error=%v", err)
				}
			} else if err != nil || actions.GetIsSDVersionValid != validator {
				t.Fatalf("same reference actions=%+v error=%v", actions, err)
			}
		})
	}
}
