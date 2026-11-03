package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The text every fake node below answers with, as a node writes it: through
// json.Marshal, which escapes each of its markup characters.
const (
	markupText   = "a < b & c > d"
	unknownField = "a_field_this_build_does_not_know"
	// past 2^53, so a float64 decode prints a different number back
	bigNumber = "1234567890123456789"
)

// markupAnswer is a node's answer carrying markup, a field this build does not
// know, and a number no float64 holds, encoded the way the node encodes it.
func markupAnswer(t *testing.T, around func(map[string]any) any) []byte {
	t.Helper()
	inner := map[string]any{
		"text":       markupText,
		unknownField: map[string]any{"note": markupText},
		"count":      json.Number(bigNumber),
	}
	raw, err := json.Marshal(around(inner))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `\u003c`) {
		t.Fatalf("the node's encoding did not escape the markup, so the case "+
			"asserts nothing: %s", raw)
	}
	return raw
}

// serveMarkup answers each of the routes a -json command reads with
// [markupAnswer], each in the envelope its command reads.
func serveMarkup(t *testing.T) *httptest.Server {
	t.Helper()
	whole := markupAnswer(t, func(m map[string]any) any { return m })
	people := markupAnswer(t, func(m map[string]any) any {
		m["people"] = []any{}
		return m
	})
	fleet := markupAnswer(t, func(m map[string]any) any {
		return map[string]any{"nodes": []any{}, "objects": m}
	})
	// A verification the node finished before it answered, so the command
	// prints it with no poll.
	verified := markupAnswer(t, func(m map[string]any) any {
		m["id"], m["repo"], m["state"] = "v-1", "web", "done"
		return m
	})
	mux := http.NewServeMux()
	answer := func(body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}
	}
	mux.HandleFunc("GET /iam/people", answer(people))
	mux.HandleFunc("GET /fleet/broker", answer(whole))
	mux.HandleFunc("GET /fleet", answer(fleet))
	mux.HandleFunc("POST /objects/migrate", answer(whole))
	// One page of the code listing, with no next page, for both `code
	// status` and the listing `code verify` reads its floor from.
	mux.HandleFunc("GET /code/repos", answer(whole))
	mux.HandleFunc("POST /code/repos/web/verify", answer(verified))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// jsonOutputs is every command with a -json flag, keyed by the function that
// declares the flag, and how to run it against a node answering markup.
//
// [TestEveryJSONFlagHasATextCase] holds the keys to the flags the source
// declares, so a new -json command cannot ship without a row here.
func jsonOutputs(t *testing.T) map[string]struct {
	args []string
	// want is the text the output must carry as written
	want []string
	// node is whether the output is a node's answer, which must keep every
	// field and number the node sent
	node bool
} {
	node := bootstrapForURL(t, serveMarkup(t).URL)
	dir := t.TempDir()
	solo := writeYAML(t, "solo.yaml",
		"store:\n  path: "+filepath.Join(dir, "c.db")+"\n"+
			"stream:\n  store_dir: "+filepath.Join(dir, "s")+"\n  replicas: 2\n"+
			fixtureKeyring)
	return map[string]struct {
		args []string
		want []string
		node bool
	}{
		"validateConfigs": {args: []string{"validate", solo, "-json"},
			want: []string{"replicas > 1 needs peers"}},
		"runIAM": {args: []string{"iam", "people", "-json", "-config", node},
			want: []string{markupText}, node: true},
		"fleetBrokerList": {args: []string{"fleet", "broker", "list", "-json", node},
			want: []string{markupText}, node: true},
		"objectsStatus": {args: []string{"objects", "status", "-json", node},
			want: []string{markupText}, node: true},
		"objectsMigrate": {args: []string{"objects", "migrate", "-to", "s3",
			"-target", "/etc/crewlet/s3.yaml", "-json", node},
			want: []string{markupText}, node: true},
		"codeStatus": {args: []string{"code", "status", "-json", node},
			want: []string{markupText}, node: true},
		"codeVerify": {args: []string{"code", "verify", "-repo", "web", "-json", node},
			want: []string{markupText}, node: true},
	}
}

// EVERY -json OUTPUT SPELLS ITS TEXT AS WRITTEN, whatever the node's encoder
// did to it. A person reads the output at a terminal, a script matches strings
// in it and a model fixes a document from it, and none of them drops it in an
// HTML page — so `replicas \u003e 1 needs peers` and a node's `a \u003c b`
// were escapes each reader had to decode back. A node's answer keeps every
// field this build does not know and every number digit for digit, which
// copying it as it came used to buy and a typed decode would lose.
//
// Mutations, each red: printing with json.MarshalIndent; fleet, either
// objects command, or `code status` or `code verify` back to copying the
// node's bytes; dropping UseNumber from the decode, which turns
// the 19-digit number into another.
func TestEveryJSONOutputSpellsItsTextAsWritten(t *testing.T) {
	t.Parallel()
	for name, tc := range jsonOutputs(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, stderr, _ := cli(t, tc.args...)
			var doc any
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("%s printed no JSON document: %v\n%s\n%s",
					strings.Join(tc.args, " "), err, out, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("the output does not carry %q as written:\n%s", want, out)
				}
			}
			for _, escape := range []string{`\u003c`, `\u003e`, `\u0026`} {
				if strings.Contains(out, escape) {
					t.Errorf("the output carries the escape %s:\n%s", escape, out)
				}
			}
			if !tc.node {
				return
			}
			if !strings.Contains(out, strconv.Quote(unknownField)) {
				t.Errorf("the output dropped the field %s the node sent:\n%s",
					unknownField, out)
			}
			if !strings.Contains(out, `"count": `+bigNumber) {
				t.Errorf("the output does not spell the node's number %s "+
					"digit for digit:\n%s", bigNumber, out)
			}
		})
	}
}

// EVERY -json FLAG HAS A ROW in [jsonOutputs]: the functions that declare one,
// read off the source, are exactly the table's keys. A new command whose -json
// printed through a writer of its own would otherwise carry the escapes this
// file removed, with nothing to notice. Mutation: a row removed, or a function
// declaring a flag the table does not name, is red.
func TestEveryJSONFlagHasATextCase(t *testing.T) {
	t.Parallel()
	var declared []string
	walkSource(t, func(file string, fset *token.FileSet, f *ast.File) {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Bool" {
					return true
				}
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"json"` {
					declared = append(declared, fn.Name.Name)
				}
				return true
			})
		}
	})
	slices.Sort(declared)
	var rows []string
	for name := range jsonOutputs(t) {
		rows = append(rows, name)
	}
	sort.Strings(rows)
	if len(declared) == 0 {
		t.Fatal("no -json flag was found in the source, so this guard asserts nothing")
	}
	if !slices.Equal(declared, rows) {
		t.Errorf("the functions declaring a -json flag are %v, and the cases "+
			"printing one are %v: give each flag a row in jsonOutputs", declared, rows)
	}
}

// ONLY THE PRINTER ENCODES FOR OUTPUT. json.Marshal stays legal, because a
// request body is read by the node's decoder; what is refused is an encoder
// that writes JSON for somebody to read, anywhere but jsonout.go — where a
// second one would decide again how markup and numbers are spelled. Mutation:
// json.MarshalIndent back in iam.go is red.
func TestOnlyThePrinterEncodesForOutput(t *testing.T) {
	t.Parallel()
	refused := map[string][]string{
		"json":      {"MarshalIndent", "NewEncoder"},
		"plainjson": {"Marshal"},
	}
	walkSource(t, func(file string, fset *token.FileSet, f *ast.File) {
		if file == "jsonout.go" {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && slices.Contains(refused[pkg.Name], sel.Sel.Name) {
				t.Errorf("%s: %s.%s writes JSON for a reader outside the "+
					"printer: print through printJSON or printAnswer",
					fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name)
			}
			return true
		})
	})
}

// walkSource hands every non-test Go file of this command to visit.
func walkSource(t *testing.T, visit func(file string, fset *token.FileSet, f *ast.File)) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		read++
		visit(file, fset, parsed)
	}
	if read < 10 {
		t.Fatalf("read %d source files, so this guard asserts nothing", read)
	}
}
