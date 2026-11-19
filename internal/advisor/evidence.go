package advisor

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The classes a finding about what the company did can have: patterns the
// engine is reducing, shown to the owner as information and never as a to-do.
const (
	ClassAgentMentionsWithoutAsk  Class = "wake.agent_mentions_without_ask"
	ClassPersonRequestsWithoutAsk Class = "person.requests_without_ask"
	ClassAskOvertaken             Class = "ask.overtaken"
	ClassReviewIdle               Class = "review.idle"
	ClassNoticeLoad               Class = "person.notice_load"
	ClassNoopShare                Class = "wake.noop_share"
	ClassPickupStillWaiting       Class = "work.pickup_still_waiting"
	ClassAuxiliaryShare           Class = "spend.auxiliary_share"
	ClassRedeliveredSeen          Class = "wake.redelivered_seen"
	// ClassNoVerifiedBackup is the owner's: no backup within the policy's age.
	ClassNoVerifiedBackup Class = "durability.no_verified_backup"
	// ClassApprovalRubberStamp is the owner's: a person chose one agent's
	// recommendation on almost every decision it asked of them.
	ClassApprovalRubberStamp Class = "approval.rubber_stamp"
	// ClassInboundSilent is the owner's: the pull requests work waited on
	// reached it only through the engine's poll, so GitHub's webhook is not
	// reaching the company (ADR-0121).
	ClassInboundSilent Class = "integration.inbound_silent"
)

// AreaWork holds the patterns in how the company's work moves, and
// AreaDurability what keeps the company's records safe.
const (
	AreaWork       Area = "work"
	AreaDurability Area = "durability"
	AreaApprovals  Area = "approvals"
)

// Window is how far back every evidence class reads: a week holds every
// weekday's schedule and the slow answers of a person away for a day.
// ReviewQuiet is how long a review its agent holder has not touched waits
// before it is idle: a day, because an agent takes many turns a day.
const (
	Window      = 7 * 24 * time.Hour
	ReviewQuiet = 24 * time.Hour
)

// PatternsWindow is the tracker read the evidence classes need at now: the
// window before it, every agent seat's reviews, quiet since ReviewQuiet ago,
// and every person's notices. The caller names the read level.
func PatternsWindow(now time.Time, c *config.Company) worklet.PatternsQuery {
	return worklet.PatternsQuery{Since: now.Add(-Window), Until: now,
		Reviewers: AgentHandles(c), QuietSince: now.Add(-ReviewQuiet), People: personHandles(c),
		Agents: AgentHandles(c)}
}

// LedgerDays is how many company days the turn ledger is read over: the
// window's week, today included.
const LedgerDays = 7

// Evidence is what the company did in the window that ends at Until.
type Evidence struct {
	Until time.Time
	Work  worklet.WorkPatterns
	// Ledger is every seat's turns by wake and outcome over [LedgerDays].
	Ledger []usage.TurnLedgerRow
	// Backup is the node's backup alarm as its latest evaluation stands.
	Backup Backup
	// Paused is every agent seat the engine paused for a failure (ADR-0117).
	Paused []PausedSeat
	// Runs is every executor run in the window that ended for memory or on
	// its round cap.
	Runs []EndedRun
	// Fires is every schedule fire in the window whose turn could be read.
	Fires []ScheduleFire
	// Stops is every turn in the window refused on a token budget.
	Stops []BudgetStop
	// Merges is every merge in the window a delegate allowed and the engine
	// put to people because its branch requires no check.
	Merges []UncheckedMerge
	// Waiting is each agent seat's wakes queued now, by handle, where its
	// holder reported them.
	Waiting map[string]int
	// The Unread flags say the caller could not read that source, so its
	// classes raise nothing and the answer names it.
	WorkUnread, LedgerUnread, AlarmsUnread, PausesUnread, RunsUnread, FiresUnread bool
	StopsUnread, MergesUnread                                                     bool
}

// source is the record a detector reads.
type source int

const (
	// sourceWork is the tracker's [worklet.WorkPatterns].
	sourceWork source = iota
	// sourceLedger is the turn ledger, [Evidence.Ledger].
	sourceLedger
	// sourceAlarms is the node's alarms, [Evidence.Backup].
	sourceAlarms
	// sourcePauses is the seats' pauses, [Evidence.Paused].
	sourcePauses
	// sourceRuns is the executor runs' records, [Evidence.MemoryKills].
	sourceRuns
	// sourceFires is the schedules' fires and what their turns wrote, [Evidence.Fires].
	sourceFires
	// sourceStops is the turns refused on a budget, [Evidence.Stops].
	sourceStops
	// sourceMerges is the access log's merge escalations, [Evidence.Merges].
	sourceMerges
)

// unread reports whether ev could not read src.
func (ev Evidence) unread(src source) bool {
	switch src {
	case sourceWork:
		return ev.WorkUnread
	case sourceLedger:
		return ev.LedgerUnread
	case sourceAlarms:
		return ev.AlarmsUnread
	case sourcePauses:
		return ev.PausesUnread
	case sourceRuns:
		return ev.RunsUnread
	case sourceFires:
		return ev.FiresUnread
	case sourceStops:
		return ev.StopsUnread
	case sourceMerges:
		return ev.MergesUnread
	}
	panic(fmt.Sprintf("advisor: a detector reads source %d, which is none", src))
}

// Backup is whether the backup alarm stands, and over how many logs.
type Backup struct {
	Standing bool
	Logs     int
}

// SeatKind is what a handle names in the company document.
type SeatKind int

// The kinds a handle can name: an agent seat, a person's seat, or no seat.
const (
	SeatUnknown SeatKind = iota
	SeatAgent
	SeatPerson
)

// detector raises one class's findings from the evidence; kind tells an
// agent seat's handle from a person's. It reads records only and calls no model.
type detector struct {
	class  Class
	source source
	detect func(ev Evidence, kind func(string) SeatKind) []Finding
}

// detectors is every evidence class's detector.
var detectors = []detector{
	{ClassAgentMentionsWithoutAsk, sourceWork, agentMentionsWithoutAsk},
	{ClassPersonRequestsWithoutAsk, sourceWork, personRequestsWithoutAsk},
	{ClassAskOvertaken, sourceWork, askOvertaken},
	{ClassReviewIdle, sourceWork, reviewIdle},
	{ClassNoticeLoad, sourceWork, noticeLoad},
	{ClassNoopShare, sourceLedger, noopShare},
	{ClassPickupStillWaiting, sourceLedger, pickupStillWaiting},
	{ClassAuxiliaryShare, sourceLedger, auxiliaryShare},
	{ClassRedeliveredSeen, sourceLedger, redeliveredSeen},
	{ClassNoVerifiedBackup, sourceAlarms, noVerifiedBackup},
	{ClassApprovalRubberStamp, sourceWork, approvalRubberStamp},
	{ClassSandboxOutOfMemory, sourceRuns, sandboxOutOfMemory},
	{ClassInboundSilent, sourceWork, inboundSilent},
	{ClassWorkBottleneck, sourceWork, workBottleneck},
	{ClassWorkHeldByPerson, sourceWork, workHeldByPerson},
	{ClassMergeNoChecks, sourceMerges, mergeNoChecks},
}

// Thresholds, each the point where a pattern stops being a slip: routing on
// asks makes the first 0 by design, and a person's Inbox should never hold
// the other two at all.
const (
	// minNamedWakes and namedWakesPer100: 5 wakes a week, and 1 per 100 agent comments.
	minNamedWakes    = 5
	namedWakesPer100 = 1
	// minNamedPerson: a person named 5 times a week with no question asked.
	minNamedPerson = 5
	// minOvertaken and overtakenPercent: 3 answers a week, and 5% of them.
	minOvertaken     = 3
	overtakenPercent = 5
	// minDecided and recommendedPercent: about three decisions a working day
	// from one agent, nine in ten of them taken as it recommended.
	minDecided         = 20
	recommendedPercent = 90
)

// The load and ledger thresholds, each a share no healthy week reaches.
const (
	// minNotices and addressedPercent: 50 notices a day, under a quarter of
	// them put to the person, make an Inbox a feed nobody reads.
	minNotices       = 350
	addressedPercent = 25
	// minStartedTurns and noopPercent: 20 turns a week, a quarter of them
	// committing nothing.
	minStartedTurns = 20
	noopPercent     = 25
	// minAbsorbed and absorbedPercent: 10 wakes a week, a fifth of all that
	// woke the seat, for changes a turn of it had already read.
	minAbsorbed     = 10
	absorbedPercent = 20
	// minPickups and stillWaitingPercent: 10 pickups a week, a fifth of them
	// ending in words only, which says the work handed out could not move.
	minPickups          = 10
	stillWaitingPercent = 20
	// minSeatTokens and auxiliaryPercent: a million tokens a week, more than
	// 15% of them spent on in-turn auxiliary calls.
	minSeatTokens    = 1_000_000
	auxiliaryPercent = 15
)

// EvidenceClasses is every class a detector, a paused seat or a schedule's fires raise.
func EvidenceClasses() []Class {
	out := make([]Class, 0, len(detectors))
	for _, d := range detectors {
		out = append(out, d.class)
	}
	for _, d := range documented {
		out = append(out, d.class)
	}
	return append(append(out, PausedClasses()...), ClassScheduleUnproductive)
}

// WorkClasses is every class a detector raises from the tracker's patterns.
func WorkClasses() []Class { return classesOf(sourceWork) }

// LedgerClasses is every class a detector raises from the turn ledger.
func LedgerClasses() []Class { return classesOf(sourceLedger) }

func classesOf(src source) []Class {
	var out []Class
	for _, d := range detectors {
		if d.source == src {
			out = append(out, d.class)
		}
	}
	return out
}

// Detect is every evidence finding ev raises, the most costly first.
func Detect(ev Evidence, kind func(handle string) SeatKind) []Finding {
	out := []Finding{}
	for _, d := range detectors {
		if ev.unread(d.source) {
			continue
		}
		out = append(out, d.detect(ev, kind)...)
	}
	if !ev.unread(sourcePauses) {
		out = append(out, pausedSeats(ev)...)
	}
	sortFindings(out)
	return out
}

// ReadWithEvidence is [Read] with the findings ev raises beside the document's.
func ReadWithEvidence(in Inputs, ev Evidence) Answer {
	answer := Read(in)
	c := in.Company
	kinds := map[string]SeatKind{}
	for role := range c.EachRole() {
		kinds[role.Seat().Handle()] = SeatAgent
		if role.Kind == org.KindHuman {
			kinds[role.Seat().Handle()] = SeatPerson
		}
	}
	found := Detect(ev, func(h string) SeatKind { return kinds[h] })
	for i := range found {
		if s := &found[i].Subject; s.Kind == SubjectSeat {
			s.Name = seatName(c, s.Seat)
		}
	}
	for _, d := range documented {
		if !ev.unread(d.source) {
			found = append(found, d.detect(c, ev)...)
		}
	}
	answer.Findings = scheduleEvidence(c, ev, append(answer.Findings, found...))
	sortFindings(answer.Findings)
	if ev.WorkUnread {
		answer.Unread = append(answer.Unread, UnreadWork)
	}
	if ev.LedgerUnread {
		answer.Unread = append(answer.Unread, UnreadLedger)
	}
	if ev.AlarmsUnread {
		answer.Unread = append(answer.Unread, UnreadAlarms)
	}
	if ev.PausesUnread {
		answer.Unread = append(answer.Unread, UnreadPauses)
	}
	if ev.RunsUnread {
		answer.Unread = append(answer.Unread, UnreadRuns)
	}
	if ev.FiresUnread {
		answer.Unread = append(answer.Unread, UnreadFires)
	}
	if ev.StopsUnread {
		answer.Unread = append(answer.Unread, UnreadStops)
	}
	if ev.MergesUnread {
		answer.Unread = append(answer.Unread, UnreadMerges)
	}
	return answer
}

func seatName(c *config.Company, handle string) string {
	for role := range c.EachRole() {
		if role.Seat().Handle() == handle {
			return role.Name
		}
	}
	return handle
}

// engineFinding is an engine class's finding in its class's words, what it is
// about in item, facts and records.
func engineFinding(class Class, subject Subject, item string, facts []Fact, records []Record) Finding {
	said := engineWords[class]
	return Finding{
		Class: class, Severity: SeverityNote, Actor: ActorEngine, Source: SourceEvidence,
		Area: AreaWork, Subject: subject, Title: said.title, Why: said.why, Item: item, Evidence: facts,
		Records: records,
		Fix:     Fix{Kind: FixNone, Options: []Option{}, Sentence: engineSentences[class]},
	}
}

// said is a class's title and why, the same for every subject it is raised for.
type said struct{ title, why string }

// engineWords is every engine class's words.
var engineWords = map[Class]said{
	ClassAgentMentionsWithoutAsk: {"Agents woke each other with comments that asked nothing.",
		"A comment that names an agent wakes it, and these asked it nothing, so each started a " +
			"turn with nothing to answer."},
	ClassPersonRequestsWithoutAsk: {"Agents asked a person for something without asking them a question.",
		"Agents asked these people for an answer or an action in comments that put no question to " +
			"them with an ask, so nothing in them reached the Inbox as a decision."},
	ClassAskOvertaken: {"People answered questions that no longer needed an answer.",
		"These answers went to a question its asker had asked again since, or whose item had " +
			"already finished."},
	ClassNoticeLoad: {"A person was sent more notices than anyone reads.",
		"Most of the notices these people were sent asked nothing of them, so what does ask them " +
			"waits among the rest."},
	ClassNoopShare: {"An agent was woken for turns that changed nothing.",
		"A quarter or more of these agents' turns committed nothing: no work moved and no words written."},
	ClassPickupStillWaiting: {"Agents picked up work and moved none of it.",
		"These pickups ended with words only (a comment or a chat post) and nothing typed on the " +
			"item, as when an agent says the item still waits on something."},
	ClassRedeliveredSeen: {"Wakes repeated changes an agent had already read.",
		"These agents were woken for changes a turn of theirs had already read, so the engine " +
			"absorbed the wakes instead of starting turns."},
	ClassAuxiliaryShare: {"An agent spent a large share of its tokens on auxiliary calls.",
		"More than 15% of the tokens these agents' turns spent went to auxiliary calls inside them " +
			"rather than to the turns' own work."},
}

// reviewWords is each way a review waits: on the agent holding it, on a
// person its holder asked, or on nobody.
var reviewWords = map[string]said{
	"holder": {"Work in review waited a day for the agent holding it.",
		"The agent holding these items in review took no turn on them in the last 24 hours (a turn " +
			"on a linked pull request counts on the change, not the item), so the work behind them waits."},
	"asked": {"Work in review waited a day for a person's answer.",
		"Their holders asked this person to review them a day ago or more, so the work behind them " +
			"waits on that answer."},
	"nobody": {"Work in review has no reviewer.",
		"Their holder moved these items into review itself and asked nobody, so no seat holds the " +
			"review. The work pickup hands each back to its holder to act on it or name a reviewer."},
}

// engineSentences is what each engine class's note says about the pattern.
var engineSentences = map[Class]string{
	ClassAgentMentionsWithoutAsk:  "The engine wakes an agent only for words that ask it something.",
	ClassPersonRequestsWithoutAsk: "A comment reaches a person's Inbox as a decision only when it asks them a question.",
	ClassAskOvertaken:             "An answer helps most on the newest question an item carries.",
	ClassReviewIdle:               "An item in review waits on whoever reviews it: the agent holding it, or the person its holder asked.",
	ClassNoticeLoad:               "Notices that ask something of a person lead their Inbox; the rest are context.",
	ClassNoopShare:                "Turns that commit nothing are counted by what woke them, so those wakes can be reduced.",
	ClassPickupStillWaiting:       "Work that waits on something else is better marked as waiting on it, so it is not handed out.",
	ClassAuxiliaryShare:           "Auxiliary calls run on the seat's auxiliary model, and a smaller one costs less.",
	ClassRedeliveredSeen:          "A wake for a change a completed turn already read starts no turn.",
}

func agentMentionsWithoutAsk(ev Evidence, kind func(string) SeatKind) []Finding {
	woken, seats := 0, 0
	var comments []string
	for _, n := range ev.Work.Named {
		if kind(n.Handle) != SeatAgent || n.Woken == 0 {
			continue
		}
		woken += n.Woken
		seats++
		comments = append(comments, n.WokenComments...)
	}
	if woken < minNamedWakes || woken*100 < namedWakesPer100*ev.Work.AgentComments {
		return nil
	}
	return []Finding{engineFinding(ClassAgentMentionsWithoutAsk, Subject{Kind: SubjectCompany},
		fmt.Sprintf("%d of %d agent comments woke %s", woken, ev.Work.AgentComments,
			words.Count(seats, "agent", "agents")),
		[]Fact{{Key: "woken", Value: int64(woken)}, {Key: "agents", Value: int64(seats)},
			{Key: "agent_comments", Value: int64(ev.Work.AgentComments)}}, recordsOf(RecordComment, comments))}
}

// personRequestsWithoutAsk is one finding per person agents asked for something
// in words alone, counted by the comment tool's own predicate
// ([worklet.RequestedWithoutAsk]): what this counts, the tool refuses.
func personRequestsWithoutAsk(ev Evidence, kind func(string) SeatKind) []Finding {
	var out []Finding
	for _, n := range ev.Work.Named {
		if kind(n.Handle) != SeatPerson || n.Requested < minNamedPerson {
			continue
		}
		out = append(out, engineFinding(ClassPersonRequestsWithoutAsk,
			Subject{Kind: SubjectSeat, Seat: n.Handle},
			words.Count(n.Requested, "comment", "comments"),
			[]Fact{{Key: "named", Value: int64(n.Requested)}}, recordsOf(RecordComment, n.RequestedComments)))
	}
	return out
}

func askOvertaken(ev Evidence, _ func(string) SeatKind) []Finding {
	w := ev.Work
	overtaken := w.Superseded + w.Moot
	if overtaken < minOvertaken || overtaken*100 < overtakenPercent*w.Answers {
		return nil
	}
	return []Finding{engineFinding(ClassAskOvertaken, Subject{Kind: SubjectCompany},
		fmt.Sprintf("%d of %d answers: %d asked again, %d finished", overtaken, w.Answers, w.Superseded, w.Moot),
		[]Fact{{Key: "answers", Value: int64(w.Answers)}, {Key: "superseded", Value: int64(w.Superseded)},
			{Key: "moot", Value: int64(w.Moot)}}, recordsOf(RecordComment, w.Overtaken))}
}

// approvalRubberStamp is one finding per asking agent whose decisions a person
// took as recommended almost every time, naming every such person: the decision
// stays the person's, and a policy letting the agent act on such a class is
// the owner's call, so the fix is a draft the owner reviews, never a change.
func approvalRubberStamp(ev Evidence, _ func(string) SeatKind) []Finding {
	type stamped struct {
		people               []words.Said
		decided, recommended int
	}
	byAsker := map[string]*stamped{}
	for _, d := range ev.Work.Decisions {
		if d.Decided < minDecided || d.Recommended*100 < recommendedPercent*d.Decided {
			continue
		}
		s := byAsker[d.Asker]
		if s == nil {
			s = &stamped{}
			byAsker[d.Asker] = s
		}
		s.people = append(s.people, words.Said(d.Person))
		s.decided += d.Decided
		s.recommended += d.Recommended
	}
	var out []Finding
	for _, asker := range slices.Sorted(maps.Keys(byAsker)) {
		s := byAsker[asker]
		people := words.And(s.people)
		out = append(out, Finding{
			Class: ClassApprovalRubberStamp, Severity: SeverityNote, Actor: ActorOwner,
			Source: SourceEvidence, Area: AreaApprovals, Subject: Subject{Kind: SubjectSeat, Seat: asker},
			Title: "A person chose an agent's recommendation in nearly every decision it asked.",
			Why: "Each decision still costs a press and makes the agent wait. Whether the agent may " +
				"act on some kinds of them without asking is your call.",
			Item: fmt.Sprintf("%s chose %s's recommendation in %d of %d decisions",
				people, asker, s.recommended, s.decided),
			Evidence: []Fact{{Key: "decided", Value: int64(s.decided)},
				{Key: "recommended", Value: int64(s.recommended)}},
			Fix: Fix{Kind: FixDraftWithAssistant, Options: []Option{},
				Sentence:  "Draft a policy with Ask Crewlet, and review it before it is saved.",
				Unchanged: "Nothing changes until you approve the policy Ask Crewlet drafts.",
				Prompt: fmt.Sprintf("In the last 7 days %s chose %s's recommended option in %d of "+
					"%d decisions it asked. Read those decisions, tell me which kinds of them were "+
					"always taken as recommended, and draft a change to the company's policies "+
					"that lets %s act on those kinds without asking, keeping every other decision "+
					"with the person. Show me the change before making it.",
					people, asker, s.recommended, s.decided, asker)},
		})
	}
	return out
}

// noVerifiedBackup reads the backup alarm, at the backup policy's own age: the
// one threshold the trim, the alarm and this finding share.
func noVerifiedBackup(ev Evidence, _ func(string) SeatKind) []Finding {
	if !ev.Backup.Standing {
		return nil
	}
	return []Finding{{
		Class: ClassNoVerifiedBackup, Severity: SeverityBlocks, Actor: ActorOwner,
		Source: SourceEvidence, Area: AreaDurability, Subject: Subject{Kind: SubjectCompany},
		Title: "Your company has no recent verified backup.",
		Why: "No verified backup within the backup policy's age has been recorded, so a lost " +
			"disk would lose the company's records, and the engine keeps every log until one is.",
		Evidence: []Fact{{Key: "logs", Value: int64(ev.Backup.Logs)}},
		// TAKEN ON THE DASHBOARD, whose Backups & retention takes one with a
		// press: an external step's sentence sent a person to `crewlet backup`
		// for it, beside the page that does it.
		Fix: Fix{Kind: FixOpenPage, Page: PageBackups, Options: []Option{},
			Sentence: "Take a backup now, then schedule backups that are copied off this server."},
	}}
}

// reviewIdle is one finding per party a review idle for [ReviewQuiet] waits on:
// the agent holding it, a person its holder asked, or nobody, for a review its
// holder submitted itself with no question open on it.
func reviewIdle(ev Evidence, _ func(string) SeatKind) []Finding {
	type waits struct{ on, how string }
	byWait := map[waits][]string{}
	var order []waits
	add := func(w waits, key string) {
		if _, ok := byWait[w]; !ok {
			order = append(order, w)
		}
		byWait[w] = append(byWait[w], key)
	}
	for _, r := range ev.Work.IdleReviews {
		switch {
		case len(r.WaitsOn) > 0:
			for _, p := range r.WaitsOn {
				add(waits{p, "asked"}, r.Key)
			}
		case r.WaitsOnNobody():
			add(waits{r.Holder, "nobody"}, r.Key)
		default:
			add(waits{r.Holder, "holder"}, r.Key)
		}
	}
	var out []Finding
	for _, w := range order {
		keys := byWait[w]
		f := engineFinding(ClassReviewIdle, Subject{Kind: SubjectSeat, Seat: w.on},
			words.Count(len(keys), "item in review", "items in review"),
			[]Fact{{Key: "idle", Value: int64(len(keys))}}, recordsOf(RecordItem, keys))
		f.Title, f.Why = reviewWords[w.how].title, reviewWords[w.how].why
		f.Focus = w.how
		out = append(out, f)
	}
	return out
}

// noticeLoad is one finding per person sent more notices than a person reads,
// most of them context rather than something put to them.
func noticeLoad(ev Evidence, _ func(string) SeatKind) []Finding {
	var out []Finding
	for _, n := range ev.Work.Notices {
		if n.Notices < minNotices || n.Addressed*100 >= addressedPercent*n.Notices {
			continue
		}
		out = append(out, engineFinding(ClassNoticeLoad, Subject{Kind: SubjectSeat, Seat: n.Handle},
			fmt.Sprintf("%d notices, %d of them asking something", n.Notices, n.Addressed),
			[]Fact{{Key: "notices", Value: int64(n.Notices)}, {Key: "addressed", Value: int64(n.Addressed)}},
			nil))
	}
	return out
}

// seatTurns is one seat's started turns in the ledger, by outcome, and its spend.
type seatTurns struct {
	handle                string
	turns, nothing        int64
	tokens, auxiliary     int64
	pickups, pickupsWords int64
	// started is every started turn, schedules' included, and absorbed the
	// wakes a completed turn had already read past (ADR-0120).
	started, absorbed int64
}

// seatsOf folds the ledger's started turns and absorbed wakes by seat, in the
// ledger's order.
func seatsOf(ledger []usage.TurnLedgerRow) []*seatTurns {
	var out []*seatTurns
	byID := map[string]*seatTurns{}
	for _, r := range ledger {
		disposition := types.WakeDisposition(r.Disposition)
		if disposition != types.DispositionStarted && disposition != types.DispositionAbsorbed {
			continue
		}
		s, ok := byID[r.AgentID]
		if !ok {
			s = &seatTurns{handle: cmp.Or(r.Handle, r.AgentID)}
			byID[r.AgentID] = s
			out = append(out, s)
		}
		if disposition == types.DispositionAbsorbed {
			s.absorbed += r.Turns
			continue
		}
		s.started += r.Turns
		s.tokens += r.Tokens()
		s.auxiliary += r.AuxInput + r.AuxOutput
		outcome := types.TurnOutcome(r.Outcome)
		// A SCHEDULE'S TURNS ARE THE OWNER'S CHOICE, and a check that finds
		// nothing commits nothing: the schedule classes read them, not this one.
		if !strings.HasPrefix(r.Wake, scheduleWake) {
			s.turns += r.Turns
			if outcome == types.OutcomeNothing {
				s.nothing += r.Turns
			}
		}
		if r.Wake == pickupWake || r.Wake == deadWaitWake {
			s.pickups += r.Turns
			if outcome == types.OutcomeWords {
				s.pickupsWords += r.Turns
			}
		}
	}
	return out
}

// pickupWake is the ledger's wake for work handed to an idle seat,
// deadWaitWake for work handed back over a dead wait (ADR-0128), and
// scheduleWake the prefix of every schedule's.
const (
	pickupWake   = types.OriginWorkItem + ":" + types.PickupReason
	deadWaitWake = types.OriginWorkItem + ":" + types.DeadWaitReason
	scheduleWake = types.OriginSchedule + ":"
)

// noopShare is one finding per agent whose started turns often committed nothing.
func noopShare(ev Evidence, _ func(string) SeatKind) []Finding {
	var out []Finding
	for _, s := range seatsOf(ev.Ledger) {
		if s.turns < minStartedTurns || s.nothing*100 < noopPercent*s.turns {
			continue
		}
		out = append(out, engineFinding(ClassNoopShare, Subject{Kind: SubjectSeat, Seat: s.handle},
			fmt.Sprintf("%d of %d turns", s.nothing, s.turns),
			[]Fact{{Key: "turns", Value: s.turns}, {Key: "nothing", Value: s.nothing}}, nil))
	}
	return out
}

// pickupStillWaiting is one finding for the company when pickups often ended
// in words only: the work handed out was waiting on something else. A pickup
// its turn answered typed, with an ask as with a change, is in the ledger as
// moved ([types.OutcomeOfPickup]), so words here are pickups nothing typed
// answered (ADR-0122).
func pickupStillWaiting(ev Evidence, _ func(string) SeatKind) []Finding {
	var pickups, words int64
	for _, s := range seatsOf(ev.Ledger) {
		pickups += s.pickups
		words += s.pickupsWords
	}
	if pickups < minPickups || words*100 < stillWaitingPercent*pickups {
		return nil
	}
	return []Finding{engineFinding(ClassPickupStillWaiting, Subject{Kind: SubjectCompany},
		fmt.Sprintf("%d of %d pickups", words, pickups),
		[]Fact{{Key: "pickups", Value: pickups}, {Key: "words", Value: words}}, nil)}
}

// redeliveredSeen is one finding per agent whose wakes often came for changes
// a turn of it had already read: the engine absorbed them, and says how many.
func redeliveredSeen(ev Evidence, _ func(string) SeatKind) []Finding {
	var out []Finding
	for _, s := range seatsOf(ev.Ledger) {
		woke := s.started + s.absorbed
		if s.absorbed < minAbsorbed || s.absorbed*100 < absorbedPercent*woke {
			continue
		}
		out = append(out, engineFinding(ClassRedeliveredSeen, Subject{Kind: SubjectSeat, Seat: s.handle},
			fmt.Sprintf("%d of %d wakes", s.absorbed, woke),
			[]Fact{{Key: "woke", Value: woke}, {Key: "absorbed", Value: s.absorbed}}, nil))
	}
	return out
}

// auxiliaryShare is one finding per agent whose in-turn auxiliary calls took
// a large share of its spend.
func auxiliaryShare(ev Evidence, _ func(string) SeatKind) []Finding {
	var out []Finding
	for _, s := range seatsOf(ev.Ledger) {
		if s.tokens < minSeatTokens || s.auxiliary*100 <= auxiliaryPercent*s.tokens {
			continue
		}
		out = append(out, engineFinding(ClassAuxiliaryShare, Subject{Kind: SubjectSeat, Seat: s.handle},
			fmt.Sprintf("%s of %s tokens", tokensSaid(s.auxiliary), tokensSaid(s.tokens)),
			[]Fact{{Key: "tokens", Value: s.tokens}, {Key: "auxiliary", Value: s.auxiliary}}, nil))
	}
	return out
}

// personHandles is every person's seat handle, in the chart's order.
func personHandles(c *config.Company) []string {
	var out []string
	for role := range c.EachRole() {
		if role.Kind == org.KindHuman {
			out = append(out, role.Seat().Handle())
		}
	}
	return out
}

// AgentHandles is every agent seat's handle, in the chart's order.
func AgentHandles(c *config.Company) []string {
	var out []string
	for _, role := range agentSeats(c) {
		out = append(out, role.Seat().Handle())
	}
	return out
}

// tokensSaid is a token count as a person reads it: 26.5M, 812K, 940.
func tokensSaid(n int64) string {
	switch {
	case n >= 1_000_000:
		return trimmed(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "M"
	case n >= 1_000:
		return fmt.Sprintf("%dK", n/1_000)
	}
	return fmt.Sprint(n)
}

// inboundSilent raises when the poll settled a change in the window and the
// webhook settled none after it: deliveries have not resumed since.
func inboundSilent(ev Evidence, _ func(string) SeatKind) []Finding {
	var polled, delivered worklet.ChangeSettles
	for _, s := range ev.Work.Settled {
		switch s.Source {
		case worklet.SettledByPoll:
			polled = s
		case worklet.SettledByWebhook:
			delivered = s
		}
	}
	if polled.Count == 0 || !polled.Newest.After(delivered.Newest) {
		return nil
	}
	kind := integration.KindGitHub
	name := SurfaceName(kind)
	return []Finding{{
		Class: ClassInboundSilent, Severity: SeverityCosts, Actor: ActorOwner, Source: SourceEvidence,
		Area: AreaIntegrations, Subject: Subject{Kind: SubjectIntegration, Name: name},
		Path:  "integrations." + string(kind),
		Title: "Pull requests your work waited on reached it only through the engine's poll.",
		Why: "When the webhook delivers a merge or a close, the item waiting on it hears within a " +
			"minute. These were learned by the poll instead, each at least ten minutes late, and no " +
			"delivery has settled one since, so the webhook is not reaching this company.",
		Item:     words.Count(polled.Count, "pull request", "pull requests"),
		Evidence: []Fact{{Key: "polled", Value: int64(polled.Count)}, {Key: "delivered", Value: int64(delivered.Count)}},
		Fix: Fix{Kind: FixExternalStep, Options: []Option{}, Setting: string(kind),
			Sentence:  "Open " + name + "'s settings and check that its webhook reaches the engine.",
			Unchanged: "The poll keeps settling what the webhook misses, ten minutes or more late."},
	}}
}
