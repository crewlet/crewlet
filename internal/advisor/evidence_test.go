package advisor_test

import (
	"go/build"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// kindOf names owner a person, ghost no seat, and every other handle an agent.
func kindOf(handle string) advisor.SeatKind {
	switch handle {
	case "owner":
		return advisor.SeatPerson
	case "ghost":
		return advisor.SeatUnknown
	}
	return advisor.SeatAgent
}

func named(handle string, told, woken int) worklet.NamedWithoutAsk {
	return worklet.NamedWithoutAsk{Handle: handle, Told: told, Woken: woken,
		Comments: []string{"c-1"}, WokenComments: []string{"c-1"}}
}

// EACH DETECTOR FIRES AT ITS THRESHOLD AND IS SILENT ONE BELOW IT, so a
// threshold moved by accident is a red case rather than a quieter screen.
func TestEachDetectorFiresAtItsThresholdAndNotBelow(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		work worklet.WorkPatterns
		want []advisor.Class
	}{
		"5 wakes in 500 agent comments": {worklet.WorkPatterns{AgentComments: 500,
			Named: []worklet.NamedWithoutAsk{named("pm", 3, 3), named("qa", 2, 2)}},
			[]advisor.Class{advisor.ClassAgentMentionsWithoutAsk}},
		"4 wakes": {worklet.WorkPatterns{AgentComments: 10,
			Named: []worklet.NamedWithoutAsk{named("pm", 4, 4)}}, nil},
		"5 wakes in 501 agent comments": {worklet.WorkPatterns{AgentComments: 501,
			Named: []worklet.NamedWithoutAsk{named("pm", 5, 5)}}, nil},
		"5 references that woke nobody": {worklet.WorkPatterns{AgentComments: 5,
			Named: []worklet.NamedWithoutAsk{named("pm", 5, 0)}}, nil},
		"5 hand-offs that said they ask": {worklet.WorkPatterns{AgentComments: 5,
			Named: []worklet.NamedWithoutAsk{{Handle: "pm", Told: 5, Asked: 5}}}, nil},
		"a removed seat named 5 times": {worklet.WorkPatterns{AgentComments: 5,
			Named: []worklet.NamedWithoutAsk{named("ghost", 5, 5)}}, nil},
		"a person named 5 times": {worklet.WorkPatterns{AgentComments: 5,
			Named: []worklet.NamedWithoutAsk{named("owner", 5, 5)}},
			[]advisor.Class{advisor.ClassPersonRequestsWithoutAsk}},
		"a person named 4 times": {worklet.WorkPatterns{AgentComments: 4,
			Named: []worklet.NamedWithoutAsk{named("owner", 4, 4)}}, nil},
		"3 overtaken of 60 answers": {worklet.WorkPatterns{Answers: 60, Superseded: 1, Moot: 2},
			[]advisor.Class{advisor.ClassAskOvertaken}},
		"3 overtaken of 61 answers": {worklet.WorkPatterns{Answers: 61, Superseded: 1, Moot: 2}, nil},
		"2 overtaken of 2 answers":  {worklet.WorkPatterns{Answers: 2, Moot: 2}, nil},
		"one idle review": {worklet.WorkPatterns{IdleReviews: []worklet.IdleReview{{Key: "W-1", Holder: "qa"}}},
			[]advisor.Class{advisor.ClassReviewIdle}},
		"no idle review": {worklet.WorkPatterns{IdleReviews: []worklet.IdleReview{}}, nil},
		"350 notices, 87 put to the person": {worklet.WorkPatterns{Notices: []worklet.NoticeLoad{
			{Handle: "owner", Notices: 350, Addressed: 87}}}, []advisor.Class{advisor.ClassNoticeLoad}},
		"349 notices": {worklet.WorkPatterns{Notices: []worklet.NoticeLoad{
			{Handle: "owner", Notices: 349, Addressed: 0}}}, nil},
		"400 notices, a quarter put to the person": {worklet.WorkPatterns{Notices: []worklet.NoticeLoad{
			{Handle: "owner", Notices: 400, Addressed: 100}}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got []advisor.Class
			for _, f := range advisor.Detect(advisor.Evidence{Work: tc.work}, kindOf) {
				got = append(got, f.Class)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("raised %v, want %v", got, tc.want)
			}
		})
	}
}

// started is one ledger row of a seat's started turns under wake with outcome.
func started(seat, wake, outcome string, turns int64) usage.TurnLedgerRow {
	return usage.TurnLedgerRow{AgentID: "id-" + seat, Handle: seat, Wake: wake,
		Disposition: "started", Outcome: outcome, Turns: turns}
}

// spent is a seat's started turns that spent tokens of their own and auxiliary.
// absorbed is a seat's wakes a completed turn had already read past.
func absorbed(seat string, wakes int64) usage.TurnLedgerRow {
	return usage.TurnLedgerRow{AgentID: "id-" + seat, Handle: seat, Wake: "work_item:comment",
		Disposition: "absorbed", Turns: wakes}
}

func spent(seat string, own, aux int64) usage.TurnLedgerRow {
	r := started(seat, "chat:message", "moved", 1)
	r.Input, r.AuxInput = own, aux
	return r
}

// EACH LEDGER DETECTOR FIRES AT ITS THRESHOLD AND IS SILENT ONE BELOW IT, and a
// wake that was held, never started, counts toward none.
func TestEachLedgerDetectorFiresAtItsThresholdAndNotBelow(t *testing.T) {
	t.Parallel()
	const pickup = "work_item:pickup"
	held := usage.TurnLedgerRow{AgentID: "id-app", Handle: "app", Wake: pickup, Disposition: "held", Turns: 500}
	for name, tc := range map[string]struct {
		ledger []usage.TurnLedgerRow
		want   []advisor.Class
	}{
		"5 of 20 turns did nothing": {[]usage.TurnLedgerRow{started("app", "chat:message", "nothing", 5),
			started("app", "chat:message", "moved", 15)}, []advisor.Class{advisor.ClassNoopShare}},
		"4 of 20 turns did nothing": {[]usage.TurnLedgerRow{started("app", "chat:message", "nothing", 4),
			started("app", "chat:message", "moved", 16)}, nil},
		"19 turns, all nothing": {[]usage.TurnLedgerRow{started("app", "chat:message", "nothing", 19), held}, nil},
		"2 of 10 pickups said words": {[]usage.TurnLedgerRow{started("app", pickup, "words", 1),
			started("qa", pickup, "words", 1), started("qa", pickup, "moved", 8)},
			[]advisor.Class{advisor.ClassPickupStillWaiting}},
		"1 of 10 pickups said words": {[]usage.TurnLedgerRow{started("app", pickup, "words", 1),
			started("qa", pickup, "moved", 9)}, nil},
		"9 pickups, all words":     {[]usage.TurnLedgerRow{started("app", pickup, "words", 9), held}, nil},
		"words under another wake": {[]usage.TurnLedgerRow{started("app", "chat:message", "words", 10)}, nil},
		"more than 15% auxiliary of a million": {[]usage.TurnLedgerRow{spent("sre", 849_999, 150_001)},
			[]advisor.Class{advisor.ClassAuxiliaryShare}},
		"15% auxiliary of a million":      {[]usage.TurnLedgerRow{spent("sre", 850_000, 150_000)}, nil},
		"half auxiliary, under a million": {[]usage.TurnLedgerRow{spent("sre", 499_999, 499_999)}, nil},
		"10 of 50 wakes already read": {[]usage.TurnLedgerRow{started("qa", "chat:message", "moved", 40),
			absorbed("qa", 10)}, []advisor.Class{advisor.ClassRedeliveredSeen}},
		"10 of 51 wakes already read": {[]usage.TurnLedgerRow{started("qa", "chat:message", "moved", 41),
			absorbed("qa", 10)}, nil},
		"9 wakes already read, all of them": {[]usage.TurnLedgerRow{absorbed("qa", 9)}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got []advisor.Class
			for _, f := range advisor.Detect(advisor.Evidence{Ledger: tc.ledger}, kindOf) {
				got = append(got, f.Class)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("raised %v, want %v", got, tc.want)
			}
		})
	}
}

// A HEALTHY WEEK'S LEDGER RAISES NOTHING: agents that mostly move work, pickups
// that move what they pick up, modest auxiliary spend, and an SRE seat whose
// schedules fire 48 times a day and find nothing, which is the schedule working.
func TestAHealthyLedgerRaisesNothing(t *testing.T) {
	t.Parallel()
	const pickup = "work_item:pickup"
	healthy := []usage.TurnLedgerRow{
		started("pm", "chat:message", "moved", 40), started("pm", "chat:message", "words", 15),
		started("pm", "chat:message", "nothing", 5),
		started("app", pickup, "moved", 25), started("app", pickup, "words", 4),
		started("app", pickup, "nothing", 1),
		started("sre", "schedule:health-check", "nothing", 336), started("sre", "chat:message", "moved", 6),
		absorbed("pm", 3),
		spent("backend", 2_000_000, 200_000),
	}
	if got := advisor.Detect(advisor.Evidence{Ledger: healthy}, kindOf); len(got) != 0 {
		t.Errorf("a healthy week raised %v", got)
	}
}

// A LEDGER FINDING NAMES ITS SEAT by the handle the ledger carries, and by the
// seat's id when the ledger carries none, so no finding is about nobody.
func TestALedgerFindingNamesItsSeat(t *testing.T) {
	t.Parallel()
	unnamed := started("", "chat:message", "nothing", 20)
	unnamed.AgentID = "id-gone"
	found := advisor.Detect(advisor.Evidence{Ledger: []usage.TurnLedgerRow{
		started("app", "chat:message", "nothing", 20), unnamed}}, kindOf)
	var seats []string
	for _, f := range found {
		seats = append(seats, f.Subject.Seat)
	}
	if !slices.Equal(seats, []string{"app", "id-gone"}) {
		t.Errorf("the no-op findings name %v, want app and the unnamed seat's id", seats)
	}
}

// THE BACKUP FINDING IS THE OWNER'S AND BLOCKS, while the backup alarm stands
// and only then; it is a step outside the document, with nothing to send.
func TestTheBackupFindingBlocksWhileTheBackupAlarmStands(t *testing.T) {
	t.Parallel()
	if got := advisor.Detect(advisor.Evidence{}, kindOf); len(got) != 0 {
		t.Fatalf("no standing alarm raised %v", got)
	}
	got := advisor.Detect(advisor.Evidence{Backup: advisor.Backup{Standing: true, Logs: 12}}, kindOf)
	if len(got) != 1 {
		t.Fatalf("a standing backup alarm raised %d findings, want 1", len(got))
	}
	f := got[0]
	if f.Class != advisor.ClassNoVerifiedBackup || f.Severity != advisor.SeverityBlocks ||
		f.Actor != advisor.ActorOwner || f.Fix.Kind != advisor.FixExternalStep ||
		len(f.Fix.Options) != 0 || f.Fix.Sentence == "" {
		t.Errorf("the backup finding is %+v, want the owner's, blocking, an external step", f)
	}
	if len(f.Evidence) != 1 || f.Evidence[0] != (advisor.Fact{Key: "logs", Value: 12}) {
		t.Errorf("the backup finding's evidence is %v, want logs=12", f.Evidence)
	}
}

// AN EVIDENCE FINDING IS THE ENGINE'S, never the owner's to-do: it offers no
// change to send, and names at most five records behind it.
func TestAnEvidenceFindingIsTheEnginesAndOffersNothingToSend(t *testing.T) {
	t.Parallel()
	many := make([]string, 9)
	for i := range many {
		many[i] = "c-" + strconv.Itoa(i)
	}
	work := worklet.WorkPatterns{AgentComments: 9, Answers: 3, Moot: 3, Overtaken: many[:5],
		Named: []worklet.NamedWithoutAsk{{Handle: "pm", Told: 9, Woken: 9, WokenComments: many[:5]},
			{Handle: "qa", Told: 4, Woken: 4, WokenComments: many[5:]}, {Handle: "owner", Told: 6, Comments: many[:5]}},
		IdleReviews: []worklet.IdleReview{{Task: "t-1", Key: "W-1", Holder: "qa"}},
		Notices:     []worklet.NoticeLoad{{Handle: "owner", Notices: 400}}}
	ledger := []usage.TurnLedgerRow{started("app", "work_item:pickup", "words", 10),
		started("app", "work_item:pickup", "nothing", 10), spent("app", 500_000, 500_000), absorbed("app", 10)}
	found := advisor.Detect(advisor.Evidence{Work: work, Ledger: ledger}, kindOf)
	// Every class but the owner's two (approval.rubber_stamp and
	// integration.inbound_silent), each held on its own.
	if len(found) != len(advisor.WorkClasses())-2+len(advisor.LedgerClasses()) {
		t.Fatalf("raised %d findings, want one per engine class the work patterns and the ledger raise", len(found))
	}
	// A COUNT NAMES NO RECORD: these classes read totals, not rows to open.
	counts := append([]advisor.Class{advisor.ClassNoticeLoad}, advisor.LedgerClasses()...)
	for _, f := range found {
		if f.Actor != advisor.ActorEngine || f.Source != advisor.SourceEvidence ||
			f.Fix.Kind != advisor.FixNone || len(f.Fix.Options) != 0 {
			t.Errorf("%s is %s from %s with a %s fix of %d options, want the engine's, "+
				"from evidence, with nothing to send", f.Class, f.Actor, f.Source, f.Fix.Kind, len(f.Fix.Options))
		}
		if len(f.Records) > worklet.MaxPatternExamples ||
			len(f.Records) == 0 != slices.Contains(counts, f.Class) {
			t.Errorf("%s names %d records, want 1 to %d, or none for a count", f.Class, len(f.Records),
				worklet.MaxPatternExamples)
		}
		if f.Title == "" || f.Why == "" || f.Fix.Sentence == "" || len(f.Evidence) == 0 {
			t.Errorf("%s has no words or no evidence: %+v", f.Class, f)
		}
	}
}

// NO DETECTOR CALLS A MODEL: a finding is a fixed reading of records, and a
// model choosing one would let words in a comment steer what the owner is told.
func TestNoDetectorCallsAModel(t *testing.T) {
	t.Parallel()
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		for _, banned := range []string{"internal/providers", "internal/assistant", "internal/agent",
			"internal/learning"} {
			if strings.Contains(imp, banned) {
				t.Errorf("internal/advisor imports %s: a detector reads records and calls no model", imp)
			}
		}
	}
}

// THE ENGINE'S PATTERNS SIT BESIDE THE DOCUMENT'S FINDINGS, a seat named as
// the chart names it, and the document's own findings unchanged.
func TestReadWithEvidenceAddsTheEnginesPatternsToTheDocuments(t *testing.T) {
	t.Parallel()
	c := liveShape(t)
	in := advisor.Inputs{Company: c, Revision: "rev-1", Integrations: []integration.State{}}
	static := advisor.Read(in)
	work := worklet.WorkPatterns{AgentComments: 20,
		Named: []worklet.NamedWithoutAsk{named("pm", 6, 6), named("owner", 5, 5)}}
	got := advisor.ReadWithEvidence(in, advisor.Evidence{Work: work})
	var engine []string
	for _, f := range got.Findings {
		if f.Actor == advisor.ActorEngine {
			engine = append(engine, string(f.Class)+":"+f.Subject.Name)
		}
	}
	slices.Sort(engine)
	want := []string{"person.requests_without_ask:Owner", "wake.agent_mentions_without_ask:"}
	if !slices.Equal(engine, want) {
		t.Errorf("engine findings %v, want %v", engine, want)
	}
	if len(got.Findings) != len(static.Findings)+len(want) || got.Revision != "rev-1" {
		t.Errorf("%d findings at %s, want the document's %d and %d more at rev-1",
			len(got.Findings), got.Revision, len(static.Findings), len(want))
	}
}

// THE TRACKER READ COVERS THE WEEK BEFORE NOW and calls a review idle after a
// day untouched, over every agent seat and no person.
func TestThePatternsWindowIsAWeekAndAReviewIsIdleAfterADay(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	q := advisor.PatternsWindow(now, liveShape(t))
	if !q.Until.Equal(now) || !q.Since.Equal(now.Add(-7*24*time.Hour)) ||
		!q.QuietSince.Equal(now.Add(-24*time.Hour)) {
		t.Errorf("the window is %s to %s, quiet since %s; want the week before %s and a day", q.Since,
			q.Until, q.QuietSince, now)
	}
	if !slices.Equal(q.Reviewers, []string{"pm", "designer", "app", "backend", "qa", "sre"}) {
		t.Errorf("the reviewers are %v, want every agent seat in the chart's order", q.Reviewers)
	}
	if !slices.Equal(q.People, []string{"owner"}) {
		t.Errorf("the notice loads read are %v, want every person and no agent", q.People)
	}
}

// A SOURCE THE CALLER COULD NOT READ RAISES NOTHING AND IS NAMED: what would
// have fired stays silent, and the answer says the list may be short.
func TestAnUnreadSourceRaisesNothingAndIsNamed(t *testing.T) {
	t.Parallel()
	firing := advisor.Evidence{Work: worklet.WorkPatterns{Answers: 4, Moot: 4, Overtaken: []string{"c-1"}},
		Ledger: []usage.TurnLedgerRow{started("app", "chat:message", "nothing", 20)},
		Backup: advisor.Backup{Standing: true}}
	classes := []advisor.Class{advisor.ClassAskOvertaken, advisor.ClassNoopShare, advisor.ClassNoVerifiedBackup}
	in := advisor.Inputs{Company: liveShape(t), Revision: "rev-1", Integrations: []integration.State{}}
	read := advisor.ReadWithEvidence(in, firing)
	if !slices.Equal(read.Unread, []string{}) {
		t.Fatalf("with every source read, unread %v", read.Unread)
	}
	for _, c := range classes {
		if !hasClass(read, c) {
			t.Fatalf("with every source read, %s did not fire: %v", c, read.Findings)
		}
	}
	blind := firing
	blind.WorkUnread, blind.LedgerUnread, blind.AlarmsUnread = true, true, true
	answer := advisor.ReadWithEvidence(in, blind)
	if slices.ContainsFunc(classes, func(c advisor.Class) bool { return hasClass(answer, c) }) {
		t.Errorf("an unread source still raised its classes: %v", answer.Findings)
	}
	if !slices.Equal(answer.Unread, []string{advisor.UnreadWork, advisor.UnreadLedger, advisor.UnreadAlarms}) {
		t.Errorf("unread %v, want work, the ledger and alarms named", answer.Unread)
	}
}

func hasClass(a advisor.Answer, class advisor.Class) bool {
	return slices.ContainsFunc(a.Findings, func(f advisor.Finding) bool { return f.Class == class })
}

// A PERSON WHO TAKES ONE AGENT'S RECOMMENDATION NEARLY EVERY TIME is told so,
// as the owner's call: the fix is a draft Ask Crewlet writes for review, never
// a change, and below the threshold nothing is said.
// ONE FINDING PER AGENT, naming every person who takes its recommendation:
// two people stamping one agent are one row, and one person stamping two
// agents is a row for each, so no two findings share their row's key.
func TestARubberStampIsOneFindingPerAgent(t *testing.T) {
	t.Parallel()
	work := worklet.WorkPatterns{Decisions: []worklet.DecisionTally{
		{Person: "admin2", Asker: "pm", Decided: 22, Recommended: 21},
		{Person: "owner", Asker: "cto", Decided: 20, Recommended: 20},
		{Person: "owner", Asker: "pm", Decided: 20, Recommended: 18},
	}}
	found := advisor.Detect(advisor.Evidence{Work: work}, kindOf)
	if len(found) != 2 {
		t.Fatalf("raised %d findings, want one for cto and one for pm: %+v", len(found), found)
	}
	bySeat := map[string]advisor.Finding{}
	for _, f := range found {
		bySeat[f.Subject.Seat] = f
	}
	pm := bySeat["pm"]
	if !strings.Contains(pm.Title, "admin2 and owner") || !strings.Contains(pm.Title, "39 of 42") {
		t.Errorf("pm's finding says %q, want both people and their 39 of 42", pm.Title)
	}
	if bySeat["cto"].Title == "" || strings.Contains(bySeat["cto"].Title, "admin2") {
		t.Errorf("cto's finding = %+v, want owner's alone", bySeat["cto"])
	}
}

func TestDecisionsTakenAsRecommendedAreTheOwnersToWeigh(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		decided, recommended int
		want                 bool
	}{
		{"nine in ten of twenty", 20, 18, true},
		{"every one of thirty", 30, 30, true},
		{"too few to say", 19, 19, false},
		{"often declined", 20, 17, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			work := worklet.WorkPatterns{Decisions: []worklet.DecisionTally{
				{Person: "owner", Asker: "pm", Decided: tc.decided, Recommended: tc.recommended}}}
			found := advisor.Detect(advisor.Evidence{Work: work}, kindOf)
			if (len(found) == 1) != tc.want || len(found) > 1 {
				t.Fatalf("raised %+v, want a finding %v", found, tc.want)
			}
			if !tc.want {
				return
			}
			f := found[0]
			if f.Class != advisor.ClassApprovalRubberStamp || f.Actor != advisor.ActorOwner ||
				f.Severity != advisor.SeverityNote || f.Subject.Seat != "pm" ||
				f.Fix.Kind != advisor.FixDraftWithAssistant || len(f.Fix.Options) != 0 {
				t.Errorf("finding = %+v, want the owner's note about pm with a draft to review", f)
			}
			for _, want := range []string{"owner", "pm", strconv.Itoa(tc.decided), "Show me the change"} {
				if !strings.Contains(f.Fix.Prompt, want) {
					t.Errorf("the draft's prompt %q does not say %q", f.Fix.Prompt, want)
				}
			}
		})
	}
}

// A SEAT THE ENGINE PAUSED FOR A FAILURE IS THE OWNER'S TO FIX: one blocking
// finding per failure kind, naming every agent it holds, with that kind's fix;
// a kind this build does not know raises nothing, and an unread store nothing.
func TestASeatPausedForAFailureIsOneFindingPerKind(t *testing.T) {
	t.Parallel()
	ev := advisor.Evidence{Paused: []advisor.PausedSeat{
		{Handle: "ops", Name: "Ops", Failure: "sandbox_configuration"},
		{Handle: "app", Name: "App", Failure: "sandbox_configuration"},
		{Handle: "pm", Name: "PM", Failure: "models_refused"},
		{Handle: "qa", Name: "QA", Failure: "a_later_kind"},
	}}
	found := advisor.Detect(ev, kindOf)
	byClass := map[advisor.Class]advisor.Finding{}
	for _, f := range found {
		byClass[f.Class] = f
	}
	if len(found) != 2 {
		t.Fatalf("raised %+v, want one finding for the sandbox and one for the models", found)
	}
	box := byClass[advisor.ClassSandboxUnstartable]
	if box.Severity != advisor.SeverityBlocks || box.Actor != advisor.ActorOwner ||
		box.Subject.Kind != advisor.SubjectCompany || !strings.Contains(box.Title, "App and Ops are paused") ||
		box.Fix.Kind != advisor.FixOpenSetting || box.Fix.Setting != "providers.sandbox" {
		t.Errorf("the sandbox finding = %+v, want the owner's block naming both, opening the sandbox", box)
	}
	models := byClass[advisor.ClassModelsCredential]
	if models.Subject.Seat != "pm" || !strings.Contains(models.Title, "PM is paused") ||
		models.Fix.Kind != advisor.FixExternalStep || !strings.Contains(models.Fix.Sentence, "Settings, Models") {
		t.Errorf("the models finding = %+v, want PM's, its credentials replaced under Settings", models)
	}
	ev.PausesUnread = true
	if found := advisor.Detect(ev, kindOf); len(found) != 0 {
		t.Errorf("an unread pause store raised %+v", found)
	}
}

// ONLY THE POLL SETTLING SAYS THE WEBHOOK IS SILENT: a pull request the poll
// settled after the webhook's newest is the owner's to look into, and a
// delivery since clears it.
func TestOnlyThePollSettlingSaysTheWebhookIsSilent(t *testing.T) {
	t.Parallel()
	at := func(h int) time.Time { return time.Date(2026, 3, 6, h, 0, 0, 0, time.UTC) }
	settled := func(polled, polledAt, delivered, deliveredAt int) []worklet.ChangeSettles {
		var out []worklet.ChangeSettles
		if polled > 0 {
			out = append(out, worklet.ChangeSettles{Source: worklet.SettledByPoll, Count: polled, Newest: at(polledAt)})
		}
		if delivered > 0 {
			out = append(out, worklet.ChangeSettles{Source: worklet.SettledByWebhook, Count: delivered,
				Newest: at(deliveredAt)})
		}
		return out
	}
	silent := func(s []worklet.ChangeSettles) []advisor.Finding {
		var out []advisor.Finding
		for _, f := range advisor.Detect(advisor.Evidence{Work: worklet.WorkPatterns{Settled: s}}, kindOf) {
			if f.Class == advisor.ClassInboundSilent {
				out = append(out, f)
			}
		}
		return out
	}
	found := silent(settled(2, 9, 1, 8))
	if len(found) != 1 {
		t.Fatalf("the poll settled after the webhook's newest and raised %d findings, want one", len(found))
	}
	f := found[0]
	if f.Actor != advisor.ActorOwner || f.Severity != advisor.SeverityCosts ||
		f.Subject.Kind != advisor.SubjectIntegration || f.Subject.Name != "GitHub" ||
		f.Fix.Kind != advisor.FixExternalStep || f.Fix.Setting != "github" || len(f.Evidence) != 2 {
		t.Errorf("the finding is %+v, want the owner's external step on GitHub with its two counts", f)
	}
	for name, s := range map[string][]worklet.ChangeSettles{
		"a delivery since":               settled(2, 9, 1, 10),
		"only deliveries":                settled(0, 0, 3, 9),
		"nothing settled":                nil,
		"a delivery at the same instant": settled(1, 9, 1, 9),
	} {
		if got := silent(s); len(got) != 0 {
			t.Errorf("%s raised %+v", name, got)
		}
	}
}
