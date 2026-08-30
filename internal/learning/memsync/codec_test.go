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
)

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "m.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

var seat = seatRef{Handle: "eng", AgentID: "11111111-2222-3333-4444-555555555555"}

// seedMemory writes one row into every table a seat's memory lives in, so a
// round trip is tested over the whole registry rather than the one table
// somebody remembered.
func seedMemory(t *testing.T, db *store.DB) {
	t.Helper()
	at := time.Now().UTC().Add(-time.Hour).UnixMicro()
	// An embedding, so the blob path is exercised: a vector carried as its
	// own base64 spelling reads back as nothing and breaks recall silently.
	embedding := []byte{0x00, 0x01, 0xfe, 0xff, 0x7f}
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := db.SQL().ExecContext(t.Context(), statement, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, statement)
		}
	}
	exec(`INSERT INTO agent_diary (id, agent_id, kind, content, source, turn_id,
		metadata, retrieval_count, embedding, created_at)
		VALUES (?, ?, 'diary_long', 'the release train is thursdays', 'reflect',
		't1', '{}', 3, ?, ?)`, "d1", seat.AgentID, embedding, at)
	exec(`INSERT INTO episodes (id, agent_handle, agent_role, turn_id, started_at,
		ended_at, plan_summary, task_summary, tool_sequence, review_outcome,
		duration_ms, embedding, kind)
		VALUES (?, ?, 'Engineer', 't1', ?, ?, 'plan', 'task', '["slack_post"]',
		'done', 1200, ?, 'raw')`, "e1", seat.Handle, at, at, embedding)
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
	exec(`INSERT INTO conversation_sessions (entry_id, agent_handle,
		conversation_key, work_key, turn_id, entry, created_at)
		VALUES ('c1', ?, 'slack:C1:123', 'wk1', 't1', 'I said this already', ?)`,
		seat.Handle, at)
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

// carry moves every one of a seat's rows from one store to another the way a
// handoff would: export from the old owner, import into the new one.
func carry(t *testing.T, from, to *store.DB) int {
	t.Helper()
	ctx := context.Background()
	carried := 0
	for _, spec := range tables {
		rows, _, err := export(ctx, from.SQL(), spec, seat, 0)
		if err != nil {
			t.Fatalf("export %s: %v", spec.name, err)
		}
		for _, row := range rows {
			body, err := encode(row)
			if err != nil {
				t.Fatalf("encode %s: %v", spec.name, err)
			}
			decoded, decodedSpec, known, err := decode(body)
			if err != nil || !known {
				t.Fatalf("decode %s: known=%v err=%v", spec.name, known, err)
			}
			if err := to.Tx(ctx, func(tx *sql.Tx) error {
				return upsert(ctx, tx, decodedSpec, decoded)
			}); err != nil {
				t.Fatalf("upsert %s: %v", spec.name, err)
			}
			carried++
		}
	}
	return carried
}

// THE BUG, stated as a test: a seat's memory has to survive moving to a node
// that has never run it. Before this package the new owner queried its own
// store and found nothing.
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

	// The content, not just the row count: a memory that arrives empty is
	// the same amnesia with extra steps.
	var content string
	if err := newOwner.SQL().QueryRowContext(t.Context(),
		"SELECT content FROM agent_diary WHERE id = 'd1'").Scan(&content); err != nil {
		t.Fatalf("read the carried diary entry: %v", err)
	}
	if content != "the release train is thursdays" {
		t.Errorf("the carried diary entry says %q", content)
	}
}

// An embedding is BYTES. Carried as text it comes back as its own base64
// spelling — a vector of nothing — and that row's recall is silently dead,
// which is worse than the row not arriving at all.
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

// Hydration is repeatable: a seat re-acquired, a redelivered message, a node
// that restarts mid-replay must not leave the seat remembering everything
// twice.
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

// The incremental read must not re-carry what it already carried, or every
// cycle republishes a seat's whole history.
func TestTheWatermarkOnlyCarriesWhatIsNew(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedMemory(t, db)
	ctx := context.Background()

	episodes := tables[1]
	if episodes.name != "episodes" {
		t.Fatalf("fixture drifted: tables[1] is %s", episodes.name)
	}
	first, high, err := export(ctx, db.SQL(), episodes, seat, 0)
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first export carried %d rows", len(first))
	}
	again, _, err := export(ctx, db.SQL(), episodes, seat, high)
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("the watermark re-carried %d rows nothing had changed about", len(again))
	}

	// A table whose rows are rewritten in place carries whole, because a
	// watermark over the rowid cannot see an update.
	profiles := tables[2]
	if profiles.name != "counterparty_profiles" || !profiles.wholeEachCycle {
		t.Fatalf("fixture drifted: tables[2] is %s", profiles.name)
	}
	whole, _, err := export(ctx, db.SQL(), profiles, seat, 1<<40)
	if err != nil {
		t.Fatalf("whole export: %v", err)
	}
	if len(whole) != 1 {
		t.Errorf("a whole-each-cycle table carried %d rows past a high watermark", len(whole))
	}
}

// One seat's memory is its own. Carrying a seat must never carry a peer's
// rows into a node that has no business holding them.
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

// A subject has to be a legal one for any value a natural key can hold. A
// counterparty is named by whatever a chat platform calls them, and dots,
// spaces and wildcards are all subject syntax.
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
	// Different keys must not collide onto one subject, or one
	// counterparty's profile would overwrite another's.
	other := map[string]any{
		"observer_handle": "eng", "subject_handle": "first",
		"subject_external_id": "last", "subject_platform": "a b>c.*",
	}
	if profiles.subject("eng", other) == got {
		t.Error("two different keys collided onto one subject")
	}
}

// THE ANTI-ROT GUARD. Memory that is not in the registry does not follow its
// seat, and the failure is silent — the seat works and has simply forgotten.
// So a table keyed by an agent has to be either carried or named here as
// deliberately not.
func TestEveryAgentKeyedTableTravels(t *testing.T) {
	t.Parallel()
	db := openStore(t)

	// Tables keyed by an agent that are deliberately NOT a seat's memory.
	notMemory := map[string]string{
		"crewlet_events": "the node's own audit log — it records what THIS node saw, " +
			"and a peer's copy would claim this node had seen it too",
		"scheduled_runs": "this node's dispatch history; the claim that stops a " +
			"double fire is the fleet's, in coordination",
		"chat_thread_follows": "re-asserted by the next mention, so it self-heals " +
			"faster than replication would carry it",
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
		for _, column := range strings.Split(columns, ",") {
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

// A node that held a seat, lost it while a peer kept learning, and took it
// back must arrive holding the PEER's memory rather than its own stale copy.
//
// This is the whole reason table.wholeEachCycle republishes a small mutable
// table every cycle: with DO NOTHING on every conflict the republish is
// carried across the wire and then discarded at the write, the returning node
// keeps the profile it had, and its next publish overwrites the peer's
// learning on the changelog for the whole fleet.
func TestAReturningNodeTakesThePeersUpdatesOverItsOwnStaleRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The node that is about to take the seat back — it still holds what
	// it wrote before it lost it.
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
		version = 3 WHERE id = 's1'`)
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
	var uses, version int
	if err := returning.SQL().QueryRowContext(ctx,
		`SELECT state, use_count, version FROM synthesized_skills WHERE id = 's1'`,
	).Scan(&state, &uses, &version); err != nil {
		t.Fatalf("read the carried skill: %v", err)
	}
	if state != "archived" || uses != 12 || version != 3 {
		t.Errorf("the skill did not take the peer's state: state=%q uses=%d version=%d",
			state, uses, version)
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

// The other half of the same split: an append-only row is immutable once
// written, so a conflict there must NOT rewrite what is already here — a
// redelivery or a second hydration is not a licence to touch a row a turn may
// be reading.
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

// Two turns with NO ledgerable trigger in one conversation are two entries,
// and they must still be two after they have travelled. Both carry ” for
// work_key and for turn_id, so anything that mistakes that quadruple for the
// row's identity collapses them — which is the same mistake the table's own
// dedupe index avoids by being partial over `work_key <> ”`.
func TestTwoUnkeyedTurnsSurviveTheTripAsTwoEntries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := openStore(t)
	for _, entry := range []struct{ id, said string }{
		{"u1", "one"}, {"u2", "two"},
	} {
		if _, err := source.SQL().ExecContext(ctx,
			`INSERT INTO conversation_sessions (entry_id, agent_handle,
			 conversation_key, work_key, turn_id, entry, created_at)
			 VALUES (?, ?, 'slack:C1:123', '', '', ?, 1)`,
			entry.id, seat.Handle, entry.said); err != nil {
			t.Fatalf("seed an unkeyed turn: %v", err)
		}
	}

	target := openStore(t)
	carry(t, source, target)

	if got := countRows(t, target, "conversation_sessions"); got != 2 {
		t.Fatalf("two unkeyed turns arrived as %d entries", got)
	}
	// And a second carry is still idempotent: entry_id is what stops a
	// re-hydration from duplicating what it already brought.
	carry(t, source, target)
	if got := countRows(t, target, "conversation_sessions"); got != 2 {
		t.Errorf("a second carry left %d entries", got)
	}
}

// A carried row that collides on a unique index the import does NOT target
// must be skipped, not raised.
//
// The collision is ordinary fleet history rather than corruption, and it can
// only be built the way it actually arises — on TWO nodes, since neither
// index lets one store hold both rows. episodes is unique on
// (agent_handle, work_key): a node that died mid-turn without acking leaves an
// episode for a work key a peer then re-ran and recorded under its own id.
// synthesized_skills is unique on (agent_handle, name): a skill re-synthesized
// under the same name gets a new id. Both rows reach the changelog under
// different subjects, so a third node replaying that seat is handed both.
//
// It matters far more than a skipped row, because hydration runs inside seat
// acquisition and a failure REFUSES THE SEAT — so a raise here is a seat that
// silently stops being placeable anywhere in the fleet.
func TestARowCollidingOnAnUntargetedIndexIsSkippedNotRaised(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour).UnixMicro()

	// Two nodes, each holding its own version of the same work.
	node := func(episodeID, skillID, body string) *store.DB {
		t.Helper()
		db := openStore(t)
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
		return db
	}
	first := node("e1", "s1", "body")
	second := node("e2", "s2", "body-again")

	// A third node replays the seat and is handed both. carry fails the
	// test on an import error, which is the assertion: this must not raise.
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

// A carried row must arrive with its column TYPES intact.
//
// The wire is JSON, which has one number type: an INTEGER column scanned out,
// marshalled and read back comes through float64, and a driver handed a float
// for an integer column can store a REAL. Timestamps are the ones that matter
// — every recency window, retention sweep and ordering in the learning
// subsystem compares them — and a REAL where an INTEGER is expected either
// fails the scan or silently sorts differently.
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
		{"SELECT typeof(created_at) FROM conversation_sessions", "integer"},
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

// An integer past 2^53 must arrive unchanged.
//
// It is the cliff JSON's single number type puts under every integer column
// here: decoded as a float64, 9007199254740993 comes back as ...92. Nothing
// in this schema reaches it today — a microsecond stamp gets there in the
// 23rd century — but the columns are int64 and the next one added may be a
// token count, a byte count or a hash, and the failure is silent.
func TestALargeIntegerArrivesUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// 2^53 + 1, the smallest integer a float64 cannot hold: it rounds to
	// 2^53. A power of two would round-trip fine however it was decoded
	// and would test nothing.
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
