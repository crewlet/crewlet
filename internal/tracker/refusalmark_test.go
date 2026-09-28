package tracker

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeFiles is every file whose errors reach a writer's caller: the writer
// itself, the decide helpers it calls, the pure validations of what a write
// carries, and the thread resolution a comment's `answers` goes through.
var writeFiles = []string{
	"catalogue.go", "coerce.go", "config.go", "depend.go", "mutation.go",
	"person.go", "project.go", "record.go", "relate.go", "sequence.go",
	"tags.go", "thread.go", "trash.go", "views.go", "write.go",
}

// unmarkedByDesign is every sentence in [writeFiles] that is built with
// fmt.Errorf, wraps nothing and is deliberately NOT a content refusal, keyed on
// the opening of its format string, with why.
//
// EACH IS A FAULT OF THE WIRING OR THE NODE rather than of what was asked:
// nothing a caller could send differently makes it land, so a surface must
// not tell a person to change their input over it.
var unmarkedByDesign = map[string]string{
	"tracker: a writer cannot act as":                      "a surface built a writer with no identity",
	"tracker: a writer has no publisher":                   "construction wiring",
	"tracker: a writer has no actor":                       "construction wiring",
	"tracker: %q is not an author kind":                    "construction wiring",
	"tracker: this writer has no store":                    "construction wiring",
	"tracker: this writer has no replicated ":              "construction wiring",
	"tracker: this writer has no coordination":             "construction wiring",
	"tracker: key %s belongs ":                             "a replicated alias row disagrees with the move that wrote it",
	"tracker: a scope carries the exactly-the-subject ":    "a scope is the writer's own, never the caller's",
	"tracker: container %q contains %q, which is ":         "a scope is the writer's own, never the caller's",
	"tracker: container %q is blank, and a blank ":         "a scope is the writer's own, never the caller's",
	"tracker: a scope enumerates %d term(s) and also ":     "a scope is the writer's own, never the caller's",
	"tracker: a scope is empty":                            "a scope is the writer's own, never the caller's",
	"tracker: a scope enumerates %d terms and the cap is ": "a scope is the writer's own, never the caller's",
	"tracker: a %s term names nothing":                     "a scope is the writer's own, never the caller's",
	"tracker: scope term kind %q is not one this build ":   "a scope is the writer's own, never the caller's",
	"tracker: a record carries version ":                   "decoding a record off the log",
	"tracker: op %q is not one this build writes":          "decoding a record off the log",
	"tracker: %q is not a barrier envelope":                "decoding a record off the log",
	"tracker: a barrier carries no op id":                  "decoding a record off the log",
	"tracker: a thread read names no task":                 "the builtin always names the task it resolved",
	"tracker: the %s record on %s must be readable ":       "a record version is the encoder's, never the caller's",
	"tracker: the %s record on %s is stamped version ":     "a record version is the encoder's, never the caller's",
}

// TestEveryWriteRefusalIsMarked walks [writeFiles] and refuses a fmt.Errorf that
// wraps nothing and is not in [unmarkedByDesign].
//
// THE WALK IS THE GATE because the failure is a NEW refusal. A writer that
// refuses a value with fmt.Errorf compiles, reads fine to a model — and
// reaches a person's write surface as the node's failure, because what is
// unmarked is read as the node's (see [ErrInvalid]). So every sentence either
// carries a sentinel through %w, is built with [invalid] or [forbidden], or is
// named here as the node's own fault with a reason.
func TestEveryWriteRefusalIsMarked(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	seen := map[string]bool{}
	marked := 0
	for _, name := range writeFiles {
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			format, ok := literal(call.Args[0])
			if !ok {
				return true
			}
			switch {
			case isIdent(call.Fun, "invalid") || isIdent(call.Fun, "forbidden"):
				marked++
			case isSelector(call.Fun, "fmt", "Errorf") && !strings.Contains(format, "%w"):
				for prefix := range unmarkedByDesign {
					if strings.HasPrefix(format, prefix) {
						seen[prefix] = true
						return true
					}
				}
				t.Errorf("%s: %q wraps nothing and is not marked — build a "+
					"content refusal with invalid(…), wrap the sentinel it "+
					"means with %%w, or name it in unmarkedByDesign as the "+
					"node's own fault", fset.Position(call.Pos()), format)
			}
			return true
		})
	}
	// TWO-SIDED: an entry that matches nothing is a sentence that was
	// reworded or removed, and an exemption nothing uses is one the next
	// refusal can hide behind.
	for prefix := range unmarkedByDesign {
		if !seen[prefix] {
			t.Errorf("unmarkedByDesign names %q and no sentence starts with it", prefix)
		}
	}
	// NOT VACUOUS: a walk that stopped recognising the helper would pass on
	// zero.
	if marked < 100 {
		t.Fatalf("the walk found %d marked refusals, want at least 100", marked)
	}
}

// TestNoRefusalSpellsThePackagePrefix walks EVERY file of the package, not
// only [writeFiles] — a read's parameter check is built with [invalid] too —
// for a refusal whose sentence opens with "tracker:". The prefix is the
// refusal's own to add ([refusal.Error]): spelled again, the log reads
// "tracker: tracker: …" and [Sentence] hands a person the package's name.
func TestNoRefusalSpellsThePackagePrefix(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	built := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 ||
				(!isIdent(call.Fun, "invalid") && !isIdent(call.Fun, "forbidden")) {
				return true
			}
			format, ok := literal(call.Args[0])
			if !ok {
				t.Errorf("%s: a refusal's sentence is not a literal, so nothing can "+
					"check it", fset.Position(call.Pos()))
				return true
			}
			built++
			if strings.HasPrefix(format, "tracker:") {
				t.Errorf("%s: %q opens with the package prefix, which the "+
					"refusal adds itself — write the sentence alone",
					fset.Position(call.Pos()), format)
			}
			return true
		})
	}
	// NOT VACUOUS, for the reason the walk above gives.
	if built < 100 {
		t.Fatalf("the walk found %d refusals, want at least 100", built)
	}
}

// TestAMarkedRefusalKeepsItsSentenceAndItsChain pins the halves of [invalid]
// and [forbidden]: the Go error reads as every error here does, prefix and
// all, which is the text a model was tuned against; a sentinel the sentence
// wraps is still reachable beside the mark; and [Sentence] is the words alone,
// which is what a person is shown.
func TestAMarkedRefusalKeepsItsSentenceAndItsChain(t *testing.T) {
	t.Parallel()
	inner := errors.New("disk")
	err := invalid("mint a key between %q and %q: %w", "a", "b", inner)
	if got, want := err.Error(), `tracker: mint a key between "a" and "b": disk`; got != want {
		t.Errorf("the error is %q, want %q", got, want)
	}
	if got, want := Sentence(err), `mint a key between "a" and "b": disk`; got != want {
		t.Errorf("the sentence is %q, want %q", got, want)
	}
	if !errors.Is(err, ErrInvalid) || !errors.Is(err, inner) || errors.Is(err, ErrForbidden) {
		t.Errorf("the chain is wrong: invalid %v, inner %v, forbidden %v",
			errors.Is(err, ErrInvalid), errors.Is(err, inner), errors.Is(err, ErrForbidden))
	}

	// FORBIDDEN SAYS WHOSE IT IS ONCE: the sentinel's own words used to be
	// appended after a sentence that had already said so.
	denied := forbidden("view %s is protected and belongs to %s", "v1", "ada")
	if got, want := denied.Error(), "tracker: view v1 is protected and belongs to ada"; got != want {
		t.Errorf("the error is %q, want %q", got, want)
	}
	if got, want := Sentence(denied), "view v1 is protected and belongs to ada"; got != want {
		t.Errorf("the sentence is %q, want %q", got, want)
	}
	if !errors.Is(denied, ErrForbidden) || errors.Is(denied, ErrInvalid) {
		t.Errorf("forbidden is classed wrong: forbidden %v, invalid %v",
			errors.Is(denied, ErrForbidden), errors.Is(denied, ErrInvalid))
	}

	// CONTEXT IS NEVER DROPPED TO TIDY A PREFIX: a refusal a caller wrapped
	// is its whole message, and an error this package did not write as a
	// refusal is too.
	wrapped := fmt.Errorf("tracker: create ENG: %w", err)
	if got := Sentence(wrapped); got != wrapped.Error() {
		t.Errorf("a wrapped refusal's sentence is %q, want its whole message", got)
	}
	if got := Sentence(ErrNoTask); got != ErrNoTask.Error() {
		t.Errorf("an unmarked error's sentence is %q, want its whole message", got)
	}
}

// literal is the format string a call opens with, joined across `+`.
func literal(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := literal(e.X)
		if !ok {
			return "", false
		}
		right, ok := literal(e.Y)
		return left + right, ok
	}
	return "", false
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}

func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name && isIdent(sel.X, pkg)
}
