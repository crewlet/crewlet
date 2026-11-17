package search_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// EVERY STATEMENT THE CHANGE INDEX ISSUES IS PLANNED ONTO THE INDEX IT NAMES, and every index of its
// tables serves one; a search reads newest first with no sorter, each other word one seek of its slots.
// Mutation: drop INDEXED BY from a scope's statement, drop slot from the term index, or read ascending.
func TestEveryChangeIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	// A CORPUS WITH A REAL CHOICE IN IT: forty repositories, two owners, four states, and a word in
	// a small share of the changes.
	for r := range 40 {
		f.addRepo(fmt.Sprintf("r%03d", r))
	}
	for i := range 2_000 {
		title := "the standup notes from today"
		if i%25 == 0 {
			title = "the migration plan moved"
		}
		f.write(changeFixtureRow{id: changeUUID(i + 1), repo: fmt.Sprintf("r%03d", i%40),
			owner: []string{"ada", "bo"}[i%2], state: []string{"draft", "open", "closed", "merged"}[i%4],
			title: title}, int64(i+1))
	}
	f.settle(2_000)
	f.sweepAll()

	type registration struct {
		statement string
		args      []any
		// reaches is every index the plan must name.
		reaches []string
		// walks is the one table this statement may SCAN, by design.
		walks string
		// held is a search's candidate read: it drives on its first step, sorts nothing and is not
		// decorrelated.
		held bool
	}
	const (
		docsKey     = "change_docs_id_idx (id=?)"
		postingsKey = "change_postings_term_idx"
		rowKey      = "INTEGER PRIMARY KEY"
	)
	parsed := uuid.MustParse(changeUUID(1))
	key := parsed[:]
	scopeValue := map[string]any{"repo_id": "r001", "owner": "ada", "state": "open"}
	registered := map[string]registration{
		"the index's state":   {statement: search.ChangeStateStatement, reaches: []string{rowKey}},
		"the walk's advance":  {statement: search.ChangeAdvanceStatement, args: []any{1, 2, 3, 4}, reaches: []string{rowKey}},
		"a purge drop's seen": {statement: search.ChangeSeenStatement, args: []any{1, 2}, reaches: []string{rowKey}},
		"a rebuild's mark":    {statement: search.ChangeBeginRebuildStatement, args: []any{2}, reaches: []string{rowKey}},
		"the copy a fresh index records": {statement: search.ChangeRecordInstanceStatement,
			args: []any{"instance", 2}, reaches: []string{rowKey}},
		"a rebuild's end": {statement: search.ChangeRebuiltStatement, args: []any{2}, reaches: []string{rowKey}},
		"what the index holds for a change": {statement: search.ChangeHeldDocStatement, args: []any{key},
			reaches: []string{docsKey}},
		"a document's write": {statement: search.ChangeDocInsertStatement,
			args: []any{1, key, "r001", "ada", "open", key}},
		"a document's postings": {statement: search.ChangePostingsInsert + " (?, ?)", args: []any{1 << 16, "x"}},
		"a document's rewrite": {statement: search.ChangeDocRewriteStatement,
			args: []any{"open", key, 1}, reaches: []string{rowKey}},
		"a document's state": {statement: search.ChangeDocStateStatement, args: []any{"open", 1},
			reaches: []string{rowKey}},
		"a document's removal": {statement: search.ChangeRemoveDocStatement, args: []any{1},
			reaches: []string{rowKey}},
		"a document's postings cleared": {statement: search.ChangeClearPostingsStatement,
			args: []any{1 << 16, 2 << 16}, reaches: []string{rowKey}},
		"the purge rotation": {statement: search.ChangeNextRepoStatement, args: []any{"r001"},
			reaches: []string{"change_docs_repo_idx (repo_id>?)"}},
		"a purged repository's documents": {statement: search.ChangeRepoDocsStatement,
			args: []any{"r001", search.ChangeDropBatch}, reaches: []string{"change_docs_repo_idx (repo_id=?)"}},
		"a rebuild's next batch": {statement: search.ChangeDropBatchStatement, args: []any{search.ChangeDropBatch},
			walks: "change_docs"},
		"a rebuild's drop of what is left": {statement: search.ChangeRebuiltDropStatement,
			walks: "change_postings"},
		"a word's count": {statement: search.ChangeTermCountStatement, args: []any{"migration", 5_001},
			reaches: []string{postingsKey + " (term=?)"}},
		"a word's floor": {statement: search.ChangeTermFloorStatement, args: []any{"migration", 4_999},
			reaches: []string{postingsKey + " (term=?)"}},
	}
	for _, scope := range search.ChangeScopes {
		index := "change_docs_" + strings.TrimSuffix(scope, "_id") + "_idx (" + scope + "=?)"
		registered[scope+"'s count"] = registration{statement: search.ChangeScopeCountStatement(scope),
			args: []any{scopeValue[scope], 5_001}, reaches: []string{index}}
		registered[scope+"'s floor"] = registration{statement: search.ChangeScopeFloorStatement(scope),
			args: []any{scopeValue[scope], 4_999}, reaches: []string{index}}
	}
	// THE CANDIDATE READS, driven on a word at every scope with the fewest and the most other words,
	// and with one scope at every count of them; driven on each scope at every residual with one word
	// and eight, and with one residual at every count of words; each floored and not.
	scopes := func(repo, state, owner bool) []any {
		var out []any
		if repo {
			out = append(out, "r001")
		}
		if state {
			out = append(out, "open")
		}
		if owner {
			out = append(out, "ada")
		}
		return out
	}
	words := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = []string{"the", "plan", "moved", "notes", "today", "from", "standup", "migration"}[i]
		}
		return out
	}
	for _, floored := range []bool{false, true} {
		floor := []any{}
		if floored {
			floor = []any{100}
		}
		for mask := range 8 {
			repo, state, owner := mask&1 != 0, mask&2 != 0, mask&4 != 0
			for _, others := range []int{0, 7} {
				args := append(append(append([]any{"migration"}, floor...), scopes(repo, state, owner)...),
					words(others)...)
				registered[fmt.Sprintf("held by a word, scope %d, %d more, floored %v", mask, others, floored)] =
					registration{statement: search.ChangeHeldByTermStatement(others, repo, state, owner, floored),
						args: append(args, 1_001), reaches: []string{postingsKey + " (term=?"}, held: true}
			}
		}
		for others := range 8 {
			args := append(append(append([]any{"migration"}, floor...), scopes(true, false, false)...),
				words(others)...)
			registered[fmt.Sprintf("held by a word in a repository, %d more, floored %v", others, floored)] =
				registration{statement: search.ChangeHeldByTermStatement(others, true, false, false, floored),
					args: append(args, 1_001), reaches: []string{postingsKey + " (term=?"}, held: true}
		}
		for _, scope := range search.ChangeScopes {
			index := "change_docs_" + strings.TrimSuffix(scope, "_id") + "_idx (" + scope + "="
			for mask := range 8 {
				repo, state, owner := mask&1 != 0, mask&2 != 0, mask&4 != 0
				for _, n := range []int{1, 8} {
					registered[fmt.Sprintf("held by %s, residual %d, %d words, floored %v", scope, mask, n,
						floored)] = registration{
						statement: search.ChangeHeldByScopeStatement(scope, n, repo, state, owner, floored),
						args:      heldByScopeArgs(scope, scopeValue[scope], floor, repo, state, owner, words(n)),
						reaches:   []string{index}, held: true}
				}
			}
			for n := 1; n <= 8; n++ {
				registered[fmt.Sprintf("held by %s, %d words, floored %v", scope, n, floored)] = registration{
					statement: search.ChangeHeldByScopeStatement(scope, n, false, false, false, floored),
					args:      heldByScopeArgs(scope, scopeValue[scope], floor, false, false, false, words(n)),
					reaches:   []string{index}, held: true}
			}
		}
	}

	plans := map[string][]string{}
	for name, r := range registered {
		plans[name] = storetest.Plan(t, f.db, r.statement, r.args)
	}
	for name, r := range registered {
		steps := plans[name]
		joined := strings.Join(steps, "\n")
		for _, step := range steps {
			if !strings.HasPrefix(step, "SCAN ") || strings.HasPrefix(step, "SCAN (") ||
				strings.HasPrefix(step, "SCAN json_each") {
				continue
			}
			if r.walks == "" || !strings.HasPrefix(step, "SCAN "+r.walks) {
				t.Errorf("%s walks a table:\n%s", name, joined)
			}
		}
		for _, index := range r.reaches {
			if !strings.Contains(joined, index) {
				t.Errorf("%s does not reach %s:\n%s", name, index, joined)
			}
		}
		if !r.held {
			continue
		}
		// DRIVEN ON WHAT IT WAS HANDED, newest first, every other word by the postings key.
		if len(steps) == 0 || !strings.Contains(steps[0], r.reaches[0]) {
			t.Errorf("%s does not drive on %s:\n%s", name, r.reaches[0], joined)
		}
		if strings.Contains(joined, "SORTER") || strings.Contains(joined, "TEMP B-TREE") {
			t.Errorf("%s sorts what it reads before its limit:\n%s", name, joined)
		}
		if strings.Contains(r.statement, "CROSS JOIN change_docs") && !documentByNumber.MatchString(joined) {
			t.Errorf("%s reaches a word's document other than by its number:\n%s", name, joined)
		}
		if n := strings.Count(r.statement, "EXISTS"); strings.Count(joined, termInSlotRange) < n {
			t.Errorf("%s reaches another word other than by one seek of the term key over the document's "+
				"slot range:\n%s", name, joined)
		}
		if line, unnested := storetest.Decorrelated(steps); unnested {
			t.Errorf("%s answers a word's test from the whole postings table (%q):\n%s", name, line, joined)
		}
	}

	// THE INVERSE: an index of the change index's tables nothing reaches is a write cost nobody reads.
	all := ""
	for _, steps := range plans {
		all += strings.Join(steps, "\n") + "\n"
	}
	for _, index := range storetest.IndexesOn(t, f.db, []string{"change_docs", "change_postings",
		"change_index_state"}) {
		if !strings.Contains(all, index) {
			t.Errorf("index %s serves no registered change index statement", index)
		}
	}

	// THE COMPLETENESS WALK: every string in the package's change*.go naming one of the index's tables
	// or a code_ table is part of a statement planned above or of a read of Gitlet's tables.
	var texts []string
	for _, r := range registered {
		texts = append(texts, normalSQL(r.statement))
	}
	for _, read := range []func() (string, []any){
		func() (string, []any) { return search.ChangeReachRead(0, 1) },
		func() (string, []any) { return search.ChangeBatchRead(0, 1) },
		func() (string, []any) { return search.ChangeReposRead([]string{"r001"}) },
	} {
		text, _ := read()
		texts = append(texts, normalSQL(text))
	}
	for _, literal := range changeSourceLiterals(t) {
		found := false
		for _, text := range texts {
			if strings.Contains(text, normalSQL(literal)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the change index issues %q, which is part of no statement this gate plans or "+
				"Gitlet's gate plans: name it and register it", literal)
		}
	}
}

// documentByNumber is a plan step reaching a document by its number: the table's own key, or a scope
// key's entry ending in it.
var documentByNumber = regexp.MustCompile(`INTEGER PRIMARY KEY \(rowid=\?\)|change_docs_\w+_idx \(\w+=\? AND seq=\?\)`)

// termInSlotRange is a plan step testing one word on one document by one seek of the term key.
const termInSlotRange = "change_postings_term_idx (term=? AND slot>=? AND slot<?)"

// heldByScopeArgs is what a candidate read driven on a scope binds.
func heldByScopeArgs(scope string, value any, floor []any, repo, state, owner bool, words []any) []any {
	args := append([]any{value}, floor...)
	for _, on := range []struct {
		named bool
		key   string
		value any
	}{{repo, "repo_id", "r001"}, {state, "state", "open"}, {owner, "owner", "ada"}} {
		if on.named && on.key != scope {
			args = append(args, on.value)
		}
	}
	return append(append(args, words...), 1_001)
}

// changeTableRead is a string naming one of the change index's tables or a Gitlet table after the words
// that read or write one.
var changeTableRead = regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE)\s+(change_|code_)`)

// changeSourceLiterals is every string literal in the change index's own source that names one.
func changeSourceLiterals(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("change*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	fset := token.NewFileSet()
	read := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		read++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, unquoteErr := strconv.Unquote(lit.Value)
			if unquoteErr != nil {
				t.Fatalf("%s: unquote %s: %v", fset.Position(lit.Pos()), lit.Value, unquoteErr)
			}
			if changeTableRead.MatchString(text) {
				out = append(out, text)
			}
			return true
		})
	}
	if read < 2 || len(out) == 0 {
		t.Fatalf("the walk read %d files and found %d statements, so it is looking somewhere other "+
			"than the change index's source", read, len(out))
	}
	return out
}
