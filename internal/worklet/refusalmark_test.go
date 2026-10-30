package worklet

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
	"worklet: a writer cannot act as":                      "a surface built a writer with no identity",
	"worklet: a writer has no publisher":                   "construction wiring",
	"worklet: a writer has no actor":                       "construction wiring",
	"worklet: %q is not an author kind":                    "construction wiring",
	"worklet: this writer has no store":                    "construction wiring",
	"worklet: this writer has no replicated ":              "construction wiring",
	"worklet: this writer has no coordination":             "construction wiring",
	"worklet: a scope carries the exactly-the-subject ":    "a scope is the writer's own, never the caller's",
	"worklet: container %q contains %q, which is ":         "a scope is the writer's own, never the caller's",
	"worklet: container %q is blank, and a blank ":         "a scope is the writer's own, never the caller's",
	"worklet: a scope enumerates %d term(s) and also ":     "a scope is the writer's own, never the caller's",
	"worklet: a scope is empty":                            "a scope is the writer's own, never the caller's",
	"worklet: a scope enumerates %d terms and the cap is ": "a scope is the writer's own, never the caller's",
	"worklet: a %s term names nothing":                     "a scope is the writer's own, never the caller's",
	"worklet: scope term kind %q is not one this build ":   "a scope is the writer's own, never the caller's",
	"worklet: a record carries version ":                   "decoding a record off the log",
	"worklet: op %q is not one this build writes":          "decoding a record off the log",
	"worklet: %q is not a barrier envelope":                "decoding a record off the log",
	"worklet: a barrier carries no op id":                  "decoding a record off the log",
	"worklet: a thread read names no task":                 "the builtin always names the task it resolved",
	"worklet: the %s record on %s must be readable ":       "a record version is the encoder's, never the caller's",
	"worklet: the %s record on %s is stamped version ":     "a record version is the encoder's, never the caller's",
}

// TestEveryWriteRefusalIsMarked walks [writeFiles] and refuses a fmt.Errorf that
// wraps nothing and is not in [unmarkedByDesign].
//
// THE WALK IS THE GATE because the failure is a NEW refusal. A writer that
// refuses a value with fmt.Errorf compiles, reads fine to a model — and
// reaches a person's write surface as the node's failure, because what is
// unmarked is read as the node's (see [ErrInvalid]). So every sentence either
// carries a sentinel through %w, is built with [invalid], [forbidden] or
// [forbiddenAs], or is named here as the node's own fault with a reason.
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
			if _, isRefusal := refusalSentence(call); isRefusal {
				marked++
				return true
			}
			format, ok := literal(call.Args[0])
			if !ok {
				return true
			}
			switch {
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
// for a refusal whose sentence opens with "worklet:". The prefix is the
// refusal's own to add ([Refusal.Error]): spelled again, the log reads
// "worklet: tracker: …" and [Sentence] hands its reader the package's name.
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
			if !ok {
				return true
			}
			sentence, isRefusal := refusalSentence(call)
			if !isRefusal {
				return true
			}
			format, ok := literal(sentence)
			if !ok {
				t.Errorf("%s: a refusal's sentence is not a literal, so nothing can "+
					"check it", fset.Position(call.Pos()))
				return true
			}
			built++
			if strings.HasPrefix(format, "worklet:") {
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

// TestAMarkedRefusalKeepsItsSentenceAndItsChain pins the halves of [invalid],
// [forbidden] and [forbiddenAs]: the Go error reads as every error here does, prefix and
// all, which is the text a model was tuned against; a sentinel the sentence
// wraps is still reachable beside the mark; and [Sentence] is the words alone,
// which is what a tool tells a model.
func TestAMarkedRefusalKeepsItsSentenceAndItsChain(t *testing.T) {
	t.Parallel()
	inner := errors.New("disk")
	err := invalid("mint a key between %q and %q: %w", "a", "b", inner)
	if got, want := err.Error(), `worklet: mint a key between "a" and "b": disk`; got != want {
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
	if got, want := denied.Error(), "worklet: view v1 is protected and belongs to ada"; got != want {
		t.Errorf("the error is %q, want %q", got, want)
	}
	if got, want := Sentence(denied), "view v1 is protected and belongs to ada"; got != want {
		t.Errorf("the sentence is %q, want %q", got, want)
	}
	if !errors.Is(denied, ErrForbidden) || errors.Is(denied, ErrInvalid) {
		t.Errorf("forbidden is classed wrong: forbidden %v, invalid %v",
			errors.Is(denied, ErrForbidden), errors.Is(denied, ErrInvalid))
	}

	// FORBIDDEN-AS CARRIES ITS OWN SENTINEL BESIDE THE CLASS, and says it
	// once: the class is what every surface maps to a status, the sentinel
	// is what the one caller that acts on this refusal tests for, and
	// neither's words are appended to the sentence.
	sentinel := errors.New("worklet: restores others")
	alone := forbiddenAs(sentinel, "%d other tasks went with %s", 2, "t-1")
	if got, want := alone.Error(), "worklet: 2 other tasks went with t-1"; got != want {
		t.Errorf("the error is %q, want %q", got, want)
	}
	if got, want := Sentence(alone), "2 other tasks went with t-1"; got != want {
		t.Errorf("the sentence is %q, want %q", got, want)
	}
	if !errors.Is(alone, ErrForbidden) || !errors.Is(alone, sentinel) || errors.Is(alone, ErrInvalid) {
		t.Errorf("forbiddenAs is classed wrong: forbidden %v, its sentinel %v, invalid %v",
			errors.Is(alone, ErrForbidden), errors.Is(alone, sentinel), errors.Is(alone, ErrInvalid))
	}

	// CONTEXT IS NEVER DROPPED TO TIDY A PREFIX: an error this package wrote
	// loses the "worklet:" it opens with and keeps every word after it, the
	// context a caller here gave it included; one another package wrapped
	// opens with its context and is its whole message.
	wrapped := fmt.Errorf("worklet: create ENG: %w", err)
	if got, want := Sentence(wrapped), strings.TrimPrefix(wrapped.Error(), "worklet: "); got != want {
		t.Errorf("a wrapped refusal's sentence is %q, want %q", got, want)
	}
	if got, want := Sentence(ErrNoTask), "no such task"; got != want {
		t.Errorf("a sentinel's sentence is %q, want %q", got, want)
	}
	outside := fmt.Errorf("move ENG-1: %w", ErrNoTask)
	if got := Sentence(outside); got != outside.Error() {
		t.Errorf("a refusal another package wrapped is %q, want its whole message", got)
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

// refusalSentence is the sentence argument of a call to one of the package's
// marked constructors, and whether call is one: [invalid] and [forbidden] take
// it first, [forbiddenAs] after the sentinel it carries. ONE reading for both
// walks, because a constructor one walk recognised and the other did not is a
// refusal the second walk passes by checking nothing.
func refusalSentence(call *ast.CallExpr) (ast.Expr, bool) {
	switch {
	case (isIdent(call.Fun, "invalid") || isIdent(call.Fun, "forbidden")) && len(call.Args) > 0:
		return call.Args[0], true
	case isIdent(call.Fun, "forbiddenAs") && len(call.Args) > 1:
		return call.Args[1], true
	}
	return nil, false
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}

func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name && isIdent(sel.X, pkg)
}
