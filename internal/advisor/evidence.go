package advisor

import (
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The classes a finding about what the company did can have: patterns the
// engine is reducing, shown to the owner as information and never as a to-do.
const (
	ClassAgentMentionsWithoutAsk  Class = "wake.agent_mentions_without_ask"
	ClassPersonRequestsWithoutAsk Class = "person.requests_without_ask"
	ClassAskOvertaken             Class = "ask.overtaken"
)

// AreaWork holds the patterns in how the company's work moves.
const AreaWork Area = "work"

// FixNone is a finding the engine acts on: nothing for the owner to send.
const FixNone FixKind = "none"

// Window is how far back every evidence class reads.
const Window = 7 * 24 * time.Hour

// Evidence is what the company did in the window that ends at Until.
type Evidence struct {
	Until time.Time
	Work  worklet.WorkPatterns
}

// detector raises one class's findings from the evidence; isAgent tells an
// agent seat's handle from a person's. It reads records only and calls no model.
type detector struct {
	class  Class
	detect func(ev Evidence, isAgent func(string) bool) []Finding
}

// detectors is every evidence class's detector.
var detectors = []detector{
	{ClassAgentMentionsWithoutAsk, agentMentionsWithoutAsk},
	{ClassPersonRequestsWithoutAsk, personRequestsWithoutAsk},
	{ClassAskOvertaken, askOvertaken},
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

// Detect is every evidence finding ev raises, the most costly first.
func Detect(ev Evidence, isAgent func(handle string) bool) []Finding {
	out := []Finding{}
	for _, d := range detectors {
		out = append(out, d.detect(ev, isAgent)...)
	}
	sortFindings(out)
	return out
}

// ReadWithEvidence is [Read] with the findings ev raises beside the document's.
func ReadWithEvidence(c *config.Company, revision string, ev Evidence) Answer {
	answer := Read(c, revision)
	agents := map[string]string{}
	for _, role := range agentSeats(c) {
		agents[role.Seat().Handle()] = role.Name
	}
	found := Detect(ev, func(h string) bool { _, ok := agents[h]; return ok })
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

func agentMentionsWithoutAsk(ev Evidence, isAgent func(string) bool) []Finding {
	woken, seats := 0, 0
	var records []string
	for _, n := range ev.Work.Named {
		if !isAgent(n.Handle) || n.Woken == 0 {
			continue
		}
		woken += n.Woken
		seats++
		records = appendExamples(records, n.Comments)
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

func personRequestsWithoutAsk(ev Evidence, isAgent func(string) bool) []Finding {
	var out []Finding
	for _, n := range ev.Work.Named {
		if isAgent(n.Handle) || n.Told < minNamedPerson {
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

func askOvertaken(ev Evidence, _ func(string) bool) []Finding {
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

func appendExamples(records, more []string) []string {
	for _, r := range more {
		if len(records) == worklet.MaxPatternExamples {
			break
		}
		records = append(records, r)
	}
	return records
}
