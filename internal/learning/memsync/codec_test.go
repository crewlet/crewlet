package memsync

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "m.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

var seat = seatRef{Handle: "eng", AgentID: "11111111-2222-3333-4444-555555555555"}

// seedMemory writes one row into every memory table, so a round trip covers
// the whole registry.
func seedMemory(t *testing.T, db *store.DB) {
	t.Helper()
	at := time.Now().UTC().Add(-time.Hour).UnixMicro()
	// An embedding, so the blob path is exercised.
	embedding := []byte{0x00, 0x01, 0xfe, 0xff, 0x7f}
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := db.SQL().ExecContext(t.Context(), statement, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, statement)
		}
	}
	exec(`INSERT INTO agent_diary (id, agent_id, kind, content, source, turn_id,
		metadata, retrieval_count, embedding, embedding_model, created_at)
		VALUES (?, ?, 'diary_long', 'the release train is thursdays', 'reflect',
		't1', '{}', 3, ?, 'fixture-model', ?)`, "d1", seat.AgentID, embedding, at)
	exec(`INSERT INTO episodes (id, agent_handle, agent_role, turn_id, started_at,
		ended_at, plan_summary, task_summary, tool_sequence, review_outcome,
		duration_ms, embedding, embedding_model, kind)
		VALUES (?, ?, 'Engineer', 't1', ?, ?, 'plan', 'task', '["slack_post"]',
		'done', 1200, ?, 'fixture-model', 'raw')`, "e1", seat.Handle, at, at, embedding)
	exec(`INSERT INTO counterparty_profiles (observer_handle, subject_handle,
		subject_external_id, subject_platform, subject_name, traits,
		first_seen_at, last_updated_at, last_corroborated_at, interaction_count)
		VALUES (?, 'pm', 'U123', 'slack', 'Pat', '{"prefers":"digests"}', ?, ?, ?, 4)`,
		seat.Handle, at, at, at)
	exec(`INSERT INTO synthesized_skills (id, agent_handle, name, description,
		content, frontmatter, tool_sequence, source_episode_ids, version,
		created_at, updated_at, state)
		VALUES (?, ?, 'ship-it', 'how to ship', 'body', '{}', '[]', '[]', 1, ?, ?, 'active')`,
		"s1", seat.Handle, at, at)
	exec(`INSERT INTO synthesized_skill_versions (id, skill_id, agent_handle, name,
		description, content, frontmatter, tool_sequence, source_episode_ids,
		version, refinement_kind, archived_at)
		VALUES (?, ?, ?, 'ship-it', 'how to ship', 'body', '{}', '[]', '[]', 1, 'seed', 0)`,
		"v1", "s1", seat.Handle)
	exec(`INSERT INTO agent_onboarding_markers (agent_id, chain_hash, agent_handle,
		role, summary, created_at, updated_at)
		VALUES (?, 'hash', ?, 'Engineer', 'read the docs', ?, ?)`,
		seat.AgentID, seat.Handle, at, at)
}

func countRows(t *testing.T, db *store.DB, tableName string) int {
	t.Helper()
	var n int
	if err := db.SQL().QueryRowContext(t.Context(),
		"SELECT count(*) FROM "+tableName).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", tableName, err)
	}
	return n
}

// exportAll reads every row of the seat's a turn would carry from spec above
// after, as a turn reads them, and returns the mark it would leave (after
// itself for a whole-each-cycle table).
func exportAll(t *testing.T, db *store.DB, spec table, after int64) ([]Row, int64) {
	t.Helper()
	if spec.wholeEachCycle {
		rows, err := exportWhole(t.Context(), db.SQL(), spec, seat)
		if err != nil {
			t.Fatalf("export %s: %v", spec.name, err)
		}
		return rows, after
	}
	var rows []Row
	for {
		page, more, err := exportPage(t.Context(), db.SQL(), spec, seat, after)
		if err != nil {
			t.Fatalf("export %s: %v", spec.name, err)
		}
		for _, read := range page {
			rows = append(rows, read.row)
			after = read.mark
		}
		if !more {
			return rows, after
		}
	}
}

// carry moves every one of a seat's rows from one store to another as a
// handoff would, through the wire encoding.
func carry(t *testing.T, from, to *store.DB) int {
	t.Helper()
	ctx := context.Background()
	carried := 0
	for _, spec := range tables {
		rows, _ := exportAll(t, from, spec, 0)
		for _, row := range rows {
			body, err := encode(row)
			if err != nil {
				t.Fatalf("encode %s: %v", spec.name, err)
			}
			decoded, err := decode(body)
			if err != nil || decoded.Table != spec.name {
				t.Fatalf("decode %s: a %q row (%v)", spec.name, decoded.Table, err)
			}
			if err := to.Tx(ctx, func(tx *sql.Tx) error {
				return upsert(ctx, tx, spec, decoded)
			}); err != nil {
				t.Fatalf("upsert %s: %v", spec.name, err)
			}
			carried++
		}
	}
	return carried
}

// A seat's memory survives moving to a node that has never run it (package
// doc, "The bug this exists for").
func TestASeatsMemoryCrossesToANodeThatHasNeverRunIt(t *testing.T) {
	t.Parallel()
	oldOwner, newOwner := openStore(t), openStore(t)
	seedMemory(t, oldOwner)

	if carried := carry(t, oldOwner, newOwner); carried != len(tables) {
		t.Fatalf("carried %d rows, want one per table (%d)", carried, len(tables))
	}

	for _, spec := range tables {
		if got := countRows(t, newOwner, spec.name); got != 1 {
			t.Errorf("%s has %d rows on the new owner, want 1", spec.name, got)
		}
	}

	// The content, not just the row count.
	var content string
	if err := newOwner.SQL().QueryRowContext(t.Context(),
		"SELECT content FROM agent_diary WHERE id = 'd1'").Scan(&content); err != nil {
		t.Fatalf("read the carried diary entry: %v", err)
	}
	if content != "the release train is thursdays" {
		t.Errorf("the carried diary entry says %q", content)
	}
}

// An embedding arrives as bytes: carried as text it would come back as its
// base64 spelling, and that row's recall would be silently dead.
func TestAnEmbeddingSurvivesAsBytes(t *testing.T) {
	t.Parallel()
	oldOwner, newOwner := openStore(t), openStore(t)
	seedMemory(t, oldOwner)
	carry(t, oldOwner, newOwner)

	for _, probe := range []struct{ table, column, id string }{
		{"agent_diary", "embedding", "d1"},
		{"episodes", "embedding", "e1"},
	} {
		var got []byte
		if err := newOwner.SQL().QueryRowContext(t.Context(),
			"SELECT "+probe.column+" FROM "+probe.table+" WHERE id = ?", probe.id,
		).Scan(&got); err != nil {
			t.Fatalf("read %s.%s: %v", probe.table, probe.column, err)
		}
		if want := []byte{0x00, 0x01, 0xfe, 0xff, 0x7f}; !slices.Equal(got, want) {
			t.Errorf("%s.%s came back as %v, want %v", probe.table, probe.column, got, want)
		}
	}
}

// A vector travels with its model: recall compares only rows of the query's
// model, so a vector without one could never be recalled.
func TestAVectorsModelTravelsWithIt(t *testing.T) {
	t.Parallel()
	oldOwner, newOwner := openStore(t), openStore(t)
	seedMemory(t, oldOwner)
	carry(t, oldOwner, newOwner)

	for _, table := range []string{"agent_diary", "episodes"} {
		var model string
		if err := newOwner.SQL().QueryRowContext(t.Context(),
			"SELECT embedding_model FROM "+table+" WHERE id IN ('d1', 'e1')").Scan(&model); err != nil {
			t.Fatalf("read %s.embedding_model: %v", table, err)
		}
		if model != "fixture-model" {
			t.Errorf("%s arrived naming model %q", table, model)
		}
	}
}

// Hydration is repeatable: a re-acquire, a redelivery or a restart mid-replay
// must not leave the seat remembering everything twice.
func TestCarryingTwiceCarriesOnce(t *testing.T) {
	t.Parallel()
	oldOwner, newOwner := openStore(t), openStore(t)
	seedMemory(t, oldOwner)

	carry(t, oldOwner, newOwner)
	carry(t, oldOwner, newOwner)

	for _, spec := range tables {
		if got := countRows(t, newOwner, spec.name); got != 1 {
			t.Errorf("%s has %d rows after two hydrations, want 1", spec.name, got)
		}
	}
}

// The incremental read never re-carries what it already carried, or every
// cycle would republish a seat's whole history.
func TestTheWatermarkOnlyCarriesWhatIsNew(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedMemory(t, db)

	episodes := tables[1]
	if episodes.name != "episodes" {
		t.Fatalf("fixture drifted: tables[1] is %s", episodes.name)
	}
	first, high := exportAll(t, db, episodes, 0)
	if len(first) != 1 {
		t.Fatalf("first export carried %d rows", len(first))
	}
	again, _ := exportAll(t, db, episodes, high)
	if len(again) != 0 {
		t.Errorf("the watermark re-carried %d rows nothing had changed about", len(again))
	}

	// A table rewritten in place carries whole: a watermark cannot see an update.
	profiles := tables[2]
	if profiles.name != "counterparty_profiles" || !profiles.wholeEachCycle {
		t.Fatalf("fixture drifted: tables[2] is %s", profiles.name)
	}
	whole, _ := exportAll(t, db, profiles, 1<<40)
	if len(whole) != 1 {
		t.Errorf("a whole-each-cycle table carried %d rows past a high watermark", len(whole))
	}
}

// Carrying a seat never carries a peer's rows.
func TestOnlyTheNamedSeatsMemoryTravels(t *testing.T) {
	t.Parallel()
	oldOwner, newOwner := openStore(t), openStore(t)
	seedMemory(t, oldOwner)
	if _, err := oldOwner.SQL().ExecContext(t.Context(),
		`INSERT INTO episodes (id, agent_handle, agent_role, turn_id, started_at,
		 ended_at, plan_summary, task_summary, tool_sequence, review_outcome,
		 duration_ms, kind)
		 VALUES ('other', 'ceo', 'CEO', 't9', 0, 0, 'p', 't', '[]', 'done', 1, 'raw')`,
	); err != nil {
		t.Fatalf("seed a peer's episode: %v", err)
	}

	carry(t, oldOwner, newOwner)

	var handles []string
	rows, err := newOwner.SQL().QueryContext(t.Context(),
		"SELECT agent_handle FROM episodes")
	if err != nil {
		t.Fatalf("read episodes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var handle string
		if err := rows.Scan(&handle); err != nil {
			t.Fatalf("scan: %v", err)
		}
		handles = append(handles, handle)
	}
	if !slices.Equal(handles, []string{seat.Handle}) {
		t.Errorf("the new owner holds episodes for %v, want only %q", handles, seat.Handle)
	}
}

// A subject is legal for any natural key, dots, spaces and wildcards included,
// and two keys never collide onto one subject.
func TestASubjectIsSafeForAnyKey(t *testing.T) {
	t.Parallel()
	profiles := tables[2]
	hostile := map[string]any{
		"observer_handle":     "eng",
		"subject_handle":      "first.last",
		"subject_external_id": "*",
		"subject_platform":    "a b>c.*",
	}
	got := profiles.subject("eng", hostile)
	if !regexp.MustCompile(`^crewlet\.memory\.eng\.counterparty_profiles\.[0-9a-f]{32}$`).MatchString(got) {
		t.Fatalf("subject %q is not a safe, addressable token", got)
	}
	// A collision would overwrite one counterparty's profile with another's.
	other := map[string]any{
		"observer_handle": "eng", "subject_handle": "first",
		"subject_external_id": "last", "subject_platform": "a b>c.*",
	}
	if profiles.subject("eng", other) == got {
		t.Error("two different keys collided onto one subject")
	}
}

// Every agent-keyed table is carried or named here as deliberately not memory:
// one in neither silently stays behind when its seat moves.
func TestEveryAgentKeyedTableTravels(t *testing.T) {
	t.Parallel()
	db := openStore(t)

	// Tables keyed by an agent that are deliberately NOT a seat's memory.
	notMemory := map[string]string{
		"crewlet_events": "the node's own audit log — it records what THIS node saw, " +
			"and a peer's copy would claim this node had seen it too",
		"scheduled_runs": "this node's dispatch history; the claim that stops a " +
			"double fire is the fleet's, in coordination",
	}

	rows, err := db.SQL().QueryContext(t.Context(),
		`SELECT m.name, group_concat(i.name) FROM sqlite_master m
		 JOIN pragma_table_info(m.name) i
		 WHERE m.type = 'table' GROUP BY m.name`)
	if err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	defer func() { _ = rows.Close() }()

	carried := map[string]bool{}
	for _, spec := range tables {
		carried[spec.name] = true
	}
	for rows.Next() {
		var name, columns string
		if err := rows.Scan(&name, &columns); err != nil {
			t.Fatalf("scan the schema: %v", err)
		}
		keyed := false
		for column := range strings.SplitSeq(columns, ",") {
			switch strings.TrimSpace(column) {
			case "agent_id", "agent_handle", "observer_handle":
				keyed = true
			}
		}
		if !keyed || carried[name] {
			continue
		}
		if _, deliberate := notMemory[name]; deliberate {
			continue
		}
		t.Errorf("%s is keyed by an agent and is neither carried by memsync nor "+
			"listed as deliberately not memory — a seat that moves node will "+
			"silently lose it", name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the schema: %v", err)
	}
}

// A node that lost a seat while a peer kept learning, and took it back, holds
// the peer's wholeEachCycle rows rather than its own stale copy, which its next
// publish would put over the peer's learning for the whole fleet.
func TestAReturningNodeTakesThePeersUpdatesOverItsOwnStaleRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The returning node still holds what it wrote before it lost the seat.
	returning := openStore(t)
	seedMemory(t, returning)

	// The peer that ran the seat in between, and kept learning.
	peer := openStore(t)
	seedMemory(t, peer)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := peer.SQL().ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("the peer's learning: %v\n%s", err, statement)
		}
	}
	exec(`UPDATE counterparty_profiles SET interaction_count = 90,
		traits = '{"prefers":"a call"}' WHERE observer_handle = ?`, seat.Handle)
	exec(`UPDATE synthesized_skills SET state = 'archived', use_count = 12,
		load_count = 5, version = 3 WHERE id = 's1'`)
	exec(`UPDATE agent_onboarding_markers SET summary = 'read the runbook too'
		WHERE agent_id = ?`, seat.AgentID)

	if carried := carry(t, peer, returning); carried == 0 {
		t.Fatal("nothing was carried")
	}

	var count int
	var traits string
	if err := returning.SQL().QueryRowContext(ctx,
		`SELECT interaction_count, traits FROM counterparty_profiles
		 WHERE observer_handle = ?`, seat.Handle).Scan(&count, &traits); err != nil {
		t.Fatalf("read the carried profile: %v", err)
	}
	if count != 90 || traits != `{"prefers":"a call"}` {
		t.Errorf("the profile is still the returning node's own: count=%d traits=%s",
			count, traits)
	}

	var state string
	var uses, loads, version int
	if err := returning.SQL().QueryRowContext(ctx,
		`SELECT state, use_count, load_count, version FROM synthesized_skills WHERE id = 's1'`,
	).Scan(&state, &uses, &loads, &version); err != nil {
		t.Fatalf("read the carried skill: %v", err)
	}
	if state != "archived" || uses != 12 || loads != 5 || version != 3 {
		t.Errorf("the skill did not take the peer's state: state=%q uses=%d loads=%d version=%d",
			state, uses, loads, version)
	}

	var summary string
	if err := returning.SQL().QueryRowContext(ctx,
		`SELECT summary FROM agent_onboarding_markers WHERE agent_id = ?`,
		seat.AgentID).Scan(&summary); err != nil {
		t.Fatalf("read the carried marker: %v", err)
	}
	if summary != "read the runbook too" {
		t.Errorf("the onboarding marker is stale: %q", summary)
	}
}

// An append-only row already here is not rewritten by a second carry: its
// bookkeeping is this node's, and a turn may be reading it.
func TestAnAppendOnlyRowIsNotRewrittenByASecondCarry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	source := openStore(t)
	seedMemory(t, source)
	target := openStore(t)
	carry(t, source, target)

	// The target's own lifecycle moves the bookkeeping on its copy.
	if _, err := target.SQL().ExecContext(ctx,
		`UPDATE agent_diary SET retrieval_count = 41 WHERE id = 'd1'`); err != nil {
		t.Fatalf("the target's own bookkeeping: %v", err)
	}
	carry(t, source, target)

	var retrievals int
	if err := target.SQL().QueryRowContext(ctx,
		`SELECT retrieval_count FROM agent_diary WHERE id = 'd1'`).Scan(&retrievals); err != nil {
		t.Fatalf("read the diary entry: %v", err)
	}
	if retrievals != 41 {
		t.Errorf("a second carry rewrote an append-only row: retrieval_count = %d", retrievals)
	}
	if got := countRows(t, target, "agent_diary"); got != 1 {
		t.Errorf("a second carry left %d diary rows", got)
	}
}

// A carried row colliding on a unique index the import does not target is
// skipped, not raised: a raise inside hydration refuses the seat everywhere.
//
// Built on two nodes, as it arises, since neither index lets one store hold
// both rows: episodes on (agent_handle, work_key), from a dead node's work a
// peer re-ran under its own id, and synthesized_skills on (agent_handle, name),
// from a skill re-synthesized under a new id. A third node is handed both.
func TestARowCollidingOnAnUntargetedIndexIsSkippedNotRaised(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour).UnixMicro()

	// Each node holds its own version of the same work. The caller opens the
	// store, so this closure seeds through one context chain.
	seed := func(db *store.DB, episodeID, skillID, body string) {
		t.Helper()
		exec := func(statement string, args ...any) {
			t.Helper()
			if _, err := db.SQL().ExecContext(ctx, statement, args...); err != nil {
				t.Fatalf("seed a node: %v\n%s", err, statement)
			}
		}
		exec(`INSERT INTO episodes (id, agent_handle, agent_role, turn_id,
			started_at, ended_at, plan_summary, task_summary, tool_sequence,
			review_outcome, duration_ms, kind, work_key)
			VALUES (?, ?, 'Engineer', 't1', ?, ?, 'p', 't', '[]', 'done', 9,
			'raw', 'wk1')`, episodeID, seat.Handle, at, at)
		exec(`INSERT INTO synthesized_skills (id, agent_handle, name, description,
			content, frontmatter, tool_sequence, source_episode_ids, version,
			created_at, updated_at, state)
			VALUES (?, ?, 'ship-it', 'how to ship', ?, '{}', '[]', '[]', 1, ?, ?,
			'active')`, skillID, seat.Handle, body, at, at)
	}
	first, second := openStore(t), openStore(t)
	seed(first, "e1", "s1", "body")
	seed(second, "e2", "s2", "body-again")

	// carry fails the test on an import error, which is the assertion.
	target := openStore(t)
	carry(t, first, target)
	carry(t, second, target)

	if got := countRows(t, target, "episodes"); got != 1 {
		t.Errorf("the target holds %d episodes, want the one the index allows", got)
	}
	if got := countRows(t, target, "synthesized_skills"); got != 1 {
		t.Errorf("the target holds %d skills, want the one the index allows", got)
	}
}

// A carried row keeps its column storage types. JSON has one number type, and
// a float handed to an integer column can store a REAL, which fails a scan or
// sorts differently; timestamps drive every recency window, sweep and ordering.
func TestCarriedColumnsKeepTheirStorageTypes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := openStore(t)
	seedMemory(t, source)
	target := openStore(t)
	carry(t, source, target)

	for _, probe := range []struct{ query, want string }{
		{"SELECT typeof(created_at) FROM agent_diary WHERE id = 'd1'", "integer"},
		{"SELECT typeof(retrieval_count) FROM agent_diary WHERE id = 'd1'", "integer"},
		{"SELECT typeof(embedding) FROM agent_diary WHERE id = 'd1'", "blob"},
		{"SELECT typeof(content) FROM agent_diary WHERE id = 'd1'", "text"},
		{"SELECT typeof(started_at) FROM episodes WHERE id = 'e1'", "integer"},
		{"SELECT typeof(duration_ms) FROM episodes WHERE id = 'e1'", "integer"},
		{"SELECT typeof(interaction_count) FROM counterparty_profiles", "integer"},
	} {
		var got string
		if err := target.SQL().QueryRowContext(ctx, probe.query).Scan(&got); err != nil {
			t.Errorf("%s: %v", probe.query, err)
			continue
		}
		if got != probe.want {
			t.Errorf("%s = %s, want %s", probe.query, got, probe.want)
		}
	}

	// And the value itself survives, to the microsecond.
	var carriedAt, originalAt int64
	if err := target.SQL().QueryRowContext(ctx,
		"SELECT created_at FROM agent_diary WHERE id = 'd1'").Scan(&carriedAt); err != nil {
		t.Fatalf("read the carried stamp: %v", err)
	}
	if err := source.SQL().QueryRowContext(ctx,
		"SELECT created_at FROM agent_diary WHERE id = 'd1'").Scan(&originalAt); err != nil {
		t.Fatalf("read the original stamp: %v", err)
	}
	if carriedAt != originalAt {
		t.Errorf("the stamp arrived as %d, was %d", carriedAt, originalAt)
	}
}

// An integer past 2^53 arrives unchanged, where a float64 decoding would
// silently change it. No column reaches it today, but every one is an int64.
func TestALargeIntegerArrivesUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// The smallest integer a float64 cannot hold; a power of two would
	// round-trip however it was decoded.
	const past2p53 = int64(1)<<53 + 1

	source := openStore(t)
	seedMemory(t, source)
	if _, err := source.SQL().ExecContext(ctx,
		"UPDATE agent_diary SET retrieval_count = ? WHERE id = 'd1'", past2p53); err != nil {
		t.Fatalf("seed the large integer: %v", err)
	}

	target := openStore(t)
	carry(t, source, target)

	var got int64
	if err := target.SQL().QueryRowContext(ctx,
		"SELECT retrieval_count FROM agent_diary WHERE id = 'd1'").Scan(&got); err != nil {
		t.Fatalf("read the carried integer: %v", err)
	}
	if got != past2p53 {
		t.Errorf("carried %d, want %d (a difference of %d)", got, past2p53, got-past2p53)
	}
}
