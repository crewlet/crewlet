package advisor_test

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// stallShape is a company whose runs take a round cap of 30, with one seat on
// a daily budget of 400,000 tokens, and two people.
const stallShape = `name: Acme
providers:
  llm:
    p: {model: m}
  sandbox: {fake: true, default_max_turns: 30}
roles:
  - name: Owner
    handle: owner
    kind: human
    contact: {slack_user_id: U0OWNER}
  - name: Admin
    handle: admin
    kind: human
    contact: {slack_user_id: U0ADMIN}
  - name: Dev
    handle: dev
    llm: p
    token_budget: {day: 400000}
  - name: QA
    handle: qa
    llm: p
  - name: Ops
    handle: ops
    llm: p
`

var monday = time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)

// stalls is every finding of class the evidence raises about the stall shape.
func stalls(t *testing.T, doc string, ev advisor.Evidence, class advisor.Class) []advisor.Finding {
	t.Helper()
	if ev.Until.IsZero() {
		ev.Until = monday
	}
	in := advisor.Inputs{Company: company(t, doc), Revision: "r1", Integrations: []integration.State{}}
	var out []advisor.Finding
	for _, f := range advisor.ReadWithEvidence(in, ev).Findings {
		if f.Class == class {
			out = append(out, f)
		}
	}
	return out
}

func capped(seat, turn string, tokens, rounds int64) advisor.EndedRun {
	return advisor.EndedRun{Seat: seat, Kind: types.ExecutorErrorMaxTurns, Turn: turn, Tokens: tokens, Rounds: rounds}
}

// A SEAT WHOSE RUNS END ON THEIR ROUND CAP is the owner's to raise: twice in a week, or once at a
// million tokens, while the cap holds; its records are the turns, costliest first, and its fix
// raises the seat's cap. Mutation: drop the token floor or count another kind, and a row fails.
func TestARunEndedOnItsCapIsTheOwnersToRaise(t *testing.T) {
	t.Parallel()
	ev := advisor.Evidence{Runs: []advisor.EndedRun{
		capped("dev", "t1", 1_000_000, 30), capped("dev", "t2", 25_500_000, 30),
		capped("qa", "t3", 100_000, 30),
		capped("ops", "t4", 2_000_000, 30),
		{Seat: "qa", Kind: types.ExecutorErrorOutOfMemory},
	}}
	found := stalls(t, stallShape, ev, advisor.ClassRoundCap)
	if len(found) != 2 || found[0].Subject.Seat != "dev" || found[1].Subject.Seat != "ops" {
		t.Fatalf("raised %+v, want dev (twice) and ops (2M tokens once), not qa", found)
	}
	dev := found[0]
	if dev.Item != "2 runs at a cap of 30 rounds, 26.5M tokens" || dev.Severity != advisor.SeverityCosts ||
		dev.Actor != advisor.ActorOwner || dev.Path != "roles[2].sandbox.max_turns" {
		t.Errorf("dev = %+v", dev)
	}
	if want := []advisor.Record{{Kind: advisor.RecordTurn, ID: "t2"}, {Kind: advisor.RecordTurn, ID: "t1"}}; !reflect.DeepEqual(dev.Records, want) {
		t.Errorf("dev's records = %+v, want the costliest turn first", dev.Records)
	}
	var caps []any
	for _, o := range dev.Fix.Options {
		caps = append(caps, o.Change.Merge["sandbox"].(map[string]any)["max_turns"])
		if o.Change.Entity != advisor.EntityRoles || o.Change.ID != "dev" {
			t.Errorf("option %+v changes another entity", o)
		}
	}
	if !slices.Equal(caps, []any{60, 0}) {
		t.Errorf("dev's options set the cap to %v, want twice it and none", caps)
	}
	raised := stallShape + "    sandbox: {max_turns: 60}\n"
	if got := stalls(t, raised, advisor.Evidence{Runs: ev.Runs[3:4]}, advisor.ClassRoundCap); len(got) != 0 {
		t.Errorf("a cap raised past the rounds the runs met still raised %+v", got)
	}
	ev.RunsUnread = true
	if got := stalls(t, stallShape, ev, advisor.ClassRoundCap); len(got) != 0 {
		t.Errorf("an unread source raised %+v", got)
	}
}

// A CAP IS RAISED NO HIGHER THAN A CAP MAY BE: twice the cap, at most
// config.RoundCapCeiling, so a seat near it is offered the ceiling, and a
// seat at it is offered no raise the config would refuse, only splitting its
// items or lifting its cap.
//
// Mutation: offer twice the cap past the ceiling, or a raise at it, and a row
// goes red.
func TestARoundCapIsRaisedNoHigherThanItsCeiling(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		cap  int
		want []any
	}{{config.RoundCapCeiling - 400, []any{config.RoundCapCeiling, 0}}, {config.RoundCapCeiling, []any{0}}} {
		doc := stallShape + fmt.Sprintf("    sandbox: {max_turns: %d}\n", c.cap)
		ev := advisor.Evidence{Runs: []advisor.EndedRun{capped("ops", "t4", 2_000_000, int64(c.cap))}}
		found := stalls(t, doc, ev, advisor.ClassRoundCap)
		if len(found) != 1 {
			t.Fatalf("a cap of %d raised %+v, want one finding", c.cap, found)
		}
		var caps []any
		for _, o := range found[0].Fix.Options {
			caps = append(caps, o.Change.Merge["sandbox"].(map[string]any)["max_turns"])
		}
		if !slices.Equal(caps, c.want) {
			t.Errorf("a cap of %d is offered %v, want %v", c.cap, caps, c.want)
		}
		if c.cap == config.RoundCapCeiling && !strings.Contains(found[0].Fix.Sentence, "split its items") {
			t.Errorf("a cap at the ceiling says %q, want its items split or its cap lifted", found[0].Fix.Sentence)
		}
	}
}

// A SEAT ITS OWN BUDGET STOPPED for half a working day while it held open work is the owner's to
// raise, at the ceiling it was refused at; the company's budget is one finding naming every seat
// it stopped. Mutation: count overlapping stops twice or drop the open-work gate, and a row fails.
func TestASeatStoppedOnItsBudgetWhileHoldingWorkIsTheOwnersToRaise(t *testing.T) {
	t.Parallel()
	at := monday.Add(-30 * time.Hour)
	stop := func(seat string, from time.Duration, hours int, company bool) advisor.BudgetStop {
		return advisor.BudgetStop{Seat: seat, Turn: seat + "-turn", Company: company, Period: "day",
			Ceiling: 400_000, At: at.Add(from), Resets: at.Add(time.Duration(hours) * time.Hour)}
	}
	open := worklet.OpenWork{Holders: []worklet.HeldOpen{
		{Handle: "dev", Open: 12, Pressing: 3}, {Handle: "qa", Open: 2}, {Handle: "ops", Open: 1}}}
	ev := advisor.Evidence{Work: worklet.WorkPatterns{Open: open}, Stops: []advisor.BudgetStop{
		stop("dev", 0, 5, false), stop("dev", time.Hour, 5, false),
		stop("qa", 0, 3, true), stop("ops", time.Hour, 6, true),
	}}
	found := stalls(t, stallShape, ev, advisor.ClassBudgetStopped)
	if len(found) != 2 {
		t.Fatalf("raised %+v, want dev's own and the company's", found)
	}
	dev, co := found[0], found[1]
	if dev.Subject.Seat == "" {
		dev, co = co, dev
	}
	if dev.Item != "stopped 5 hours, holding 12 open items, 3 urgent or high" || dev.Fix.Kind != advisor.FixConfigChange {
		t.Errorf("dev = %+v, want 5 hours once, holding its work, with a budget change", dev)
	}
	var ceilings []any
	for _, o := range dev.Fix.Options {
		ceilings = append(ceilings, o.Change.Merge["token_budget"].(map[string]any)["day"])
	}
	if !slices.Equal(ceilings, []any{600_000, 800_000}) {
		t.Errorf("dev's options set its day to %v, want one and a half and twice 400,000", ceilings)
	}
	if co.Subject.Kind != advisor.SubjectCompany || !slices.Equal(co.Names, []string{"QA", "Ops"}) ||
		co.Fix.Setting != "token_budget" {
		t.Errorf("the company's = %+v, want QA and Ops named, opening token_budget", co)
	}
	raised := replace(t, stallShape, "token_budget: {day: 400000}", "token_budget: {day: 800000}")
	ev.Stops = ev.Stops[:2]
	if got := stalls(t, raised, ev, advisor.ClassBudgetStopped); len(got) != 0 {
		t.Errorf("a ceiling raised past the refused one still raised %+v", got)
	}
	ev.Work.Open.Holders = nil
	if got := stalls(t, stallShape, ev, advisor.ClassBudgetStopped); len(got) != 0 {
		t.Errorf("a seat holding no open work raised %+v", got)
	}
}

func replace(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("%q not in the shape", old)
	}
	return strings.Replace(s, old, new, 1)
}

// ONE AGENT HOLDING MOST OPEN WORK while another has little is the owner's to share out: 20 items
// and half of what agents hold, with its queued wakes. Mutation: drop the idle-agent gate or
// count a person's items, and a row goes red.
func TestOneAgentHoldingMostOpenWorkIsTheOwnersToShare(t *testing.T) {
	t.Parallel()
	holders := []worklet.HeldOpen{{Handle: "dev", Open: 25, Keys: []string{"ENG-1", "ENG-2"}},
		{Handle: "owner", Open: 40}, {Handle: "qa", Open: 1}, {Handle: "ops", Open: 3}}
	ev := advisor.Evidence{Work: worklet.WorkPatterns{Open: worklet.OpenWork{Holders: holders}},
		Waiting: map[string]int{"dev": 363}}
	found := stalls(t, stallShape, ev, advisor.ClassWorkConcentrated)
	if len(found) != 1 || found[0].Subject.Seat != "dev" ||
		found[0].Item != "25 of 29 open items agents hold, 363 wakes queued" ||
		found[0].Fix.Page != advisor.PageSeat || found[0].Fix.Target != "dev" {
		t.Fatalf("raised %+v, want dev's, naming its wakes, opening its page", found)
	}
	ev.Work.Open.Holders[3].Open, ev.Work.Open.Holders[2].Open = 10, 10
	if got := stalls(t, stallShape, ev, advisor.ClassWorkConcentrated); len(got) != 0 {
		t.Errorf("with no agent idle it raised %+v", got)
	}
}

// AN ITEM FIVE OPEN ITEMS WAIT ON is the critical path, named with its holder;
// one a person holds is said as the person's, not here.
func TestAnItemTheMostWorkWaitsOnIsNamedWithItsHolder(t *testing.T) {
	t.Parallel()
	blockers := []worklet.WaitedOn{
		{Key: "ENG-1", Holder: "dev", Blocks: 12, Waiting: []string{"ENG-2", "ENG-3"}},
		{Key: "ENG-4", Holder: "", Blocks: 5},
		{Key: "ENG-5", Holder: "qa", Blocks: 4},
		{Key: "ENG-6", Holder: "owner", Blocks: 9},
		{Key: "ENG-7", Holder: "dev", Blocks: 7, Asked: []string{"owner"}},
	}
	found := advisor.Detect(advisor.Evidence{Work: worklet.WorkPatterns{Open: worklet.OpenWork{Blockers: blockers}}}, kindOf)
	var got []string
	for _, f := range found {
		if f.Class == advisor.ClassWorkBottleneck {
			got = append(got, f.Item+"|"+f.Subject.Seat+"|"+f.Fix.Target)
		}
	}
	want := []string{"ENG-4: 5 items wait on it||ENG-4", "ENG-1: 12 items wait on it|dev|ENG-1"}
	if !slices.Equal(got, want) {
		t.Errorf("bottlenecks = %v, want %v", got, want)
	}
}

// AGENT WORK THAT NEEDS NO DECISION, BEHIND A PERSON, is named with the person
// and the item: three ready agent items or more.
func TestReadyAgentWorkBehindAPersonIsNamed(t *testing.T) {
	t.Parallel()
	blockers := []worklet.WaitedOn{
		{Key: "ENG-6", Holder: "owner", Blocks: 4, Ready: 3, ReadyKeys: []string{"ENG-8", "ENG-9", "ENG-10"}},
		{Key: "ENG-7", Holder: "dev", Blocks: 5, Ready: 4, Asked: []string{"owner"}, Asks: []string{"c-1"}},
		{Key: "ENG-11", Holder: "owner", Blocks: 2, Ready: 2},
		{Key: "ENG-12", Holder: "dev", Blocks: 6, Ready: 6},
	}
	var got []string
	for _, f := range advisor.Detect(advisor.Evidence{Work: worklet.WorkPatterns{Open: worklet.OpenWork{Blockers: blockers}}}, kindOf) {
		if f.Class == advisor.ClassWorkHeldByPerson {
			got = append(got, f.Item+"|"+f.Subject.Seat)
		}
	}
	want := []string{"ENG-6: 3 agent items wait on it|owner", "ENG-7: 4 agent items wait on it, 1 open question|owner"}
	if !slices.Equal(got, want) {
		t.Errorf("held behind a person = %v, want %v", got, want)
	}
}

// MERGES INTO A BRANCH THAT REQUIRES NO CHECK are one finding per repository,
// each request once however many delegates answered it; a GitHub repository's
// step is at its branch settings, a Gitlet one's in the document.
func TestMergesThatWaitedForWantOfAChecksAreOnePerRepository(t *testing.T) {
	t.Parallel()
	gh := func(req, n string) advisor.UncheckedMerge {
		return advisor.UncheckedMerge{Request: req, Change: "https://github.com/acme/web/pull/" + n, Repo: "acme/web", GitHub: true}
	}
	ev := advisor.Evidence{Merges: []advisor.UncheckedMerge{gh("r1", "1"), gh("r1", "1"), gh("r2", "2"), gh("r3", "3"),
		{Request: "r4", Change: "api!1", Repo: "api"}, {Request: "r5", Change: "api!2", Repo: "api"},
		{Request: "r6", Change: "api!3", Repo: "api"}, {Request: "r7", Change: "docs!1", Repo: "docs"}}}
	byRepo := map[string]advisor.Finding{}
	for _, f := range advisor.Detect(ev, kindOf) {
		if f.Class == advisor.ClassMergeNoChecks {
			byRepo[f.Subject.Name] = f
		}
	}
	web, api := byRepo["acme/web"], byRepo["api"]
	if len(byRepo) != 2 || web.Item != "3 merges" || web.Fix.Kind != advisor.FixExternalStep ||
		web.Fix.URL != "https://github.com/acme/web/settings/branches" || len(web.Records) != 3 {
		t.Errorf("acme/web = %+v, want 3 merges, once each, at its branch settings", web)
	}
	if api.Fix.Kind != advisor.FixOpenSetting || api.Fix.Setting != "code.repos" {
		t.Errorf("api = %+v, want Gitlet's protection in the document", api)
	}
	ev.MergesUnread = true
	for _, f := range advisor.Detect(ev, kindOf) {
		if f.Class == advisor.ClassMergeNoChecks {
			t.Errorf("an unread source raised %+v", f)
		}
	}
}

// richAnswer is an answer in which every evidence class is raised for two
// subjects or more, over a company with a static warning of each kind.
func richAnswer(t *testing.T) advisor.Answer {
	t.Helper()
	at := monday.Add(-30 * time.Hour)
	stop := func(seat string) advisor.BudgetStop {
		return advisor.BudgetStop{Seat: seat, Turn: "t", Period: "day", Ceiling: 400_000, At: at, Resets: at.Add(6 * time.Hour)}
	}
	doc := replace(t, stallShape, "  - name: QA\n    handle: qa\n    llm: p\n",
		"  - name: QA\n    handle: qa\n    llm: p\n    token_budget: {day: 400000}\n")
	ev := advisor.Evidence{
		Work: worklet.WorkPatterns{
			Named:       []worklet.NamedWithoutAsk{requested("owner", 6), requested("admin", 7)},
			Notices:     []worklet.NoticeLoad{{Handle: "owner", Notices: 400}, {Handle: "admin", Notices: 500}},
			IdleReviews: []worklet.IdleReview{{Key: "W-1", Holder: "qa"}, {Key: "W-2", Holder: "dev"}},
			Decisions: []worklet.DecisionTally{{Person: "owner", Asker: "dev", Decided: 20, Recommended: 20},
				{Person: "owner", Asker: "qa", Decided: 21, Recommended: 20}},
			Open: worklet.OpenWork{
				Holders: []worklet.HeldOpen{{Handle: "dev", Open: 3}, {Handle: "qa", Open: 2}},
				Blockers: []worklet.WaitedOn{{Key: "ENG-1", Holder: "dev", Blocks: 5}, {Key: "ENG-2", Holder: "qa", Blocks: 6},
					{Key: "ENG-3", Holder: "owner", Blocks: 3, Ready: 3}, {Key: "ENG-4", Holder: "qa", Blocks: 4, Ready: 4, Asked: []string{"admin"}}}},
		},
		Ledger: []usage.TurnLedgerRow{started("dev", "message", "nothing", 20), started("qa", "message", "nothing", 30),
			absorbed("dev", 10), absorbed("qa", 12), spent("dev", 800_000, 400_000), spent("qa", 900_000, 600_000)},
		Runs: []advisor.EndedRun{capped("dev", "a", 2_000_000, 30), capped("qa", "b", 3_000_000, 30),
			{Seat: "dev", Kind: types.ExecutorErrorOutOfMemory}, {Seat: "dev", Kind: types.ExecutorErrorOutOfMemory},
			{Seat: "qa", Kind: types.ExecutorErrorOutOfMemory}, {Seat: "qa", Kind: types.ExecutorErrorOutOfMemory}},
		Stops: []advisor.BudgetStop{stop("dev"), stop("qa")},
		Merges: []advisor.UncheckedMerge{{Request: "1", Repo: "a"}, {Request: "2", Repo: "a"}, {Request: "3", Repo: "a"},
			{Request: "4", Repo: "b"}, {Request: "5", Repo: "b"}, {Request: "6", Repo: "b"}},
	}
	in := advisor.Inputs{Company: company(t, doc), Revision: "r1", Integrations: []integration.State{}}
	return advisor.ReadWithEvidence(in, advisor.Evidence{Until: monday, Work: ev.Work, Ledger: ev.Ledger,
		Runs: ev.Runs, Stops: ev.Stops, Merges: ev.Merges})
}

// EVERY CLASS SAYS ONE THING ABOUT EVERY SUBJECT, so a class reads as one row naming them all;
// only the subject, item, facts and records differ. Each class here is raised for two subjects.
// Mutation: put a seat's number back into a class's why, and its row splits.
func TestEveryClassSaysOneThingAboutEverySubject(t *testing.T) {
	t.Parallel()
	answer := richAnswer(t)
	byClass := map[advisor.Class][]advisor.Finding{}
	for _, f := range answer.Findings {
		byClass[f.Class] = append(byClass[f.Class], f)
	}
	for _, class := range []advisor.Class{advisor.ClassPersonRequestsWithoutAsk, advisor.ClassNoticeLoad,
		advisor.ClassReviewIdle,
		advisor.ClassRedeliveredSeen, advisor.ClassAuxiliaryShare, advisor.ClassSandboxOutOfMemory,
		advisor.ClassRoundCap, advisor.ClassBudgetStopped, advisor.ClassWorkBottleneck,
		advisor.ClassWorkHeldByPerson, advisor.ClassMergeNoChecks} {
		found := byClass[class]
		if len(found) < 2 {
			t.Errorf("%s: %d findings, want two or more to compare", class, len(found))
			continue
		}
		for _, f := range found[1:] {
			if f.Title != found[0].Title || f.Why != found[0].Why || f.Fix.Kind != found[0].Fix.Kind {
				t.Errorf("%s says two things: %q / %q and %q / %q", class, found[0].Title, found[0].Why, f.Title, f.Why)
			}
		}
	}
}
