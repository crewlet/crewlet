package code

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// EACH WRITE OF A CHANGE'S ROW REWRITES ONLY THE INDEXES IT MOVES.
//
// The pinned driver rewrites a row's entry in every index whose key or
// partial predicate holds a column a write sets, whether or not the value
// moved ([changeWrite]), so what a write of code_changes costs every data
// node is decided by the columns it NAMES. This plans each write as the code
// makes it — a push record's through the change book's own write
// ([changeBook.write]), and each write of a change's lifecycle through the
// write its op makes ([lifecycleWrite]) — and reads its PROGRAM (EXPLAIN, the
// bytecode, not the query plan): the indexes it deletes a row's entry from
// (IdxDelete) are the ones it rewrites, since an insert deletes none and a
// write over a row deletes the entry of every index it keeps, a partial
// index's predicate deciding only whether the delete runs. Each write is held
// to EXACTLY the indexes its kind moves, written out here rather than derived
// from the schema, so an index a migration adds on a column a write sets
// fails that write's case until somebody decides what it costs — above all
// for a push's head move, the write a change meets most, which rewrites
// code_changes_target_idx alone.
//
// Every kind of write the package declares is one a case makes, and every
// write of code_changes a case makes is a declared kind, so a kind added for a
// new write is held from the change that adds it.
//
// Mutation: set the state beside the head in changeHeadMoved, and the head
// move rewrites the state, owner, open and repository-state listings; name the
// work item in changeEdited, and an edit of the title rewrites
// code_changes_work_idx; drop a case, and its kind is declared and never
// planned; read IdxInsert rather than IdxDelete, and the opens rewrite every
// index.
func TestAChangeWriteRewritesOnlyTheIndexesItMoves(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	const repo = "repo-a"
	e.apply(create(t, repo, "web"))
	e.seedLiveChanges(repo, 4)
	ctx := t.Context()
	vars := e.db.Caps().MaxVariables
	change := func(i int) string { return fmt.Sprintf("%s-change-%06d", repo, i) }

	const (
		open   = "code_changes_open_idx"
		target = "code_changes_target_idx"
		merge  = "code_changes_merge_idx"
	)
	// EVERY INDEX THE STATE IS IN, by its key or by its predicate — the open
	// and target indexes partial on the live states, and the three listings
	// keyed by it.
	stated := []string{open, target, changesStateKey, changesOwnerKey, changesRepoStateKey}
	merged := append(slices.Clone(stated), merge)

	// push plans what a push record of entries writes about the changes its
	// commands reach, as its apply does (applyChangeMoves).
	push := func(entries ...PushEntry) func(x *explainer) error {
		return func(x *explainer) error {
			book, err := readChangeBook(ctx, x.tx, repo, entries, 7, store.EncodeTime(applyClock))
			if err != nil {
				return err
			}
			book.next = 5
			for _, entry := range entries {
				book.move(entry)
				if entry.Change == nil {
					continue
				}
				if _, why := book.open(entry); why != "" {
					return fmt.Errorf("entry %s opened nothing: %s", entry.PushID, why)
				}
			}
			_, err = book.write(ctx, x, vars)
			return err
		}
	}
	// lifecycle plans what a record of a change's lifecycle carrying payload
	// writes of its change's row, as its apply does (applyChangeWrite).
	lifecycle := func(payload any) func(x *explainer) error {
		return func(x *explainer) error {
			_, id, write, err := lifecycleWrite(payload)
			if err != nil {
				return err
			}
			row, found, err := changeRowIn(ctx, x.tx, id)
			switch {
			case err != nil:
				return err
			case !found:
				return fmt.Errorf("no change %s to write", id)
			}
			return writeChanges(ctx, x, vars, write, []*changeRow{&row})
		}
	}
	opening := pushEntry("open", move("refs/heads/topic", ZeroOID, oid("topic")))
	opening.Change = &ChangeOption{Target: "refs/heads/main", Title: "Opened by its push", Body: "about it"}
	title, body := "A better title", "what it does, and why"
	item := types.WorkItem{Backend: types.WorkBackend("native"), ID: "task-1", Key: "ENG-1", Project: "ENG"}

	declared := declaredChangeWrites(t)
	planned := map[changeWrite]bool{}
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		roots := changeIndexRoots(t, tx)
		// THE READING, ON WRITES WHOSE VERDICT IS KNOWN: a write of every
		// column a change's lifecycle may move keeps every index but the
		// number's, whose key no write moves — so a reading that found
		// nothing, which every case below expecting no index would pass, is
		// caught here — and a write of the title alone keeps none.
		for _, control := range []struct {
			name, sql string
			args      []any
			rewrites  []string
		}{
			{"the whole row", `UPDATE code_changes SET title = ?, body_bytes = ?, state = ?, target_ref = ?,
				head_oid = ?, revision = ?, work_backend = ?, work_id = ?, work_key = ?, work_project = ?,
				closed_at = ?, close_reason = ?, merged_at = ?, merged_via = ?, merge_oid = ?, version = ?,
				position = ? WHERE id = ?`, []any{"t", 0, "open", "refs/heads/main", oid("h"), 2, "", "", "", "",
				nil, "", nil, "", "", 2, 7, change(0)},
				append(slices.Clone(merged), changesWorkKey)},
			{"the title", `UPDATE code_changes SET title = ? WHERE id = ?`, []any{"t", change(0)}, nil},
		} {
			x := &explainer{t: t, tx: tx, roots: roots}
			if _, err := x.ExecContext(ctx, control.sql, control.args...); err != nil {
				return err
			}
			if got, want := x.rewritten(), sorted(control.rewrites...); !slices.Equal(got, want) {
				t.Errorf("control %q: its program rewrites %q, want %q — a reading that misses an index "+
					"passes every write", control.name, got, want)
			}
		}

		for _, tc := range []struct {
			name     string
			plan     func(x *explainer) error
			rewrites []string
		}{
			// A PUSH: the heads it moves, the hottest write a change meets —
			// the head is in one index's key — and what it merges and closes,
			// each moving the state; and the change it opens, an insert.
			{"a push moving changes' heads", push(pushEntry("heads",
				move(liveSource(0), liveHead(0), oid("next-0")),
				move(liveSource(1), liveHead(1), oid("next-1")))), []string{target}},
			{"a push merging a change", push(pushEntry("merging",
				move("refs/heads/main", ZeroOID, liveHead(2)))), merged},
			{"a push closing a change", push(pushEntry("closing",
				move(liveSource(3), liveHead(3), ZeroOID))), stated},
			{"a push opening a change", push(opening), nil},
			// A CHANGE'S LIFECYCLE.
			{"an edit of the title", lifecycle(ChangeEdit{RepoID: repo, ChangeID: change(0), Title: &title}), nil},
			{"an edit of the description", lifecycle(ChangeEdit{RepoID: repo, ChangeID: change(0), Body: &body}),
				nil},
			{"an edit setting the work item", lifecycle(ChangeEdit{RepoID: repo, ChangeID: change(0),
				WorkItem: &item}), []string{changesWorkKey}},
			{"an edit clearing the work item", lifecycle(ChangeEdit{RepoID: repo, ChangeID: change(0),
				ClearWorkItem: true}), []string{changesWorkKey}},
			{"a draft's state", lifecycle(ChangeDraft{RepoID: repo, ChangeID: change(0), Draft: true}), stated},
			{"a retarget", lifecycle(ChangeRetarget{RepoID: repo, ChangeID: change(0),
				TargetRef: "refs/heads/release"}), []string{target}},
			{"a close", lifecycle(ChangeClose{RepoID: repo, ChangeID: change(0), Reason: "not needed"}), stated},
			{"a reopen", lifecycle(ChangeReopen{RepoID: repo, ChangeID: change(0), Head: oid("back")}), stated},
			{"a landing", lifecycle(ChangeLanded{RepoID: repo, ChangeID: change(0), Revision: 1,
				HeadOID: liveHead(0), TargetRef: "refs/heads/main", TargetOID: oid("main")}), merged},
		} {
			x := &explainer{t: t, tx: tx, roots: roots, kinds: declared}
			if err := tc.plan(x); err != nil {
				return fmt.Errorf("%s: %w", tc.name, err)
			}
			if len(x.planned) == 0 {
				t.Errorf("%s wrote no change's row", tc.name)
			}
			for _, kind := range x.planned {
				planned[kind] = true
			}
			if got, want := x.rewritten(), sorted(tc.rewrites...); !slices.Equal(got, want) {
				t.Errorf("%s rewrites %q, want %q: a write sets only the columns it moves, and an index on "+
					"a column it sets is an entry it rewrites on every data node", tc.name, got, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("plan the writes: %v", err)
	}
	for kind, name := range declared {
		if !planned[kind] {
			t.Errorf("%s is a kind of write no case makes: hold what it rewrites here", name)
		}
	}
}

// explainer is a [store.Execer] that plans every statement handed to it
// rather than running it: it reads each one's program and keeps the indexes
// of code_changes the program deletes a row's entry from — what a write over a
// row rewrites — and the kind of write ([changeWrite]) of each that writes a
// change's row.
type explainer struct {
	t  *testing.T
	tx *sql.Tx
	// roots is every index of code_changes by its root page, as the programs
	// open them; and kinds every kind of write the package declares, by its
	// clause, which a statement writing a change's row must end in.
	roots map[int64]string
	kinds map[changeWrite]string
	// deletes is every index a planned program rewrote, and planned the kind
	// of each statement that wrote a change's row.
	deletes map[string]bool
	planned []changeWrite
}

func (x *explainer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	x.t.Helper()
	if strings.HasPrefix(query, insertChangesSQL) {
		kind, ok := x.kindOf(query)
		if !ok {
			x.t.Errorf("a write of code_changes ends in a clause no changeWrite declares:\n%s", query)
		}
		x.planned = append(x.planned, kind)
	}
	if x.deletes == nil {
		x.deletes = map[string]bool{}
	}
	for _, index := range rewrites(x.t, program(x.t, x.tx, query, args), x.roots) {
		x.deletes[index] = true
	}
	return driver.RowsAffected(0), nil
}

// kindOf is the kind of write whose clause query ends in.
func (x *explainer) kindOf(query string) (changeWrite, bool) {
	for kind := range x.kinds {
		if strings.HasSuffix(query, " "+string(kind)) {
			return kind, true
		}
	}
	return "", false
}

// rewritten is every index the planned programs rewrote, in name order.
func (x *explainer) rewritten() []string {
	var out []string
	for index := range x.deletes {
		out = append(out, index)
	}
	slices.Sort(out)
	return out
}

// instruction is one step of a statement's program, as EXPLAIN prints it:
// the opcode and its first two operands — for OpenWrite the cursor and the
// root page it opens, for IdxDelete the cursor it deletes through.
type instruction struct {
	opcode string
	p1, p2 int64
}

// program is the program the driver compiles query to, bound to args.
func program(t *testing.T, tx *sql.Tx, query string, args []any) []instruction {
	t.Helper()
	rows, err := tx.QueryContext(context.Background(), "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN:\n%s\n%v", query, err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("EXPLAIN's columns: %v", err)
	}
	// addr, opcode, p1, p2, p3, p4, p5, comment.
	if len(columns) < 4 || columns[1] != "opcode" || columns[2] != "p1" || columns[3] != "p2" {
		t.Fatalf("EXPLAIN prints %q, not the opcode and its operands this reads", columns)
	}
	var out []instruction
	for rows.Next() {
		values := make([]any, len(columns))
		into := make([]any, len(columns))
		for i := range values {
			into[i] = &values[i]
		}
		if err := rows.Scan(into...); err != nil {
			t.Fatalf("read EXPLAIN: %v", err)
		}
		out = append(out, instruction{opcode: text(values[1]), p1: integer(t, values[2]),
			p2: integer(t, values[3])})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read EXPLAIN: %v", err)
	}
	return out
}

// rewrites is every index of roots a program deletes a row's entry from: the
// root page of the cursor each IdxDelete names, as the OpenWrite that opened
// the cursor names it. An index of another table is no write of a change's.
func rewrites(t *testing.T, program []instruction, roots map[int64]string) []string {
	t.Helper()
	opened := map[int64]int64{}
	for _, in := range program {
		if in.opcode == "OpenWrite" {
			opened[in.p1] = in.p2
		}
	}
	var out []string
	for _, in := range program {
		if in.opcode != "IdxDelete" {
			continue
		}
		root, ok := opened[in.p1]
		if !ok {
			t.Fatalf("an IdxDelete through cursor %d, which no OpenWrite opened", in.p1)
		}
		if index, ok := roots[root]; ok && !slices.Contains(out, index) {
			out = append(out, index)
		}
	}
	return out
}

// changeIndexRoots is every index of code_changes by its root page.
func changeIndexRoots(t *testing.T, tx *sql.Tx) map[int64]string {
	t.Helper()
	rows, err := tx.QueryContext(context.Background(), `SELECT name, rootpage FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'code_changes'`)
	if err != nil {
		t.Fatalf("read code_changes' indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var name string
		var root int64
		if err := rows.Scan(&name, &root); err != nil {
			t.Fatalf("read code_changes' indexes: %v", err)
		}
		out[root] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read code_changes' indexes: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("code_changes has no index")
	}
	return out
}

// declaredChangeWrites is every [changeWrite] constant this package's source
// declares, by its clause, with its name.
func declaredChangeWrites(t *testing.T) map[changeWrite]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[changeWrite]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if typ, ok := vs.Type.(*ast.Ident); !ok || typ.Name != "changeWrite" {
					continue
				}
				for i, value := range vs.Values {
					lit, ok := value.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s declares %s as no one string literal", name, vs.Names[i].Name)
					}
					clause, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", vs.Names[i].Name, err)
					}
					out[changeWrite(clause)] = vs.Names[i].Name
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the package declares no changeWrite")
	}
	return out
}

// text is an EXPLAIN column's value as text.
func text(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

// integer is an EXPLAIN operand's value as an integer.
func integer(t *testing.T, v any) int64 {
	t.Helper()
	if n, ok := v.(int64); ok {
		return n
	}
	n, err := strconv.ParseInt(text(v), 10, 64)
	if err != nil {
		t.Fatalf("an EXPLAIN operand %#v is no integer", v)
	}
	return n
}
