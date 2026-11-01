package secrets

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A SOURCE IS A WRITE PATH'S OWN WORD AND NOTHING ELSE. The set is closed by
// the paths, so every word one of them stamps is admitted, and what a caller
// used to be able to choose — a provisioning command's own name, a client's
// name for itself, the empty word, a word in another case, the `rekey` no
// path stamps any more — is refused at the write, naming the word, rather than
// stored for every reader to guess at. Mutation: make Valid answer true and the
// second half fails; drop a path from Sources and the first does.
func TestASourceIsAWritePathsWordAndNothingElse(t *testing.T) {
	t.Parallel()
	for _, s := range []Source{SourceAPI, SourceSetup, SourceProvision,
		SourceCLI, SourceLLMLogin, SourceIAM, SourceModels} {
		if !s.Valid() {
			t.Errorf("%q is a write path's word and is not Valid", s)
		}
		if err := CheckSource(s); err != nil {
			t.Errorf("CheckSource(%q) = %v, want it admitted", s, err)
		}
	}
	for _, s := range []Source{"", "gitlab-provision", "operator", "API", "rekey", "migrated"} {
		if s.Valid() {
			t.Errorf("%q is no write path's word and is Valid", s)
		}
		err := CheckSource(s)
		if err == nil {
			t.Errorf("CheckSource(%q) admitted a word no path stamps", s)
			continue
		}
		if !strings.Contains(err.Error(), strconv.Quote(string(s))) {
			t.Errorf("CheckSource(%q) = %v, want the refusal to name the word", s, err)
		}
	}
}

// EVERY SOURCE CONSTANT IS IN Sources, read off this package's own source. The
// list is what every store admits, so a path given a constant and left out of
// it would have each of its writes refused — and a list kept beside the
// constants by hand is exactly the copy that drifts. Mutation: declare another
// Source constant without listing it and this fails naming it.
func TestEverySourceConstantIsListed(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "record.go", nil, 0)
	if err != nil {
		t.Fatalf("parse record.go: %v", err)
	}
	var declared []Source
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			if typ, ok := value.Type.(*ast.Ident); !ok || typ.Name != "Source" {
				continue
			}
			for _, v := range value.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("a Source constant is not a string literal: %#v", v)
				}
				word, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				declared = append(declared, Source(word))
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no Source constant in record.go, so this checked nothing")
	}
	listed := slices.Clone(Sources)
	slices.Sort(declared)
	slices.Sort(listed)
	if !slices.Equal(declared, listed) {
		t.Errorf("record.go declares %q and Sources lists %q; each constant "+
			"is a write path's word, and Sources is what every store admits",
			declared, listed)
	}
}

// A VALUE IS HELD TO ONE SIZE, AND ITS REFUSAL NAMES THE SIZE, NEVER THE
// VALUE.
//
// Every store asks this one function at the write, because every reader of a
// revealed value through a node is sized from the same number: a store that
// took more could hold a credential a reveal cannot give back, which is what
// a `crewlet secrets set` run with the engine stopped, writing the fleet's
// store directly, could leave there past the API's limit. A value exactly at
// the limit is a value, or the guard is an outage at its own boundary.
func TestAValueIsHeldToTheSizeEveryReaderIsSizedFor(t *testing.T) {
	t.Parallel()
	if err := CheckValue(strings.Repeat("Q", MaxValueBytes)); err != nil {
		t.Fatalf("a value of exactly MaxValueBytes was refused: %v", err)
	}
	if err := CheckValue(""); err != nil {
		t.Fatalf("the empty value was refused: %v", err)
	}
	err := CheckValue(strings.Repeat("Q", MaxValueBytes+1))
	if !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("a value a byte past MaxValueBytes answered %v, want ErrValueTooLarge", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(MaxValueBytes)) {
		t.Errorf("the refusal %q does not name the limit", err)
	}
	if strings.Contains(err.Error(), "QQQQ") {
		t.Error("the refusal quotes the value it refused")
	}
}
