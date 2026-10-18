package artifactdata_test

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/statelog"
)

// filter is one query filter.
func filter(path string, op query.Op, value string) query.Filter {
	return query.Filter{Path: path, Op: op, Value: json.RawMessage(value)}
}

// A QUERY IS ANSWERED BY THE FIELD INDEX: equality, ranges, membership, chips
// and prefixes on indexed paths, ordered with the id as the tie-break, a page
// at a time, and a path the index does not hold is refused rather than
// scanned.
func TestAQueryIsAnsweredByTheFieldIndex(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	n.configure(jane, artifacts.Collection{Name: "items", Preset: artifacts.PresetShared,
		Indexed: []string{"meta.city"}})
	long := strings.Repeat("z", 300)
	n.applied(jane,
		set("items", "a", map[string]string{"n": `3`, "tag": `"red"`, "chips": `["x","y"]`, "meta": `{"city":"Oslo"}`, "name": `"Alpha"`}),
		set("items", "b", map[string]string{"n": `1`, "tag": `"blue"`, "chips": `["y"]`, "meta": `{"city":"Bergen"}`, "name": `"Beta"`}),
		set("items", "c", map[string]string{"n": `2`, "tag": `"red"`, "chips": `[]`, "name": `"` + long + `a"`}),
		set("items", "d", map[string]string{"n": `5`, "tag": `"green"`, "name": `"` + long + `b"`}))
	ask := func(q query.Query) []string {
		t.Helper()
		page, err := n.reader.Query(t.Context(), sheet, reading("jane", artifacts.RoleOwner), "items", q)
		if err != nil {
			t.Fatalf("query %+v: %v", q, err)
		}
		var ids []string
		for _, d := range page.Docs {
			ids = append(ids, d.ID)
		}
		return ids
	}
	for name, tc := range map[string]struct {
		q    query.Query
		want []string
	}{
		"equality":            {query.Query{Where: []query.Filter{filter("tag", query.OpEq, `"red"`)}}, []string{"a", "c"}},
		"inequality":          {query.Query{Where: []query.Filter{filter("tag", query.OpNe, `"red"`)}}, []string{"b", "d"}},
		"a range":             {query.Query{Where: []query.Filter{filter("n", query.OpGe, `2`), filter("n", query.OpLt, `5`)}}, []string{"a", "c"}},
		"membership":          {query.Query{Where: []query.Filter{{Path: "tag", Op: query.OpIn, Values: []json.RawMessage{raw(`"blue"`), raw(`"green"`)}}}}, []string{"b", "d"}},
		"a chip":              {query.Query{Where: []query.Filter{filter("chips", query.OpContains, `"y"`)}}, []string{"a", "b"}},
		"a prefix":            {query.Query{Where: []query.Filter{filter("name", query.OpPrefix, `"Al"`)}}, []string{"a"}},
		"a dotted path":       {query.Query{Where: []query.Filter{filter("meta.city", query.OpEq, `"Bergen"`)}}, []string{"b"}},
		"a long string":       {query.Query{Where: []query.Filter{filter("name", query.OpEq, `"`+long+`b"`)}}, []string{"d"}},
		"a path nobody holds": {query.Query{Where: []query.Filter{filter("never", query.OpEq, `1`)}}, nil},
		"an order":            {query.Query{OrderBy: &query.Order{Path: "n", Desc: true}}, []string{"d", "a", "c", "b"}},
	} {
		if got := ask(tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	// A PAGE AT A TIME, with the cursor continuing exactly where it stopped.
	page, err := n.reader.Query(t.Context(), sheet, reading("jane", artifacts.RoleOwner), "items",
		query.Query{OrderBy: &query.Order{Path: "n"}, Limit: 2})
	if err != nil || len(page.Docs) != 2 || page.After == "" {
		t.Fatalf("the first page: %+v, %v", page, err)
	}
	rest, err := n.reader.Query(t.Context(), sheet, reading("jane", artifacts.RoleOwner), "items",
		query.Query{OrderBy: &query.Order{Path: "n"}, Limit: 2, After: page.After})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range append(page.Docs, rest.Docs...) {
		ids = append(ids, d.ID)
	}
	if !slices.Equal(ids, []string{"b", "c", "a", "d"}) {
		t.Fatalf("two pages ordered by n read %v", ids)
	}
	// AN UNINDEXED DOTTED PATH is refused, naming the fix.
	_, err = n.reader.Query(t.Context(), sheet, reading("jane", artifacts.RoleOwner), "items",
		query.Query{Where: []query.Filter{filter("meta.zip", query.OpEq, `"0150"`)}})
	if !errors.Is(err, query.ErrNotIndexed) {
		t.Fatalf("a query on an unindexed path: %v, want ErrNotIndexed", err)
	}
}

// A ROW HIDDEN FROM A READER IS HIDDEN FROM EVERY READ PATH (03 section 4.5):
// the listing, a query, the document, the history, a cell's history, the
// catch-up and the export each leave out a private row the reader did not
// write, and an `editors` collection is not there at all for a viewer.
func TestARowHiddenFromAReaderIsHiddenFromEveryReadPath(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	n.configure(jane, artifacts.Collection{Name: "votes", Preset: artifacts.PresetPrivate},
		artifacts.Collection{Name: "notes", Read: artifacts.ReadEditors,
			Create: artifacts.RoleEditor, Delete: artifacts.RoleEditor})
	n.applied(person("dana", artifacts.RoleViewer), set("votes", "dana-vote", map[string]string{"choice": `"no"`}))
	n.applied(person("bob", artifacts.RoleViewer), set("votes", "bob-vote", map[string]string{"choice": `"yes"`}))
	n.applied(jane, set("notes", "n1", map[string]string{"text": `"editors only"`}))

	bob := reading("bob", artifacts.RoleViewer)
	ctx := t.Context()
	page, err := n.reader.Docs(ctx, sheet, bob, "votes", 0, "", 0)
	if err != nil || len(page.Docs) != 1 || page.Docs[0].ID != "bob-vote" {
		t.Fatalf("bob's listing of votes: %+v, %v", page.Docs, err)
	}
	if catchup, err := n.reader.Docs(ctx, sheet, bob, "votes", 1, "", 0); err != nil || len(catchup.Docs) != 1 {
		t.Fatalf("bob's catch-up of votes: %+v, %v", catchup.Docs, err)
	}
	q, err := n.reader.Query(ctx, sheet, bob, "votes", query.Query{Where: []query.Filter{filter("choice", query.OpEq, `"no"`)}})
	if err != nil || len(q.Docs) != 0 {
		t.Fatalf("bob's query found dana's vote: %+v, %v", q.Docs, err)
	}
	if _, err := n.reader.Doc(ctx, sheet, bob, "votes", "dana-vote"); !errors.Is(err, artifactdata.ErrNotFound) {
		t.Fatalf("bob reading dana's vote: %v, want not found", err)
	}
	history, err := n.reader.History(ctx, sheet, bob, artifactdata.HistoryFilter{}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range history.Entries {
		for _, ch := range e.Changes {
			if ch.Doc == "dana-vote" || ch.Collection == "notes" {
				t.Fatalf("bob's history shows %s/%s, a row he may not read", ch.Collection, ch.Doc)
			}
		}
		if e.Actor.Name == "dana" || e.Actor.Name == "jane" && len(e.Changes) == 0 && e.Op == artifactdata.OpWrite {
			t.Fatalf("bob's history shows a row of %s's with nothing he may read: %+v", e.Actor.Name, e)
		}
	}
	if _, err := n.reader.History(ctx, sheet, bob, artifactdata.HistoryFilter{Collection: "votes",
		Doc: "dana-vote", Field: "choice"}, "", 0); err != nil {
		t.Fatal(err)
	} else if cell, _ := n.reader.History(ctx, sheet, bob, artifactdata.HistoryFilter{Collection: "votes",
		Doc: "dana-vote", Field: "choice"}, "", 0); len(cell.Entries) != 0 {
		t.Fatalf("bob reads the cell history of dana's vote: %+v", cell.Entries)
	}
	var out bytes.Buffer
	if err := n.reader.Export(ctx, sheet, reading("jane", artifacts.RoleEditor), "votes", &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "dana-vote") || strings.Contains(out.String(), "bob-vote") {
		t.Fatalf("the editor's export of a private collection holds another's rows:\n%s", out.String())
	}
	if _, err := n.reader.Docs(ctx, sheet, bob, "notes", 0, "", 0); !errors.Is(err, artifactdata.ErrNotFound) {
		t.Fatalf("a viewer listing an editors-only collection: %v, want not found", err)
	}
	// AN EDITOR reads the editors' collection and every row's history.
	if page, err := n.reader.Docs(ctx, sheet, reading("jane", artifacts.RoleEditor), "notes", 0, "", 0); err != nil || len(page.Docs) != 1 {
		t.Fatalf("an editor's listing of notes: %+v, %v", page.Docs, err)
	}
}

// A DOCUMENT'S AND A CELL'S HISTORY ARE ONE INDEXED READ, newest first, with
// the values before and after each change.
func TestACellsHistoryNamesEachChangeAndItsAuthor(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane, sam := person("jane", artifacts.RoleEditor), person("sam", artifacts.RoleEditor)
	n.applied(jane, set("tasks", "t", map[string]string{"status": `"todo"`, "title": `"T"`}))
	n.applied(sam, merge("tasks", "t", map[string]string{"status": `"doing"`}))
	n.applied(jane, merge("tasks", "t", map[string]string{"title": `"T2"`}))
	n.applied(jane, set("tasks", "other", map[string]string{"status": `"x"`}))
	cell, err := n.reader.History(t.Context(), sheet, reading("jane", artifacts.RoleEditor),
		artifactdata.HistoryFilter{Collection: "tasks", Doc: "t", Field: "status"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cell.Entries) != 2 {
		t.Fatalf("the status cell's history holds %d entries, want 2: %+v", len(cell.Entries), cell.Entries)
	}
	latest := cell.Entries[0]
	if latest.Actor.Name != "sam" || string(latest.Changes[0].Fields[0].Before) != `"todo"` ||
		string(latest.Changes[0].Fields[0].After) != `"doing"` {
		t.Fatalf("the latest change is %+v", latest)
	}
	if latest.Digest == "" || !latest.Undoable {
		t.Fatalf("the history row carries digest %q and undoable %v", latest.Digest, latest.Undoable)
	}
	// A PAGE AT A TIME.
	page, err := n.reader.History(t.Context(), sheet, reading("jane", artifacts.RoleEditor),
		artifactdata.HistoryFilter{}, "", 2)
	if err != nil || len(page.Entries) != 2 || page.After == "" {
		t.Fatalf("the first page of history: %+v, %v", page, err)
	}
	next, err := n.reader.History(t.Context(), sheet, reading("jane", artifacts.RoleEditor),
		artifactdata.HistoryFilter{}, page.After, 2)
	if err != nil || len(next.Entries) != 2 || next.Entries[0].Seq >= page.Entries[1].Seq {
		t.Fatalf("the second page of history: %+v, %v", next, err)
	}
}

// THE CSV EXPORT IS A SPREADSHEET'S: option labels for ids, a list's elements
// joined, the row's attribution, and a cell a spreadsheet would run as a
// formula neutralised.
func TestTheExportIsASpreadsheetsAndRunsNoFormula(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	n.configure(jane, reviewSheet())
	n.applied(jane, set("review", "r1", map[string]string{"title": `"=HYPERLINK(\"http://evil\")"`,
		"status": `"done"`, "labels": `["copy","design"]`, "score": `-2`}))
	var out bytes.Buffer
	if err := n.reader.Export(t.Context(), sheet, reading("jane", artifacts.RoleEditor), "review", &out); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(&out).ReadAll()
	if err != nil {
		t.Fatalf("the export is not CSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("the export holds %d records, want a header and one row", len(records))
	}
	header, row := records[0], records[1]
	col := func(name string) string {
		i := slices.Index(header, name)
		if i < 0 {
			t.Fatalf("the header %v has no %s", header, name)
		}
		return row[i]
	}
	if got := col("title"); !strings.HasPrefix(got, "'=") {
		t.Errorf("a formula cell is written %q, unneutralised", got)
	}
	if col("status") != "Done" || col("labels") != "Copy; Design" || col("score") != "-2" {
		t.Errorf("the row renders status %q, labels %q, score %q", col("status"), col("labels"), col("score"))
	}
	if col("created_by") != "jane" || col("id") != "r1" {
		t.Errorf("the row's id and author are %q and %q", col("id"), col("created_by"))
	}
	if _, err := strconv.ParseInt(col("version"), 10, 64); err != nil {
		t.Errorf("the version cell is %q", col("version"))
	}
}

// THE APPLIER READS NO OTHER DOMAIN: every file the apply runs imports nothing
// that belongs to another domain or to the world outside the record (config,
// the chart, IAM's estate, the clock's own packages), and every table its SQL
// names is this domain's own.
//
// Mutation: read the artifact's shares from `artifact_shares` in an apply, or
// import internal/config into one, and this goes red.
func TestTheApplierReadsNoOtherDomain(t *testing.T) {
	t.Parallel()
	applier := []string{"apply.go", "evaluate.go", "configure.go", "history.go", "storerows.go",
		"values.go", "rules.go", "payload.go", "record.go", "subject.go", "scope.go", "tables.go",
		"domain.go", "moved.go", "errors.go"}
	other := []string{"writer.go", "read.go", "export.go", "rows.go", "ratelimit.go", "doc.go"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !slices.Contains(applier, name) && !slices.Contains(other, name) {
			t.Errorf("%s is in neither list: a new file says whether the applier runs it", name)
		}
	}
	// THE SUBJECT CONSTANTS (internal/queue/topics) are names, not a
	// reader of anything, and are the one queue package the applier names.
	forbidden := []string{"artifactsdomain", "iamdomain", "/tracker", "/pages", "/usage", "/search",
		"/authz", "/config", "/org", "/estate", "/engine", "/coord", "/queue/", "/objstore"}
	table := regexp.MustCompile(`\b(?:FROM|JOIN|INTO|UPDATE)\s+([a-z_][a-z0-9_]*)`)
	fset := token.NewFileSet()
	for _, name := range applier {
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				if strings.Contains(path, "internal/"+strings.TrimPrefix(bad, "/")) &&
					path != "github.com/crewlet/crewlet/internal/queue/topics" {
					t.Errorf("%s imports %s, which the applier may not read", name, path)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, m := range table.FindAllStringSubmatch(s, -1) {
				if !strings.HasPrefix(m[1], "artifact_data_") && !strings.HasPrefix(m[1], "artifactdata_") {
					t.Errorf("%s names table %s, which is not this domain's", name, m[1])
				}
			}
			return true
		})
	}
}

// THE APPLIER'S DECISION IS TOTAL: whatever a record carries, every write ends
// in a verdict and none in an error that would stop the log on every node
// (review AV3). Seeded with the hostile shapes the review names; `go test
// -fuzz` explores further.
func FuzzTheApplierDecisionIsTotal(f *testing.F) {
	for _, seed := range []string{
		`[{"op":"set","collection":"tasks","doc":"a","fields":{"x":1}}]`,
		`[{"op":"increment","collection":"tasks","doc":"a","field":"x","value":1e308},{"op":"increment","collection":"tasks","doc":"a","field":"x","value":1e308}]`,
		`[{"op":"set","collection":"tasks","doc":"a","fields":{"x":[[[[[[[[1]]]]]]]]}}]`,
		`[{"op":"merge","collection":"tasks","doc":"a","fields":{"x":"\u0000"}}]`,
		`[{"op":"append","collection":"tasks","doc":"a","field":"x","value":{"o":1}}]`,
		`[{"op":"delete","collection":"tasks","doc":"a","if_version":-1}]`,
		`[{"op":"remove","collection":"_state","doc":"zz","field":"x","value":1}]`,
		`[{"op":"add","collection":"tasks","doc":"a","field":"x","value":123456789012345678901234567890123456789012345678901234567890123456789}]`,
		`[{"op":"set","collection":"tasks","doc":"a","fields":{"x":1,"y":{"a":{"b":{"c":1}}}}},{"op":"set","collection":"tasks","doc":"a","fields":{}}]`,
	} {
		f.Add(seed)
	}
	db := openStore(f, "fuzz.db")
	applier := artifactdata.NewApplier("fuzz-node", nil)
	var seq atomic.Uint64
	f.Fuzz(func(t *testing.T, ops string) {
		var batch []artifacts.DataOp
		if err := json.Unmarshal([]byte(ops), &batch); err != nil || len(batch) == 0 || len(batch) > 8 {
			return
		}
		jane := person("jane", artifacts.RoleEditor).Authority
		payload, err := json.Marshal(artifactdata.WritePayload{V: 1, Authority: jane,
			Write: artifacts.DataWrite{Ops: batch}})
		if err != nil {
			return
		}
		at := seq.Add(1)
		body, err := artifactdata.Encode(artifactdata.MutationRecord{
			RecordEnvelope: artifactdata.RecordEnvelope{OpID: fmt.Sprintf("fuzz-%d", at),
				Subject: artifactdata.StoreSubject(sheet), Op: artifactdata.OpWrite,
				Scope: artifactdata.ScopeSet{Subject: true}},
			Mutation: payload, Actor: jane.Actor.Name, ActorKind: jane.Actor.Kind,
		})
		if err != nil {
			return
		}
		spec := artifactdata.Domain{}.Stream()
		env, err := artifactdata.Domain{}.Envelope(body)
		if err != nil {
			t.Fatalf("the envelope of a record the encoder wrote: %v", err)
		}
		rec := statelog.Record{Envelope: env, Payload: body, StoredAt: monday,
			Position: statelog.Position{Stream: spec.Name, Seq: at}}
		// A VERDICT IS THE ONLY ACCEPTABLE OUTCOME: an error here is a
		// record that would stop the log on every node.
		if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := applier.Apply(t.Context(), tx, rec, statelog.ApplyOptions{
				Now: monday, StoredAt: monday, MaxVariables: 999})
			return err
		}); err != nil {
			t.Fatalf("the applier failed on %s: %v", ops, err)
		}
	})
}
