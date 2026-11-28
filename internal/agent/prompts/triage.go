package prompts

import (
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/words"
)

// THE WAKE TRIAGE (ADR-0168): one message that reached a seat while its
// executor runs, from somebody other than whom the run works for or about
// other work, described by a cheap model as one of four kinds. The model only
// DESCRIBES the message: whether its writer may steer the run, and whether it
// owes an answer, the engine decides from the chart and the delivery, never
// from anything the model says.

// TriageTool is the one call a triage submits its decision by.
const TriageTool = "decide_wake"

// WakeTriageSystem is the whole contract the triage model is held to. The four
// decisions are internal/engine's triageDecisions, held equal by a test there.
//
// THE UNSURE CASES LEAN TO WHAT LOSES NOTHING: input over noise, since a noise
// verdict may drop a message, and request over steer, since a request waits
// for a turn of its own.
const WakeTriageSystem = "You sort one message for a seat of a company of AI agents. The seat's executor " +
	"is in the middle of a task, and the message reached the seat while it runs. Call " + TriageTool +
	" once, with a decision and a reason of one plain sentence:\n" +
	"- input: it tells the running task something the task needs (a fact, a correction of a fact, a " +
	"constraint, an answer the task was waiting for), without asking for other work.\n" +
	"- steer: its writer tells the running task to change what it does or how (stop, change course, a " +
	"different target or approach).\n" +
	"- request: it asks for work of its own, apart from the running task.\n" +
	"- noise: it asks nothing and adds nothing the running task needs (thanks, an acknowledgement, " +
	"chatter, or news about other work).\n" +
	"Decide from what the message says and where it was written. A message written in another " +
	"conversation is about that conversation's own subject unless it names the running task's work: " +
	"there, \"this\" or \"it\" names that subject, and words the same as the task's ask for that " +
	"other work, never repeat the task. You say what kind of message it is, never who may do what: the " +
	"engine decides that from the company's chart. Unsure between input and noise, choose input; between " +
	"steer and request, choose request.\n\n" + provenance.StandingInstruction

// WakeTriage is one message as the triage reads it.
type WakeTriage struct {
	// Seat is the busy seat's handle, and Task what its running turn was
	// asked, in that turn's own words (a run's task description).
	Seat string
	Task string `untrusted:"fence"`

	// Requester is whom the running turn works for, "" for nobody.
	Requester string

	// Wake is the message: its writer, its words in its writer's fence, and
	// whether it asks for an answer.
	Wake steer.Wake

	// Place is where the message was written against the conversation the
	// turn works in, the engine's fact from both conversations' identities.
	Place WakePlace

	// AboutItem, AboutWritten and ByLead are why else the message is weighed,
	// the engine's facts: about the work item the turn is charged to, about a
	// record its run created or wrote (a reply under its post, a review of the
	// change it opened), or by somebody who leads the seat.
	AboutItem, AboutWritten, ByLead bool
}

// WakePlace is where a message was written, against the running task's
// conversation.
type WakePlace string

const (
	// PlaceHere is the conversation the running task works in.
	PlaceHere WakePlace = "here"
	// PlaceRoom is elsewhere in the room the running task works in: another
	// thread of it, or the room's top level beside the task's thread.
	PlaceRoom WakePlace = "room"
	// PlaceElsewhere is another conversation.
	PlaceElsewhere WakePlace = "elsewhere"
)

// wakePlaces is how the triage's message says each place.
var wakePlaces = map[WakePlace]string{
	PlaceHere:      "in the conversation the running task works in",
	PlaceRoom:      "elsewhere in the room the running task works in, not in the task's own conversation",
	PlaceElsewhere: "in another conversation than the one the running task works in",
}

// BuildWakeTriage is the user message for one triage under fence, minted for
// the call: the running task, fenced as what the seat's turn was asked
// ([provenance.ExecutorSession]), whom it works for, then the message in its
// writer's fence with the engine's lines saying who wrote it and where, why
// else it is weighed and whether it asks for an answer. Every name is held to
// one line and neutralised, so nothing a writer chose opens a line or a fence.
func BuildWakeTriage(fence provenance.Boundary, in WakeTriage) string {
	var b strings.Builder
	b.WriteString("## The running task\n")
	b.WriteString(provenance.ExecutorSession(in.Seat).Fence(fence, strings.TrimSpace(in.Task)))
	b.WriteString("\nIt works for " + wakeName(fence, in.Requester, "nobody in particular") + ".\n\n")
	b.WriteString("## The message\n")
	writer := WakeWriter(in.Wake, fence)
	place, ok := wakePlaces[in.Place]
	if !ok {
		place = wakePlaces[PlaceElsewhere]
	}
	line := writer + " wrote it " + place + ", while the task was running."
	var about []words.Said
	if in.AboutItem {
		about = append(about, "the work item the running task is charged to")
	}
	if in.AboutWritten {
		about = append(about, "something the running task created or wrote")
	}
	if len(about) > 0 {
		line += " It is about " + words.And(about) + "."
	}
	if in.ByLead {
		line += " " + writer + " leads the seat."
	}
	if in.Wake.OwesAnswer {
		line += " It asks for an answer."
	} else {
		line += " It asks for no answer."
	}
	if in.Wake.Cut > 0 {
		line += " It runs " + strconv.Itoa(in.Wake.Cut) + " characters past what is shown here."
	}
	b.WriteString(line + "\n\n")
	b.WriteString(provenance.Fence(fence, in.Wake.Label.Source, in.Wake.Label.Trust, in.Wake.Label.Authors,
		in.Wake.Text))
	b.WriteString("\n")
	return b.String()
}
