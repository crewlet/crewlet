package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// ONE NODE READS A SIGN-IN, AND THE NEXT TAKES OVER WHEN IT GOES QUIET. Rank 0
// reads on its own schedule; the node ranked k reads once the newest reading
// (or its own first sight) is k intervals old; a heard limit reads at once,
// at most once a minute; nothing reads before its back-off or after a read
// that ran a model turn.
//
// Mutation: let every rank read on its schedule, and the runner-up reads a
// sign-in rank 0 read a minute ago; drop the heard-limit gap, and a burst of
// refusals is a burst of reads.
func TestOneNodeReadsASignInAndTheNextTakesOver(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.November, 25, 6, 0, 0, 0, time.UTC)
	first := now.Add(-time.Hour)
	for name, tc := range map[string]struct {
		row            planReadRow
		rank           int
		newest, wanted time.Time
		due            bool
	}{
		"the reader, never read":              {row: planReadRow{first: now}, due: true},
		"the reader, before its next read":    {row: planReadRow{first: first, next: now.Add(time.Minute)}},
		"the runner-up, a fresh reading":      {row: planReadRow{first: first}, rank: 1, newest: now.Add(-time.Minute)},
		"the runner-up, a reading gone quiet": {row: planReadRow{first: first}, rank: 1, newest: now.Add(-6 * time.Minute), due: true},
		"the runner-up, just started":         {row: planReadRow{first: now}, rank: 1},
		"the third, one interval quiet":       {row: planReadRow{first: first}, rank: 2, newest: now.Add(-6 * time.Minute)},
		"a heard limit, on any rank":          {row: planReadRow{first: first}, rank: 2, newest: now.Add(-time.Minute), wanted: now, due: true},
		"a heard limit, read a moment ago":    {row: planReadRow{first: first, asked: now.Add(-10 * time.Second)}, rank: 1, newest: now.Add(-time.Minute), wanted: now},
		"a read that ran a model turn":        {row: planReadRow{first: first, off: true}, due: false},
		"a node that does not read this plan": {row: planReadRow{first: first}, rank: -1},
	} {
		if got := planReadDue(tc.row, tc.rank, tc.newest, tc.wanted, now); got != tc.due {
			t.Errorf("%s: due = %v, want %v", name, got, tc.due)
		}
	}
}

// A PEER'S READING IS THIS NODE'S TOO: limits another live node published for
// the same sign-in are kept here at the instant it saw them, and only nodes
// that sign the plan in with a login are counted as its readers.
//
// Mutation: skip the peers' limits, and this node holds no reading.
func TestAPeersReadingIsKeptAndRanksTheReaders(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agent, err := harness.New(harness.Config{Key: "plan", Harness: "claude-code", StateDir: dir,
		Overrides: map[string]any{"binary": filepath.Join(dir, "no-cli-here")}, Timeout: time.Minute,
		Auth:  harness.Auth{Mode: harness.AuthAPIKey, APIKey: "sk-ant-api03-not-real"},
		Plans: harness.NewPlans([]byte("a fleet key"))})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	short := agent.SignInID().Short()
	seen := time.Date(2026, time.November, 25, 6, 0, 0, 0, time.UTC)
	used := 0.4
	nodes := map[string][]coord.ModelStatus{
		"node-b": {{Key: "plan", Plan: short, SignIn: "credentials", Resolves: true,
			Limits: []coord.ModelLimit{{Window: "weekly", Hours: 168, State: "ok", Used: &used, Seen: seen}}}},
		"node-c": {{Key: "plan", Plan: short, SignIn: "token", Resolves: true}},
		"node-d": {{Key: "other", Plan: "000000000000", SignIn: "credentials", Resolves: true}},
	}
	rank := heardFromPeers(agent, agent.SignInID(), nodes, "node-a")
	if rank != 0 && rank != 1 {
		t.Errorf("rank = %d, want a place among the two readers (node-a, node-b)", rank)
	}
	if got := agent.Plan().LastRead(); !got.Equal(seen) {
		t.Errorf("the plan's newest reading is %v, want the peer's, seen %v", got, seen)
	}
}

// AN ASK CREWLET CALL ON A FLEET MOVES PAST A MEMBER THAT REPORTS A LIMIT BEFORE
// ANY OUTPUT, and answers from the next: no error while any member has room.
// Only a fleet every member of which refuses fails, as the fleet's walk.
//
// Mutation: hand helpers the walk's first member alone, and the spent member's
// limit is the answer.
func TestAHelperOnAFleetHandsOffALimitToTheNextMember(t *testing.T) {
	t.Parallel()
	now := parkedAt
	clock := func() time.Time { return now }
	spent := &limitedModel{reset: now.Add(6 * 24 * time.Hour), now: clock}
	ready := &limitedModel{now: clock}
	ledger := fleet.NewLedger(clock)
	used := map[string]float64{"spent": 0.10, "ready": 0.40}
	entries := []phase.Entry{{Key: "spent", Provider: spent}, {Key: "ready", Provider: ready}}
	reg, err := phase.NewFleetRegistry(entries, map[string]phase.Fleet{
		"opus": {Strategy: fleet.Balanced, Threshold: 5, Members: []string{"spent", "ready"}},
	}, phase.Balance{Ledger: ledger, Signals: func(key string) fleet.Signals {
		u := used[key]
		return fleet.Signals{Kind: fleet.KindPlan, Readings: []fleet.Reading{{
			LimitReading: llm.LimitReading{Window: llm.LimitSession, State: llm.LimitOK, Used: &u}, Seen: now}}}
	}})
	if err != nil {
		t.Fatalf("NewFleetRegistry: %v", err)
	}
	c := &Company{Models: reg, Helpers: map[string]llm.Provider{"spent": spent, "ready": ready}}
	run, err := helperPick(c, "assistant.model", "fleet:opus", "assistant", true)
	if err != nil || run.key != "spent" {
		t.Fatalf("the pick = %q, %v; want the spent member first, its reading not yet in", run.key, err)
	}
	provider, err := run.called(func(_ string, inner llm.Provider) llm.Provider { return inner })
	if err != nil {
		t.Fatalf("called: %v", err)
	}
	if _, ok := provider.(fleetDecidedProvider); !ok {
		t.Error("the walk does not say why its first member was picked")
	}
	got, err := provider.Complete(t.Context(), llm.Request{})
	if err != nil || got.ProviderKey != "ready" {
		t.Fatalf("the call answered %+v, %v; want the ready member's answer", got, err)
	}

	ready.reset = now.Add(time.Hour)
	_, err = provider.Complete(t.Context(), llm.Request{})
	limit, limited := chain.Limited(err)
	if !limited || limit.Fleet != "opus" || !slices.Equal(limit.Keys, []string{"spent", "ready"}) {
		t.Errorf("every member refusing = %v (%+v), want the fleet's walk at its limit", err, limit)
	}
}

// fleetDecidedProvider is what Ask Crewlet reads a fleet's decision through.
type fleetDecidedProvider interface {
	llm.Provider
	FleetDecision() types.FleetDecision
}

// ASK CREWLET IS UNREADY ONLY WHEN NO MEMBER OF ITS FLEET IS READY, and then as
// the fleet's walk, so its notice names the fleet.
//
// Mutation: read the head alone, and a spent head refuses a question a ready
// member would answer.
func TestAskCrewletIsUnreadyOnlyWhenNoFleetMemberIs(t *testing.T) {
	t.Parallel()
	spentA, spentB := &unreadyModel{err: limitErr()}, &unreadyModel{err: limitErr()}
	run := helperRun{job: fleet.Job{Fleet: "opus"}, members: []chain.Member{
		{Key: "a", Provider: spentA}, {Key: "b", Provider: &unreadyModel{}}}}
	if err := run.unready(t.Context()); err != nil {
		t.Errorf("a fleet with a ready member is unready: %v", err)
	}
	run.members[1].Provider = spentB
	err := run.unready(t.Context())
	var walked *chain.Error
	if !errors.As(err, &walked) || walked.Fleet != "opus" || len(walked.Failures) != 2 {
		t.Errorf("a fleet with no ready member = %v, want its walk exhausted", err)
	}
}

// unreadyModel says, without a call, whether its next call would fail.
type unreadyModel struct{ err error }

func (m *unreadyModel) Model() string                 { return "opus" }
func (m *unreadyModel) Unready(context.Context) error { return m.err }
func (m *unreadyModel) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return nil, m.err
}

func limitErr() error {
	return &llm.Error{Kind: llm.KindRateLimit, RetryAt: parkedAt.Add(time.Hour), Err: errors.New("limit")}
}
