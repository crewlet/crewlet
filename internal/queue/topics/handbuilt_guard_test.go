package topics_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"go/ast"
	"go/parser"
	"go/token"
)

// TestNoPackageBuildsASubjectByHand fails the build when any package outside
// this one writes a Crewlet subject or consumer-group name as a string
// literal.
//
// This is the reason the package exists, restated as a check. A producer and
// a consumer that disagree about a name raise nothing: the publish succeeds
// into a subject nobody reads, the seat never wakes, and the only symptom is
// a company that looks idle. Nine call sites in the Python engine formatted
// crewlet.agent.{handle}.inbox by hand — nine chances for a grammar change to
// reach some of them and not the others.
//
// # What it walks
//
// Every non-test .go file under internal/ and cmd/, parsed with go/parser.
// The subject of the match is a LANGUAGE construct — *ast.BasicLit of kind
// STRING — not a line, not a regexp over source text. Line scanning cannot
// see scope and has been wrong in both directions in this repo: a fixed
// N-line window under-reports, and a brace scan runs past a closure's `}, arg)`
// into the enclosing function and over-reports. A literal is a literal
// wherever it sits, and a doc comment naming crewlet.agent..inbox — which
// several files legitimately do — is not one.
//
// # What it looks for
//
// The markers are DERIVED from this package's own exported constants rather
// than typed out again, so a new piece of grammar is covered by adding it to
// topics.go and nothing else. A marker with a dot is matched by containment,
// which is safe because the dotted forms (crewlet.agent, crewlet.events,
// crewlet.notifications, crewlet.config, dlq., .inbox, .control) appear in no
// other kind of string. A marker without one (agent-, -control) is matched by
// SHAPE instead — the literal is exactly the marker, or is made only of the
// characters a wire name may contain — because "agent-" is also ordinary
// English hyphenation and "agent-only field set on a human seat" is a
// sentence, not a consumer group.
//
// # Coverage boundary
//
// It sees a subject that is WRITTEN as a literal, in a package the glob
// covers. It does not see:
//
//   - a subject assembled from a variable or a fmt.Sprintf whose format
//     string carries no marker (`domain + ".inbox"` is caught by the .inbox
//     marker; `a + "." + b + "." + c` is not caught at all);
//   - a subject built in the web/ tree, or in generated code
//     outside internal/ and cmd/;
//   - a subject fragment shorter than a marker — memory.go tests for an
//     inbound-notification topic with strings.HasSuffix(topic, ".inbound"),
//     and ".inbound" is not a marker because NotificationsInbound is a whole
//     subject rather than a named suffix;
//   - TEST code. Deliberately, and it is the largest gap: see the census this
//     test logs. Bringing tests in would need an allowance list an order of
//     magnitude longer than the drift it guards, and the failure mode there
//     is different in kind — a wrong literal in a test fails that test, where
//     a wrong literal in the engine silently swallows live traffic.
//
// If the subject of this guard ever legitimately goes away — every backend
// deriving its subjects from this package with none left to write down —
// DELETE it rather than weakening its assertions. A guard kept alive past its
// subject is how a count gets quietly lowered to zero.
func TestNoPackageBuildsASubjectByHand(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	markers := subjectMarkers(t, filepath.Join(root, "internal", "queue", "topics"))

	// A guard asserting an ABSENCE passes identically when the thing is
	// absent and when the guard has stopped working. These four assertions
	// are what tell those apart, and the last is the strongest: it runs the
	// matcher on strings whose verdict is known, so a matcher that has gone
	// inert fails here rather than certifying a clean tree.
	for _, want := range []string{
		"crewlet.agent", "crewlet.events", "crewlet.notifications",
		"crewlet.config", "dlq.", ".inbox", ".control", "agent-",
	} {
		if !markers[want] {
			t.Errorf("marker %q was not derived from topics.go's constants; the "+
				"derivation no longer recognises the grammar it is meant to cover", want)
		}
	}
	for _, positive := range []string{
		"crewlet.agent.alice.inbox", "crewlet.agent.", "crewlet.events.>",
		"crewlet.notifications.inbound", "crewlet.config.>", "dlq.x.y",
		"agent-", "agent-alice", "agent-alice-control",
	} {
		if _, hit := violation(markers, positive); !hit {
			t.Errorf("control: %q is a hand-built name and the matcher did not flag it", positive)
		}
	}
	for _, negative := range []string{
		"crewlet.db",
		"https://docs.crewlet.ai/schema/bootstrap.schema.json",
		"agent-only field set on a human seat",
		"Maximum depth of agent-to-agent delegation chains.",
		"github.com/crewlet/crewlet/internal/queue/topics",
		"Prefix on each bot username, e.g. agent-.",
	} {
		if marker, hit := violation(markers, negative); hit {
			t.Errorf("control: %q is not a subject but the matcher flagged it on %q",
				negative, marker)
		}
	}

	found := walkForLiterals(t, root, markers, false)
	census := walkForLiterals(t, root, markers, true)

	if found.files == 0 {
		t.Fatal("parsed no source files — this guard was certifying nothing. Check the " +
			"module root and the internal/ and cmd/ globs")
	}
	if found.literals == 0 {
		t.Fatal("found no string literals to inspect. The walk keys on *ast.BasicLit of " +
			"kind STRING; if that stopped matching, every file reads as literal-free and " +
			"the guard passes forever having examined nothing")
	}

	for _, v := range found.hits {
		if _, known := acknowledgedDrift[driftKey{v.pkg, v.literal}]; known {
			continue
		}
		t.Errorf("%s: package %s builds the subject %q by hand.\n"+
			"\tuse internal/queue/topics — a producer and a consumer that disagree "+
			"about a name never raise, the publish just lands where nobody reads",
			v.pos, v.pkg, v.literal)
	}

	// The allowance is exact in BOTH directions. An entry whose drift has
	// been fixed must be deleted, or the list becomes a place where a future
	// violation can hide behind a stale excuse.
	seen := map[driftKey]bool{}
	for _, v := range found.hits {
		seen[driftKey{v.pkg, v.literal}] = true
	}
	for key, why := range acknowledgedDrift {
		if !seen[key] {
			t.Errorf("acknowledgedDrift still excuses %q in package %s, but the walk no "+
				"longer finds it. Delete the entry — it was: %s", key.literal, key.pkg, why)
		}
	}

	t.Logf("scanned %d files / %d string literals in internal/ and cmd/: "+
		"%d hand-built names, %d of them acknowledged drift",
		found.files, found.literals, len(found.hits), len(acknowledgedDrift))
	t.Logf("BOUNDARY, not enforced: the test tree (_test.go files and the *test "+
		"support packages) carries %d hand-built names across %d files — a wrong "+
		"literal there fails that test rather than swallowing live traffic",
		len(census.hits), census.files)
}

// driftKey names a known hand-built literal by its package rather than its
// file, so moving it within the package does not spuriously fail the guard
// while removing it does.
type driftKey struct{ pkg, literal string }

// acknowledgedDrift is the closed set of hand-built names that predate this
// guard and live in packages this change does not own. It is a ratchet, not a
// permission: nothing may be added without fixing the cause, and an entry
// whose drift is gone fails the test above.
//
// Both entries have the same cause and the same one-word fix, now that
// topics.go names the two domains: jetstream/stream.go builds three of its
// five stream subjects as topics.AgentInboxPrefix+">",
// topics.EventsPrefix+">" and topics.DeadLetterPrefix+">", and hand-writes
// the other two only because there was no constant to reach for.
var acknowledgedDrift = map[driftKey]string{
	{"jetstream", "crewlet.notifications.>"}: "stream topology; use topics.NotificationsPrefix + \">\"",
	{"jetstream", "crewlet.config.>"}:        "stream topology; use topics.ConfigPrefix + \">\"",
}

type hit struct {
	pos     string
	pkg     string
	literal string
}

type walkResult struct {
	files    int
	literals int
	hits     []hit
}

// walkForLiterals parses .go files under internal/ and cmd/ and collects the
// string literals that name a subject or a consumer group.
//
// tests selects which half of the tree to read, and the two halves partition
// it: false is the enforced half — production code — and true is everything
// the enforced half skips, which is _test.go files plus the *test support
// packages (queuetest, coordtest, …) whose ordinary .go files are test code
// too. That is the census the boundary note reports.
//
// A support package is recognised by its own directory name, so a nested
// package underneath one would read as production. None exists; if one
// appears, this is where it has to be taught.
func walkForLiterals(t *testing.T, root string, markers map[string]bool, tests bool) walkResult {
	t.Helper()

	topicsDir := filepath.Join(root, "internal", "queue", "topics")
	fset := token.NewFileSet()
	var out walkResult

	for _, tree := range []string{"internal", "cmd"} {
		treeRoot := filepath.Join(root, tree)
		err := filepath.WalkDir(treeRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				base := filepath.Base(path)
				// The grammar's own package is the one place these
				// literals belong, in either half. testdata is not
				// compiled at all.
				if path == topicsDir || base == "testdata" {
					return fs.SkipDir
				}
				if !tests && path != treeRoot && isSupportPackage(base) {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			isTest := strings.HasSuffix(path, "_test.go") ||
				isSupportPackage(filepath.Base(filepath.Dir(path)))
			if isTest != tests {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Errorf("parse %s: %v", path, err)
				return nil
			}
			out.files++
			ast.Inspect(file, func(n ast.Node) bool {
				if _, isImport := n.(*ast.ImportSpec); isImport {
					// An import path is a string literal that is never a
					// subject; skipping the node beats teaching the
					// matcher about module paths.
					return false
				}
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				out.literals++
				if _, bad := violation(markers, value); bad {
					out.hits = append(out.hits, hit{
						pos:     shortPos(root, fset.Position(lit.Pos()).String()),
						pkg:     file.Name.Name,
						literal: value,
					})
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", tree, err)
		}
	}
	return out
}

// isSupportPackage reports a directory holding test code that does not live
// in _test.go files — the repo's *test convention (queuetest, coordtest,
// storetest, jetstreamtest). Keying on a repo-local naming convention rather
// than on a language construct is a weakness, which is why the walk asserts
// it parsed files and found literals rather than trusting its own reach.
func isSupportPackage(dir string) bool { return strings.HasSuffix(dir, "test") }

// violation reports whether a string literal names a subject or a consumer
// group, and on which marker.
func violation(markers map[string]bool, value string) (string, bool) {
	for marker := range markers {
		if strings.Contains(marker, ".") {
			if strings.Contains(value, marker) {
				return marker, true
			}
			continue
		}
		// A dotless marker ("agent-", "-control") is also ordinary
		// English, so containment alone would flag prose. Require the
		// literal to BE the marker — the base of a concatenation — or to
		// be made only of the characters a wire name may contain, which
		// no sentence and no struct tag is.
		if value == marker {
			return marker, true
		}
		if strings.Contains(value, marker) && isWireName(value) {
			return marker, true
		}
	}
	return "", false
}

// isWireName reports whether every character could appear in a consumer group
// the grammar mints: a handle is ^[a-z0-9][a-z0-9-]*$ and the group affixes
// add nothing else.
func isWireName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// subjectMarkers derives what to look for from the grammar's own constants,
// so the guard extends itself when the grammar does.
//
// A constant's value is reduced to the part that identifies its DOMAIN:
// "crewlet.agent." and "crewlet.events.>" both become "crewlet.agent" /
// "crewlet.events", so a hand-written wildcard over a domain is caught as
// readily as a hand-written leaf. A leading-dot value (".inbox") keeps its
// dot, and a dotless one ("agent-") is kept whole.
func subjectMarkers(t *testing.T, dir string) map[string]bool {
	t.Helper()

	values := constStrings(t, dir)
	if len(values) == 0 {
		t.Fatal("derived no constants from the topics package; the guard has nothing to look for")
	}
	markers := map[string]bool{}
	for _, v := range values {
		if m := markerFor(v); m != "" {
			markers[m] = true
		}
	}
	return markers
}

func markerFor(v string) string {
	if v == "" {
		return ""
	}
	parts := strings.Split(v, ".")
	switch {
	case len(parts) == 1:
		// "agent-", "-control": no domain to reduce to.
		return v
	case parts[0] == "":
		// ".inbox", ".control": a suffix keeps its separator.
		return "." + parts[1]
	case parts[1] == "":
		// "dlq.": a one-segment domain.
		return parts[0] + "."
	default:
		return parts[0] + "." + parts[1]
	}
}

// constStrings evaluates every top-level string constant in a package.
//
// It folds concatenation and constant references itself rather than reaching
// for go/types, because the grammar defines its leaves from its prefixes
// (NotificationsInbound = NotificationsPrefix + "inbound") and reading only
// the literals would yield the fragment "inbound" instead of the subject.
func constStrings(t *testing.T, dir string) map[string]string {
	t.Helper()

	// The directory is read and parsed file by file rather than through
	// go/parser's ParseDir: that helper cannot see build tags, so it
	// groups files into packages by name alone, and it is deprecated for
	// exactly that reason. Nothing here needs the grouping — every
	// non-test file in this ONE directory holds grammar, and a file that
	// stopped being parsed would silently shrink the marker set this
	// guard searches for, which is the failure that reads as a pass.
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the topics package at %s: %v", dir, err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", filepath.Join(dir, name), err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatalf("no non-test Go files under %s; the guard would derive no markers", dir)
	}

	type binding struct {
		name string
		expr ast.Expr
	}
	var bindings []binding
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						bindings = append(bindings, binding{name.Name, vs.Values[i]})
					}
				}
			}
		}
	}

	env := map[string]string{}
	// Each round resolves at least one more binding or none at all, so the
	// number of bindings bounds the rounds.
	for range bindings {
		progress := false
		for _, b := range bindings {
			if _, done := env[b.name]; done {
				continue
			}
			if v, ok := evalString(b.expr, env); ok {
				env[b.name] = v
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	return env
}

func evalString(e ast.Expr, env map[string]string) (string, bool) {
	switch n := e.(type) {
	case *ast.BasicLit:
		if n.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(n.Value)
		return v, err == nil
	case *ast.Ident:
		v, ok := env[n.Name]
		return v, ok
	case *ast.ParenExpr:
		return evalString(n.X, env)
	case *ast.BinaryExpr:
		if n.Op != token.ADD {
			return "", false
		}
		l, ok := evalString(n.X, env)
		if !ok {
			return "", false
		}
		r, ok := evalString(n.Y, env)
		if !ok {
			return "", false
		}
		return l + r, true
	}
	return "", false
}

// moduleRoot locates the go module this test lives in, from its own source
// path rather than from the working directory, which `go test` sets to the
// package under test and a future caller may not.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", root, err)
	}
	return root
}

func shortPos(root, pos string) string {
	if rel, err := filepath.Rel(root, pos); err == nil {
		return rel
	}
	return pos
}
