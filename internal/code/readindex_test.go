package code

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// EVERY CODE READ SEEKS A NAMED INDEX.
//
// # Why it is held rather than reviewed
//
// Every data node applies every record of the code log on the one write lane
// every other domain's applier waits behind, and every read a git node, a seat
// or a person makes is answered from one of those copies: a statement that
// scans a code table costs what the largest repository holds, on every node,
// at every call, and it shows as a slow push or a stalled lane, never as a
// failure. TestEveryCodeIndexServesARegisteredQuery holds each REGISTERED
// statement to the exact seek it is written for; what no registry can see is
// the statement nobody registered — a read added beside it is planned by
// nobody. So this walks the package's source (readwalk_test.go) for every text
// that reads a code table and holds it to the registry, both ways:
//
//   - every text the driver plans whole is a statement registeredStatements
//     names, by its exact text — the package's own constant, so there is no
//     copy to drift from what runs;
//   - every text it will not plan whole is a FRAGMENT, and every function
//     that uses one is a builder [planShapes] registers;
//   - every function a statement's text cannot be followed out of to
//     constants assembles one at run time, and is a builder [planShapes]
//     registers, keyed by what decides the shapes it builds;
//   - every registered statement is spelled in the source, and every shape
//     names a builder the walk found — and objstore's, over the
//     declarations [referenceTables] lists, are each registered for every
//     declaration the builder admits, at every width its callers build it
//     at ([referenceWidths]), as exactly the text it spells there
//     (holdReferences).
//
// # What a plan must do
//
// Reach every table of the replicated estate it reads by a SEARCH — on an
// index the schema declares, or on the primary key — whatever else it does.
// A SCAN reads every row of what it scans: an index's every entry is still
// every row. A LIMIT is no bound on one either: nothing here requires a
// scanning statement to carry a LIMIT, and a LIMIT bounds the rows a scan
// RETURNS, not the rows it reads — every row a filter no index serves
// rejects is read before the LIMIT fills, which is every row the table holds
// when none qualifies. So a scan is allowed only of
// json_each, the statement's own list; of a subquery's or a common table
// expression's result, which is no table; and of a table [scanOK] names with
// what bounds it, which is never a repository's size. A SEARCH on an index the
// schema does not declare is refused too: an automatic index is a scan and a
// sort a statement, built where its plan is run. Planned on the estate
// seededPlanEstate fills, with NO ANALYZE, because no deployment runs one.
// What is judged is every statement and shape the registries name, and the
// read a migration claims for an index no statement reads yet
// ([claimedReads]): no code issues a claim, so the match above never sees
// one, but the read its index's reader will issue is held to this rule from
// the migration that declares the index.
//
// Mutation: read a repository by its description (`WHERE description = ?`,
// which no index leads with) — in the judgement's own control, or in a
// registered constant — and the gate reports the scan; spell a statement over
// a code table nobody registers, issue one assembled at run time, register a
// statement the source no longer spells, drop a shape objstore builds or plan
// it at a width no caller builds it at, or give scanOK a table nothing scans,
// and it fails.
func TestEveryCodeReadSeeksANamedIndex(t *testing.T) {
	t.Parallel()
	db := seededPlanEstate(t)
	schema := schemaOf(t, db)

	// THE JUDGEMENT, ON STATEMENTS WHOSE VERDICT IS KNOWN — the mutation
	// first: a repository found by a column no index leads with reads every
	// repository the company holds, and the gate must say so of a SELECT, an
	// UPDATE and a join alike, and of an index read whole.
	for name, tc := range map[string]struct {
		sql  string
		args []any
		// wrong is what the judgement must report, "" for a plan it passes.
		wrong string
	}{
		"a repository by its description": {`SELECT id FROM code_repos WHERE description = ?`,
			[]any{"web"}, "reads code_repos by a scan"},
		"an update by a repository's description": {`UPDATE code_repos SET status = 'active'
			WHERE description = ?`, []any{"web"}, "reads code_repos by a scan"},
		"every repository counted": {`SELECT COUNT(*) FROM code_repos`, nil,
			"reads code_repos by a scan"},
		"the activation fence read whole": {`SELECT fence FROM code_activation`, nil,
			"reads code_activation by a scan"},
		"a join that scans its inner table": {`SELECT r.id FROM code_repos r JOIN code_names n
			ON n.repo_id = r.id WHERE r.id = ?`, []any{"repo-1"}, "reads code_names by a scan"},
		"a repository by its key":      {repoNameSQL, []any{"repo-1"}, ""},
		"a batch driven from its list": {refsNamedSQL, []any{"repo-1", `["refs/heads/r000001"]`}, ""},
		"a seek inside a common table expression": {`WITH due AS (SELECT id FROM code_repos
			WHERE id = ?) SELECT id FROM due`, []any{"repo-1"}, ""},
		"the evictions listing, which scanOK bounds": {evictionsSQL, nil, ""},
	} {
		scanned, problems := judgePlan(storetest.Plan(t, db, tc.sql, tc.args), schema)
		switch {
		case tc.wrong == "" && len(problems) > 0:
			t.Errorf("control %q: the judgement refused a plan that seeks every table: %q", name, problems)
		case tc.wrong != "" && !slices.ContainsFunc(problems, func(p string) bool {
			return strings.Contains(p, tc.wrong)
		}):
			t.Errorf("control %q: the judgement reported %q, want %q — a gate that passes this "+
				"passes every scan", name, problems, tc.wrong)
		}
		if name == "the evictions listing, which scanOK bounds" && !slices.Contains(scanned, "code_evictions") {
			t.Errorf("control %q: the judgement did not count the scan scanOK allows: %q", name, scanned)
		}
	}

	// THE MATCH, ON INPUT WHOSE VERDICT IS KNOWN: what the walk could find,
	// held to a registry, with the driver's verdict on each text given.
	{
		whole := map[string]bool{"SELECT a FROM code_x WHERE id = ?": true,
			"SELECT b FROM code_y WHERE id = ?": true, "SELECT c FROM code_z WHERE id = ?": true}
		spelled := func(at string, users ...string) *spelling {
			s := &spelling{reads: []string{"code_x"}, at: []string{at}, users: map[string]bool{}}
			for _, u := range users {
				s.users[u] = true
			}
			return s
		}
		folded := spelled("x.go:6")
		folded.folded = true
		walk := readWalk{
			spelled: map[string]*spelling{
				// Registered, a builder's fragment and a fragment folded
				// into another text: none is a finding.
				"SELECT a FROM code_x WHERE id = ?":    spelled("x.go:1", "direct"),
				"SELECT a FROM code_x WHERE":           spelled("x.go:3", "assembles"),
				"SELECT a FROM code_x WHERE folded = ": folded,
				// Whole and unregistered, a fragment outside any builder,
				// and a fragment nothing uses: each is.
				"SELECT b FROM code_y WHERE id = ?":  spelled("x.go:2", "direct"),
				"SELECT a FROM code_x WHERE b IN (":  spelled("x.go:4", "unregistered"),
				"SELECT a FROM code_x WHERE dead = ": spelled("x.go:5"),
			},
			builders: map[string][]string{"assembles": {"x.go:7: assembled"},
				"unshaped": {"x.go:8: assembled"}},
			declared: map[string]bool{"direct": true, "assembles": true, "unshaped": true,
				"unregistered": true, "plain": true},
		}
		registered := map[string]statement{
			"a":    {sql: "SELECT a FROM code_x WHERE id = ?"},
			"gone": {sql: "SELECT c FROM code_z WHERE id = ?"},
		}
		shapes := map[string]shape{
			"assembled":       {builder: "assembles"},
			"of a stranger":   {builder: "stranger"},
			"of a plain read": {builder: "plain"},
		}
		got := holdReads(walk, registered, shapes, func(text string) (bool, error) {
			return whole[text], fmt.Errorf("incomplete input")
		})
		for _, want := range []string{
			"x.go:2 reads code_x, and plan_test.go registers no statement of its text",
			"x.go:4 reads code_x, and the driver will not plan it whole (incomplete input): a " +
				"fragment, which only a builder planShapes registers may use — and unregistered, " +
				"which uses it, is none",
			"x.go:5 reads code_x, and the driver will not plan it whole (incomplete input): a " +
				"fragment, and no function uses it",
			"unshaped builds a statement at run time (x.go:8: assembled)",
			`"of a stranger" names stranger, which the package does not declare`,
			`"of a plain read" names plain, which builds no statement at run time`,
			`"gone" is spelled nowhere`,
		} {
			if !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, want) }) {
				t.Errorf("the match reported no finding saying %q; found %q", want, got)
			}
		}
		for _, f := range got {
			for _, clean := range []string{"x.go:1", "x.go:3", "x.go:6", "x.go:7"} {
				if strings.Contains(f, clean) {
					t.Errorf("the match reported %q, about what the registry or a builder answers for", f)
				}
			}
		}
		if len(got) != 7 {
			t.Errorf("the match reported %d findings, want the seven known ones: %q", len(got), got)
		}
	}

	// THE REFERENCES' MATCH, ON SHAPES WHOSE VERDICT IS KNOWN: the collector's
	// question planned at three keys under the name of the five hundred it
	// asks and a walk's page planned by nothing — each a statement that runs
	// and that no plan answers for — and a shape at a width no caller builds,
	// a plan of a statement that never runs.
	{
		shapes := planShapes()
		full := referenceShape("ObjectsAmong", PackReferences.Table, collect.JudgeBatch)
		narrow := referenceShape("ObjectsAmong", PackReferences.Table, 3)
		page := referenceShape("ReferencesAfter", PackReferences.Table, objstore.WalkPage)
		three, err := PackReferences.ObjectsAmong(3)
		if err != nil {
			t.Fatalf("objstore builds no question of three keys over %s: %v", PackReferences.Table, err)
		}
		mislabelled := shapes[full]
		mislabelled.sql = three
		shapes[full], shapes[narrow] = mislabelled, mislabelled
		delete(shapes, page)
		declared := readWalk{references: map[string]bool{}}
		for name := range referenceTables {
			declared.references[name] = true
		}
		got := holdReferences(declared, shapes)
		for _, want := range []string{
			fmt.Sprintf("shape %q plans %q, and objstore's ObjectsAmong spells", full, three),
			fmt.Sprintf("shape %q names %sObjectsAmong, which builds no such statement", narrow, objstoreBuilder),
			fmt.Sprintf("planShapes registers no shape %q", page),
		} {
			if !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, want) }) {
				t.Errorf("the references' match reported no finding saying %q; found %q", want, got)
			}
		}
		if len(got) != 3 {
			t.Errorf("the references' match reported %d findings, want the three known ones: %q", len(got), got)
		}
	}

	// THE PACKAGE.
	walk := walkForReads(t, sourcetree.Root(t), modulePath)
	if walk.files == 0 || walk.sites == 0 || len(walk.spelled) == 0 {
		t.Fatalf("the walk read %d files of %s, judged %d statement sites and found %d texts "+
			"reading a code table — this gate was certifying nothing", walk.files, codeDir,
			walk.sites, len(walk.spelled))
	}
	for _, f := range walk.findings {
		t.Error(f)
	}
	registered, shapes := registeredStatements(), planShapes()
	for _, f := range holdReads(walk, registered, shapes, func(text string) (bool, error) {
		return plannable(t, db, text)
	}) {
		t.Error(f)
	}
	for _, f := range holdReferences(walk, shapes) {
		t.Error(f)
	}
	// EVERY PLAN THE PLAN TESTS EXPLAIN: the registered statements, the
	// shapes, and the read a migration claims for an index no statement
	// reads yet ([claimedReads]) — the read its reader will issue, judged
	// now as that statement will be once it is registered.
	judged := everyStatement(t)
	for index, st := range claimedReads() {
		name := claimName(index)
		if _, taken := judged[name]; taken {
			t.Fatalf("%q names a registered statement or a shape and a claim: one name, one statement", name)
		}
		judged[name] = st
	}
	scanned := map[string]bool{}
	for name, st := range judged {
		plan := storetest.Plan(t, db, st.sql, st.args)
		scans, problems := judgePlan(plan, schema)
		for _, table := range scans {
			scanned[table] = true
		}
		for _, p := range problems {
			t.Errorf("%s %s — seek it by an index the schema declares, and declare one in a "+
				"replicated migration where none serves it:\n%s", name, p, strings.Join(plan, "\n"))
		}
	}
	for _, table := range slices.Sorted(maps.Keys(scanOK)) {
		if !scanned[table] {
			t.Errorf("scanOK lets %s be read whole and no registered statement reads it so: an "+
				"allowance nothing takes is read by nobody against what it allows — drop it", table)
		}
	}
	t.Logf("read %d files of %s: %d statement sites, %d texts reading a code table, %d "+
		"registered statements, %d shapes", walk.files, codeDir, walk.sites, len(walk.spelled),
		len(registered), len(shapes))
}

// scanOK is every table a statement may read whole, with what bounds it —
// never a repository's size, which no statement here may be bounded by.
var scanOK = map[string]string{
	// THE TRIM'S LISTING ([Domain.Evictions]): one row a node an operator
	// has evicted from the code log, kept through its readmission — as many
	// rows as the fleet has had evictions, whatever its repositories hold —
	// and the trim reads every one of them each pass.
	"code_evictions": "one row a node an operator evicted from the code log, whatever its repositories hold",
}

// planSchema is what a plan is judged against: every table and every index
// the replicated estate declares.
type planSchema struct{ tables, indexes map[string]bool }

// schemaOf reads the replicated estate's tables and indexes, its primary
// keys' automatic indexes among them.
func schemaOf(t *testing.T, db store.ReplicatedHandle) planSchema {
	t.Helper()
	schema := planSchema{tables: map[string]bool{}, indexes: map[string]bool{}}
	if err := db.Read(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(),
			`SELECT type, name FROM sqlite_master WHERE type IN ('table', 'index')`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var kind, name string
			if err := rows.Scan(&kind, &name); err != nil {
				return err
			}
			if kind == "table" {
				schema.tables[name] = true
			} else {
				schema.indexes[name] = true
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the replicated estate's schema: %v", err)
	}
	if !schema.tables["code_repos"] || !schema.indexes[repoKey] {
		t.Fatalf("the replicated estate declares no code_repos or no %s: the schema read missed them", repoKey)
	}
	return schema
}

// judgePlan judges one statement's plan, a detail line a step: every table
// of the replicated estate it reads, sought by an index the schema declares or
// by the primary key, or scanned where [scanOK] lets it be. It answers the
// tables it scanned so and what it found wrong.
//
// The driver names a table a step reads first — the table on a scan
// ("SCAN code_repos AS r"), its alias on a search ("SEARCH r USING …") — and
// names a subquery's result "(subquery-N)", a common table expression's by
// its name and a scalar's "CONSTANT ROW", none of them a table.
func judgePlan(plan []string, schema planSchema) (scanned, problems []string) {
	for _, line := range plan {
		verb, rest, _ := strings.Cut(line, " ")
		name, how, _ := strings.Cut(rest, " ")
		switch {
		case name == "json_each":
			// The statement's own list, which a batch is driven from.
		case verb == "SCAN" && !schema.tables[name]:
			// No table: a subquery's, a common table expression's or a
			// constant row's.
		case verb == "SCAN":
			if _, ok := scanOK[name]; ok {
				scanned = append(scanned, name)
				continue
			}
			problems = append(problems, fmt.Sprintf("reads %s by a scan (%s)", name, line))
		case verb == "SEARCH":
			_, using, found := strings.Cut(how, "USING ")
			if strings.HasPrefix(using, "INTEGER PRIMARY KEY ") || strings.HasPrefix(using, "PRIMARY KEY ") {
				continue
			}
			index, isIndex := strings.CutPrefix(strings.TrimPrefix(using, "COVERING "), "INDEX ")
			index, _, _ = strings.Cut(index, " ")
			if !found || !isIndex || !schema.indexes[index] {
				problems = append(problems, fmt.Sprintf("searches %s by no index the schema "+
					"declares (%s): one built where the statement runs is a scan and a sort", name, line))
			}
		}
	}
	return scanned, problems
}

// plannable reports whether the driver plans a text whole — a statement — or
// refuses it, and why: a FRAGMENT of one, which only the builder that
// assembles it can be planned through. Every parameter the text binds is
// bound to nothing, since a plan reads no argument's value.
func plannable(t *testing.T, db store.ReplicatedHandle, text string) (bool, error) {
	t.Helper()
	err := storetest.Plannable(t, db, text, make([]any, paramCount(text)))
	return err == nil, err
}

// holdReads holds what the walk found to the statements and shapes plan_test.go
// registers, both ways, and answers what it found wrong. plannable is the
// driver's verdict on a text: whole, or a fragment and why.
func holdReads(walk readWalk, registered map[string]statement, shapes map[string]shape,
	plannable func(string) (bool, error)) []string {

	var out []string
	report := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	registers := map[string]bool{}
	for _, st := range registered {
		registers[st.sql] = true
	}
	shaped := map[string]bool{}
	for _, sh := range shapes {
		shaped[sh.builder] = true
	}
	// builds is every function the walk found assembling a statement, from a
	// fragment or at a site: what a shape of the package's own may name.
	builds := map[string]bool{}
	for _, text := range slices.Sorted(maps.Keys(walk.spelled)) {
		s := walk.spelled[text]
		if registers[text] {
			continue
		}
		whole, why := plannable(text)
		if whole {
			report("%s reads %s, and plan_test.go registers no statement of its text %q: register "+
				"it in registeredStatements with the seek it is written for", s,
				strings.Join(s.reads, ", "), text)
			continue
		}
		// A FRAGMENT: what a builder assembles a statement from — or a
		// statement the driver refuses, which the reason it gives says.
		if len(s.users) == 0 && !s.folded {
			report("%s reads %s, and the driver will not plan it whole (%v): a fragment, and no "+
				"function uses it — delete it, or mend it where it is a statement the driver refuses",
				s, strings.Join(s.reads, ", "), why)
		}
		for _, user := range slices.Sorted(maps.Keys(s.users)) {
			builds[user] = true
			if !shaped[user] {
				report("%s reads %s, and the driver will not plan it whole (%v): a fragment, which only "+
					"a builder planShapes registers may use — and %s, which uses it, is none: register "+
					"the shapes it assembles, spell the statement whole, or mend it where it is one the "+
					"driver refuses", s, strings.Join(s.reads, ", "), why, orPackageLevel(user))
			}
		}
	}
	for _, fn := range slices.Sorted(maps.Keys(walk.builders)) {
		builds[fn] = true
		if !shaped[fn] {
			report("%s builds a statement at run time (%s), and planShapes registers no shape of it: "+
				"register each statement it can assemble, keyed by what decides it — or spell them "+
				"as constants", orPackageLevel(fn), strings.Join(walk.builders[fn], "; "))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(shapes)) {
		sh := shapes[name]
		switch {
		case strings.HasPrefix(sh.builder, objstoreBuilder):
			// objstore's, over this package's declarations: holdReferences.
		case !walk.declared[sh.builder]:
			report("shape %q names %s, which the package does not declare", name, sh.builder)
		case !builds[sh.builder]:
			report("shape %q names %s, which builds no statement at run time: the statements it "+
				"issues are constants, which registeredStatements names", name, sh.builder)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(registered)) {
		if _, ok := walk.spelled[registered[name].sql]; !ok {
			report("registered statement %q is spelled nowhere in %s's non-test source as a text "+
				"reading a code table: a registered statement no code issues certifies nothing — "+
				"drop it, or register the text that is issued", name, codeDir)
		}
	}
	return out
}

// objstoreBuilder prefixes a shape's builder where objstore assembles the
// statement over one of this package's declarations.
const objstoreBuilder = "objstore.ReferenceTable."

// holdReferences holds the object store's builders over this package's
// declarations to the shapes: referenceTables to the declarations the source
// makes, referenceBuilders to objstore.ReferenceTable's statement builders
// and referenceWidths to referenceBuilders, each both ways, and every
// statement a builder admits over a declaration — its standing refuses the
// rest — to a shape of it at each width its callers build it at, planning
// exactly the text the builder spells there.
func holdReferences(walk readWalk, shapes map[string]shape) []string {
	var out []string
	report := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	for _, name := range slices.Sorted(maps.Keys(referenceTables)) {
		if !walk.references[name] {
			report("referenceTables lists %s, which %s declares no objstore.ReferenceTable as", name, codeDir)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(walk.references)) {
		if _, ok := referenceTables[name]; !ok {
			report("%s declares %s, an objstore.ReferenceTable, and referenceTables does not list it: "+
				"objstore builds statements over it, which planShapes registers", codeDir, name)
		}
	}
	builders := referenceStatementMethods()
	for _, method := range builders {
		if _, ok := referenceBuilders[method]; !ok {
			report("objstore.ReferenceTable.%s builds a statement, and referenceBuilders does not list "+
				"it: register its shape over each declaration it admits", method)
		}
	}
	for _, method := range slices.Sorted(maps.Keys(referenceBuilders)) {
		if !slices.Contains(builders, method) {
			report("referenceBuilders lists %s, which is no statement builder of objstore.ReferenceTable", method)
		}
		if len(referenceWidths[method]) == 0 {
			report("referenceWidths names no width objstore's %s is built at: list every width its "+
				"callers build it at, from their own constants", method)
		}
	}
	for _, method := range slices.Sorted(maps.Keys(referenceWidths)) {
		if _, ok := referenceBuilders[method]; !ok {
			report("referenceWidths lists %s, which referenceBuilders does not", method)
		}
	}
	admitted := map[string]bool{}
	for _, method := range slices.Sorted(maps.Keys(referenceBuilders)) {
		for _, name := range slices.Sorted(maps.Keys(referenceTables)) {
			table := referenceTables[name]
			for _, width := range referenceWidths[method] {
				key := referenceShape(method, table.Table, width)
				text, refused := referenceBuilders[method](table, width)
				sh, has := shapes[key]
				switch {
				case refused == nil && !has:
					report("objstore builds %s over %s (%s) at %d, and planShapes registers no shape %q of it",
						method, name, table.Table, width, key)
				case refused == nil && sh.builder != objstoreBuilder+method:
					report("shape %q names %s, and objstore's %s builds it", key, sh.builder, method)
				case refused == nil && sh.sql != text:
					report("shape %q plans %q, and objstore's %s spells %q at %d: plan the statement its "+
						"callers run", key, sh.sql, method, text, width)
				case refused != nil && has:
					report("planShapes registers %q, which objstore refuses to build: %v", key, refused)
				}
				if refused == nil {
					admitted[key] = true
				}
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(shapes)) {
		if sh := shapes[key]; strings.HasPrefix(sh.builder, objstoreBuilder) && !admitted[key] {
			report("shape %q names %s, which builds no such statement over a declaration "+
				"referenceTables lists at a width referenceWidths names — a statement planned at a "+
				"width no caller builds certifies a statement nothing runs", key, sh.builder)
		}
	}
	return out
}

// referenceStatementMethods is every method of objstore.ReferenceTable that
// builds a statement: a width in, the statement's text and an error out.
func referenceStatementMethods() []string {
	rt := reflect.TypeFor[objstore.ReferenceTable]()
	errorType := reflect.TypeFor[error]()
	var out []string
	for i := range rt.NumMethod() {
		ft := rt.Method(i).Type
		if ft.NumIn() == 2 && ft.In(1).Kind() == reflect.Int && ft.NumOut() == 2 &&
			ft.Out(0).Kind() == reflect.String && ft.Out(1) == errorType {
			out = append(out, rt.Method(i).Name)
		}
	}
	return out
}

// orPackageLevel names a function, or the package level for "".
func orPackageLevel(fn string) string {
	if fn == "" {
		return "a package-level declaration"
	}
	return fn
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN — and the lexer under it, on
// statements whose verdict is known. Every way the package spells a statement
// must be found: declared, inline, built of constants, an UPDATE, a DELETE, an
// INSERT that selects, a comma join, a quoted table, a common table
// expression, a subquery, a context-free method, a field's handle, a
// connection DB.Conn opened, a file this platform does not build. What only
// writes, a column a table qualifies, a comment, a string literal and a log
// key are none. Every way a statement is assembled at run time must make its
// function a builder: an append, a format, a package-level variable, an
// exported function's parameter, a closure's, a range over something the walk
// cannot read whole, and a caller handing a helper what it built; and every
// way a text is merely CHOSEN among constants must not: a range over a
// literal, an assignment in each branch, a helper every caller hands a
// constant, a method likewise, a local that shadows a constant — and a
// statement prepared from a constant and run with arguments only the run
// knows, whose one site is its Prepare. A method that is no statement is no
// site, a statement method taken as a value or on a value the walk cannot
// type is reported, and a test file is never read.
func TestTheReadGateFindsEveryStatement(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		sql    string
		reads  []string
		params int
	}{
		{`SELECT a FROM code_x WHERE id = ?`, []string{"code_x"}, 1},
		{`select a from Code_X where id = ?`, []string{"code_x"}, 1},
		{`SELECT a FROM json_each(?2) AS j CROSS JOIN code_refs AS r WHERE r.repo_id = ?1`,
			[]string{"code_refs"}, 2},
		{`UPDATE OR IGNORE code_repos SET a = ? WHERE id = ?`, []string{"code_repos"}, 2},
		{`INSERT INTO code_x (a) VALUES (?) ON CONFLICT (a) DO UPDATE SET a = code_x.a + 1`, nil, 1},
		{`DELETE FROM main.code_refs WHERE ref = :ref OR target = :target OR ref = :ref`,
			[]string{"code_refs"}, 2},
		{`SELECT a FROM t, code_y AS y, (SELECT b FROM code_z) AS s`, []string{"code_y", "code_z"}, 0},
		{"SELECT 'FROM code_q', \"a\" FROM code_a -- FROM code_c ?\n/* JOIN code_b ? */ WHERE '?' = ?",
			[]string{"code_a"}, 1},
		{`SELECT a FROM code_x INDEXED BY code_x_idx WHERE a = ?`, []string{"code_x"}, 1},
		{"SELECT a FROM [code_br] JOIN `code_bt` ON 1 JOIN \"code_dq\" USING (a, b), code_after",
			[]string{"code_br", "code_bt", "code_dq", "code_after"}, 0},
		{`SELECT code_count FROM t WHERE code_flag = 1`, nil, 0},
		{`WITH c AS (SELECT a FROM code_w) INSERT INTO code_v SELECT a FROM c`, []string{"code_w"}, 0},
		{`SELECT a FROM code_l LEFT JOIN code_m ON code_m.a = code_l.a, code_n`,
			[]string{"code_l", "code_m", "code_n"}, 0},
		{`SELECT (SELECT 1 FROM code_s), b, c FROM t`, []string{"code_s"}, 0},
		{`REPLACE INTO code_r SELECT * FROM code_u WHERE a IN (?, ?5, ?)`, []string{"code_u"}, 6},
		{`SELECT a FROM code_p UNION SELECT b, c FROM t, code_q2`, []string{"code_p", "code_q2"}, 0},
		{`code_commit_failed`, nil, 0},
	} {
		if got := codeReads(tc.sql); !slices.Equal(got, tc.reads) {
			t.Errorf("codeReads(%q) = %q, want %q", tc.sql, got, tc.reads)
		}
		if got := paramCount(tc.sql); got != tc.params {
			t.Errorf("paramCount(%q) = %d, want %d", tc.sql, got, tc.params)
		}
	}

	root := t.TempDir()
	write := func(rel, body string) {
		file := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/code/code.go", fixtureCode)
	write("internal/code/elsewhere_windows.go", "//go:build windows\n\npackage code\n\n"+
		"const elsewhereSQL = `SELECT id FROM code_refs WHERE repo_id = ?`\n")
	write("internal/code/code_test.go", "package code\n\nimport (\n\t\"context\"\n\t\"database/sql\"\n)\n\n"+
		"func tested(ctx context.Context, tx *sql.Tx, q string) {\n"+
		"\t_, _ = tx.QueryContext(ctx, q+`SELECT 1 FROM code_tested`)\n}\n")
	write("internal/objstore/objstore.go", "package objstore\n\ntype ReferenceTable struct{ Table string }\n")
	write("internal/statelog/statelog.go", "package statelog\n\ntype Scope struct{}\n\n"+
		"type Freshness struct{}\n\n"+
		"func (Freshness) Query(scope Scope, linearizable bool) string { return \"\" }\n")
	walk := walkForReads(t, root, "example.com/m")

	if walk.files != 2 {
		t.Errorf("the walk read %d files, want the package's two non-test ones", walk.files)
	}
	for text, want := range map[string]struct {
		reads, names, users []string
	}{
		`SELECT id FROM code_repos WHERE id = ?`: {[]string{"code_repos"}, []string{"declaredSQL"},
			[]string{"appended", "closure", "direct", "handle.viaField", "opaque"}},
		`SELECT id, name FROM code_repos WHERE name > ?`: {[]string{"code_repos"}, []string{"joinedSQL"},
			[]string{"chosen", "handle.callsMethod"}},
		`SELECT target FROM code_refs INDEXED BY sqlite_autoindex_code_refs_1 WHERE repo_id = ? AND ref = ?`: {
			[]string{"code_refs"}, []string{"indexedSQL"}, []string{"chosen"}},
		`UPDATE OR IGNORE code_repos SET name = ? WHERE id = ?`: {[]string{"code_repos"},
			[]string{"updateSQL"}, []string{"direct"}},
		`DELETE FROM code_names WHERE token = ?`: {[]string{"code_names"}, []string{"deleteSQL"},
			[]string{"ranged"}},
		`INSERT INTO code_names (token, name, repo_id) SELECT name, name, id FROM code_repos WHERE id = ?`: {
			[]string{"code_repos"}, []string{"copySQL"}, []string{"ranged"}},
		`SELECT r.ref FROM json_each(?) AS j, "code_refs" AS r WHERE r.ref = CAST(j.value AS TEXT)`: {
			[]string{"code_refs"}, []string{"commaSQL"}, []string{"callsHelper"}},
		`WITH due AS (SELECT id FROM code_packs WHERE repo_id = ?) SELECT id FROM due`: {
			[]string{"code_packs"}, []string{"cteSQL"}, []string{"chosen"}},
		`SELECT (SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?1), ?2`: {[]string{"code_pushes"},
			[]string{"nestedSQL"}, []string{"chosen"}},
		"SELECT id FROM code_protections -- FROM code_secret\n" +
			"WHERE pattern = 'FROM code_quoted' /* JOIN code_hidden */ AND repo_id = ?": {
			[]string{"code_protections"}, []string{"commentSQL"}, []string{"callsHelper"}},
		`SELECT id FROM code_deletions WHERE `: {[]string{"code_deletions"}, []string{"fragmentSQL"}, nil},
		`SELECT id FROM code_deletions WHERE repo_id IN (%s)`: {[]string{"code_deletions"}, nil,
			[]string{"formatted"}},
		`SELECT name FROM code_ops WHERE id = ?`: {[]string{"code_ops"}, nil, []string{""}},
		`SELECT name FROM code_log_generations WHERE generation = ?`: {[]string{"code_log_generations"},
			nil, []string{"direct"}},
		`SELECT token FROM code_names WHERE repo_id = ?`: {[]string{"code_names"}, nil, []string{"direct"}},
		`SELECT id FROM code_evictions`:                  {[]string{"code_evictions"}, []string{"shadowed"}, nil},
		`SELECT id FROM code_refs WHERE repo_id = ?`:     {[]string{"code_refs"}, []string{"elsewhereSQL"}, nil},
		`SELECT target FROM code_refs WHERE ref = ?`:     {[]string{"code_refs"}, []string{"preparedSQL"}, []string{"prepared"}},
		`UPDATE code_packs SET state = 'retired' WHERE repo_id = ?`: {[]string{"code_packs"},
			[]string{"connSQL"}, []string{"onConn"}},
	} {
		s, ok := walk.spelled[text]
		if !ok {
			t.Errorf("the walk did not find %q, which reads %q", text, want.reads)
			continue
		}
		if users := slices.Sorted(maps.Keys(s.users)); !slices.Equal(s.reads, want.reads) ||
			!slices.Equal(s.names, want.names) || !slices.Equal(users, want.users) {
			t.Errorf("the walk found %q reading %q, declared as %q, used by %q; want %q, %q, %q",
				text, s.reads, s.names, users, want.reads, want.names, want.users)
		}
	}
	if s := walk.spelled[`SELECT id FROM code_deletions WHERE `]; s != nil && !s.folded {
		t.Error("the walk did not see fragmentSQL folded into the text formatted assembles")
	}
	for text := range walk.spelled {
		for _, never := range []string{"code_secret", "code_quoted", "code_hidden", "code_tested",
			"VALUES", "commit_failed", "SELECT 1 FROM"} {
			if strings.Contains(text, never) && !strings.Contains(text, "code_protections") {
				t.Errorf("the walk found %q, which reads no code table", text)
			}
		}
	}
	if len(walk.spelled) != 19 {
		t.Errorf("the walk found %d texts reading a code table, want the fixture's 19: %q",
			len(walk.spelled), slices.Sorted(maps.Keys(walk.spelled)))
	}

	builders := slices.Sorted(maps.Keys(walk.builders))
	if want := []string{"Exported", "appended", "closure", "formatted", "fromPackageVar",
		"handsBuilt", "overListed"}; !slices.Equal(builders, want) {
		t.Errorf("the walk found the builders %q, want %q:\n%v", builders, want, walk.builders)
	}
	for fn, why := range map[string]string{
		"appended":       "is added to (+=)",
		"formatted":      "fmt.Sprintf",
		"fromPackageVar": "a package-level variable",
		"Exported":       "a caller outside the package",
		"closure":        "a parameter of a function literal",
		"handsBuilt":     `"SELECT 1 FROM " + table`,
		"overListed":     "is assembled at run time",
	} {
		if got := strings.Join(walk.builders[fn], "; "); !strings.Contains(got, why) {
			t.Errorf("the walk says %s builds a statement because %q, want it to say %q", fn, got, why)
		}
	}
	for _, want := range []string{"tx.QueryContext is taken as a value", "cannot type mystery()"} {
		if !slices.ContainsFunc(walk.findings, func(f string) bool { return strings.Contains(f, want) }) {
			t.Errorf("no finding says %q; found %q", want, walk.findings)
		}
	}
	if len(walk.findings) != 2 {
		t.Errorf("the walk reported %q, want the fixture's two", walk.findings)
	}
	if !walk.references["Packs"] || len(walk.references) != 1 {
		t.Errorf("the walk found the reference tables %q, want the fixture's Packs",
			slices.Sorted(maps.Keys(walk.references)))
	}
	for _, fn := range []string{"direct", "handle.viaField", "handle.method", "Exported", "helper"} {
		if !walk.declared[fn] {
			t.Errorf("the walk declares no %s; it read %q", fn, slices.Sorted(maps.Keys(walk.declared)))
		}
	}
	// Twenty-one calls of a statement method on a database/sql handle — on
	// the connection DB.Conn's own declaration types, and a statement's
	// Prepare, among them; the untyped one, the method value and the runs of
	// the prepared statement not.
	if walk.sites != 21 {
		t.Errorf("the walk judged %d statement sites, want the fixture's 21", walk.sites)
	}
}

// fixtureCode is the fixture package TestTheReadGateFindsEveryStatement walks.
const fixtureCode = "package code\n\n" + `import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"example.com/m/internal/objstore"
	"example.com/m/internal/statelog"
)

const (
	declaredSQL = ` + "`SELECT id FROM code_repos WHERE id = ?`" + `
	columns     = ` + "`id, name`" + `
	joinedSQL   = ` + "`SELECT ` + columns + ` FROM code_repos WHERE name > ?`" + `
	refsIdx     = "sqlite_autoindex_code_refs_1"
	indexedSQL  = ` + "`SELECT target FROM code_refs INDEXED BY ` + refsIdx + ` WHERE repo_id = ? AND ref = ?`" + `
	aliasSQL    = declaredSQL
	updateSQL   = ` + "`UPDATE OR IGNORE code_repos SET name = ? WHERE id = ?`" + `
	deleteSQL   = ` + "`DELETE FROM code_names WHERE token = ?`" + `
	copySQL     = ` + "`INSERT INTO code_names (token, name, repo_id) SELECT name, name, id FROM code_repos WHERE id = ?`" + `
	commaSQL    = ` + "`SELECT r.ref FROM json_each(?) AS j, \"code_refs\" AS r WHERE r.ref = CAST(j.value AS TEXT)`" + `
	cteSQL      = ` + "`WITH due AS (SELECT id FROM code_packs WHERE repo_id = ?) SELECT id FROM due`" + `
	nestedSQL   = ` + "`SELECT (SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?1), ?2`" + `
	commentSQL  = "SELECT id FROM code_protections -- FROM code_secret\n" +
		"WHERE pattern = 'FROM code_quoted' /* JOIN code_hidden */ AND repo_id = ?"
	insertSQL = ` + "`INSERT INTO code_names (token, name, repo_id) VALUES (?, ?, ?)`" + `
	upsertSQL = ` + "`INSERT INTO code_activation (id, fence) VALUES (?, ?) ON CONFLICT (id) DO UPDATE SET fence = MAX(code_activation.fence, excluded.fence)`" + `
	logKey      = "code_commit_failed"
	fragmentSQL = ` + "`SELECT id FROM code_deletions WHERE `" + `
	shadowed    = ` + "`SELECT id FROM code_evictions`" + `
	preparedSQL = ` + "`SELECT target FROM code_refs WHERE ref = ?`" + `
	connSQL     = ` + "`UPDATE code_packs SET state = 'retired' WHERE repo_id = ?`" + `
)

var listed = []string{"code_repos", "code_names"}

var shared = ` + "`SELECT name FROM code_ops WHERE id = ?`" + `

var Packs = objstore.ReferenceTable{Table: "code_packs"}

func direct(ctx context.Context, tx *sql.Tx) {
	_, _ = tx.ExecContext(ctx, updateSQL, 1, 2)
	_ = tx.QueryRowContext(ctx, ` + "`SELECT name FROM code_log_generations WHERE generation = ?`" + `, 1)
	_, _ = tx.Query(` + "`SELECT token FROM code_names WHERE repo_id = ?`" + `, 1)
	_, _ = tx.QueryContext(ctx, aliasSQL, 1)
	_, _ = tx.ExecContext(ctx, insertSQL, 1, 2, 3)
	_, _ = tx.ExecContext(ctx, upsertSQL, 1, 2)
}

func ranged(ctx context.Context, tx *sql.Tx) {
	for _, stmt := range []string{deleteSQL, copySQL} {
		_, _ = tx.ExecContext(ctx, stmt, 1)
	}
}

func chosen(ctx context.Context, tx *sql.Tx, first bool) {
	q := joinedSQL
	if !first {
		q = indexedSQL
	}
	var r string
	switch {
	case first:
		r = cteSQL
	default:
		r = nestedSQL
	}
	_, _ = tx.QueryContext(ctx, q, 1)
	_, _ = tx.QueryContext(ctx, r, 1)
}

func helper(ctx context.Context, tx *sql.Tx, query string) { _, _ = tx.QueryContext(ctx, query) }

func callsHelper(ctx context.Context, tx *sql.Tx) {
	helper(ctx, tx, commaSQL)
	helper(ctx, tx, commentSQL)
}

func handsBuilt(ctx context.Context, tx *sql.Tx, table string) { helper(ctx, tx, "SELECT 1 FROM "+table) }

func appended(ctx context.Context, tx *sql.Tx, state string) {
	q := declaredSQL
	if state != "" {
		q += " AND state = ?"
	}
	_, _ = tx.QueryContext(ctx, q, 1)
}

func formatted(ctx context.Context, db *sql.DB, in string) {
	_, _ = db.QueryContext(ctx, fmt.Sprintf(fragmentSQL+"repo_id IN (%s)", in))
}

func Exported(ctx context.Context, tx *sql.Tx, query string) { _, _ = tx.QueryContext(ctx, query) }

func fromPackageVar(ctx context.Context, tx *sql.Tx) { _, _ = tx.QueryContext(ctx, shared, 1) }

func closure(ctx context.Context, tx *sql.Tx) {
	run := func(query string) { _, _ = tx.QueryContext(ctx, query) }
	run(declaredSQL)
}

func overListed(ctx context.Context, tx *sql.Tx) {
	for _, table := range listed {
		_, _ = tx.QueryContext(ctx, "SELECT 1 FROM "+table)
	}
}

type handle struct{ tx *sql.Tx }

func (h handle) viaField(ctx context.Context) { _, _ = h.tx.QueryContext(ctx, declaredSQL, 1) }

func (h handle) method(ctx context.Context, query string) { _, _ = h.tx.QueryContext(ctx, query) }

func (h handle) callsMethod(ctx context.Context) { h.method(ctx, joinedSQL) }

func notAStatement(f statelog.Freshness) { _ = f.Query(statelog.Scope{}, false) }

func methodValue(tx *sql.Tx) func(context.Context, string, ...any) (*sql.Rows, error) {
	return tx.QueryContext
}

func opaque(ctx context.Context) { _, _ = mystery().QueryContext(ctx, declaredSQL) }

func localShadow(ctx context.Context, tx *sql.Tx) {
	shadowed := "SELECT 1"
	_, _ = tx.QueryContext(ctx, shadowed)
}

func prepared(ctx context.Context, tx *sql.Tx, refs []string) {
	stmt, _ := tx.PrepareContext(ctx, preparedSQL)
	for _, ref := range refs {
		key := strings.ToLower(ref)
		_, _ = stmt.ExecContext(ctx, key)
		_ = stmt.QueryRow(key)
	}
}

func onConn(ctx context.Context, db *sql.DB) {
	conn, _ := db.Conn(ctx)
	_, _ = conn.ExecContext(ctx, connSQL, 1)
}
`
