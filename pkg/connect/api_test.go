package connect

import (
	"bytes"
	"flag"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var updateAPI = flag.Bool("update-api", false, "rewrite testdata/api.golden after an intentional, versioned API change")

// exportedAPI renders every exported declaration of this package from source,
// without bodies, comments or unexported fields, one sorted block per declaration.
func exportedAPI(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var blocks []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() || d.Recv != nil && !receiverExported(d.Recv) {
					continue
				}
				d.Body, d.Doc = nil, nil
			case *ast.GenDecl:
				if d.Tok == token.IMPORT || !ast.FilterDecl(d, ast.IsExported) {
					continue
				}
				d.Doc = nil
				dropUnexportedFields(d)
			}
			var b bytes.Buffer
			if err := printer.Fprint(&b, fset, decl); err != nil {
				t.Fatal(err)
			}
			blocks = append(blocks, b.String())
		}
	}
	sort.Strings(blocks)
	return strings.Join(blocks, "\n\n") + "\n"
}

func dropUnexportedFields(d *ast.GenDecl) {
	for _, spec := range d.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			continue
		}
		kept := st.Fields.List[:0]
		for _, field := range st.Fields.List {
			if len(field.Names) > 0 && !field.Names[0].IsExported() {
				continue
			}
			kept = append(kept, field)
		}
		st.Fields.List = kept
	}
}

func receiverExported(recv *ast.FieldList) bool {
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	id, ok := expr.(*ast.Ident)
	return ok && id.IsExported()
}

// aliasedSurface pins members of exported aliases of internal types, which the
// source declaration alone does not show.
func aliasedSurface() string {
	var lines []string
	for _, v := range []any{Credentials{}, &AnalyticsRangeError{}} {
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumMethod(); i++ {
			lines = append(lines, typ.String()+"."+typ.Method(i).Name+" "+typ.Method(i).Type.String())
		}
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		for i := 0; typ.Kind() == reflect.Struct && i < typ.NumField(); i++ {
			if typ.Field(i).IsExported() {
				lines = append(lines, typ.String()+" field "+typ.Field(i).Name+" "+typ.Field(i).Type.String())
			}
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// TestExportedAPIIsPinned fails on any change to the exported surface. An
// intentional change must follow docs/ADAPTER.md: bump ContractVersion, then run
// go test ./pkg/connect -run TestExportedAPIIsPinned -update-api.
func TestExportedAPIIsPinned(t *testing.T) {
	got := "contract " + ContractVersion + "\n\n" + exportedAPI(t) + "\naliased members\n" + aliasedSurface()
	golden := filepath.Join("testdata", "api.golden")
	if *updateAPI {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("pkg/connect exported API changed without updating %s and ContractVersion\n--- want\n%s\n--- got\n%s", golden, want, got)
	}
}
