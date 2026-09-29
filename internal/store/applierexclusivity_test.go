package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/store"
)

// ONLY A STATE LOG'S APPLIER WRITES A PARTITION.
//
// The rule, stated once: a mutation to replicated state is published as ONE
// record on its log, the broker arbitrates it, and it reaches SQL only through
// that log's deterministic applier, in the partition file the log belongs to.
// The stream is the write-ahead log and the files are the derived durable
// state — never the reverse. [internal/statelog] argues the whole of it;
// adr/0002 is the decision, and this test is what that decision's
// `Enforced-by:` names.
//
// # Why an absence needs a test, and why this one in particular
//
// Every other rule about the estates already had a gate and has never been
// re-litigated: no statement spans two files, no table is declared in both
// estates. This one had none, and it is the rule with the failure that cannot
// be seen from inside the node that breaks it. A local write to a partition
// succeeds, the rows look right on the node that made them, and the divergence
// only appears as two nodes answering one question differently — at which
// point the write that caused it is months behind in the log.
//
// The tree carried three such writes when this was added — a version reset
// after a reanchor, a probe-flag clear and an inbox sweep — and each had to
// state its case in terms of the rule. The version reset is gone: its case did
// not survive the arbitration anchor moving off `version` (see
// internal/tracker/reanchor.go), and a reanchor now writes nothing but the
// framework's own checkpoint. The two that remain are allowed below.
//
// # What it walks, and what it cannot see
//
// Every non-test .go file under internal/ and cmd/, parsed with go/parser,
// looking for three things:
//
//  1. A WRITE HANDLE. A partition reaches a caller as one of two types:
//     [store.PartitionHandle], which can open a write transaction and pin a
//     Writer, and [store.PartitionReader], which can only read. Every reader
//     in the tree is handed the second, so the first IS the capability this
//     rule is about, and the walk reports every place that holds it: the type
//     named anywhere — a field, a parameter, a result, a variable — a call to
//     any function the tree declares as answering one, unless the answer is
//     narrowed on the spot ([readOnlyOfHandle]), and a call that takes a
//     partition's own [store.DB]: `.DB()`, `.PartitionDB(name)`,
//     `.OpenPartition(…)`. The framework's own write seam, `statelog.Estate`,
//     is the same capability as an interface, and is reported wherever it is
//     named outside the framework.
//  2. A DML string literal naming a table the PARTITION schema declares, in a
//     file that is not an applier. This one catches a write whose handle
//     arrived by a route the first missed — an interface the walk cannot
//     name, a value `:=` inferred from a call it did not recognise.
//  3. A DML statement whose table is COMPUTED, which no reader of the source
//     can check either.
//
// Before the estate was divided into partitions the write side was one
// method, `Replicated()`, and the walk followed its result: a chained write,
// or a handoff to anything not declared a reader. The handle is now a value
// the runtime hands every domain, and following it would have flagged every
// domain's constructor; what replaced the handoff list is the TYPE, which a
// reader is handed instead of declaring itself one.
//
// Matching is on the NAME rather than on a resolved type, which is the safe
// direction for the reason [TestOnlyOnePlaceWritesARunningStreamsConfiguration]
// gives: a false positive costs one allowance entry with a reason, and a false
// negative is the silent divergence above. The one name it trusts is an
// import's: a call qualified by an imported package is that package's
// function, and is judged by what that function answers.
//
// It cannot see a write assembled by reflection. None exists here.
//
// # The allowance is two-sided
//
// An entry that matches no site in the tree FAILS, the same way
// internal/skipgate's `Always` entries do. An allowance for a write somebody
// deleted is an allowance that would silently cover the next write into the
// same file.
func TestOnlyTheApplierWritesThePartitions(t *testing.T) {
	t.Parallel()

	partition := tablesIn(t, store.EstatePartition)
	if len(partition) == 0 {
		t.Fatal("no tables were derived from the partition schema; this guard " +
			"is watching an estate it cannot see and would pass whatever the " +
			"tree did")
	}

	// THE READ-ONLY METHODS ARE THE HANDLE'S OWN, EXACTLY. The walk lets a
	// call that answers a write handle through when the answer is narrowed
	// on the spot by one of these, so a method added to the handle is a
	// decision this list has to make: a write method that no entry here
	// names would be flagged, and one wrongly named here would be a write
	// the walk waves through.
	handle := reflect.TypeFor[store.PartitionHandle]()
	var writes []string
	for i := range handle.NumMethod() {
		if name := handle.Method(i).Name; !readOnlyOfHandle[name] {
			writes = append(writes, name)
		}
	}
	for name := range readOnlyOfHandle {
		if _, ok := handle.MethodByName(name); !ok {
			t.Errorf("readOnlyOfHandle names %q, which store.PartitionHandle "+
				"does not have — an exemption for a method nobody can call "+
				"would silently cover the next one of that name", name)
		}
	}
	if want := []string{"DB", "Tx", "Writer"}; !slices.Equal(writes, want) {
		t.Errorf("store.PartitionHandle's methods outside readOnlyOfHandle are "+
			"%v, want %v: a new method either writes — and this walk must "+
			"flag a call that reaches it — or it does not and belongs in "+
			"readOnlyOfHandle with the reason", writes, want)
	}

	// THE MATCHERS, ON INPUT WHOSE VERDICT IS KNOWN. A guard asserting an
	// absence passes identically when the thing is absent and when the
	// guard has gone inert, and every allowance below only ever runs
	// against sites the walk found — so a matcher that stopped matching
	// would report a clean tree and take the allowances with it.
	controlAccessors := handleAccessors{
		methods: map[string]bool{"PartitionHandle": true, "estate": true},
		funcs:   map[string]map[string]bool{"internal/store/storetest": {"EstateOf": true}},
	}
	for _, positive := range []string{
		`var h store.PartitionHandle`,
		`type deps struct{ DB store.PartitionHandle }`,
		`g := func(db store.PartitionHandle) {}`,
		`h := node.PartitionHandle(name)`,
		`node.PartitionHandle(name).Tx(ctx, fn)`,
		`run(s.estate(p))`,
		`h := storetest.EstateOf(node)`,
		`db, err := h.DB()`,
		`part, err := node.PartitionDB(name)`,
		`part, err := node.OpenPartition(ctx, f)`,
		`var e statelog.Estate`,
	} {
		if !reachesPartitionWrite(t, positive, controlAccessors) {
			t.Errorf("control: %q holds a partition's write side and the "+
				"matcher did not flag it", positive)
		}
	}
	for _, negative := range []string{
		`r := node.PartitionHandle(name).Reader()`,
		`err := s.estate(p).Read(ctx, fn)`,
		`var r store.PartitionReader`,
		`g := func(db store.PartitionReader) {}`,
		`p := prompts.Partition(msg)`,
		`key := r.prompts.Partition(msg)`,
		`db := c.PartitionDB(t, i)`,
		`w, err := db.Writer(ctx)`,
		`err := db.Tx(ctx, fn)`,
		`p := statelog.EstatePartition`,
	} {
		if reachesPartitionWrite(t, negative, controlAccessors) {
			t.Errorf("control: %q does not hold a partition's write side and "+
				"the matcher flagged it", negative)
		}
	}
	fakePartition := map[string]bool{"tracker_tasks": true}
	for _, positive := range []string{
		`DELETE FROM tracker_tasks WHERE created_at < ?`,
		`UPDATE tracker_tasks SET rank = ?`,
		`INSERT INTO tracker_tasks (id) VALUES (?)`,
	} {
		if !writesTable(positive, fakePartition) {
			t.Errorf("control: %q writes a partition table and the matcher "+
				"did not flag it", positive)
		}
	}
	for _, negative := range []string{
		`SELECT id FROM tracker_tasks WHERE project = ?`,
		`tracker_tasks is written only by the applier`,
		`DELETE FROM crewlet_events WHERE created_at < ?`,
	} {
		if writesTable(negative, fakePartition) {
			t.Errorf("control: %q does not write a partition table and the "+
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

	root := sourcetree.Root(t)
	files := parseTree(t, root, "internal", "cmd")
	accessors := collectAccessors(files)
	// THE COLLECTOR, ON THE TREE. The store's own constructor is declared
	// with a bare `PartitionHandle` result and the runtime's accessor with
	// a qualified one; an accessor list missing either would let every call
	// of it through as though it answered nothing.
	for _, want := range []string{"PartitionHandle", "estate"} {
		if !accessors.methods[want] {
			t.Errorf("the walk did not find the method %q among those that "+
				"answer a write handle, so no call of it would be judged", want)
		}
	}

	var found []site
	for _, f := range files {
		consts := stringConsts(f.file)
		inspectWithParent(f.file, func(n, parent ast.Node) bool {
			if why, ok := partitionWriteAt(n, parent, f.names, accessors); ok {
				found = append(found, site{
					File: f.rel, Line: f.fset.Position(n.Pos()).Line, Why: why,
				})
				return true
			}
			text, ok := composedString(n, consts)
			if !ok {
				return true
			}
			switch {
			case writesTable(text, partition):
				found = append(found, site{
					File: f.rel, Line: f.fset.Position(n.Pos()).Line,
					Why: "DML on " + strings.Join(strings.Fields(text), " "),
				})
			case writesComputedTable(text):
				found = append(found, site{
					File: f.rel, Line: f.fset.Position(n.Pos()).Line,
					Why: "DML whose table is COMPUTED: " + strings.Join(strings.Fields(text), " "),
				})
			}
			// A composed string's own operands are literals this walk
			// would otherwise report a second time, at a worse
			// position. Rendering the whole expression IS the report.
			_, isLit := n.(*ast.BasicLit)
			return isLit
		})
	}
	if len(found) == 0 {
		t.Fatal("found no writes to a partition at all, not even the " +
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
		t.Errorf("%s:%d reaches a partition's write side (%s).\n"+
			"\tA partition is derived state: a mutation is published as one "+
			"record on its log, arbitrated by the broker, and written by that "+
			"log's applier on every node holding the partition. A local write "+
			"succeeds, looks correct on this node, and shows up later as two "+
			"nodes answering one question differently. See "+
			"adr/0002-the-stream-is-the-write-ahead-log.md. A holder that only "+
			"reads takes store.PartitionReader — the handle's Reader() — and is "+
			"not reported. If this write is genuinely not a record — a column "+
			"no record owns, or a sweep of rows whose class permits it — add it "+
			"to allowedPartitionWriter with the reason.", s.File, s.Line, s.Why)
	}

	// AND THE OTHER DIRECTION.
	for _, a := range allowedPartitionWriter {
		if !a.Kind.Valid() {
			t.Errorf("%s is allowed with kind %q — an entry says which of the "+
				"three things it is, so a reviewer can tell the mechanism from "+
				"the writes this rule actually tolerates", a.Prefix, a.Kind)
		}
		if !used[a.Prefix] {
			t.Errorf("%s is allowed to write a partition and does not — the "+
				"reason on file is %q. An allowance for a write nobody makes any "+
				"more silently covers the next one into the same file; delete it",
				a.Prefix, a.Why)
		}
	}
	t.Logf("parsed %d files; partition writers: %d site(s) across %d allowance(s)",
		len(files), len(found), len(allowedPartitionWriter))
}

// site is one place the walk found a write, for the report.
type site struct {
	File string
	Line int
	Why  string
}

// allowanceKind is what an entry claims about itself.
type allowanceKind string

const (
	// mechanism: this file IS a state log's applier, or the framework and
	// the runtime the appliers run on. A write here is how the rule works
	// rather than an exception to it.
	mechanism allowanceKind = "mechanism"

	// exception: a genuine write to a partition that is not a record,
	// because no record could own what it touches. Read both before adding
	// a third.
	exception allowanceKind = "exception"

	// notReplicated: the walk flagged a statement whose table it could not
	// resolve, and that table is not in a partition at all. The claim is
	// made in the register rather than inferred from silence, because a
	// computed table name is exactly what a reader cannot check.
	notReplicated allowanceKind = "not_replicated"
)

// Valid reports whether k is one of the three. The zero value is not.
func (k allowanceKind) Valid() bool {
	return k == mechanism || k == exception || k == notReplicated
}

// allowance is one file permitted to write a partition, with why.
//
// Keyed on the FILE rather than the line, so an edit above a write does not
// have to be reflected here — the same choice internal/clientsource argues for
// in keying on the declaration rather than the path, applied one level down.
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

// allowedPartitionWriter is every file that holds a partition's write side,
// and the case each one makes.
//
// ADDING AN ENTRY IS THE DECISION, so it belongs in a diff somebody reviews
// rather than in a count somebody raises. A new applier file is an ordinary
// entry; a new EXCEPTION is a change to what this rule means and should be
// argued in the pull request, not just in the Why.
var allowedPartitionWriter = []allowance{
	// -----------------------------------------------------------------
	// The mechanism. These are the appliers this rule names as the one
	// writer, plus the framework they run on and the runtime that opens
	// each partition and hands it out.
	// -----------------------------------------------------------------
	{
		Prefix: "internal/statelog/", Kind: mechanism,
		Why: "THE FRAMEWORK: the apply transaction itself, the snapshot " +
			"install and the reanchor. A write here is the mechanism — " +
			"including the operation ledger's per-node sweep and the " +
			"watermark it moves, because the ledger is statelog.Divergent " +
			"(it travels, and its applied_at is each node's own clock), so " +
			"two nodes legitimately hold different rows of it.",
	},
	{
		Prefix: "internal/store/", Kind: mechanism,
		Why: "The partitions' OWNER — it opens each file, runs the " +
			"migrations and constructs the handle. A write here is the " +
			"schema, not a row.",
	},
	{
		Prefix: "internal/engine/statelog.go", Kind: mechanism,
		Why: "THE RUNTIME: it opens and closes each partition its layout " +
			"places here, and hands the write handle to the framework's own " +
			"loops — the appliers, the snapshotter, the adoption and its " +
			"legacy fold. It writes no row of its own.",
	},
	{
		Prefix: "internal/engine/reanchor.go", Kind: mechanism,
		Why: "Hands the framework's reanchor the partition whose checkpoint " +
			"it rewrites. It writes no row of its own.",
	},
	{
		Prefix: "internal/engine/maintenance.go", Kind: mechanism,
		Why: "Where the runtime hands the tracker's partition to the tracker's " +
			"two exceptions below — the duty's probe clear and the inbox " +
			"sweep. It writes no row of its own.",
	},
	{
		Prefix: "internal/engine/retention", Kind: mechanism,
		Why: "The retention report and the capacity check take each open " +
			"partition's database to size its FILE. Neither writes a row.",
	},
	{
		Prefix: "internal/backup/backup.go", Kind: mechanism,
		Why: "Takes the partition's database to copy the FILE — VACUUM INTO " +
			"and the manifest — never to write a row.",
	},
	{
		Prefix: "cmd/crewlet/ops.go", Kind: mechanism,
		Why: "`crewlet migrate` opens every partition its layout names, " +
			"which is how an operator's migration reaches each file. The " +
			"only write is the schema.",
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
			"one file over. It never names a partition table.",
	},

	// -----------------------------------------------------------------
	// THE EXCEPTIONS. Two writes that are not a record, each because no
	// record could own what it touches. Read these before adding a third.
	// -----------------------------------------------------------------
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
	for _, a := range allowedPartitionWriter {
		p := filepath.FromSlash(a.Prefix)
		if strings.HasPrefix(file, p) && len(p) > len(best) {
			best, found = a.Prefix, true
		}
	}
	return best, found
}

// readOnlyOfHandle names the [store.PartitionHandle] methods that do not
// write: a call answering a write handle and narrowed on the spot by one of
// these holds nothing a writer could use. `Reader` is the one that matters —
// it is how the runtime hands a domain's reader its partition — and the rest
// are what a caller asks of the handle in passing. The test holds this list
// against the handle's own method set, so a method added there is a decision
// made here.
var readOnlyOfHandle = map[string]bool{
	"Reader": true,
	"Read":   true,
	"Name":   true,
	"IsZero": true,
	"Caps":   true,
}

// partitionWriteAt reports whether one node, whose parent in the tree is
// parent, holds a partition's write side, and how.
func partitionWriteAt(n, parent ast.Node, f fileNames, acc handleAccessors) (string, bool) {
	switch v := n.(type) {
	case *ast.SelectorExpr:
		id, ok := v.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		switch {
		case f.store != "" && id.Name == f.store && v.Sel.Name == "PartitionHandle":
			return "holds a store.PartitionHandle", true
		case f.statelog != "" && id.Name == f.statelog && v.Sel.Name == "Estate":
			return "holds a statelog.Estate, the framework's write seam", true
		}
	case *ast.CallExpr:
		why, ok := answersWrite(v, f, acc)
		if !ok {
			return "", false
		}
		if sel, isSel := parent.(*ast.SelectorExpr); isSel && sel.X == v && readOnlyOfHandle[sel.Sel.Name] {
			return "", false
		}
		return why, true
	}
	return "", false
}

// answersWrite reports whether a call answers a partition's write side: a
// write handle, or a partition's own database.
func answersWrite(call *ast.CallExpr, f fileNames, acc handleAccessors) (string, bool) {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if acc.funcs[f.dir][fun.Name] {
			return "calls " + fun.Name + ", which answers a write handle", true
		}
	case *ast.SelectorExpr:
		if id, ok := fun.X.(*ast.Ident); ok {
			if path, isPkg := f.imports[id.Name]; isPkg {
				if acc.funcs[moduleDir(path)][fun.Sel.Name] {
					return "calls " + id.Name + "." + fun.Sel.Name +
						", which answers a write handle", true
				}
				return "", false
			}
		}
		switch name := fun.Sel.Name; {
		case acc.methods[name]:
			return "calls ." + name + ", which answers a write handle", true
		case name == "DB" && len(call.Args) == 0:
			return "takes a partition's database through .DB()", true
		case name == "PartitionDB" && len(call.Args) == 1:
			return "takes a partition's database through .PartitionDB", true
		case name == "OpenPartition":
			return "opens a partition's database", true
		}
	}
	return "", false
}

// handleAccessors is every function in the tree declared as ANSWERING a write
// handle, so a call to one is judged exactly as the store's own constructor
// is: a value-typed handle flows by `:=` from any of them without its type
// ever being written at the call.
type handleAccessors struct {
	// methods is by method name, matched on any receiver that is not an
	// imported package.
	methods map[string]bool
	// funcs is by the declaring package's repository-relative directory,
	// then by name.
	funcs map[string]map[string]bool
}

// collectAccessors reads every function declaration whose results name a
// write handle.
func collectAccessors(files []parsedFile) handleAccessors {
	acc := handleAccessors{methods: map[string]bool{}, funcs: map[string]map[string]bool{}}
	for _, f := range files {
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Type.Results == nil || !namesWriteHandle(fn.Type.Results, f.names) {
				continue
			}
			if fn.Recv != nil {
				acc.methods[fn.Name.Name] = true
				continue
			}
			if acc.funcs[f.names.dir] == nil {
				acc.funcs[f.names.dir] = map[string]bool{}
			}
			acc.funcs[f.names.dir][fn.Name.Name] = true
		}
	}
	return acc
}

// namesWriteHandle reports whether a declaration's type expression mentions a
// write handle anywhere in it — the handle itself, a pointer to it, a slice of
// them. Inside internal/store the type is named bare.
func namesWriteHandle(n ast.Node, f fileNames) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := v.X.(*ast.Ident); ok && f.store != "" && id.Name == f.store &&
				v.Sel.Name == "PartitionHandle" {
				found = true
			}
		case *ast.Ident:
			if f.dir == "internal/store" && v.Name == "PartitionHandle" {
				found = true
			}
		}
		return !found
	})
	return found
}

// reachesPartitionWrite parses one snippet and reports whether the matcher
// flags it, so the controls exercise the same code the walk does.
func reachesPartitionWrite(t *testing.T, src string, acc handleAccessors) bool {
	t.Helper()
	// Wrapped in a function, because the controls include statements as
	// well as expressions and only a declaration parses both — and under
	// the imports the controls name, because an import is the one name the
	// matcher trusts.
	file, err := parser.ParseFile(token.NewFileSet(), "control.go", `package p

import (
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

func f() {
`+src+`
}
`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("control %q does not parse: %v", src, err)
	}
	names := namesOf(file, "internal/p")
	flagged := false
	inspectWithParent(file, func(n, parent ast.Node) bool {
		if _, ok := partitionWriteAt(n, parent, names, acc); ok {
			flagged = true
		}
		return true
	})
	return flagged
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

// mustParse is one expression for a control above.
func mustParse(t *testing.T, src string) ast.Expr {
	t.Helper()
	expr, err := parser.ParseExpr(src)
	if err != nil {
		t.Fatalf("control %q does not parse: %v", src, err)
	}
	return expr
}
