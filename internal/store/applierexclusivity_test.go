package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// ONLY A STATE LOG'S APPLIER WRITES THE REPLICATED ESTATE.
//
// The rule, stated once: a mutation to replicated state is published as ONE
// record on its domain's ordered stream, the broker arbitrates it, and it
// reaches SQL only through that domain's deterministic applier. The stream is
// the write-ahead log and this file is the derived durable state — never the
// reverse. [internal/statelog] argues the whole of it; adr/0002 is the
// decision, and this test is what that decision's `Enforced-by:` names.
//
// # Why an absence needs a test, and why this one in particular
//
// Every other rule about the estates already had a gate and has never been
// re-litigated: no statement names both estates, no table is declared in both.
// This one had none, and it is the rule with the failure that cannot be seen
// from inside the node that breaks it. A local write to a replicated table
// succeeds, the rows look right on the node that made them, and the divergence
// only appears as two nodes answering one question differently — at which
// point the write that caused it is months behind in the log.
//
// The tree already carried three such writes when this was added — a version
// reset after a reanchor, a probe-flag clear and an inbox sweep. All three are
// allowed below, and each had to state its case in terms of the rule.
//
// # What it walks, and what it cannot see
//
// Every non-test .go file under internal/ and cmd/, parsed with go/parser,
// looking for three things:
//
//  1. A chained WRITE: `….Replicated().Writer(…)` or `….Replicated().Tx(…)`.
//     `Read` is not a write and is not flagged; it is the overwhelming
//     majority of what this estate's handle is used for.
//  2. A HANDOFF: the result of `.Replicated()` reaching anything other than a
//     read — passed as an argument, stored in a variable, put in a struct.
//     Past that point the walk cannot follow it, so the handoff is where it
//     has to be judged. [readOnlyOfReplicated] names the calls that take the
//     handle and only read through it.
//  3. A DML string literal naming a table the REPLICATED schema declares, in
//     a file that is not an applier. This one catches a write whose handle
//     arrived by a route the first two missed.
//
// Matching is on the NAME rather than on a resolved type, which is the safe
// direction for the reason [TestOnlyOnePlaceWritesARunningStreamsConfiguration]
// gives: a false positive costs one allowance entry with a reason, and a false
// negative is the silent divergence above.
//
// It cannot see a write assembled by reflection or made through an interface
// whose method it cannot name. Neither exists here.
//
// # The allowance is two-sided
//
// An entry that matches no site in the tree FAILS, the same way
// internal/skipgate's `Always` entries do. An allowance for a write somebody
// deleted is an allowance that would silently cover the next write into the
// same file.
func TestOnlyTheApplierWritesTheReplicatedEstate(t *testing.T) {
	t.Parallel()

	replicated := tablesIn(t, store.EstateReplicated)
	if len(replicated) == 0 {
		t.Fatal("no tables were derived from the replicated estate's schema; " +
			"this guard is watching an estate it cannot see and would pass " +
			"whatever the tree did")
	}

	// THE MATCHERS, ON INPUT WHOSE VERDICT IS KNOWN. A guard asserting an
	// absence passes identically when the thing is absent and when the
	// guard has gone inert, and every allowance below only ever runs
	// against sites the walk found — so a matcher that stopped matching
	// would report a clean tree and take the allowances with it.
	fakeReplicated := map[string]bool{"tracker_tasks": true}
	for _, positive := range []string{
		`db.Replicated().Writer(ctx)`,
		`d.deps.DB.Replicated().Tx(ctx, fn)`,
		`tracker.ResetVersions(ctx, e.backends.Store.Replicated(), gen)`,
		`peer := r.node.Replicated()`,
		`statelog.Deps{DB: e.backends.Store.Replicated()}`,
	} {
		if !reachesReplicatedWrite(t, positive) {
			t.Errorf("control: %q reaches a write on the replicated estate "+
				"and the matcher did not flag it", positive)
		}
	}
	for _, negative := range []string{
		`db.Replicated().Read(ctx, fn)`,
		`db.Replicated().Path()`,
		`db.Replicated().Backup(ctx, part)`,
		`db.Writer(ctx)`,
		`db.Tx(ctx, fn)`,
		`statelog.CursorFor(ctx, s.db.Replicated(), name)`,
	} {
		if reachesReplicatedWrite(t, negative) {
			t.Errorf("control: %q does not reach a write on the replicated "+
				"estate and the matcher flagged it", negative)
		}
	}
	for _, positive := range []string{
		`DELETE FROM tracker_tasks WHERE created_at < ?`,
		`UPDATE tracker_tasks SET rank = ?`,
		`INSERT INTO tracker_tasks (id) VALUES (?)`,
	} {
		if !writesTable(positive, fakeReplicated) {
			t.Errorf("control: %q writes a replicated table and the matcher "+
				"did not flag it", positive)
		}
	}
	for _, negative := range []string{
		`SELECT id FROM tracker_tasks WHERE project = ?`,
		`tracker_tasks is written only by the applier`,
		`DELETE FROM crewlet_events WHERE created_at < ?`,
	} {
		if writesTable(negative, fakeReplicated) {
			t.Errorf("control: %q does not write a replicated table and the "+
				"matcher flagged it", negative)
		}
	}

	root := moduleRoot(t)
	var found []site
	files := 0
	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), func(fset *token.FileSet, file *ast.File) {
			files++
			rel := shortPos(root, fset.Position(file.Pos()).Filename)
			ast.Inspect(file, func(n ast.Node) bool {
				if why, ok := replicatedWriteAt(n); ok {
					found = append(found, site{
						File: rel, Line: fset.Position(n.Pos()).Line, Why: why,
					})
					return true
				}
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				if writesTable(text, replicated) {
					found = append(found, site{
						File: rel, Line: fset.Position(lit.Pos()).Line,
						Why: "DML on " + strings.Join(strings.Fields(text), " "),
					})
				}
				return true
			})
		})
	}
	if files == 0 {
		t.Fatal("parsed no source files — this guard was certifying nothing")
	}
	if len(found) == 0 {
		t.Fatal("found no writes to the replicated estate at all, not even the " +
			"applier's own — so this guard is watching for a shape nobody " +
			"writes and would pass whatever the tree did. Fix the walk")
	}

	used := map[string]bool{}
	var unexpected []site
	for _, s := range found {
		if a, ok := allowanceFor(s.File); ok {
			used[a] = true
			continue
		}
		unexpected = append(unexpected, s)
	}
	slices.SortFunc(unexpected, func(a, b site) int {
		if a.File != b.File {
			return strings.Compare(a.File, b.File)
		}
		return a.Line - b.Line
	})
	for _, s := range unexpected {
		t.Errorf("%s:%d reaches a write on the replicated estate (%s).\n"+
			"\tThat estate is derived state: a mutation is published as one "+
			"record on its domain's ordered stream, arbitrated by the broker, "+
			"and written by that domain's applier on every node. A local "+
			"write succeeds, looks correct on this node, and shows up later "+
			"as two nodes answering one question differently. See "+
			"adr/0002-the-stream-is-the-write-ahead-log.md. If this write is "+
			"genuinely not a record — a column no record owns, or a sweep of "+
			"rows whose class permits it — add it to allowedReplicatedWriter "+
			"with the reason.", s.File, s.Line, s.Why)
	}

	// AND THE OTHER DIRECTION.
	for _, a := range allowedReplicatedWriter {
		if !used[a.Prefix] {
			t.Errorf("%s is allowed to write the replicated estate and does "+
				"not — the reason on file is %q. An allowance for a write "+
				"nobody makes any more silently covers the next one into the "+
				"same file; delete it", a.Prefix, a.Why)
		}
	}
	t.Logf("parsed %d files; replicated-estate writers: %d site(s) across %d allowance(s)",
		files, len(found), len(allowedReplicatedWriter))
}

// site is one place the walk found a write, for the report.
type site struct {
	File string
	Line int
	Why  string
}

// allowance is one file permitted to write the replicated estate, with why.
//
// Keyed on the FILE rather than the line, so an edit above a write does not
// have to be reflected here — the same choice internal/clientsource argues for
// in keying on the declaration rather than the path, applied one level down.
type allowance struct {
	// Prefix is the repository-relative file, or directory, it covers.
	Prefix string

	// Applier marks the mechanism rather than an exception to it: this file
	// IS a state log's applier, or the framework and infrastructure the
	// appliers run on. The split is what keeps the list readable — the
	// three entries below that are NOT appliers are the three writes this
	// rule actually tolerates, and a reviewer should be able to see that at
	// a glance rather than by reading seventeen reasons.
	Applier bool

	// Why must say what makes this write legitimate in terms of the RULE
	// rather than of the code: which class the rows are, or why no record
	// could own them. "It is a sweep" is not a reason; "the rows are
	// Divergent, so two nodes legitimately hold different ones" is.
	Why string
}

// allowedReplicatedWriter is every file that writes the replicated estate, and
// the case each one makes.
//
// ADDING AN ENTRY IS THE DECISION, so it belongs in a diff somebody reviews
// rather than in a count somebody raises. A new applier file is an ordinary
// entry; a new EXCEPTION is a change to what this rule means and should be
// argued in the pull request, not just in the Why.
var allowedReplicatedWriter = []allowance{
	// -----------------------------------------------------------------
	// The mechanism. These are the appliers this rule names as the one
	// writer, plus the framework they run on and the two places that hold
	// the estate's handle without writing a row through it.
	// -----------------------------------------------------------------
	{
		Prefix: "internal/statelog/", Applier: true,
		Why: "THE FRAMEWORK: the apply transaction itself, the snapshot " +
			"install and the reanchor. A write here is the mechanism.",
	},
	{
		Prefix: "internal/store/", Applier: true,
		Why: "The estate's OWNER — it opens the file, runs the migrations " +
			"and serves the handle. A write here is the schema, not a row.",
	},
	{
		Prefix: "internal/engine/", Applier: true,
		Why: "The wiring that hands the replicated handle to the framework: " +
			"the reanchor's deps and the version reset it drives. Every " +
			"write it reaches is allowed on its own terms below.",
	},
	{
		Prefix: "internal/backup/", Applier: true,
		Why: "Takes the handle to copy the FILE — VACUUM INTO and the " +
			"manifest — never to write a row.",
	},
	{
		Prefix: "cmd/crewlet/ops.go", Applier: true,
		Why: "Picks one of the two handles to inspect for an operator " +
			"command. It reads.",
	},
	{
		Prefix: "internal/tracker/apply", Applier: true,
		Why: "The tracker domain's applier, across the files it is split " +
			"over: the record, the task, the objects, the history and the " +
			"closure walk.",
	},
	{
		Prefix: "internal/tracker/fieldvalues.go", Applier: true,
		Why: "Applier.explodeFieldValues and the two row writers it calls. " +
			"It is applier code in a file the apply* prefix does not cover, " +
			"which is why it is named rather than inferred from the name.",
	},
	{
		Prefix: "internal/pages/apply", Applier: true,
		Why: "The knowledge base's applier — the container half and the " +
			"page half.",
	},
	{
		Prefix: "internal/search/apply", Applier: true,
		Why: "The embedding domain's applier. The vectors are compacted and " +
			"claim no identity, but they are still written only from a " +
			"committed record.",
	},

	// -----------------------------------------------------------------
	// THE EXCEPTIONS. Three writes that are not a record, each because no
	// record could own what it touches. Read these before adding a fourth.
	// -----------------------------------------------------------------
	{
		Prefix: "internal/tracker/reanchor.go",
		Why: "ResetVersions puts every object row's version at a new " +
			"generation's floor after a reanchor. It cannot be a record: the " +
			"stream it would be published to is the one being replaced. " +
			"Every node runs it against its own copy and computes the same " +
			"floor, so it converges rather than diverges.",
	},
	{
		Prefix: "internal/tracker/duty.go",
		Why: "clearProbe clears `rank_duplicate_pending`, a column written " +
			"by every node's own applier from its own probe and by no " +
			"record — so a record clearing it would be a record about a " +
			"column no record owns.",
	},
	{
		Prefix: "internal/tracker/inboxsweep.go",
		Why: "purgeInbox range-deletes `tracker_notifications`, which is " +
			"statelog.Divergent: it travels inside a snapshot but is not in " +
			"the identity claim, because what it holds depends on the " +
			"epoch's own horizon. Two nodes legitimately hold different " +
			"rows, so deleting on this node's own authority diverges nothing.",
	},
}

// allowanceFor reports whether a file is allowed to write, and which entry
// covers it.
func allowanceFor(file string) (string, bool) {
	// The longest prefix wins, so a file-specific entry is not shadowed by
	// a package-wide one and both stay two-sided.
	best, found := "", false
	for _, a := range allowedReplicatedWriter {
		p := filepath.FromSlash(a.Prefix)
		if strings.HasPrefix(file, p) && len(p) > len(best) {
			best, found = a.Prefix, true
		}
	}
	return best, found
}

// readOnlyOfReplicated names the calls that take the replicated estate's
// handle and only read through it.
//
// Two of them, and both are worth naming rather than inferring: a handoff is
// the point past which this walk cannot follow the handle, so the difference
// between "hands it to a reader" and "hands it to a writer" has to be
// declared. The list is exercised by the controls above, so an entry that
// stopped being a reader shows up as a control failure rather than as silence.
var readOnlyOfReplicated = map[string]bool{
	"CursorFor": true, // statelog.CursorFor reads a domain's checkpoint.
	"Evictions": true, // tracker.Evictions reads the eviction rows.
}

// replicatedWriteAt reports whether one node reaches a write on the
// replicated estate, and what kind it is.
func replicatedWriteAt(n ast.Node) (string, bool) {
	switch v := n.(type) {
	case *ast.CallExpr:
		// A chained write: X.Replicated().Writer(…) / .Tx(…).
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok && isReplicatedCall(sel.X) {
			switch sel.Sel.Name {
			case "Writer", "Tx":
				return "Replicated()." + sel.Sel.Name, true
			}
			return "", false
		}
		// A handoff by argument, unless the callee only reads.
		if name, ok := calleeName(v.Fun); ok && readOnlyOfReplicated[name] {
			return "", false
		}
		for _, arg := range v.Args {
			if isReplicatedCall(arg) {
				name, _ := calleeName(v.Fun)
				return "handed to " + name, true
			}
		}
	case *ast.AssignStmt:
		for _, rhs := range v.Rhs {
			if isReplicatedCall(rhs) {
				return "assigned out of the estate handle", true
			}
		}
	case *ast.KeyValueExpr:
		if isReplicatedCall(v.Value) {
			return "stored in a struct", true
		}
	}
	return "", false
}

// isReplicatedCall reports whether an expression IS the call `….Replicated()`.
func isReplicatedCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Replicated"
}

// calleeName is the last identifier of a call's function expression.
func calleeName(fun ast.Expr) (string, bool) {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name, true
	case *ast.SelectorExpr:
		return f.Sel.Name, true
	}
	return "", false
}

// dml introduces a table a statement WRITES. SELECT is deliberately absent.
var dml = regexp.MustCompile(`(?is)\b(?:INSERT\s+(?:OR\s+\w+\s+)?INTO|UPDATE|DELETE\s+FROM)\s+([a-z_][a-z0-9_]*)`)

// writesTable reports whether one statement writes a table of the given set.
//
// TABLES IN VERB POSITION ONLY, for the reason [bothEstates] gives one file
// over: a bare containment test reads a package doc that names a table in a
// sentence about writing as a write.
func writesTable(text string, tables map[string]bool) bool {
	for _, m := range dml.FindAllStringSubmatch(text, -1) {
		if tables[strings.ToLower(m[1])] {
			return true
		}
	}
	return false
}

// reachesReplicatedWrite parses one snippet and reports whether the matcher
// flags it, so the controls exercise the same code the walk does.
func reachesReplicatedWrite(t *testing.T, src string) bool {
	t.Helper()
	// Wrapped in a function, because the controls include statements as
	// well as expressions and only a declaration parses both.
	file, err := parser.ParseFile(token.NewFileSet(), "control.go",
		"package p\nfunc f() {\n"+src+"\n}\n", parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("control %q does not parse: %v", src, err)
	}
	flagged := false
	ast.Inspect(file, func(n ast.Node) bool {
		if _, ok := replicatedWriteAt(n); ok {
			flagged = true
		}
		return true
	})
	return flagged
}
