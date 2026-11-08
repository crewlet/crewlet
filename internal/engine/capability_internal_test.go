package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/config"
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
