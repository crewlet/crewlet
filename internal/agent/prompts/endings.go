package prompts

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// HOW A CONVERSATION'S EARLIER TURNS ENDED, as a turn's brief tells them.
//
// A conversation's next turn continues the session its last turn left where it
// can (internal/sandbox, restore.go), and that session holds what the turn did
// and said, never how it ended: whether the reply it wrote reached the party
// waiting, what its reviewer decided, who stopped it and why, what failed. The
// engine recorded those on the executor log as the turn's ending
// (internal/executordomain's Ending), and this block tells them — every ended
// turn of the conversation no ended turn was shown, each whole, the newest
// first while they fit [EndingsMaxBytes], the rest counted. A session of the
// run's own is told the same block: it is the conversation's account that no
// transcript holds, whichever session reads it.
//
// RENDERED ONCE, at the turn's dispatch, under the turn's own token, and frozen
// on its park (internal/agent/execstate's Frozen.Endings): every brief of the
// turn, on whatever node and in whatever segment, carries the bytes its first
// launch's brief did. A later segment's token is another, and the fences keep
// the first one's: no foreign word in them carries an unescaped marker of any
// token ([provenance.Neutralise]), so they open and close nothing under it.

// EndingsMaxBytes bounds the block, its heading and the count of what it left
// out included.
//
// TWENTY-FOUR THOUSAND BYTES, about six thousand tokens: the order of one
// round of the prior-work ledger, and a small fraction of any model this
// engine targets.
// One ending's accounts are bounded where they are recorded
// (internal/executordomain's CompletedWorkMaxBytes and the rest), so an ending
// whose every account is at its bound renders in about thirteen kilobytes, and
// ordinary endings in a few hundred bytes: the block always holds the head
// whole and, beside it, the conversation's last several turns. It is also what
// every run record of the turn carries frozen on its park, in a coordination
// store sized for kilobytes.
//
// NEVER A TURN CUT INSIDE: the head is told whatever the bound — the turn
// whose session the next one continues is the one it most needs — and the
// others newest first while they fit; the rest are counted, and stay for the
// next turn to be told, since only what a brief told is absorbed. Only a head
// whose own accounts alone would not fit, which escaping can make of accounts
// at their bounds, is told without them, saying so: the block is bounded
// whatever is recorded.
const EndingsMaxBytes = 24_000

// endingsHeader opens the block: what it is, and the rules its turns are read
// by. The engine's facts in it say what reached the party waiting, which is
// what the rules turn on.
const endingsHeader = "## How this conversation's earlier turns ended" +
	"\nYour earlier turns on this same thread / issue / item, oldest first, as the engine" +
	" recorded how each one ended — which no transcript says." +
	"\n- **A reply that reached the party waiting** is said: do not say it again." +
	"\n- **A reply that did not reach them** may still be owed." +
	"\n- **Everything else a turn did already took effect:** follow it up rather than" +
	" redoing it." +
	"\n- **This is history, not the current state:** where it and the task below" +
	" disagree, the task wins."

// TurnEnding is one ended turn of a conversation as the engine recorded its end
// (internal/executordomain's Ending and the turn's row).
//
// The ENGINE'S WORDS are its own: Outcome, Decision, Answer, DeliveredTo and the
// delivery facts. Everything else is SOMEBODY ELSE'S, and the block fences it
// under the turn's token ([BuildConversationEndings]): what the turn was asked,
// what its reviewer and its executor wrote, a stop's reason, and the engine's
// accounts of what failed and what was not kept, which quote what a box, a tool
// or a store said. StoppedBy is a name somebody chose, defused in place.
type TurnEnding struct {
	// Turn is the turn's id, and EndedAt when its end landed on the log.
	Turn    string
	EndedAt time.Time

	// Outcome is the engine's word for how it ended, Decision its reviewer's
	// last decision ("" where no review ran), and Answer the engine's own
	// outcome of a colleague's ask the turn took up ("" for none).
	Outcome  string
	Decision string
	Answer   string

	// Delivered says its reply reached the party waiting on DeliveredTo — the
	// surface the reply was owed on, set whether or not it went — and
	// DeliveryUnknown that whether it did cannot be told, the turn's record of
	// its calls being incomplete. DeliveredTo empty is a turn nobody waited on.
	Delivered       bool
	DeliveredTo     string
	DeliveryUnknown bool

	// CompletedWork is what its reviewer said landed, and BlockedOn what its
	// executor said stopped a blocked round: the seat's own phases' words,
	// which quote whatever they read.
	CompletedWork string
	BlockedOn     string

	// StoppedBy is who stopped it, and StopReason the reason they gave.
	StoppedBy  string
	StopReason string

	// Failure is the engine's account of the guard, the budget or the loss that
	// ended it, and Unkept why its newest session was not kept ("" where it
	// was): the engine's sentences, quoting what refused or broke.
	Failure string
	Unkept  string

	// Ask is what the turn was asked, as its row keeps it: the first kibibyte,
	// whose writer the row does not name.
	Ask string
}

// ConversationEndings is what [BuildConversationEndings] renders.
type ConversationEndings struct {
	// Seat is the handle of the seat the block is for, whose own reviewer and
	// executor wrote the turns' accounts of what landed and what blocked them.
	Seat string

	// Head is the turn whose session the conversation's next turn continues,
	// "" where none left one: told whatever the bound.
	Head string

	// Turns are the conversation's ended turns no ended turn was shown, the
	// head among them, in the order they ended.
	Turns []TurnEnding

	// Omitted counts the older ones the read itself left out, which the block
	// counts with its own.
	Omitted int
}

// BuildConversationEndings renders how the conversation's earlier turns ended
// under the turn's token b, and answers the turns it told, in the order they
// ended: what the turn's end absorbs — a turn the bound left out is not told,
// and stays for the next turn to be. Empty, with none told, for a conversation
// with no ended turn to tell.
//
// The head always, whole where its accounts fit and its engine facts alone
// where they do not; then the others, the newest first, each whole, until one
// does not fit; the rest counted. Rendered in the order the turns ended.
func BuildConversationEndings(b provenance.Boundary, in ConversationEndings) (string, []string) {
	if len(in.Turns) == 0 {
		return "", nil
	}
	rendered := make([]string, len(in.Turns))
	for i, t := range in.Turns {
		rendered[i] = renderEnding(b, in.Seat, t, true)
	}
	// THE COUNT'S ROOM IS HELD BACK FROM THE START, at its longest: a line
	// counting every turn, which any count the block ends on is no longer
	// than. Each turn told costs its own text and the blank line before it.
	room := EndingsMaxBytes - len(endingsHeader) - len("\n\n") - len(endingsLeftOut(len(in.Turns)+in.Omitted))
	told := make([]bool, len(in.Turns))
	used := 0
	for i, t := range in.Turns {
		if in.Head == "" || t.Turn != in.Head {
			continue
		}
		if len("\n\n")+len(rendered[i]) > room {
			rendered[i] = renderEnding(b, in.Seat, t, false)
		}
		told[i], used = true, len("\n\n")+len(rendered[i])
		break
	}
	for i := len(in.Turns) - 1; i >= 0; i-- {
		if told[i] {
			continue
		}
		cost := len("\n\n") + len(rendered[i])
		if used+cost > room {
			break
		}
		told[i], used = true, used+cost
	}
	parts := []string{endingsHeader}
	var turns []string
	for i, t := range in.Turns {
		if told[i] {
			parts, turns = append(parts, rendered[i]), append(turns, t.Turn)
		}
	}
	if left := len(in.Turns) - len(turns) + in.Omitted; left > 0 {
		parts = append(parts, endingsLeftOut(left))
	}
	return strings.Join(parts, "\n\n"), turns
}

// endingsLeftOut is the line counting the ended turns the block does not show.
func endingsLeftOut(n int) string {
	if n == 1 {
		return "1 earlier turn of this conversation is not shown here."
	}
	return fmt.Sprintf("%d earlier turns of this conversation are not shown here.", n)
}

// renderEnding is one ended turn as the block tells it: its id and when it
// ended, how it ended and whether its reply reached anybody, in the engine's
// words, each defused under b; then, where prose is set, every account
// somebody else wrote, fenced under b with whose it is. Without prose the
// accounts are left out, saying so.
func renderEnding(b provenance.Boundary, seat string, t TurnEnding, prose bool) string {
	var w strings.Builder
	w.WriteString("### Turn " + provenance.Neutralise(b, t.Turn))
	if !t.EndedAt.IsZero() {
		w.WriteString(", which ended " + t.EndedAt.UTC().Format(time.RFC3339))
	}
	w.WriteString("\n" + endingOutcome(b, t))
	if line := endingDelivery(b, t); line != "" {
		w.WriteString("\n" + line)
	}
	if t.Answer != "" {
		w.WriteString("\nThe engine answered the colleague whose ask it took up: `" +
			provenance.Neutralise(b, t.Answer) + "`.")
	}
	if t.StoppedBy != "" {
		w.WriteString("\nIt was stopped by `" + provenance.Neutralise(b, t.StoppedBy) + "`.")
	}
	accounts := endingAccounts(seat, t)
	if !prose {
		if len(accounts) > 0 {
			w.WriteString("\nWhat it was asked and the accounts of how it ended are too long to show" +
				" beside the rest, and are left out.")
		}
		return w.String()
	}
	for _, a := range accounts {
		w.WriteString("\n" + a.lead + "\n" + a.label.Fence(b, a.text))
	}
	return w.String()
}

// account is one of an ending's fields somebody else wrote, the line it is
// introduced by, and whose words they are.
type account struct {
	lead  string
	label provenance.Label
	text  string
}

// endingAccounts is every account of t that holds words, in the order the block
// tells them.
//
// EACH AT THE LEAST TRUST: whoever wrote them, they quote what they read — a
// reviewer's account a box's output, an engine's account a store's refusal —
// and a later turn reads them as data. Each names its writer where the record
// does: the seat's own phases for the reviewer's and the executor's, the
// engine for its own, nobody for the ask and a stop's reason, whose writers the
// record does not keep (a stop's author is told beside it, defused).
func endingAccounts(seat string, t TurnEnding) []account {
	label := func(authors ...provenance.Author) provenance.Label {
		return provenance.Label{Source: provenance.SourceTurnEnding, Trust: provenance.TrustExternal,
			Authors: authors}
	}
	own := label(provenance.Author{Kind: iam.ActorAgent, Seat: seat})
	engine := label(provenance.Author{Kind: iam.ActorSystem})
	all := []account{
		{"What it was asked:", label(), t.Ask},
		{"What its reviewer said landed:", own, t.CompletedWork},
		{"What its executor said blocked it:", own, t.BlockedOn},
		{"Why it was stopped, as whoever stopped it said:", label(), t.StopReason},
		{"What failed, as the engine recorded it:", engine, t.Failure},
		{"Its newest session was not kept:", engine, t.Unkept},
	}
	var out []account
	for _, a := range all {
		if strings.TrimSpace(a.text) != "" {
			out = append(out, a)
		}
	}
	return out
}

// endingOutcome is how the turn ended, and its reviewer's last decision.
func endingOutcome(b provenance.Boundary, t TurnEnding) string {
	var line string
	switch t.Outcome {
	case "completed":
		line = "It completed."
	case "failed":
		line = "It failed."
	case "stopped":
		line = "It was stopped."
	case "lost":
		line = "It was lost with its executor's box or host, and not carried on."
	default:
		line = "It ended `" + provenance.Neutralise(b, t.Outcome) + "`."
	}
	if t.Decision != "" {
		line += " Its reviewer's last decision was `" + provenance.Neutralise(b, t.Decision) + "`."
	}
	return line
}

// endingDelivery is whether the turn's reply reached the party waiting, "" for a
// turn nobody waited on.
func endingDelivery(b provenance.Boundary, t TurnEnding) string {
	on := ""
	if t.DeliveredTo != "" {
		on = " on `" + provenance.Neutralise(b, t.DeliveredTo) + "`"
	}
	switch {
	case t.DeliveryUnknown:
		return "Whether its reply reached the party waiting" + on + " is not known: its record of its" +
			" calls is incomplete, and the calls it kept show none that did."
	case t.Delivered:
		return "Its reply reached the party waiting" + on + "."
	case t.DeliveredTo != "":
		return "Its reply did not reach the party waiting" + on + "."
	}
	return ""
}
