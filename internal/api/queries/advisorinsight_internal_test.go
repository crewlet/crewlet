package queries

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A FINDING LEADS WITH ITS HEADLINE, ITS NUMBERS AND ONE PRIMARY ACTION: the
// headline its class wrote, or its title; its facts as stats; its seat; and
// its fix as actions, a configuration change's first option primary and the
// others beside it, every other kind the one step it takes. Mutation: make
// every option primary, or leave a fix with no action, and the page cannot
// say what to do first.
func TestAFindingLeadsWithItsHeadlineNumbersAndOnePrimaryAction(t *testing.T) {
	t.Parallel()
	f := advisor.Finding{Title: "Agents that learn on their main model.", Subject: advisor.Subject{Kind: advisor.SubjectSeat,
		Seat: "dev", Name: "Developer"}, Fix: advisor.Fix{Kind: advisor.FixConfigChange,
		Options: []advisor.Option{{Label: "Learn on small"}, {Label: "Learn on mini"}}}}
	got := insightOf(f, []AdvisorFact{{Key: "runs", Label: "Runs", Value: 3, Unit: advisor.UnitCount}})
	if got.Headline != f.Title || len(got.Stats) != 1 || got.Stats[0].Value != 3 || len(got.Seats) != 1 ||
		got.Seats[0].Handle != "dev" || got.Seats[0].Name != "Developer" {
		t.Errorf("insight = %+v", got)
	}
	if p := got.Actions.Primary; p == nil || p.Kind != ActionApply || p.Option == nil || *p.Option != 0 ||
		len(got.Actions.Secondary) != 1 || *got.Actions.Secondary[0].Option != 1 {
		t.Errorf("actions = %+v, want option 0 primary and option 1 beside it", got.Actions)
	}
	f.Headline = "Nine seats learn on their main model."
	if got := insightOf(f, nil); got.Headline != f.Headline {
		t.Errorf("headline = %q, want the class's own", got.Headline)
	}
	for _, c := range []struct {
		fix  advisor.Fix
		kind string
	}{
		{advisor.Fix{Kind: advisor.FixOpenSetting, Setting: "providers.llm"}, ActionOpen},
		{advisor.Fix{Kind: advisor.FixOpenPage, Page: "item", Target: "ENG-1"}, ActionOpen},
		{advisor.Fix{Kind: advisor.FixExternalStep, URL: "https://example.com"}, ActionStep},
		{advisor.Fix{Kind: advisor.FixAskCrewlet, Prompt: "Draft a policy"}, ActionAsk},
		{advisor.Fix{Kind: advisor.FixAtlasAct}, ActionFix},
	} {
		a := actionsOf(c.fix)
		if a.Primary == nil || a.Primary.Kind != c.kind || a.Primary.Label == "" || a.Secondary == nil {
			t.Errorf("%s: actions = %+v, want one %s", c.fix.Kind, a, c.kind)
		}
	}
	if a := actionsOf(advisor.Fix{Kind: advisor.FixNone}); a.Primary != nil {
		t.Errorf("a fix that is nobody's offers %+v", a.Primary)
	}
	moved := actionsOf(advisor.Fix{Kind: advisor.FixOpenPage, Page: advisor.PageSeat, Target: "dev",
		Reassign: &advisor.Reassign{Items: []string{"ENG-1", "ENG-2"}, To: "dev-2"}})
	if p := moved.Primary; p == nil || p.Kind != ActionReassign || p.To == nil || *p.To != "dev-2" ||
		!slices.Equal(p.Items, []string{"ENG-1", "ENG-2"}) || len(moved.Secondary) != 1 ||
		moved.Secondary[0].Kind != ActionOpen {
		t.Errorf("a fix that moves work offers %+v, want the move first and the seat's page beside it", moved)
	}
	raw, err := json.Marshal(AdvisorAction{Kind: ActionAsk, Label: "x"})
	if err != nil || string(raw) != `{"kind":"ask","label":"x","option":null,"items":null,"to":null,"page":null,`+
		`"target":null,"setting":null,"url":null,"question":null}` {
		t.Errorf("an action is sent as %s (%v), want every key", raw, err)
	}
}

// A PAGE FIX'S PRESS SAYS WHAT IT DOES, by the page it opens: "Take a
// backup", "Open ENG-1", never "Open it", which names neither the act nor
// its object. Mutation: label every page alike, and a row's one press tells
// the reader nothing until it is pressed.
func TestAPageFixIsLabelledWithWhatItDoes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		page, target, want string
	}{
		{"backups", "", "Take a backup"},
		{"item", "ENG-1", "Open ENG-1"},
		{"item", "", "Open the item"},
		{"seat", "dev", "Open the seat"},
	} {
		a := actionsOf(advisor.Fix{Kind: advisor.FixOpenPage, Page: advisor.Page(c.page), Target: c.target})
		if a.Primary == nil || a.Primary.Label != c.want {
			t.Errorf("%s %q: primary = %+v, want %q", c.page, c.target, a.Primary, c.want)
		}
	}
}

// A RUN'S SUMMARY NAMES WHAT IT CONCLUDED ABOUT EACH CLASS, apart from its
// scopes: a class's line is listed in `classes` with its reason, and never
// counted as a scope run. Mutation: count the classes' lines as scopes, and
// classes_run doubles.
func TestARunsSummaryNamesWhatItConcludedAboutEachClass(t *testing.T) {
	t.Parallel()
	lines, err := json.Marshal([]duty.ScopeRecord{
		{Class: "evidence/flow", Status: advisordomain.ClassDone, Opened: 1},
		{Class: "capacity.add_seat", Status: advisordomain.ClassDone, Outcome: advisor.OutcomeSilent, Reason: "Why."},
		{Class: "run.round_cap", Status: advisordomain.ClassDone, Outcome: advisor.OutcomeClear},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := runSummary(advisordomain.RunRow{ID: "r", Status: advisordomain.RunDone, Classes: lines})
	if err != nil {
		t.Fatal(err)
	}
	if got.ClassesRun != 1 || got.Opened != 1 || got.Kind != RunKindRules || len(got.Classes) != 2 ||
		got.Classes[0].Status != advisor.OutcomeSilent || got.Classes[0].Reason == nil ||
		*got.Classes[0].Reason != "Why." || got.Classes[1].Reason != nil || got.Progress == nil {
		t.Errorf("summary = %+v", got)
	}
}

// snapWork is a work reader answering flow hours and open work, and keeping
// the flow windows it was asked for.
type snapWork struct {
	WorkReader
	windows *[]worklet.FlowHoursQuery
}

func (w snapWork) FlowHours(_ context.Context, q worklet.FlowHoursQuery) ([]worklet.FlowHours, error) {
	*w.windows = append(*w.windows, q)
	return []worklet.FlowHours{{Seat: "dev", State: worklet.FlowDue, Stood: time.Hour}}, nil
}

func (snapWork) FlowPickups(context.Context, worklet.FlowHoursQuery) ([]time.Duration, error) {
	return []time.Duration{}, nil
}

func (snapWork) WorkPatterns(context.Context, worklet.PatternsQuery) (worklet.WorkPatterns, error) {
	return worklet.WorkPatterns{}, errors.New("unread")
}

// A SNAPSHOT IS READ OVER ITS RANGE AND THE ONE BEFORE, in windows the flow
// read takes, and a range it does not offer is refused. Mutation: read thirty
// days in one window, and the flow read refuses it.
func TestASnapshotIsReadOverItsRangeAndTheOneBefore(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"5d", "7", "thirty"} {
		if _, err := snapshotDays(raw); !errors.Is(err, ErrBadParams) {
			t.Errorf("range %q answered %v, want a bad parameter", raw, err)
		}
	}
	var asked usage.SpendQuery
	var windows []worklet.FlowHoursQuery
	now := time.Date(2030, 1, 31, 15, 0, 0, 0, time.UTC)
	s := Sources{Work: snapWork{windows: &windows}, Usage: ledgerUsage{asked: &asked}}
	snap := s.AdvisorSnapshot(t.Context(), now, 30)
	if snap.Range.Days != 30 || !snap.Range.From.Equal(time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("range = %+v, want 30 company days ending today", snap.Range)
	}
	if len(windows) != 6 {
		t.Errorf("read the flow in %d windows, want three for each 30-day range", len(windows))
	}
	for _, w := range windows {
		//fixedwait:stated the windows the read asked for, bounds the test states
		if w.Until.Sub(w.Since) > worklet.MaxFlowHoursWindow {
			t.Errorf("a flow window of %s, past what one read takes", w.Until.Sub(w.Since)) //fixedwait:stated the same window
		}
	}
	if st, ok := snapStat(snap, "wait_hours"); !ok || st.Value != 3 || st.Was == nil || *st.Was != 3 {
		t.Errorf("wait hours = %+v (%v), want an hour from each of three windows, beside the range before", st, ok)
	}
	if asked.From != "2029-12-03" || asked.To != "2030-01-01" {
		t.Errorf("the ledger was last read over %s..%s, want the 30 days before", asked.From, asked.To)
	}
	if len(snap.Unread) != 2 || snap.Unread[0] != advisor.UnreadWork || snap.Unread[1] != advisor.UnreadCapacity {
		t.Errorf("unread = %v, want the work and the park records it could not read", snap.Unread)
	}
}

func snapStat(s advisor.Snapshot, key string) (advisor.Stat, bool) {
	for _, st := range s.Stats {
		if st.Key == key {
			return st, true
		}
	}
	return advisor.Stat{}, false
}
