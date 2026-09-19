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

	// THE COMPUTED-TABLE MATCHER, which is the half that was missing.
	for _, positive := range []string{
		`UPDATE %s SET version = ?`,
		"DELETE FROM " + unresolved + " WHERE id = ?",
		"INSERT INTO " + unresolved + " (a) VALUES (?)",
	} {
		if !writesComputedTable(positive) {
			t.Errorf("control: %q writes a table this walk cannot resolve and "+
				"the matcher did not flag it", positive)
		}
	}
	for _, negative := range []string{
		`UPDATE tracker_tasks SET rank = ?`,
		"SELECT * FROM " + unresolved,
		`a sentence that merely says %s somewhere`,
	} {
		if writesComputedTable(negative) {
			t.Errorf("control: %q is not a write to a computed table and the "+
				"matcher flagged it", negative)
		}
	}

	// AND THE RENDERER, on the two shapes that evaded the literal walk.
	for _, c := range []struct{ src, want string }{
		{`"UPDATE " + tbl + " SET x = ?"`, "UPDATE " + unresolved + " SET x = ?"},
		{`"DELETE FROM tracker_tasks"`, "DELETE FROM tracker_tasks"},
		{`fmt.Sprintf("UPDATE %s SET v = ?", t)`, "UPDATE %s SET v = ?"},
	} {
		got, ok := composedString(mustParse(t, c.src), map[string]string{})
		if !ok || got != c.want {
			t.Errorf("control: composedString(%s) = %q, %v; want %q", c.src, got, ok, c.want)
		}
	}
	if got, ok := composedString(mustParse(t, `"a" + b`), map[string]string{"b": "table"}); !ok || got != "atable" {
		t.Errorf("control: a constant did not fold: got %q, %v", got, ok)
	}

	root := moduleRoot(t)
	var found []site
	files := 0
	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), func(fset *token.FileSet, file *ast.File) {
			files++
			rel := shortPos(root, fset.Position(file.Pos()).Filename)
			consts := stringConsts(file)
			ast.Inspect(file, func(n ast.Node) bool {
				if why, ok := replicatedWriteAt(n); ok {
					found = append(found, site{
						File: rel, Line: fset.Position(n.Pos()).Line, Why: why,
					})
					return true
				}
				text, ok := composedString(n, consts)
				if !ok {
					return true
				}
				switch {
				case writesTable(text, replicated):
					found = append(found, site{
						File: rel, Line: fset.Position(n.Pos()).Line,
						Why: "DML on " + strings.Join(strings.Fields(text), " "),
					})
				case writesComputedTable(text):
					found = append(found, site{
						File: rel, Line: fset.Position(n.Pos()).Line,
						Why: "DML whose table is COMPUTED: " + strings.Join(strings.Fields(text), " "),
					})
				}
				// A composed string's own operands are literals this walk
				// would otherwise report a second time, at a worse
				// position. Rendering the whole expression IS the report.
				_, isLit := n.(*ast.BasicLit)
				return isLit
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
		if !a.Kind.Valid() {
			t.Errorf("%s is allowed with kind %q — an entry says which of the "+
				"three things it is, so a reviewer can tell the mechanism from "+
				"the writes this rule actually tolerates", a.Prefix, a.Kind)
		}
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
// allowanceKind is what an entry claims about itself.
type allowanceKind string

const (
	// mechanism: this file IS a state log's applier, or the framework and
	// infrastructure the appliers run on. A write here is how the rule
	// works rather than an exception to it.
	mechanism allowanceKind = "mechanism"

	// exception: a genuine write to the replicated estate that is not a
	// record, because no record could own what it touches. Read all three
	// before adding a fourth.
	exception allowanceKind = "exception"

	// notReplicated: the walk flagged a statement whose table it could not
	// resolve, and that table is not in the replicated estate at all. The
	// claim is made in the register rather than inferred from silence,
	// because a computed table name is exactly what a reader cannot check.
	notReplicated allowanceKind = "not_replicated"
)

// Valid reports whether k is one of the three. The zero value is not.
func (k allowanceKind) Valid() bool {
	return k == mechanism || k == exception || k == notReplicated
}

type allowance struct {
	// Prefix is the repository-relative file, or directory, it covers.
	Prefix string

	// Kind says which of three things this allowance is, so a reviewer can
	// tell them apart at a glance rather than by reading seventeen reasons.
	//
	// It was a bool — applier or not — and a third case arrived the moment
	// the walk learned to read a computed table name: a statement this gate
	// flags that is not a replicated write at all. A bool would have filed
	// that under one of the two meanings it does not have.
	Kind allowanceKind

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
		Prefix: "internal/statelog/", Kind: mechanism,
		Why: "THE FRAMEWORK: the apply transaction itself, the snapshot " +
			"install and the reanchor. A write here is the mechanism.",
	},
	{
		Prefix: "internal/store/", Kind: mechanism,
		Why: "The estate's OWNER — it opens the file, runs the migrations " +
			"and serves the handle. A write here is the schema, not a row.",
	},
	{
		Prefix: "internal/engine/reanchor.go", Kind: mechanism,
		Why: "The wiring that hands the replicated handle to the framework " +
			"for a log reanchor: the deps it builds and the version reset it " +
			"drives. Every write it reaches is internal/statelog's or " +
			"internal/tracker/reanchor.go's, each allowed on its own terms. " +
			"Named by FILE rather than by package, so a new direct write " +
			"elsewhere in internal/engine still fails.",
	},
	{
		Prefix: "internal/engine/statelog.go", Kind: mechanism,
		Why: "Holds the replicated handle to run the framework's own loops — " +
			"the checkpoint reads, the snapshotter, the adoption. It passes " +
			"the handle on; it writes no row of its own.",
	},
	{
		Prefix: "internal/engine/retention", Kind: mechanism,
		Why: "The retention report and the capacity check take the handle to " +
			"size the FILE and to read the eviction rows. Both read.",
	},
	{
		Prefix: "internal/backup/backup.go", Kind: mechanism,
		Why: "Takes the handle to copy the FILE — VACUUM INTO and the " +
			"manifest — never to write a row.",
	},
	{
		Prefix: "cmd/crewlet/ops.go", Kind: mechanism,
		Why: "Picks one of the two handles to inspect for an operator " +
			"command. It reads.",
	},
	{
		Prefix: "internal/tracker/apply", Kind: mechanism,
		Why: "The tracker domain's applier, across the files it is split " +
			"over: the record, the task, the objects, the history and the " +
			"closure walk.",
	},
	{
		Prefix: "internal/tracker/fieldvalues.go", Kind: mechanism,
		Why: "Applier.explodeFieldValues and the two row writers it calls. " +
			"It is applier code in a file the apply* prefix does not cover, " +
			"which is why it is named rather than inferred from the name.",
	},
	{
		Prefix: "internal/pages/apply", Kind: mechanism,
		Why: "The knowledge base's applier — the container half and the " +
			"page half.",
	},
	{
		Prefix: "internal/search/apply", Kind: mechanism,
		Why: "The embedding domain's applier. The vectors are compacted and " +
			"claim no identity, but they are still written only from a " +
			"committed record.",
	},

	{
		Prefix: "internal/learning/memsync/codec.go", Kind: notReplicated,
		Why: "Builds its INSERT from the carried row's own table name, so the " +
			"walk cannot resolve which table it writes and reports a computed " +
			"one. The registry that name comes from is a closed list of SEVEN " +
			"node-estate tables — agent_diary, episodes, counterparty_profiles, " +
			"synthesized_skills and its versions, agent_onboarding_markers, " +
			"conversation_sessions — every one placed in nodeEstatePlacements " +
			"one file over. It never names a replicated table.",
	},

	// -----------------------------------------------------------------
	// THE EXCEPTIONS. Three writes that are not a record, each because no
	// record could own what it touches. Read these before adding a fourth.
	// -----------------------------------------------------------------
	{
		Prefix: "internal/tracker/reanchor.go", Kind: exception,
		Why: "ResetVersions puts every object row's version at a new " +
			"generation's floor after a reanchor. It cannot be a record: the " +
			"stream it would be published to is the one being replaced. " +
			"Every node runs it against its own copy and computes the same " +
			"floor, so it converges rather than diverges.",
	},
	{
		Prefix: "internal/tracker/duty.go", Kind: exception,
		Why: "clearProbe clears `rank_duplicate_pending`, a column written " +
			"by every node's own applier from its own probe and by no " +
			"record — so a record clearing it would be a record about a " +
			"column no record owns.",
	},
	{
		Prefix: "internal/tracker/inboxsweep.go", Kind: exception,
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

// unresolved stands for a part of a composed string this walk cannot read: a
// variable operand, or a verb in a format string.
//
// A RUNE NO SQL CARRIES, so a statement that genuinely contains it cannot be
// confused for one this walk resolved, and [writesComputedTable] can match on
// it without a second parse.
const unresolved = "\u2370"

// composedString renders a string-valued expression, as far as it can.
//
// # Why a literal alone was not enough
//
// The first version of this walk read *ast.BasicLit and nothing else, which
// left two evasions a contributor reaches by accident rather than by trying: a
// statement built by concatenation names no table in any literal, and one
// built by fmt.Sprintf hides the table behind a verb. Both shapes are already
// in this tree, and a probe written in a package with no allowance passed the
// gate on the first of them — an interface handle over the estate, a table
// name assembled from two constants, and a clean run.
//
// So the renderer folds what it can and marks what it cannot. A part it
// resolves contributes its text; a part it does not contributes [unresolved],
// which [writesComputedTable] reports as a write whose table this walk cannot
// certify. That is the honest verdict rather than a pass: a statement whose
// table is computed is exactly the one a READER cannot check either.
//
// Sprintf's FORMAT STRING is the first argument and is rendered with its verbs
// in place. The arguments are deliberately not resolved: a walk that tried
// would be guessing at the one point where guessing is what this gate exists
// to prevent.
func composedString(n ast.Node, consts map[string]string) (string, bool) {
	switch v := n.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(v.Value)
		if err != nil {
			return "", false
		}
		return text, true
	case *ast.Ident:
		text, ok := consts[v.Name]
		return text, ok
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		left, lok := composedString(v.X, consts)
		if !lok {
			left = unresolved
		}
		right, rok := composedString(v.Y, consts)
		if !rok {
			right = unresolved
		}
		if !lok && !rok {
			return "", false
		}
		return left + right, true
	case *ast.CallExpr:
		name, ok := calleeName(v.Fun)
		if !ok || name != "Sprintf" || len(v.Args) == 0 {
			return "", false
		}
		return composedString(v.Args[0], consts)
	}
	return "", false
}

// stringConsts is the file's own top-level string constants, folded.
//
// FILE SCOPE ONLY, and that is a stated limit rather than an oversight: a
// constant from another package would need the type checker, and a table name
// this walk cannot resolve is reported as a computed write anyway — the same
// outcome by a different route, and the safe one.
//
// TWO PASSES, so a constant defined in terms of an earlier one folds whichever
// order the file declares them in.
func stringConsts(file *ast.File) map[string]string {
	out := map[string]string{}
	for range 2 {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				val, ok := spec.(*ast.ValueSpec)
				if !ok || len(val.Names) != len(val.Values) {
					continue
				}
				for i, name := range val.Names {
					text, ok := composedString(val.Values[i], out)
					if ok && !strings.Contains(text, unresolved) {
						out[name.Name] = text
					}
				}
			}
		}
	}
	return out
}

// computedDML matches a write whose table this walk could not resolve.
var computedDML = regexp.MustCompile(
	`(?is)\b(?:INSERT\s+(?:OR\s+\w+\s+)?INTO|UPDATE|DELETE\s+FROM)\s+(?:` +
		unresolved + `|%[a-zA-Z])`)

// writesComputedTable reports a write whose table name is not in the source.
func writesComputedTable(text string) bool { return computedDML.MatchString(text) }

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

// mustParse is one expression for a control above.
func mustParse(t *testing.T, src string) ast.Expr {
	t.Helper()
	expr, err := parser.ParseExpr(src)
	if err != nil {
		t.Fatalf("control %q does not parse: %v", src, err)
	}
	return expr
}
