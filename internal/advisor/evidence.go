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
	// ClassNoVerifiedBackup is the owner's: no backup within the policy's age.
	ClassNoVerifiedBackup Class = "durability.no_verified_backup"
	// ClassApprovalRubberStamp is the owner's: a person chose one agent's
	// recommendation on almost every decision it asked of them.
	ClassApprovalRubberStamp Class = "approval.rubber_stamp"
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
		Reviewers: AgentHandles(c), QuietSince: now.Add(-ReviewQuiet), People: personHandles(c)}
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
	// WorkUnread, LedgerUnread, AlarmsUnread and PausesUnread say the caller
	// could not read that source, so its classes raise nothing and the answer names it.
	WorkUnread, LedgerUnread, AlarmsUnread, PausesUnread bool
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
	{ClassNoVerifiedBackup, sourceAlarms, noVerifiedBackup},
	{ClassApprovalRubberStamp, sourceWork, approvalRubberStamp},
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
	// minPickups and stillWaitingPercent: 10 pickups a week, a fifth of them
	// ending in words only, which says the work handed out could not move.
	minPickups          = 10
	stillWaitingPercent = 20
	// minSeatTokens and auxiliaryPercent: a million tokens a week, more than
	// 15% of them spent on in-turn auxiliary calls.
	minSeatTokens    = 1_000_000
	auxiliaryPercent = 15
)

// EvidenceClasses is every class a detector or a paused seat raises.
func EvidenceClasses() []Class {
	out := make([]Class, 0, len(detectors))
	for _, d := range detectors {
		out = append(out, d.class)
	}
	return append(out, pausedClasses()...)
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
	answer.Findings = append(answer.Findings, found...)
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

func engineFinding(class Class, subject Subject, title, why string, facts []Fact, records []string) Finding {
	return Finding{
		Class: class, Severity: SeverityNote, Actor: ActorEngine, Source: SourceEvidence,
		Area: AreaWork, Subject: subject, Title: title, Why: why, Evidence: facts,
		Records: slices.Clone(records),
		Fix:     Fix{Kind: FixNone, Options: []Option{}, Sentence: engineSentences[class]},
	}
}

// engineSentences is what each engine class's note says about the pattern.
var engineSentences = map[Class]string{
	ClassAgentMentionsWithoutAsk:  "The engine wakes an agent only for words that ask it something.",
	ClassPersonRequestsWithoutAsk: "A comment reaches a person's Inbox as a decision only when it asks them a question.",
	ClassAskOvertaken:             "An answer helps most on the newest question an item carries.",
	ClassReviewIdle:               "An item in review waits on the agent holding it until its next turn on the item.",
	ClassNoticeLoad:               "Notices that ask something of a person lead their Inbox; the rest are context.",
	ClassNoopShare:                "Turns that commit nothing are counted by what woke them, so those wakes can be reduced.",
	ClassPickupStillWaiting:       "Work that waits on something else is better marked as waiting on it, so it is not handed out.",
	ClassAuxiliaryShare:           "Auxiliary calls run on the seat's auxiliary model, and a smaller one costs less.",
}

func agentMentionsWithoutAsk(ev Evidence, kind func(string) SeatKind) []Finding {
	woken, seats := 0, 0
	var records []string
	for _, n := range ev.Work.Named {
		if kind(n.Handle) != SeatAgent || n.Woken == 0 {
			continue
		}
		woken += n.Woken
		seats++
		records = appendExamples(records, n.WokenComments)
	}
	if woken < minNamedWakes || woken*100 < namedWakesPer100*ev.Work.AgentComments {
		return nil
	}
	return []Finding{engineFinding(ClassAgentMentionsWithoutAsk, Subject{Kind: SubjectCompany},
		"Agents woke each other with comments that asked nothing.",
		fmt.Sprintf("In the last 7 days, %d agent comments named %d agents without asking "+
			"anything and each started a turn, out of %d agent comments.",
			woken, seats, ev.Work.AgentComments),
		[]Fact{{Key: "woken", Value: int64(woken)}, {Key: "agents", Value: int64(seats)},
			{Key: "agent_comments", Value: int64(ev.Work.AgentComments)}}, records)}
}

func personRequestsWithoutAsk(ev Evidence, kind func(string) SeatKind) []Finding {
	var out []Finding
	for _, n := range ev.Work.Named {
		if kind(n.Handle) != SeatPerson || n.Told < minNamedPerson {
			continue
		}
		out = append(out, engineFinding(ClassPersonRequestsWithoutAsk,
			Subject{Kind: SubjectSeat, Seat: n.Handle},
			"Agents named a person without asking them a question.",
			fmt.Sprintf("In the last 7 days, agents named %s in %d comments with no question "+
				"attached, so nothing in them reached the Inbox as a decision.", n.Handle, n.Told),
			[]Fact{{Key: "named", Value: int64(n.Told)}}, n.Comments))
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
		"People answered questions that no longer needed an answer.",
		fmt.Sprintf("In the last 7 days, %d of %d answers went to a question asked again "+
			"since (%d) or whose item had already finished (%d).",
			overtaken, w.Answers, w.Superseded, w.Moot),
		[]Fact{{Key: "answers", Value: int64(w.Answers)}, {Key: "superseded", Value: int64(w.Superseded)},
			{Key: "moot", Value: int64(w.Moot)}}, w.Overtaken)}
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
			Title: fmt.Sprintf("%s chose %s's recommendation in %d of %d decisions.",
				people, asker, s.recommended, s.decided),
			Why: fmt.Sprintf("In the last 7 days %s asked %s %d decisions and the recommended "+
				"option was taken %d times. Each one still costs a press and makes the agent "+
				"wait. Whether %s may act on some kinds of them without asking is your call.",
				asker, people, s.decided, s.recommended, asker),
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
		Fix: Fix{Kind: FixExternalStep, Options: []Option{},
			Sentence: "Take a backup now with `crewlet backup` or from the dashboard, then " +
				"schedule it to a place off this server."},
	}}
}

// reviewIdle is one finding per agent holding a review it has taken no turn on
// for [ReviewQuiet]: a review is the holder's next move, and none came.
func reviewIdle(ev Evidence, _ func(string) SeatKind) []Finding {
	byHolder := map[string][]string{}
	var holders []string
	for _, r := range ev.Work.IdleReviews {
		if _, ok := byHolder[r.Holder]; !ok {
			holders = append(holders, r.Holder)
		}
		byHolder[r.Holder] = append(byHolder[r.Holder], r.Key)
	}
	var out []Finding
	for _, h := range holders {
		keys := byHolder[h]
		out = append(out, engineFinding(ClassReviewIdle, Subject{Kind: SubjectSeat, Seat: h},
			"Work in review waited a day for the agent holding it.",
			fmt.Sprintf("%s held by %s had no turn by it on the item in the last 24 hours "+
				"(a turn on a linked pull request counts on the change, not the item), so "+
				"the work behind them waits.", words.Count(len(keys), "item in review", "items in review"), h),
			[]Fact{{Key: "idle", Value: int64(len(keys))}}, appendExamples(nil, keys)))
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
			"A person was sent more notices than anyone reads.",
			fmt.Sprintf("In the last 7 days, %s was sent %d notices, and %d of them asked "+
				"something of them, so what does ask them waits among the rest.",
				n.Handle, n.Notices, n.Addressed),
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
}

// seatsOf folds the ledger's started turns by seat, in the ledger's order.
func seatsOf(ledger []usage.TurnLedgerRow) []*seatTurns {
	var out []*seatTurns
	byID := map[string]*seatTurns{}
	for _, r := range ledger {
		if types.WakeDisposition(r.Disposition) != types.DispositionStarted {
			continue
		}
		s, ok := byID[r.AgentID]
		if !ok {
			s = &seatTurns{handle: cmp.Or(r.Handle, r.AgentID)}
			byID[r.AgentID] = s
			out = append(out, s)
		}
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
		if r.Wake == pickupWake {
			s.pickups += r.Turns
			if outcome == types.OutcomeWords {
				s.pickupsWords += r.Turns
			}
		}
	}
	return out
}

// pickupWake is the ledger's wake for work handed to an idle seat, and
// scheduleWake the prefix of every schedule's.
const (
	pickupWake   = types.OriginWorkItem + ":" + types.PickupReason
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
			"An agent was woken for turns that changed nothing.",
			fmt.Sprintf("In the last 7 days, %d of %s's %d turns committed nothing: no work "+
				"moved and no words written.", s.nothing, s.handle, s.turns),
			[]Fact{{Key: "turns", Value: s.turns}, {Key: "nothing", Value: s.nothing}}, nil))
	}
	return out
}

// pickupStillWaiting is one finding for the company when pickups often ended
// in words only: the work handed out was waiting on something else.
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
		"Agents picked up work and moved none of it.",
		fmt.Sprintf("In the last 7 days, %d of %d pickups ended with words only (a comment, "+
			"a question or a chat post) and no work moved, as when an agent says the item "+
			"still waits on something.", words, pickups),
		[]Fact{{Key: "pickups", Value: pickups}, {Key: "words", Value: words}}, nil)}
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
			"An agent spent a large share of its tokens on auxiliary calls.",
			fmt.Sprintf("In the last 7 days, %d of %s's %d tokens went to auxiliary calls "+
				"inside its turns rather than to the turns' own work.", s.auxiliary, s.handle, s.tokens),
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

func appendExamples(records, more []string) []string {
	for _, r := range more {
		if len(records) == worklet.MaxPatternExamples {
			break
		}
		records = append(records, r)
	}
	return records
}
