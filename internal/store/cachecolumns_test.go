package store_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// cachedPhase is one phase record as the runner publishes it: a cached prefix
// inside its input, and a provider key that is NOT the model it reported.
func cachedPhase(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(types.AgentPhaseCompleted{
		Agent: "a-1", RoleName: "Lead", TurnID: "run-1", WorkKey: "wk-1",
		Phase: "execute", Iteration: 1,
		Model: "claude-sonnet-5", ProviderKey: "primary",
		InputTokens: 1000, OutputTokens: 200, TotalTokens: 1200,
		CacheReadTokens: 800, CacheWriteTokens: 150,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// appendPhase writes one phase completion the way the publish listener does:
// tags and spend derived from the serialized event.
func appendPhase(t *testing.T, log *store.EventLog, id string, at time.Time,
	payload []byte, spend *store.Spend) {

	t.Helper()
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: "agent_phase_completed", Source: "engine",
		Category: "agent", Time: at,
		Tags:    store.ExtractTags(payload),
		Spend:   spend,
		Payload: payload,
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

func phaseTokens(t *testing.T, log *store.EventLog) []tokens.Record {
	t.Helper()
	got, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{SinceDays: 1})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	return got
}

// THE CACHE COUNTS AND THE PROVIDER KEY REACH THE ROLLUP FROM THE STORE.
//
// The rollup reads COLUMNS (schema/0015), so a value the writer leaves in the
// payload is a value every stored window reads as zero — which is what the
// cache share did before schema/0030: summed by internal/tokens and filled by
// no producer, so every screen reported a cache that never hit.
func TestAPhasesCacheAndProviderKeyReachTheStoredRollup(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	payload := cachedPhase(t)
	appendPhase(t, log, "p1", time.Now().UTC().Add(-time.Minute), payload,
		store.SpendFor("agent_phase_completed", payload))

	got := phaseTokens(t, log)
	if len(got) != 1 {
		t.Fatalf("records = %d, want the one appended", len(got))
	}
	rec := got[0]
	if rec.CacheReadTokens != 800 || rec.CacheWriteTokens != 150 {
		t.Errorf("cache = %d read / %d written, want 800 / 150 — the writer "+
			"left the counts in the payload", rec.CacheReadTokens, rec.CacheWriteTokens)
	}
	if rec.ProviderKey != "primary" {
		t.Errorf("provider key = %q, want the entry that served the call", rec.ProviderKey)
	}
	if rec.Model != "claude-sonnet-5" {
		t.Errorf("model = %q: the key must not displace the reported model", rec.Model)
	}
	// And the rollup divides what it was given: the cache is a BREAKDOWN
	// of the input, so the totals are unchanged by it.
	roll := tokens.Aggregate(got, tokens.Options{})
	if roll.Totals.CacheReadTokens != 800 || roll.Totals.InputTokens != 1000 ||
		roll.Totals.TotalTokens != 1200 {
		t.Errorf("totals = %+v, want 800 cached of 1000 input and 1200 total",
			roll.Totals)
	}
}

// AN OLDER PEER'S RECORD READS NO CACHE, and still names its key.
//
// A build that predates cache counting publishes no such field, and a rolling
// upgrade puts its records in this store. Zero is what the column means —
// nothing anybody reported — and the write must not fail over the absence.
func TestAPhaseRecordWithNoCacheCountsReadsZero(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	payload := []byte(`{"agent_id":"a-1","role":"Lead","turn_id":"run-1",` +
		`"phase":"execute","model":"m","provider_key":"primary",` +
		`"input_tokens":10,"output_tokens":2,"total_tokens":12}`)
	appendPhase(t, log, "p1", time.Now().UTC().Add(-time.Minute), payload,
		store.SpendFor("agent_phase_completed", payload))

	got := phaseTokens(t, log)
	if len(got) != 1 {
		t.Fatalf("records = %d, want one", len(got))
	}
	if got[0].CacheReadTokens != 0 || got[0].CacheWriteTokens != 0 {
		t.Errorf("cache = %d/%d on a record that reported none",
			got[0].CacheReadTokens, got[0].CacheWriteTokens)
	}
	if got[0].ProviderKey != "primary" || got[0].TotalTokens != 12 {
		t.Errorf("record = %+v, want its key and its 12 tokens", got[0])
	}
}

// THE BACKFILL PROMOTES WHAT THE PAYLOAD ALREADY HELD.
//
// Every phase completion stored before schema/0030 has its cache counts and
// provider key in the payload and zero in the new columns. Without the
// backfill an upgrade would report a cache that stopped hitting the moment the
// node restarted. The row is written exactly as a pre-0030 build left it — the
// promoted counts set, the three new columns at their defaults — and the
// SHIPPED file's UPDATE is run over it, so the case exercises the statement an
// operator's database runs rather than a copy that can drift.
func TestTheBackfillPromotesTheCacheCountsAndTheKey(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	payload := cachedPhase(t)
	appendPhase(t, log, "legacy", time.Now().UTC().Add(-time.Minute), payload,
		&store.Spend{Phase: "execute", Model: "claude-sonnet-5", TurnID: "run-1",
			WorkKey: "wk-1", Iteration: 1,
			InputTokens: 1000, OutputTokens: 200, TotalTokens: 1200})
	// A row that is not a phase completion but happens to carry the same
	// field names: the backfill is scoped to the one type with a spend.
	other := []byte(`{"cache_read_tokens":5,"provider_key":"nope"}`)
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "other", Type: "agent_turn_completed", Source: "engine",
		Category: "agent", Time: time.Now().UTC().Add(-time.Minute),
		Payload: other,
	}); err != nil {
		t.Fatal(err)
	}

	if got := phaseTokens(t, log)[0]; got.CacheReadTokens != 0 || got.ProviderKey != "" {
		t.Fatalf("the seeded row is not a pre-0030 row: %+v", got)
	}
	for _, stmt := range nodeBackfill(t, "0030_a_phase_says_what_the_cache_served.sql") {
		if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), stmt)
			return err
		}); err != nil {
			t.Fatalf("run the backfill: %v", err)
		}
	}

	got := phaseTokens(t, log)[0]
	if got.CacheReadTokens != 800 || got.CacheWriteTokens != 150 || got.ProviderKey != "primary" {
		t.Errorf("after the backfill: cache %d/%d, key %q — want 800/150 and "+
			"%q, the values the payload held", got.CacheReadTokens,
			got.CacheWriteTokens, got.ProviderKey, "primary")
	}
	var otherKey string
	var otherCache int
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT provider_key, cache_read_tokens FROM crewlet_events WHERE event_id = 'other'`).
			Scan(&otherKey, &otherCache)
	}); err != nil {
		t.Fatal(err)
	}
	if otherKey != "" || otherCache != 0 {
		t.Errorf("a turn completion acquired a phase's columns: key %q, cache %d",
			otherKey, otherCache)
	}
}

// THE LIVE WINDOW AND A STORED ONE HAND THE ROLLUP THE SAME RECORD.
//
// internal/tokens folds both, so a field one producer carries and the other
// drops is a rollup whose numbers change when the window crosses the live
// edge — a dashboard that reads a cache share for the last day and none for
// the last week. One serialized phase record goes through both.
func TestTheLiveAndStoredProducersAgreeOnAPhase(t *testing.T) {
	t.Parallel()
	payload := cachedPhase(t)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

	log := open(t).Events()
	appendPhase(t, log, "p1", at, payload, store.SpendFor("agent_phase_completed", payload))
	stored := phaseTokens(t, log)
	if len(stored) != 1 {
		t.Fatalf("stored records = %d, want one", len(stored))
	}

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	live := livestate.New()
	live.Apply(&livestate.Envelope{ID: "p1", Type: "agent_phase_completed",
		Timestamp: at.Format(time.RFC3339Nano), Category: "agent", Payload: body})
	records := live.SpendRecords()
	if len(records) != 1 {
		t.Fatalf("live records = %d, want one", len(records))
	}

	want, got := stored[0], records[0]
	if want != got {
		t.Errorf("the producers disagree about one phase:\n stored %+v\n   live %+v",
			want, got)
	}
}

// nodeBackfill is every UPDATE a node migration carries, read out of the
// shipped file.
func nodeBackfill(t *testing.T, name string) []string {
	t.Helper()
	body, err := store.SchemaFile(store.EstateNode, name)
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	// COMMENTS FIRST, then statements: a migration's prose is free to
	// contain a semicolon, and splitting before stripping cuts a comment in
	// two and hands its tail to the statement that follows.
	var kept []string
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			kept = append(kept, line)
		}
	}
	var out []string
	for _, statement := range strings.Split(strings.Join(kept, "\n"), ";") {
		if trimmed := strings.TrimSpace(statement); strings.HasPrefix(trimmed, "UPDATE ") {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s carries no UPDATE, so this case certifies nothing", name)
	}
	return out
}

// A COLLECTED CODING RUN IS COUNTED WHERE EVERY OTHER PHASE IS.
//
// The run's own record is the only place its tokens reach a reader: the
// executor's resumed record states the executor's rounds, and nothing else
// carries the run's. So the record has to fold through the stored rollup and
// the live one alike, under its own phase and its own model — a record either
// producer dropped would be a coding run's whole spend missing from the screen
// that exists to show spend.
func TestASandboxPhaseIsCountedInTheRollup(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(types.AgentPhaseCompleted{
		Agent: "a-1", RoleName: "Lead", TurnID: "run-1", WorkKey: "wk-1",
		Phase: types.PhaseSandbox, Iteration: 2, LaunchID: "job-1",
		Backend: types.BackendSandbox, CodingAgent: "claude-code", Model: "claude-sonnet-5",
		InputTokens: 9000, OutputTokens: 700, TotalTokens: 9700,
		CacheReadTokens: 6000, CacheWriteTokens: 1500, CostUSD: 0.42,
		ActivityTranscript: "[tool] bash: go test ./...",
	})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	log := open(t).Events()
	appendPhase(t, log, "p1", at, payload, store.SpendFor("agent_phase_completed", payload))

	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	live := livestate.New()
	live.Apply(&livestate.Envelope{ID: "p1", Type: "agent_phase_completed",
		Timestamp: at.Format(time.RFC3339Nano), Category: "agent", Payload: body})

	for name, records := range map[string][]tokens.Record{
		"stored": phaseTokens(t, log), "live": live.SpendRecords(),
	} {
		roll := tokens.Aggregate(records, tokens.Options{})
		var sandbox *tokens.PhaseRow
		for i := range roll.ByPhase {
			if roll.ByPhase[i].Phase == string(types.PhaseSandbox) {
				sandbox = &roll.ByPhase[i]
			}
		}
		if sandbox == nil {
			t.Errorf("%s: the rollup has no sandbox phase: %+v", name, roll.ByPhase)
			continue
		}
		if sandbox.TotalTokens != 9700 || sandbox.CacheReadTokens != 6000 || sandbox.CostUSD != 0.42 {
			t.Errorf("%s: the sandbox phase holds %+v, want the run's 9700 tokens, 6000 cached, $0.42",
				name, sandbox.Bucket)
		}
		if len(roll.ByModel) != 1 || roll.ByModel[0].Model != "claude-sonnet-5" {
			t.Errorf("%s: models = %+v, want the run's own", name, roll.ByModel)
		}
	}
}
