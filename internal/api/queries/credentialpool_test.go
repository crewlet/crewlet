package queries_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/credential"
)

var poolNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// ledger is the fleet's cooldown ledger, or one that cannot be read.
type ledger struct {
	cooled map[string]time.Time
	err    error
}

func (l ledger) Since(context.Context, time.Time) (map[string]time.Time, error) {
	return l.cooled, l.err
}

// poolFixture is three entries: a bag of five keys in every state, one whose
// every key is benched, and a cli-agent login.
func poolFixture() []engine.CredentialPool {
	return []engine.CredentialPool{
		{
			Provider: "smart", Type: config.LLMAnthropic, Model: "claude-test", Pooled: true,
			RateLimit: time.Hour, Auth: 5 * time.Minute,
			Keys: []engine.CredentialKey{
				{Ref: "KEY_A", Hint: "aaaaaaaaaaaa", Uses: 7, InFlight: 1},
				// Benched HERE for ten minutes, and on the ledger for
				// longer: the later deadline wins.
				{Ref: "KEY_B", Hint: "bbbbbbbbbbbb", Cooling: 10 * time.Minute},
				// Benched by a PEER: this node's pool has not pulled it.
				{Ref: "KEY_C", Hint: "cccccccccccc"},
				{Ref: "KEY_UNSET"},
				{Ref: "KEY_A", Hint: "aaaaaaaaaaaa", Duplicate: true},
			},
		},
		{
			Provider: "fast", Type: config.LLMOpenAI, Model: "gpt-test", Pooled: true,
			RateLimit: time.Hour, Auth: 5 * time.Minute,
			Keys: []engine.CredentialKey{
				{Ref: "OPENAI_API_KEY", Default: true, Hint: "dddddddddddd", Cooling: time.Minute},
				{Inline: true, Hint: "eeeeeeeeeeee", Cooling: time.Minute},
			},
		},
		{Provider: "subscription", Type: config.LLMCLIAgent, Model: "sonnet"},
	}
}

func poolSources(l ledger) queries.Sources {
	return queries.Sources{
		CredentialPools: poolFixture,
		Cooldowns:       l,
		NodeID:          "node-a",
		Now:             func() time.Time { return poolNow },
	}
}

func fleetLedger() ledger {
	return ledger{cooled: map[string]time.Time{
		credential.FleetKey("smart", "bbbbbbbbbbbb"): poolNow.Add(40 * time.Minute),
		credential.FleetKey("smart", "cccccccccccc"): poolNow.Add(20 * time.Minute),
		// The same hint under ANOTHER entry's scope benches nothing here:
		// one entry's burst is not another entry's rate limit.
		credential.FleetKey("other", "aaaaaaaaaaaa"): poolNow.Add(time.Hour),
	}}
}

func askPool(t *testing.T, s queries.Sources) queries.CredentialPoolAnswer {
	t.Helper()
	got, ok := answer(t, s, "credential_pool", nil).(queries.CredentialPoolAnswer)
	if !ok {
		t.Fatalf("credential_pool answered %T", got)
	}
	return got
}

// EVERY KEY IS JUDGED FROM THE POOL AND THE FLEET'S LEDGER, the later deadline
// winning — so a key a peer benched reads cooling before this node's refresher
// pulled it, and a bench whose publish failed still reads cooling here.
func TestEachKeyIsJudgedFromThePoolAndTheFleetsLedger(t *testing.T) {
	t.Parallel()
	got := askPool(t, poolSources(fleetLedger()))
	if !got.Fleet || got.FleetError != "" || got.Node != "node-a" {
		t.Fatalf("answer = %+v, want the fleet's ledger read on node-a", got)
	}
	if len(got.Providers) != 3 {
		t.Fatalf("providers = %+v, want every entry in config order", got.Providers)
	}
	smart := got.Providers[0]
	if smart.Key != "smart" || smart.State != queries.PoolDegraded || smart.Ready != 1 ||
		smart.RateLimitSeconds != 3600 || smart.AuthSeconds != 300 {
		t.Errorf("smart = %+v, want degraded with one ready key and its bench times", smart)
	}
	want := []struct {
		state  queries.CredentialKeyState
		until  time.Duration
		sameAs int
	}{
		{queries.KeyReady, 0, 0},
		{queries.KeyCooling, 40 * time.Minute, 0},
		{queries.KeyCooling, 20 * time.Minute, 0},
		{queries.KeyUnresolved, 0, 0},
		{queries.KeyDuplicate, 0, 1},
	}
	for i, w := range want {
		k := smart.Keys[i]
		if k.State != w.state || k.SameAs != w.sameAs {
			t.Errorf("key %d (%s) = %+v, want %s same_as %d", i+1, k.Ref, k, w.state, w.sameAs)
		}
		switch {
		case w.until == 0 && k.CoolingUntil != nil:
			t.Errorf("key %d is %s and says it lifts at %v", i+1, k.State, k.CoolingUntil)
		case w.until != 0 && (k.CoolingUntil == nil || !k.CoolingUntil.Equal(poolNow.Add(w.until))):
			t.Errorf("key %d lifts at %v, want %v", i+1, k.CoolingUntil, poolNow.Add(w.until))
		}
	}
	if a := smart.Keys[0]; a.Uses != 7 || a.InFlight != 1 || a.Source != queries.KeyFromReference || a.Hint != "aaaaaaaaaaaa" {
		t.Errorf("KEY_A = %+v, want this node's counts, its reference and its hint", a)
	}

	fast := got.Providers[1]
	if fast.State != queries.PoolExhausted || fast.Ready != 0 {
		t.Errorf("fast = %+v, want exhausted: every key it has is cooling", fast)
	}
	if fast.Keys[0].Source != queries.KeyFromDefault || fast.Keys[1].Source != queries.KeyInline || fast.Keys[1].Ref != "" {
		t.Errorf("fast's sources = %+v, want the conventional variable and an unnamed inline value", fast.Keys)
	}

	if sub := got.Providers[2]; sub.State != queries.PoolLogin || len(sub.Keys) != 0 {
		t.Errorf("subscription = %+v, want a login with no keys", sub)
	}
}

// THE ENTRY STATES ARE TOLD APART at their edges: every key ready is ready,
// and nothing resolving is no key at all rather than exhausted.
func TestAnEntryIsReadyOrHasNoKeyAtItsEdges(t *testing.T) {
	t.Parallel()
	s := queries.Sources{
		CredentialPools: func() []engine.CredentialPool {
			return []engine.CredentialPool{
				{Provider: "ok", Pooled: true, Keys: []engine.CredentialKey{
					{Ref: "A", Hint: "aaaaaaaaaaaa"}, {Ref: "A", Hint: "aaaaaaaaaaaa", Duplicate: true},
				}},
				{Provider: "none", Pooled: true, Keys: []engine.CredentialKey{{Ref: "UNSET"}}},
			}
		},
		Now: func() time.Time { return poolNow },
	}
	got := askPool(t, s)
	if got.Providers[0].State != queries.PoolReady {
		t.Errorf("ok = %s, want ready: a duplicate is not a missing key", got.Providers[0].State)
	}
	if got.Providers[1].State != queries.PoolNoKey {
		t.Errorf("none = %s, want no_key", got.Providers[1].State)
	}
}

// AN UNREADABLE LEDGER IS SAID, NOT HIDDEN: this node's pool is still a true
// answer about this node, and it is sent marked as exactly that — never as the
// company's, and never as an error that blanks the screen.
func TestAnUnreadableLedgerAnswersThisNodeAndSaysSo(t *testing.T) {
	t.Parallel()
	got := askPool(t, poolSources(ledger{err: errors.New("the coordination store did not answer")}))
	if got.Fleet || got.FleetError != "the coordination store did not answer" {
		t.Errorf("fleet = %v %q, want the ledger's failure named", got.Fleet, got.FleetError)
	}
	smart := got.Providers[0]
	if b := smart.Keys[1]; b.State != queries.KeyCooling || !b.CoolingUntil.Equal(poolNow.Add(10*time.Minute)) {
		t.Errorf("KEY_B = %+v, want this node's own ten-minute bench", b)
	}
	if c := smart.Keys[2]; c.State != queries.KeyReady {
		t.Errorf("KEY_C = %+v, want ready: only the ledger knew about it", c)
	}

	bare := askPool(t, queries.Sources{CredentialPools: poolFixture, Now: func() time.Time { return poolNow }})
	if bare.Fleet || bare.FleetError == "" {
		t.Errorf("a node with no ledger answered fleet=%v %q, want it said", bare.Fleet, bare.FleetError)
	}
}

// ON THE CONFIGURATION READ — which variable holds each model's keys and
// which of them a vendor is refusing is what the company document's reader is
// trusted with — and unregistered with nothing to read from.
func TestTheCredentialPoolIsReadOnTheConfigurationGrant(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, poolSources(fleetLedger()))
	needsExactly(t, r, "credential_pool", iam.GrantConfigRead)
	bare := queries.NewRegistry()
	queries.Register(bare, queries.Sources{})
	if slices.Contains(bare.Names(), "credential_pool") {
		t.Error("credential_pool is registered with no pools to answer from")
	}
}

// THE MODELS SCREEN READS WHAT THIS ANSWER SENDS, every shape held both ways,
// and knows exactly the states and sources the engine sends.
func TestTheModelsScreenReadsWhatTheCredentialPoolSends(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, poolSources(fleetLedger()))
	raw, err := r.Answer(everyGrant(t), "credential_pool", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := asMap(t, raw)
	holdShape(t, "CredentialPoolAnswer", []map[string]any{body}, false)
	providers := rowsOf(t, body["providers"])
	holdShape(t, "CredentialPoolRow", providers, false)
	var keys []map[string]any
	for _, p := range providers {
		keys = append(keys, rowsOf(t, p["keys"])...)
	}
	holdShape(t, "CredentialKeyRow", keys, false)

	for name, engine := range map[string][]string{
		"CredentialKeyState":  stringsOf(queries.CredentialKeyStates),
		"CredentialKeySource": stringsOf(queries.CredentialKeySources),
		"CredentialPoolState": stringsOf(queries.CredentialPoolStates),
	} {
		got, err := clientsource.Union(clientsource.Tree(t), name)
		if err != nil {
			t.Fatalf("%v — this gate cannot run without the client's declaration", err)
		}
		slices.Sort(got)
		if want := slices.Sorted(slices.Values(engine)); !slices.Equal(got, want) {
			t.Errorf("the dashboard's %s is %v; the engine sends %v", name, got, want)
		}
	}
	for _, s := range queries.CredentialKeyStates {
		if !s.Valid() {
			t.Errorf("key state %q is listed and not valid", s)
		}
	}
	for _, s := range queries.CredentialKeySources {
		if !s.Valid() {
			t.Errorf("key source %q is listed and not valid", s)
		}
	}
	for _, s := range queries.CredentialPoolStates {
		if !s.Valid() {
			t.Errorf("entry state %q is listed and not valid", s)
		}
	}
}
