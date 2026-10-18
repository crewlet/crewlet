package search_test

import (
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// EVERY STATEMENT THE CHAT INDEX ISSUES IS PLANNED ONTO THE INDEX IT NAMES, AND
// EVERY INDEX THE CHAT INDEX'S TABLES CARRY SERVES ONE OF THEM.
//
// The lexical and vector gates beside this one exist because an index mistake
// looks exactly like a mistake nobody made: the answers stay correct and only
// the time moves. This index is the one whose corpus grows without bound — a
// year at the declared census is millions of messages — so a statement that
// walks a table it meant to seek is a cost that grows with the company's whole
// history, on every sweep or every search.
//
// # What it holds
//
//   - The statements are the PRODUCTION ones, exported from the package, and
//     a walk of the package's own source refuses a string that names a chat
//     table after FROM, JOIN or UPDATE and is not part of one of them — so a
//     statement added inline is a statement this gate refuses rather than one
//     it never planned.
//   - Each declares the index it reaches; a plan that does not reach it fails.
//   - No plan SCANS a chat table, except the walks declared here: a rebuild's
//     next batch of documents and its drop of the statistics left at its end,
//     and the corpus total over the rooms table, which holds a row per room
//     rather than per message.
//   - The posting scan DRIVES ON THE TERM and reaches its document by key,
//     walking the list in the order it is stored, so neither a bucket, a room
//     list nor a speaker turns it into a walk of the documents, and no sorter
//     stands between the list and the budget.
//   - The inverse: every index on the chat index's own tables appears in some
//     plan, and so do the two transcript indexes the replicated schema made for
//     this index's walks.
func TestEveryChatIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	x := search.NewChatIndexer(db, db.Replicated().Reader())

	// A CORPUS WITH A REAL CHOICE IN IT: forty rooms, a vocabulary in which
	// the queried term is in a small share of the messages, two speakers,
	// and erasures, so a planner that drove on the documents rather than the
	// term would be choosing the larger side.
	var rows []chatRow
	for i := range 2_000 {
		body := "the standup notes from today"
		if i%25 == 0 {
			body = "the migration plan moved"
		}
		author := "ada"
		if i%2 == 0 {
			author = "bo"
		}
		rows = append(rows, chatRow{id: fmt.Sprintf("m%05d", i),
			room: fmt.Sprintf("r%03d", i%40), author: author, body: body,
			createdAt: int64(1000 + i), version: int64(100 + i)})
	}
	writeChatRows(t, db, rows...)
	sweepAll(ctx, t, x)
	eraseMessages(t, db, 9000, "m00007", "m00011")
	sweepAll(ctx, t, x)

	type registration struct {
		statement  string
		args       []any
		replicated bool
		// reaches is every index the plan must name.
		reaches []string
		// walks is the one table this statement may SCAN, by design.
		walks string
	}
	const (
		docsKey     = "sqlite_autoindex_chat_docs_1"
		postingsKey = "sqlite_autoindex_chat_postings_1"
		roomsKey    = "sqlite_autoindex_chat_index_rooms_1"
		termsKey    = "sqlite_autoindex_chat_index_terms_1"
		stateKey    = "INTEGER PRIMARY KEY"
		messagesKey = "sqlite_autoindex_chat_messages_1"
	)
	rooms := []any{"r001", "r002", "r003"}
	registered := map[string]registration{
		"the transcript batch's reach": {statement: search.ChatChangedReachStatement,
			args: []any{0, search.ChatIndexBatch}, replicated: true,
			reaches: []string{"chat_messages_watermark_idx"}},
		"the transcript batch": {statement: search.ChatChangedRowsStatement,
			args: []any{0, 500}, replicated: true,
			reaches: []string{"chat_messages_watermark_idx"}},
		"the erasures' reach": {statement: search.ChatErasedReachStatement,
			args: []any{0, search.ChatIndexBatch}, replicated: true,
			reaches: []string{"chat_deletions_version_idx"}},
		"the erasures": {statement: search.ChatErasedRowsStatement,
			args: []any{0, 9000}, replicated: true,
			reaches: []string{"chat_deletions_version_idx"}},
		"a room's oldest message": {statement: search.ChatLiveOldestStatement,
			args: []any{"r001"}, replicated: true,
			reaches: []string{"chat_messages_prune_idx"}},
		"the hit hydration": {statement: search.ChatHydrateStatement(2),
			args: []any{"m00001", "m00002"}, replicated: true,
			reaches: []string{messagesKey}},

		"a room's oldest document": {statement: search.ChatHeldOldestStatement,
			args: []any{"r001"}, reaches: []string{"chat_docs_prune_idx"}},
		"the prune rotation": {statement: search.ChatRoomRotationStatement,
			args: []any{"", search.ChatPruneFollowChannels}, reaches: []string{roomsKey}},
		"what the index holds for a message": {statement: search.ChatHeldDocStatement,
			args: []any{"m00001"}, reaches: []string{docsKey}},
		"a document's terms": {statement: search.ChatDocTermsStatement,
			args: []any{"m00001"}, reaches: []string{"chat_postings_doc_idx"}},
		"a re-index's postings clear": {statement: search.ChatClearPostingsStatement,
			args: []any{"m00001"}, reaches: []string{"chat_postings_doc_idx"}},
		"a document's removal": {statement: search.ChatRemoveDocStatement,
			args: []any{"m00001"}, reaches: []string{docsKey}},
		"a document's write": {statement: search.ChatDocUpsertStatement,
			args: []any{"m00001", "r001", "ada", 1, 5, 1001}},
		"a prune's size": {statement: search.ChatRangeSizeStatement,
			args: []any{"r001", 1500}, reaches: []string{"chat_docs_prune_idx"}},
		"a prune's terms": {statement: search.ChatRangeTermsStatement,
			args:    []any{"r001", 1500},
			reaches: []string{"chat_docs_prune_idx", "chat_postings_doc_idx"}},
		"a prune's drop": {statement: search.ChatRangeDropStatement,
			args: []any{"r001", 1500}, reaches: []string{"chat_docs_prune_idx"}},
		"the searched rooms' statistics": {statement: search.ChatRoomsCorpusStatement(3),
			args: rooms, reaches: []string{roomsKey}},
		"a term's document count": {statement: search.ChatTermDocsStatement(3),
			args: append([]any{"migration"}, rooms...), reaches: []string{termsKey}},
		"the posting scan": {statement: search.ChatPostingScanStatement(3, false),
			args:    append(append([]any{"migration"}, rooms...), search.MaxPostingScan),
			reaches: []string{postingsKey, docsKey}},
		"the posting scan narrowed to a speaker": {
			statement: search.ChatPostingScanStatement(3, true),
			args: append(append([]any{"migration"}, rooms...), "ada",
				search.MaxPostingScan),
			reaches: []string{postingsKey, docsKey}},
		"an emptied room's removal": {statement: search.ChatEmptiedRoomsStatement(2),
			args: []any{"r001", "r002"}, reaches: []string{roomsKey}},
		"an emptied term's removal": {statement: search.ChatEmptiedTermsStatement(2),
			args: []any{"r001", "migration", "standup"}, reaches: []string{termsKey}},
		"the transcript watermark": {statement: search.ChatWatermarkStatement,
			reaches: []string{stateKey}},
		"the erasure watermark": {statement: search.ChatErasedThroughStatement,
			reaches: []string{stateKey}},
		"the transcript watermark's advance": {
			statement: search.ChatAdvanceWatermarkStatement, args: []any{1, 2},
			reaches: []string{stateKey}},
		"the erasure watermark's advance": {
			statement: search.ChatAdvanceErasedStatement, args: []any{1, 2},
			reaches: []string{stateKey}},
		"the index's state": {statement: search.ChatStateStatement,
			reaches: []string{stateKey}},
		"a rebuild's mark": {statement: search.ChatBeginRebuildStatement,
			args: []any{2}, reaches: []string{stateKey}},
		"the copy a fresh index records": {statement: search.ChatRecordInstanceStatement,
			args: []any{"instance", 2}, reaches: []string{stateKey}},
		"a rebuild's next batch": {statement: search.ChatDropBatchStatement,
			args: []any{search.ChatIndexBatch}, walks: "chat_docs"},
		"a rebuild's end": {statement: search.ChatRebuiltStatement,
			args: []any{2}, reaches: []string{stateKey}},
		"the corpus total": {statement: search.ChatCorpusStatement,
			walks: "chat_index_rooms"},
	}
	for _, drop := range search.ChatRebuiltDrops {
		table := strings.TrimPrefix(drop, "DELETE FROM ")
		registered["a rebuild's drop of "+table] = registration{statement: drop, walks: table}
	}

	// THE PLANS, the node estate's in a WRITE transaction because several of
	// its statements are the index's own writes, which a read transaction
	// refuses even to plan.
	plans := map[string][]string{}
	plan := func(tx *sql.Tx, replicated bool) error {
		for name, r := range registered {
			if r.replicated != replicated {
				continue
			}
			steps, err := explain(t, tx, r.statement, r.args...)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			plans[name] = steps
		}
		return nil
	}
	if err := db.Tx(ctx, func(tx *sql.Tx) error { return plan(tx, false) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Replicated().Reader().Read(ctx, func(tx *sql.Tx) error {
		return plan(tx, true)
	}); err != nil {
		t.Fatal(err)
	}

	for name, r := range registered {
		joined := strings.Join(plans[name], "\n")
		for _, step := range plans[name] {
			// A TABLE, never a subquery's own result: a batch's reach
			// reads its at most ChatIndexBatch positions through one.
			if !strings.HasPrefix(step, "SCAN ") || strings.HasPrefix(step, "SCAN (") {
				continue
			}
			if r.walks == "" || !strings.HasPrefix(step, "SCAN "+r.walks) {
				t.Errorf("%s walks a table:\n%s\n\nOn a corpus that grows with "+
					"the company's history that is a cost on every sweep or "+
					"search; seek it on the index the statement was written for",
					name, joined)
			}
		}
		for _, index := range r.reaches {
			if !strings.Contains(joined, index) {
				t.Errorf("%s does not reach %s:\n%s", name, index, joined)
			}
		}
	}

	// THE POSTING SCAN DRIVES ON THE TERM, in the stored order. A room list,
	// a speaker and a bucket are each a predicate a planner could seek
	// chat_docs by instead, which walks every document they select and
	// probes the postings per document — far more rows than one term's
	// list holds — and a sorter between the list and the budget would read
	// the whole list to keep five thousand of it.
	for _, name := range []string{"the posting scan", "the posting scan narrowed to a speaker"} {
		steps := plans[name]
		joined := strings.Join(steps, "\n")
		if len(steps) == 0 || !strings.Contains(steps[0], "chat_postings") {
			t.Errorf("%s does not drive on the posting list:\n%s", name, joined)
		}
		if !strings.Contains(joined, "id=?") {
			t.Errorf("%s reaches chat_docs by something other than its key:\n%s",
				name, joined)
		}
		// Turso says SORTER where SQLite says TEMP B-TREE; either is the
		// whole list read and ordered before five thousand of it is kept.
		if strings.Contains(joined, "SORTER") || strings.Contains(joined, "TEMP B-TREE") {
			t.Errorf("%s sorts the posting list before its budget:\n%s", name, joined)
		}
	}

	// THE INVERSE: an index nothing registered reaches is a copy of the row
	// order nobody reads, and one a planner can still choose.
	var declared []string
	if err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT name FROM sqlite_master
			 WHERE type = 'index' AND name NOT LIKE 'sqlite_autoindex%'
			   AND tbl_name IN ('chat_docs', 'chat_postings', 'chat_index_rooms',
			                    'chat_index_terms', 'chat_index_state')`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			declared = append(declared, name)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(declared) == 0 {
		t.Fatal("no indexes were read back, so this half of the check cannot " +
			"fail and is not asserting anything")
	}
	// The transcript's two indexes the replicated schema declares for this
	// index's walks are held here too: no other reader in the tree reaches
	// them, so nothing else would notice them go unread.
	declared = append(declared, "chat_messages_watermark_idx", "chat_deletions_version_idx")
	all := ""
	for _, steps := range plans {
		all += strings.Join(steps, "\n") + "\n"
	}
	for _, index := range declared {
		if !strings.Contains(all, index) {
			t.Errorf("index %s serves no registered chat index statement:\n%s", index, all)
		}
	}

	// THE COMPLETENESS WALK: every string in the chat index's own source
	// that names a chat table after FROM, JOIN or UPDATE is part of a
	// statement planned above.
	var texts []string
	for _, r := range registered {
		texts = append(texts, normalSQL(r.statement))
	}
	for _, literal := range chatSourceLiterals(t) {
		found := false
		for _, text := range texts {
			if strings.Contains(text, normalSQL(literal)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the chat index issues %q, which is part of no statement "+
				"this gate plans — name it and register it here", literal)
		}
	}
}

// chatTableRead is a string naming a table after the words that read or
// rewrite one. An INSERT is not among them: what it writes has no plan to get
// wrong beyond the key its conflict clause names.
var chatTableRead = regexp.MustCompile(`(?i)\b(FROM|JOIN|UPDATE)\s+chat_`)

// chatSourceLiterals is every string literal in the chat index's own source
// that names a chat table after FROM, JOIN or UPDATE.
func chatSourceLiterals(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("chat*.go")
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
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: unquote %s: %v", fset.Position(lit.Pos()), lit.Value, err)
			}
			if chatTableRead.MatchString(text) {
				out = append(out, text)
			}
			return true
		})
	}
	if read < 2 || len(out) == 0 {
		t.Fatalf("the walk read %d files and found %d statements, so it is "+
			"looking somewhere other than the chat index's source", read, len(out))
	}
	return out
}

// normalSQL collapses a statement's whitespace, so a fragment and the
// statement built from it compare by their words.
func normalSQL(statement string) string { return strings.Join(strings.Fields(statement), " ") }
