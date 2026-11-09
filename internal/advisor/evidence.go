package advisor

import (
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The classes a finding about what the company did can have: patterns the
// engine is reducing, shown to the owner as information and never as a to-do.
const (
	ClassAgentMentionsWithoutAsk  Class = "wake.agent_mentions_without_ask"
	ClassPersonRequestsWithoutAsk Class = "person.requests_without_ask"
	ClassAskOvertaken             Class = "ask.overtaken"
	// ClassNoVerifiedBackup is the owner's: no backup within the policy's age.
	ClassNoVerifiedBackup Class = "durability.no_verified_backup"
)

// AreaWork holds the patterns in how the company's work moves, and
// AreaDurability what keeps the company's records safe.
const (
	AreaWork       Area = "work"
	AreaDurability Area = "durability"
)

// FixNone is a finding the engine acts on: nothing for the owner to send.
// FixExternalStep is one the owner takes outside the company document.
const (
	FixNone         FixKind = "none"
	FixExternalStep FixKind = "external_step"
)

// Window is how far back every evidence class reads.
const Window = 7 * 24 * time.Hour

// Evidence is what the company did in the window that ends at Until.
type Evidence struct {
	Until time.Time
	Work  worklet.WorkPatterns
	// Backup is the node's backup alarm as its latest evaluation stands.
	Backup Backup
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
	class Class
	// work marks a detector that reads the tracker's [worklet.WorkPatterns].
	work   bool
	detect func(ev Evidence, kind func(string) SeatKind) []Finding
}

// detectors is every evidence class's detector.
var detectors = []detector{
	{ClassAgentMentionsWithoutAsk, true, agentMentionsWithoutAsk},
	{ClassPersonRequestsWithoutAsk, true, personRequestsWithoutAsk},
	{ClassAskOvertaken, true, askOvertaken},
	{ClassNoVerifiedBackup, false, noVerifiedBackup},
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
)

// EvidenceClasses is every class a detector raises.
func EvidenceClasses() []Class {
	out := make([]Class, 0, len(detectors))
	for _, d := range detectors {
		out = append(out, d.class)
	}
	return out
}

// WorkClasses is every class a detector raises from the tracker's patterns.
func WorkClasses() []Class {
	var out []Class
	for _, d := range detectors {
		if d.work {
			out = append(out, d.class)
		}
	}
	return out
}

// Detect is every evidence finding ev raises, the most costly first.
func Detect(ev Evidence, kind func(handle string) SeatKind) []Finding {
	out := []Finding{}
	for _, d := range detectors {
		out = append(out, d.detect(ev, kind)...)
	}
	sortFindings(out)
	return out
}

// ReadWithEvidence is [Read] with the findings ev raises beside the document's.
func ReadWithEvidence(c *config.Company, revision string, ev Evidence) Answer {
	answer := Read(c, revision)
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
		Fix: Fix{Kind: FixNone, Options: []Option{},
			Sentence: "The engine wakes an agent only for words that ask it something."},
	}
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

func appendExamples(records, more []string) []string {
	for _, r := range more {
		if len(records) == worklet.MaxPatternExamples {
			break
		}
		records = append(records, r)
	}
	return records
}
