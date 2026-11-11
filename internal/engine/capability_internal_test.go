package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SEAT'S ACCESS, TOLD, over a running node: what a turn's dispatch tells
// the seat, read against what the access log says it was last told, and the
// told record its first launch writes. The company's policies are part of a
// seat's snapshot, so a revision that changes one is a change of access with
// no vendor to stand up.

// capabilityCompany is the test company with policies.
func capabilityCompany(t *testing.T, policies ...string) *Company {
	t.Helper()
	doc := nativeCleanupCompany
	if len(policies) > 0 {
		doc += "policies:\n"
		for _, p := range policies {
			doc += "  - " + p + "\n"
		}
	}
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	c, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return c
}

// dispatchTurn is one turn of the seat dispatched on company: what it is told,
// and its id.
func dispatchTurn(t *testing.T, e *Engine, company *Company) (*toldTurn, string) {
	t.Helper()
	turn := uuid.NewString()
	return e.capabilityFor(t.Context(), company, "ceo", turn), turn
}

// launchTurn is the turn's first launch reaching a run: what records it told.
func launchTurn(t *testing.T, e *Engine, told *toldTurn, turn string) {
	t.Helper()
	e.tellCapability(t.Context(), &turnctx.Turn{RunID: turn, WorkKey: "wk-" + turn,
		WorkSince: time.Now().UTC()}, told)
}

// toldRows is what the seat's told records say, newest first.
func toldRows(t *testing.T, e *Engine) []accessdomain.ToldRow {
	t.Helper()
	page, err := e.AccessReader().Told(t.Context(), "ceo", "", accessdomain.MaxPageSize,
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the told records: %v", err)
	}
	return page.Rows
}

// A BASELINE TURN CARRIES NO BLOCK. A seat never told anything is told its
// access in full by "Your access", so its first turn carries no change block,
// and its first launch writes a baseline the next turn is measured against.
//
// Mutation: tell a seat with no told record its snapshot as changes and the
// first turn carries a block.
func TestABaselineTurnCarriesNoBlock(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	told, turn := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	if told.changes != "" || told.pending == nil || !told.pending.Baseline {
		t.Fatalf("a seat's first turn is told %q with %+v, want no block and a baseline to record",
			told.changes, told.pending)
	}
	launchTurn(t, e, told, turn)
	rows := toldRows(t, e)
	if len(rows) != 1 || rows[0].Turn != turn || !rows[0].Baseline || !rows[0].Latest {
		t.Fatalf("the told records are %+v, want the first turn's baseline", rows)
	}
}

// A SEAT IS TOLD A CHANGE ONCE. A policy added between two turns is told to the
// turn after it, recorded when that turn's first launch reaches a run, and not
// told again: the next turn's diff is against what the seat was last told. A
// second write of the told turn, by a relaunch or another node, records
// nothing.
//
// Mutation: drop the told record's write at a launch and the change is told on
// every turn after it.
func TestASeatIsToldAChangeOnce(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	before := capabilityCompany(t, "Ship small changes.")
	first, turn := dispatchTurn(t, e, before)
	launchTurn(t, e, first, turn)

	after := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	changed, changedTurn := dispatchTurn(t, e, after)
	if !strings.Contains(changed.changes, "## What changed since your last turn") ||
		!strings.Contains(changed.changes, "Only the SRE operates production.") {
		t.Fatalf("the turn after the policy is told:\n%s", changed.changes)
	}
	launchTurn(t, e, changed, changedTurn)
	// THE SAME TURN AGAIN, as another node's launch of it would write it.
	again := &toldTurn{pending: changed.pending, lines: changed.lines}
	launchTurn(t, e, again, changedTurn)

	next, _ := dispatchTurn(t, e, after)
	if next.changes != "" || next.pending != nil {
		t.Errorf("the turn after the told one is told %q with %+v, want nothing", next.changes, next.pending)
	}
	rows := toldRows(t, e)
	if len(rows) != 2 || rows[0].Turn != changedTurn || rows[0].Baseline || !rows[0].Latest {
		t.Fatalf("the told records are %+v, want the baseline and the one change", rows)
	}
	var changes []capability.Change
	if err := json.Unmarshal(rows[0].Changes, &changes); err != nil || len(changes) != 1 ||
		changes[0].Kind != capability.ChangePolicyAdded {
		t.Errorf("the told change is %s (%v), want the policy added", rows[0].Changes, err)
	}
}

// MANY REVISIONS ARE TOLD AS ONE. Two revisions land while the seat runs no
// turn, each adding a policy; the seat's next turn is told both in one block,
// measured from what it was last told rather than from the revision before.
//
// Mutation: diff against the previous revision and the turn is told only the
// second policy.
func TestManyRevisionsAreToldAsOne(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	first, turn := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	launchTurn(t, e, first, turn)
	// A REVISION NO TURN RAN UNDER, then the one the next turn runs under.
	_ = capabilityCompany(t, "Ship small changes.", "Review every migration.")
	latest := capabilityCompany(t, "Ship small changes.", "Review every migration.",
		"Only the SRE operates production.")
	told, _ := dispatchTurn(t, e, latest)
	for _, want := range []string{"Review every migration.", "Only the SRE operates production."} {
		if !strings.Contains(told.changes, want) {
			t.Errorf("the turn is not told %q:\n%s", want, told.changes)
		}
	}
	if strings.Count(told.changes, "## What changed since your last turn") != 1 || len(told.lines) != 2 {
		t.Errorf("the two revisions are told as %d lines in:\n%s", len(told.lines), told.changes)
	}
}

// A TURN THAT NEVER LAUNCHED TOLD NOTHING. A turn dispatched after a change and
// gone before its first launch reached a run recorded nothing, so the turn
// after it is told the same change.
//
// Mutation: write the told record at the dispatch and the change is lost with
// the turn that never ran.
func TestATurnThatNeverLaunchedToldNothing(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	first, turn := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	launchTurn(t, e, first, turn)
	after := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	lost, _ := dispatchTurn(t, e, after)
	if lost.changes == "" {
		t.Fatal("the premise: the turn after the policy is told nothing")
	}
	next, _ := dispatchTurn(t, e, after)
	if next.changes != lost.changes {
		t.Errorf("the turn after one that never launched is told:\n%s\nwant:\n%s", next.changes, lost.changes)
	}
	if rows := toldRows(t, e); len(rows) != 1 {
		t.Errorf("the told records are %+v, want the baseline alone", rows)
	}
}

// A TOLD RECORD IS WRITTEN BY A LAUNCH THAT REACHED A RUN, ONCE. Through the
// launcher itself: a launch that failed writes nothing, the turn's next launch
// that reaches a run writes the record, and a launch after it writes no second.
//
// Mutation: tell before the launch, or on its error path, and the failed
// launch writes the record; drop the tell and none is written.
func TestATurnsLaunchWritesItsToldRecordOnce(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	company := capabilityCompany(t, "Ship small changes.")
	told, turn := dispatchTurn(t, e, company)
	launches, fail := 0, errors.New("the box could not be made")
	l := e.agentRunFor(company, "ceo", &turnctx.Turn{RunID: turn, WorkKey: "wk-" + turn,
		WorkSince: time.Now().UTC()}, nil, nil, told).(*agentLauncher)
	l.run = func(context.Context, runner.AgentRunRequest) (runner.Launched, error) {
		launches++
		if launches == 1 {
			return runner.Launched{}, fail
		}
		return runner.Launched{}, nil
	}
	req := runner.AgentRunRequest{Fence: func() error { return nil }}
	if _, err := l.LaunchExecutor(t.Context(), req); !errors.Is(err, fail) {
		t.Fatalf("the failed launch answers %v", err)
	}
	if rows := toldRows(t, e); len(rows) != 0 {
		t.Fatalf("a launch that never reached a run wrote %+v", rows)
	}
	for range 2 {
		if _, err := l.LaunchExecutor(t.Context(), req); err != nil {
			t.Fatalf("a launch that reached a run: %v", err)
		}
	}
	if rows := toldRows(t, e); len(rows) != 1 || rows[0].Turn != turn {
		t.Fatalf("two launches that reached a run wrote %+v, want the turn's one record", rows)
	}
}

// A BASELINE THIS NODE MAY NOT HOLD WHOLE TELLS NOTHING. Where this node holds
// a record of the seat it could not decode, the baseline it read may not be
// the seat's newest: the turn is told its access and no change, and writes
// nothing.
//
// Mutation: diff an incomplete answer and the turn is told a change another
// node may already have told.
func TestAnIncompleteBaselineTellsOnlyTheAccess(t *testing.T) {
	t.Parallel()
	old := capabilityCompany(t, "Ship small changes.")
	now := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	before := capability.Take(capability.Input{Company: old.Config, Handle: "ceo"})
	after := capability.Take(capability.Input{Company: now.Config, Handle: "ceo"})
	baseline := &accessdomain.ToldRow{Handle: "ceo", Turn: uuid.NewString(), Digest: before.Digest(),
		Snapshot: before.JSON(), Latest: true}
	got := decideTold(after, accessdomain.Recent{Baseline: baseline, Complete: false}, "ceo",
		uuid.NewString(), time.Now().UTC(), accessdomain.SnapshotMaxBytes)
	if got.changes != "" || got.pending != nil {
		t.Errorf("an incomplete baseline tells %q and writes %+v", got.changes, got.pending)
	}
	complete := decideTold(after, accessdomain.Recent{Baseline: baseline, Complete: true}, "ceo",
		uuid.NewString(), time.Now().UTC(), accessdomain.SnapshotMaxBytes)
	if complete.changes == "" || complete.pending == nil {
		t.Error("the premise: a complete baseline tells the change")
	}
}

// A SNAPSHOT PAST THE RECORD'S BOUND IS KEPT AS ITS DIGEST. The turn is still
// told what changed; its record keeps the digest alone, so the next turn under
// the same access is told nothing, and under other access is told its access
// afresh as a new baseline rather than a change against nothing.
//
// Mutation: write the whole snapshot and the record is refused, so every
// turn is told the same change again.
func TestASnapshotPastItsBoundIsKeptAsItsDigest(t *testing.T) {
	t.Parallel()
	old := capabilityCompany(t, "Ship small changes.")
	now := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	before := capability.Take(capability.Input{Company: old.Config, Handle: "ceo"})
	after := capability.Take(capability.Input{Company: now.Config, Handle: "ceo"})
	baseline := &accessdomain.ToldRow{Handle: "ceo", Turn: uuid.NewString(), Digest: before.Digest(),
		Snapshot: before.JSON(), Latest: true}
	tiny := len(before.JSON()) - 1
	got := decideTold(after, accessdomain.Recent{Baseline: baseline, Complete: true}, "ceo",
		uuid.NewString(), time.Now().UTC(), tiny)
	if got.changes == "" || got.pending == nil || string(got.pending.Snapshot) != string(toldWithheld) ||
		got.pending.Digest != after.Digest() {
		t.Fatalf("a snapshot past its bound tells %q and writes %+v", got.changes, got.pending)
	}
	withheld := &accessdomain.ToldRow{Handle: "ceo", Turn: got.pending.Turn, Digest: after.Digest(),
		Snapshot: toldWithheld, Latest: true}
	same := decideTold(after, accessdomain.Recent{Baseline: withheld, Complete: true}, "ceo",
		uuid.NewString(), time.Now().UTC(), tiny)
	if same.changes != "" || same.pending != nil {
		t.Errorf("the same access after a withheld baseline tells %q and writes %+v", same.changes, same.pending)
	}
	moved := decideTold(before, accessdomain.Recent{Baseline: withheld, Complete: true}, "ceo",
		uuid.NewString(), time.Now().UTC(), accessdomain.SnapshotMaxBytes)
	if moved.changes != "" || moved.pending == nil || !moved.pending.Baseline {
		t.Errorf("other access after a withheld baseline tells %q and writes %+v, want a baseline",
			moved.changes, moved.pending)
	}
}

// A TOLD RECORD WHOSE CHANGES DO NOT DECODE IS SAID ONCE. Its changes mark no
// note, the other records' still do, and a second read of it says nothing more.
//
// Mutation: skip the record silently and nothing records that it was skipped.
func TestAToldRecordWhoseChangesDoNotDecodeIsSaidOnce(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	good, _ := json.Marshal([]capability.Change{{Kind: capability.ChangeLevel, Vendor: "Cloudflare",
		Lane: "cloudflare", Line: "Cloudflare: your Crewlet grant now sets your level to Secure (was Operate)."}})
	rows := []accessdomain.ToldRow{
		{Handle: "ceo", Turn: "t-bad", Changes: json.RawMessage(`{"not":"a list"}`)},
		{Handle: "ceo", Turn: "t-good", Changes: good},
	}
	for range 2 {
		if marks := e.toldMemory(t.Context(), rows); len(marks) != 1 || marks[0].Vendor != "Cloudflare" {
			t.Fatalf("the told memory is %+v, want the good record's change alone", marks)
		}
	}
	said := 0
	e.toldUnreadable.Range(func(any, any) bool { said++; return true })
	if said != 1 {
		t.Errorf("the unreadable record is held %d times, want once", said)
	}
}

// A SEAT'S ACCESS INSTANT DOES NOT AGE OUT. A capability note is held against
// when the seat was last told its access, its newest told record however old,
// so a note written before a change told long past the told memory window is
// held still; a turn that tells a change holds against now, and a seat never
// told holds nothing back for it.
//
// Mutation: read the instant off the window's records, or ignore the turn's
// own change, and a row goes red.
func TestASeatsAccessInstantDoesNotAgeOut(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	old := now.Add(-3 * toldMemoryWindow)
	quiet := accessdomain.Recent{Complete: true, Baseline: &accessdomain.ToldRow{Handle: "ceo", At: old}}
	if got := toldAt(quiet, now, false); !got.Equal(old) {
		t.Errorf("after quiet months the seat's access instant is %v, want its told record's %v", got, old)
	}
	if got := toldAt(quiet, now, true); !got.Equal(now) {
		t.Errorf("a turn telling a change holds against %v, want now", got)
	}
	if got := toldAt(accessdomain.Recent{Complete: true}, now, false); !got.IsZero() {
		t.Errorf("a seat never told holds against %v, want nothing", got)
	}
}

// A TURN HOLDS ITS NOTES AGAINST WHAT THE SEAT WAS LAST TOLD: once the seat has
// a told record, every later turn's dispatch carries that record's instant to
// the memory prefetch, and the turn that tells a change carries its own.
//
// Mutation: hand the prefetch no access instant, and this goes red.
func TestATurnHoldsItsNotesAgainstWhatTheSeatWasLastTold(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	first, turn := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	launchTurn(t, e, first, turn)
	rows := toldRows(t, e)
	if len(rows) != 1 {
		t.Fatalf("the told records are %+v, want the baseline", rows)
	}
	next, _ := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	if !next.accessAt.Equal(rows[0].At) {
		t.Errorf("the next turn holds against %v, want the baseline's %v", next.accessAt, rows[0].At)
	}
	before := time.Now().UTC()
	changed, _ := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes.", "Only the SRE deploys."))
	if changed.changes == "" || changed.accessAt.Before(before) {
		t.Errorf("a turn telling a change holds against %v, want its own instant", changed.accessAt)
	}
}

// A TURN NAMES A LEVEL AS ITS VENDOR DOES, from what the integration's last
// writing sync recorded, with no vendor asked at dispatch: a Datadog role is
// its name, never its id; and a fixed level this build names (SSH's Operate)
// is its name before any sync recorded one.
//
// Mutation: drop the recorded words from the snapshot's input and the seat is
// told the role's id.
func TestATurnNamesALevelAsItsVendorDoes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := &Engine{backends: &Backends{Fleet: coordmemory.NewFleet()}}
	const role = "90b75548-0000-4000-8000-000000000002"
	on := true
	cfg := &config.Company{Name: "Example", Mission: "Ship it.",
		Integrations:       config.Integrations{Datadog: &config.Datadog{}},
		SeatAccessMigrated: []string{"datadog"},
		Roles: []config.Role{{Name: "Site Reliability Engineer", Handle: "sre", Kind: org.KindAgent,
			Integrations: config.RoleIntegrations{Datadog: &config.RoleDatadog{Enabled: &on, Role: role}}}}}
	told := func() string {
		t.Helper()
		in, err := e.capabilityInputOf(ctx, cfg, nil, "sre")
		if err != nil {
			t.Fatalf("capabilityInputOf: %v", err)
		}
		return capability.YourAccess(capability.Take(in))
	}
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		t.Fatalf("SeatAccessLedger: %v", err)
	}
	account := seataccess.Observed{Exists: true, Enabled: true, Level: role}
	if err := ledger.Put(ctx, seataccess.Record{Kind: integration.KindDatadog, Handle: "sre",
		Lanes: map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneDatadog: {Observed: account}}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := told(); !strings.Contains(got, "- Datadog: "+role+".") {
		t.Fatalf("before any sync recorded its name the seat is told\n%s", got)
	}
	if err := ledger.Put(ctx, seataccess.Record{Kind: integration.KindDatadog, Handle: "sre",
		Lanes: map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneDatadog: {Observed: account,
			Words: &seataccess.GrantWords{Levels: []seataccess.Level{{ID: role, Name: "Datadog Admin Role"}}}}}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := told(); !strings.Contains(got, "- Datadog: Datadog Admin Role.") || strings.Contains(got, role) {
		t.Errorf("once a sync recorded the role's name the seat is told\n%s", got)
	}

	cfg.Integrations = config.Integrations{SSH: &config.SSH{Enabled: true}}
	cfg.SeatAccessMigrated = []string{"ssh"}
	cfg.Roles[0].Integrations = config.RoleIntegrations{SSH: &config.RoleSSH{Enabled: &on,
		Level: config.SSHLevelOperate, Hosts: []string{"db-1"}}}
	if err := ledger.Put(ctx, seataccess.Record{Kind: integration.KindSSH, Handle: "sre",
		Lanes: map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneSSH: {Observed: seataccess.Observed{
			Exists: true, Enabled: true, Level: config.SSHLevelOperate}}}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := told(); !strings.Contains(got, "- SSH (db-1): Operate.") ||
		strings.Contains(got, "no tool server") {
		t.Errorf("an SSH seat at Operate is told\n%s", got)
	}
}
