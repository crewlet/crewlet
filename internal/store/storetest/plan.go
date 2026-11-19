package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// Handle is what a plan is read through: either estate's handle — the node's
// own file (*store.DB) or the replicated estate (store.ReplicatedHandle) — as
// the two transactions every plan needs, a read for a query and a write
// transaction for a write.
//
// EITHER ESTATE, because a gate holds a statement to the plan it runs under
// wherever the statement runs: the event log, the chat index and the custody
// ledger are the node's own, the tracker, the knowledge base and the vectors
// replicated. Taking the replicated handle alone, every node-estate gate read
// its plans its own way, and a reading of a plan spelled twice is two readings
// a driver bump can blind one at a time.
type Handle interface {
	Read(ctx context.Context, fn func(*sql.Tx) error) error
	Tx(ctx context.Context, fn func(*sql.Tx) error) error
}

// PlanLine is one line of a statement's plan: its id, the line it sits under —
// 0 for the statement's own top level, which is what tells a sort of the whole
// statement from a sort inside one of its subqueries — and what it says.
type PlanLine struct {
	ID, Parent int
	Detail     string
}

// PlanTree is the planner's account of one statement, a line per step with
// each line's place in the plan's tree.
//
// PLANNED AS A DEPLOYMENT PLANS IT: nothing runs ANALYZE, so the fallback row
// estimates apply. A WRITE IS PLANNED IN A WRITE TRANSACTION, since the read
// path runs query_only, which refuses even its EXPLAIN. Nothing is executed.
//
// A PLAN THAT SAYS NOTHING FAILS, since a gate passing on an empty plan
// certified nothing; except an INSERT of values, which reads no table (its
// conflict probe included). An insert that reads a table plans that read.
func PlanTree(t testing.TB, db Handle, statement string, args []any) []PlanLine {

	t.Helper()
	plan, verb, err := planOf(t.Context(), db, statement, args)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN:\n%s\n%v", statement, err)
	}
	if len(plan) == 0 && verb != "INSERT" {
		t.Fatalf("the planner said nothing about:\n%s", statement)
	}
	return plan
}

// Plannable is whether the driver plans a text whole, and why not where it
// refuses: nil for a statement, and the driver's own error for a FRAGMENT of
// one — a clause a builder splices into a statement — or anything else that is
// not a statement. A gate that walks its package's source for every text that
// may be a statement asks this before it holds one to a plan, where [PlanTree]
// fails the test on a refusal; and it asks here rather than spelling an
// EXPLAIN of its own, for the reason [TestEveryPlanIsReadThroughStoretest]
// holds every plan to this file.
func Plannable(t testing.TB, db Handle, statement string, args []any) error {
	t.Helper()
	_, _, err := planOf(t.Context(), db, statement, args)
	return err
}

// planOf is [PlanTree]'s reading, the error returned rather than failed on,
// and the statement's verb, which decides the transaction it is planned in.
func planOf(ctx context.Context, db Handle, statement string,
	args []any) ([]PlanLine, string, error) {

	in := db.Read
	var verb string
	if words := strings.Fields(statement); len(words) > 0 {
		verb = strings.ToUpper(words[0])
	}
	switch verb {
	case "UPDATE", "DELETE", "INSERT":
		in = db.Tx
	}
	var plan []PlanLine
	err := in(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+statement, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var line PlanLine
			var notUsed int
			if err := rows.Scan(&line.ID, &line.Parent, &notUsed, &line.Detail); err != nil {
				return err
			}
			plan = append(plan, line)
		}
		return rows.Err()
	})
	return plan, verb, err
}

// Plan is [PlanTree]'s lines alone.
func Plan(t testing.TB, db Handle, statement string, args []any) []string {
	t.Helper()
	return Details(PlanTree(t, db, statement, args))
}

// Details is what each line of a plan says, in order.
func Details(tree []PlanLine) []string {
	details := make([]string, len(tree))
	for i, line := range tree {
		details[i] = line.Detail
	}
	return details
}

// IndexesIn reads the index names out of a plan.
//
// AN INTERSECTION NAMES SEVERAL: this engine plans two equality terms on two
// indexes as `MULTI-INDEX AND h (a, b)`, a seek on each and the row ids they
// share, so both are read — and a disjunction of two as `MULTI-INDEX OR`, a
// seek on each and the row ids of either.
func IndexesIn(plan []string) []string {
	var found []string
	for _, line := range plan {
		_, tail, ok := strings.Cut(line, "MULTI-INDEX AND ")
		if !ok {
			_, tail, ok = strings.Cut(line, "MULTI-INDEX OR ")
		}
		if ok {
			_, list, _ := strings.Cut(tail, "(")
			list, _, _ = strings.Cut(list, ")")
			for _, name := range strings.Split(list, ",") {
				found = append(found, strings.TrimSpace(name))
			}
			continue
		}
		_, tail, ok = strings.Cut(line, "USING INDEX ")
		if !ok {
			_, tail, ok = strings.Cut(line, "USING COVERING INDEX ")
		}
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(tail, " ")
		found = append(found, strings.TrimSpace(name))
	}
	return found
}

// Seeks reports a plan that SEEKS index — `SEARCH … USING INDEX`, or one leg
// of an intersection or a union — rather than walking it.
func Seeks(plan []string, index string) bool {
	for _, line := range plan {
		if (strings.HasPrefix(line, "SEARCH ") || strings.HasPrefix(line, "MULTI-INDEX AND ") ||
			strings.HasPrefix(line, "MULTI-INDEX OR ")) &&
			slices.Contains(IndexesIn([]string{line}), index) {
			return true
		}
	}
	return false
}

// SeeksJoined reports a plan that reads the relation joined under alias by
// seeking an index (`SEARCH <alias> USING INDEX …`), rather than scanning it
// once for every row it is joined to, as `json_each` joined bare is.
// [TestAJoinedRelationIsSoughtOnlyThroughAnIndex] holds it to the driver.
func SeeksJoined(plan []string, alias string) bool {
	for _, line := range plan {
		if strings.HasPrefix(line, "SEARCH "+alias+" ") && len(IndexesIn([]string{line})) > 0 {
			return true
		}
	}
	return false
}

// Walks reports a plan that reads through index in the index's own order —
// `SCAN … USING INDEX` — with no term on its leading columns to seek.
func Walks(plan []string, index string) bool {
	for _, line := range plan {
		if strings.HasPrefix(line, "SCAN ") &&
			slices.Contains(IndexesIn([]string{line}), index) {
			return true
		}
	}
	return false
}

// Sorts reports a plan that sorts the rows it read before it returns any.
//
// MATCHED ON `FOR ORDER BY`, the part of the line both spellings share — this
// engine's `USE SORTER FOR ORDER BY` and SQLite's `USE TEMP B-TREE FOR ORDER
// BY` — because the word for the structure is the engine's and the step is
// the same: every row the read selected, buffered and sorted before the first
// is returned.
func Sorts(plan []string) bool {
	return slices.ContainsFunc(plan, func(line string) bool {
		return strings.Contains(line, "FOR ORDER BY")
	})
}

// ScansHeap reports a plan line that reads a whole table with no index.
//
// AN INDEX SCAN IS NOT THIS. "SCAN t USING INDEX x" walks an index in its own
// order and a LIMIT stops it early, which is the right plan for a query whose
// point is the order; "SCAN t" reads the table itself, every row through the
// row heap.
func ScansHeap(plan []string, table string) bool {
	for _, line := range plan {
		if strings.HasPrefix(line, "SCAN "+table) && !strings.Contains(line, "INDEX") {
			return true
		}
	}
	return false
}

// IndexesOn is every index the schema declares on these tables — its
// autoindexes, which no DDL names, left out.
func IndexesOn(t testing.TB, db Handle, tables []string) []string {
	t.Helper()
	var found []string
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT name, tbl_name FROM sqlite_master WHERE type = 'index'
			 AND sql IS NOT NULL ORDER BY name`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name, table string
			if err := rows.Scan(&name, &table); err != nil {
				return err
			}
			if slices.Contains(tables, table) {
				found = append(found, name)
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the indexes: %v", err)
	}
	if len(found) == 0 {
		t.Fatalf("the schema declares no index on %v, so this test is measuring "+
			"its own reader", tables)
	}
	return found
}

// SourceLiteral is one fragment [StatementLiterals] found, and the file it is
// in, relative to the module's root.
type SourceLiteral struct {
	File, Text string
}

// StatementLiterals is every fragment of the module's non-test Go source that
// reads one of tables (names it after FROM, JOIN or UPDATE), so a gate can hold
// its registered statements COMPLETE: an unplanned reader's index or heap scan
// is unchecked. own is the gate's package, relative to the module root; the
// walk fails when it read no file outside it. It uses [sourcetree.Walk], so a
// nested checkout's copy of a file is never read.
//
// A FRAGMENT IS A RUN OF ADJACENT LITERALS, folded: `"… FROM " + "pages_heads
// p …"` is one, since neither half names the table alone. A run ends where the
// expression stops being literal, and a gate matches it WHOLE. A table name in
// no literal (chosen at run time) is invisible, so every such builder is
// registered by hand. An INSERT is no reader.
func StatementLiterals(t testing.TB, own string, tables ...string) []SourceLiteral {
	t.Helper()
	names := make([]string, len(tables))
	for i, table := range tables {
		names[i] = regexp.QuoteMeta(table)
	}
	reads := regexp.MustCompile(`(?i)\b(FROM|JOIN|UPDATE)\s+(` +
		strings.Join(names, "|") + `)\b`)
	tree, err := moduleLiterals()
	if err != nil {
		t.Fatalf("read the module's source: %v", err)
	}
	own = filepath.Clean(own)
	if !slices.ContainsFunc(tree.files, func(file string) bool {
		return filepath.Dir(file) != own
	}) {
		t.Fatalf("the walk read no Go file outside %s, so it holds only that "+
			"package's readers of %v", own, tables)
	}
	var out []SourceLiteral
	for _, literal := range tree.literals {
		if reads.MatchString(literal.Text) {
			out = append(out, literal)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no literal of the module reads %v, so this walk is reading "+
			"nothing", tables)
	}
	return out
}

// moduleLiterals is every literal run of the module's non-test Go source and
// every file it read, relative to the module's root — read once for the test
// binary, since every walk asks the same tree.
var moduleLiterals = sync.OnceValues(func() (moduleSource, error) {
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		return moduleSource{}, err
	}
	fset := token.NewFileSet()
	var out moduleSource
	err = sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == "testdata":
			// NOT THE MODULE'S CODE: nothing compiles it.
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go"):
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		out.files = append(out.files, rel)
		for _, run := range literalRuns(file) {
			out.literals = append(out.literals, SourceLiteral{rel, run})
		}
		return nil
	})
	return out, err
})

// moduleSource is what [moduleLiterals] read.
type moduleSource struct {
	literals []SourceLiteral
	files    []string
}

// literalRuns is every maximal run of adjacent string literals in a file's
// concatenations, each folded into the one string it spells.
func literalRuns(file *ast.File) []string {
	runs := literalRunsAt(file)
	out := make([]string, len(runs))
	for i, run := range runs {
		out[i] = run.text
	}
	return out
}

// literalRun is one of [literalRuns] and where it starts: its first literal.
type literalRun struct {
	pos  token.Pos
	text string
}

// literalRunsAt is [literalRuns] with each run's position, for a walk that
// names the line it refuses.
func literalRunsAt(file *ast.File) []literalRun {
	var out []literalRun
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return true
			}
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
		default:
			return true
		}
		// A CONCATENATION, flattened left to right: each run of literals
		// is one fragment, and every other operand is visited on its own,
		// because a call inside a concatenation can hold a statement too.
		var operands []ast.Expr
		var flatten func(ast.Expr)
		flatten = func(e ast.Expr) {
			switch y := e.(type) {
			case *ast.BinaryExpr:
				if y.Op == token.ADD {
					flatten(y.X)
					flatten(y.Y)
					return
				}
			case *ast.ParenExpr:
				flatten(y.X)
				return
			}
			operands = append(operands, e)
		}
		flatten(n.(ast.Expr))
		var run strings.Builder
		var at token.Pos
		held := false
		end := func() {
			if held {
				out = append(out, literalRun{pos: at, text: run.String()})
			}
			run.Reset()
			held = false
		}
		for _, operand := range operands {
			if lit, ok := operand.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil {
					if !held {
						at = lit.Pos()
					}
					run.WriteString(value)
					held = true
				}
				continue
			}
			end()
			ast.Inspect(operand, visit)
		}
		end()
		return false
	}
	ast.Inspect(file, visit)
	return out
}

// Decorrelated reports the first plan line where the planner UNNESTED a
// correlated scalar subquery into one grouped read of the whole child table.
//
// The pinned driver (tursogo v0.8.1; turso's optimizer/unnest.rs) may rewrite
// `(SELECT COUNT(*) FROM child c WHERE c.parent = p.id)`, a per-row seek, as a
// LEFT JOIN over the child GROUPED BY its correlation columns, reading EVERY
// child row: for ONE aggregate, no GROUP BY, ORDER BY, OFFSET, DISTINCT or
// LIMIT but 1, equalities on ONE outer table, when its cost is no higher over
// a never-ANALYZEd database's fallback estimates. The form is a property of no
// line of the statement and a bump can flip it, so a gate asks here and
// rewrites a hit as `ORDER BY … LIMIT 1` (min, max) or one `WHERE parent IN
// (…) GROUP BY parent` over the batch in hand (count, sum).
func Decorrelated(plan []string) (string, bool) {
	for _, line := range plan {
		if strings.Contains(line, decorrelatedOperand) {
			return line, true
		}
	}
	return "", false
}

// decorrelatedOperand is the name the driver gives a scalar subquery it has
// rewritten into a join operand, as in `SEARCH scalar_subquery_t2 USING INDEX
// ephemeral_subquery_t2 (correlation_key_0=?) LEFT-JOIN`. NOT the ephemeral
// index alone: a subquery written in a FROM clause is searched through one too,
// and that is a statement saying what it reads.
// [TestTheDecorrelationCheckSeesTheDriverUnnest] holds it to the driver in both
// directions, so a bump fails there rather than blinding every gate.
const decorrelatedOperand = "scalar_subquery_"
