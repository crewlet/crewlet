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
// PLANNED AS A DEPLOYMENT PLANS IT: nothing in the engine runs ANALYZE, so a
// plan is taken over the fallback row estimates whatever the fixture holds. A
// WRITE IS PLANNED IN A WRITE TRANSACTION, because the read path runs
// query_only, which refuses even the EXPLAIN of one. Nothing is executed either
// way.
//
// A PLAN THAT SAYS NOTHING FAILS, because a gate that passes on an empty plan
// certified nothing — with one exception, an INSERT: an insert of values reads
// no table, its conflict probe included, and the planner says nothing about
// one, so a gate registering a write that must stay that way reads it as the
// empty plan it is. An insert that reads a table plans that read like any
// other statement.
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
// reads one of tables — names it after FROM, JOIN or UPDATE — so a gate can
// hold its registered statements COMPLETE: a reader it does not plan is a
// reader whose index, or whose heap scan, nobody checked. own is the package
// the gate belongs to, relative to the module's root; the walk fails rather
// than passes when it read no file outside it, because then it holds that
// package's readers alone. It reads this tree through [sourcetree.Walk], so a
// nested checkout's copy of a file is never read as this tree's.
//
// A FRAGMENT IS A RUN OF ADJACENT LITERALS, folded: `"… FROM " +
// "pages_heads p …"` is one fragment, because a statement written across two
// literals reads the table exactly as one written in one does, and matched
// literal by literal neither half names it. A run ends wherever the expression
// stops being literal (a placeholder list, a compiled clause, a named
// constant), and a gate matches the fragment WHOLE against its registered
// statements, so a statement assembled from several runs is held by each of
// them. A table name spelled in no literal — a statement over a table the code
// chooses at run time — is invisible here, which is why every such builder is
// registered by hand. An INSERT finds no rows, which is why it is no reader.
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

// Decorrelated reports the first line of a plan at which this engine's planner
// has UNNESTED a correlated scalar subquery — turned a per-row aggregate into
// one grouped read of the whole child table, joined back to the outer rows —
// and whether there is one.
//
// # The rule, stated once
//
// A correlated scalar subquery reads, for each outer row, the child rows its
// equality names: `(SELECT COUNT(*) FROM child c WHERE c.parent = p.id)` is a
// seek of an index on `parent` per row of `p`, and its cost is the rows the
// statement answers. The pinned driver (tursogo v0.8.1, whose rule is
// core/translate/optimizer/unnest.rs in turso's source) may rewrite one such
// subquery as a LEFT JOIN against the child table GROUPED BY its correlation
// columns, materialised and searched through an ephemeral index — which reads
// EVERY row of the child table, whatever the outer rows are. It is eligible
// when the subquery selects ONE aggregate (avg, count, max, min, total), has
// no GROUP BY, ORDER BY, OFFSET or DISTINCT, takes no LIMIT but the literal 1,
// and is correlated only by equalities on ONE outer table; and the planner
// plans both forms and keeps the rewrite whenever its cost is no higher. That
// cost is reckoned over the fallback row estimates of a database nobody has
// ANALYZEd, which is every database this engine opens — so whether a
// statement reads one child row per answer or the whole child table is a
// property of no line of its code, and an added index or a driver bump can
// flip it. Measured: the unfiltered `MIN` below is rewritten, the same `MIN`
// over a key-bound outer is not, and `ORDER BY … LIMIT 1` never is.
//
// So a gate does not ask whether a statement LOOKS correlated; it asks the
// plan, through this one predicate, and a site it fires on is rewritten in a
// form the rule cannot reach: a minimum or maximum as `ORDER BY … LIMIT 1`, a
// count or a sum read for the batch of rows already in hand, in one
// `… WHERE parent IN (…) GROUP BY parent` over their keys.
//
// # What it matches
//
// The rewrite's own operand: the subquery becomes a table the driver names
// `scalar_subquery_<n>`, read (as measured, `SEARCH scalar_subquery_t2 USING
// INDEX ephemeral_subquery_t2 (correlation_key_0=?) LEFT-JOIN`) beside the
// outer table. NOT the ephemeral index alone: a subquery written in a FROM
// clause and joined is materialised and searched through an
// `ephemeral_subquery_<n>` index too, and that is a statement saying what it
// reads, not a planner deciding it. [TestTheDecorrelationCheckSeesTheDriverUnnest]
// holds the predicate to the driver in both directions, so a bump that renames
// the rewrite or stops making it fails there rather than leaving every gate
// that asks this blind.
func Decorrelated(plan []string) (string, bool) {
	for _, line := range plan {
		if strings.Contains(line, decorrelatedOperand) {
			return line, true
		}
	}
	return "", false
}

// decorrelatedOperand is the name the driver gives a scalar subquery it has
// rewritten into a join operand.
const decorrelatedOperand = "scalar_subquery_"
