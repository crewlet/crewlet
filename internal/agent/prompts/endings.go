package prompts

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// HOW A CONVERSATION'S EARLIER TURNS ENDED, as a turn's brief tells them. A
// continued session (internal/sandbox, restore.go) holds what a turn did, never
// how it ended — whether its reply reached the party waiting, its reviewer's
// decision, who stopped it, what failed — which the executor log records
// (internal/executorkeep's Ending). Every session of the conversation is told
// this block, a schedule's in its words ([ConversationEndings.Fires]).
//
// RENDERED ONCE, at dispatch, under the turn's token, and frozen on its park
// (internal/agent/execstate's Frozen.Endings), so every brief of the turn
// carries the same bytes. A later segment's token differs and the fences keep
// the first's; they open and close nothing under it, since every foreign word
// is neutralised against any token ([provenance.Neutralise]).

// EndingsMaxBytes bounds the whole block, heading and count included, as its
// JSON encoding in the run record (Frozen.Endings, in every park of the turn;
// the coordination store sizes it in kilobytes). 24,000 bytes, about six
// thousand tokens, small beside any targeted model's context: the order of one
// ledger round, enough for the head at its recorded bounds (about 13 kB;
// executorkeep's CompletedWorkMaxBytes and the rest) plus several ordinary
// turns. Measured encoded because JSON writes `<`, `>`, `&`, U+2028 and U+2029
// as six bytes and accounts quote markup freely; the rendered text is never
// longer. No turn is cut inside: the head is told whatever the bound (its
// accounts dropped, saying so, only where escaping makes them alone too long),
// the rest newest first while they fit, then counted, left for the next turn.
const EndingsMaxBytes = 24_000

// endingsHeader opens the block with the rules its turns are read by, which
// turn on whether a reply reached the party waiting.
const endingsHeader = "## How this conversation's earlier turns ended" +
	"\nYour earlier turns on this same thread / issue / item, oldest first, as the engine" +
	" recorded how each one ended — which no transcript says." +
	"\n- **A reply that reached the party waiting** is said: do not say it again." +
	"\n- **A reply that did not reach them** may still be owed." +
	"\n- **Everything else a turn did already took effect:** follow it up rather than" +
	" redoing it." +
	"\n- **This is history, not the current state:** where it and the task below" +
	" disagree, the task wins."

// scheduleEndingsHeader is [endingsHeader] for a schedule's conversation, each
// of whose turns is a fire: the same rules, told of earlier fires.
const scheduleEndingsHeader = "## How this schedule's earlier fires ended" +
	"\nYour earlier fires of this same schedule, oldest first, as the engine recorded how each" +
	" one ended — which no transcript says." +
	"\n- **A reply that reached the party waiting** is said: do not say it again." +
	"\n- **A reply that did not reach them** may still be owed." +
	"\n- **Everything else a fire did already took effect:** follow it up rather than" +
	" redoing it." +
	"\n- **This is history, not the current state:** where it and the task below" +
	" disagree, the task wins."

// endingWords are the words the block is told in: its header, each turn's
// heading, and the left-out count's singular and plural.
type endingWords struct {
	header    string
	turn      string
	one, many string
}

var (
	turnWords = endingWords{header: endingsHeader, turn: "Turn",
		one: "earlier turn of this conversation is", many: "earlier turns of this conversation are"}

	fireWords = endingWords{header: scheduleEndingsHeader, turn: "Fire",
		one: "earlier fire of this schedule is", many: "earlier fires of this schedule are"}
)

// endingWordsFor is a schedule's words where fires is set, a conversation's
// otherwise.
func endingWordsFor(fires bool) endingWords {
	if fires {
		return fireWords
	}
	return turnWords
}

// TurnEnding is one ended turn of a conversation as the engine recorded its end
// (internal/executorkeep's Ending and the turn's row).
//
// Outcome, Decision, Answer, DeliveredTo and the delivery facts are the
// engine's words; every other text is fenced under the turn's token
// ([endingAccounts]), even the engine's accounts, which quote what a box, a
// tool or a store said. StoppedBy is a name somebody chose, defused in place.
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

	// Delivered says its reply reached the party waiting on DeliveredTo, the
	// surface it was owed on (set whether or not it went; empty where nobody
	// waited), and DeliveryUnknown that the turn's record of its calls is too
	// incomplete to tell.
	Delivered       bool
	DeliveredTo     string
	DeliveryUnknown bool

	// CompletedWork is what its reviewer said landed, and BlockedOn what its
	// executor said stopped a blocked round.
	CompletedWork string
	BlockedOn     string

	// StoppedBy is who stopped it, and StopReason the reason they gave.
	StoppedBy  string
	StopReason string

	// Failure is the engine's account of the guard, budget or loss that ended
	// it, Unkept why its newest session was not kept ("" where it was), and
	// Spent how a launch of it spent its session and what that means ("" where
	// none did).
	Failure string
	Unkept  string
	Spent   string

	// Ask is the first kibibyte of what the turn was asked; the row does not
	// name its writer.
	Ask string
}

// ConversationEndings is what [BuildConversationEndings] renders.
type ConversationEndings struct {
	// Seat is the handle of the seat the block is for, whose phases wrote the
	// accounts of what landed and what blocked.
	Seat string

	// Head is the turn whose session the next turn continues ("" where none
	// left one): told whatever the bound.
	Head string

	// Turns are the ended turns not yet shown, the head among them, in the
	// order they ended.
	Turns []TurnEnding

	// Omitted counts older turns the read itself left out, added to the
	// block's own count.
	Omitted int

	// Fires says the conversation is a schedule's (internal/notify's
	// Scheduled), told in the schedule's words.
	Fires bool
}

// BuildConversationEndings renders how the conversation's earlier turns ended
// under the turn's token b, within [EndingsMaxBytes], and returns the turns it
// told in the order they ended: only those are absorbed by the turn's end, so
// one left out stays for the next turn. Empty for no ended turn.
func BuildConversationEndings(b provenance.Boundary, in ConversationEndings) (string, []string) {
	if len(in.Turns) == 0 {
		return "", nil
	}
	words := endingWordsFor(in.Fires)
	rendered := make([]string, len(in.Turns))
	for i, t := range in.Turns {
		rendered[i] = renderEnding(b, in.Seat, words, t, true)
	}
	// The count's room is held back from the start at its longest (every
	// turn counted). Encoded lengths of the parts add up to the block's,
	// since JSON escapes rune by rune ([encodedLen]).
	gap := encodedLen("\n\n")
	room := EndingsMaxBytes - len(`""`) - encodedLen(words.header) - gap -
		encodedLen(words.leftOut(len(in.Turns)+in.Omitted))
	told := make([]bool, len(in.Turns))
	used := 0
	for i, t := range in.Turns {
		if in.Head == "" || t.Turn != in.Head {
			continue
		}
		if gap+encodedLen(rendered[i]) > room {
			rendered[i] = renderEnding(b, in.Seat, words, t, false)
		}
		told[i], used = true, gap+encodedLen(rendered[i])
		break
	}
	for i := len(in.Turns) - 1; i >= 0; i-- {
		if told[i] {
			continue
		}
		cost := gap + encodedLen(rendered[i])
		if used+cost > room {
			break
		}
		told[i], used = true, used+cost
	}
	parts := []string{words.header}
	var turns []string
	for i, t := range in.Turns {
		if told[i] {
			parts, turns = append(parts, rendered[i]), append(turns, t.Turn)
		}
	}
	if left := len(in.Turns) - len(turns) + in.Omitted; left > 0 {
		parts = append(parts, words.leftOut(left))
	}
	return strings.Join(parts, "\n\n"), turns
}

// encodedLen is how many bytes s takes inside a JSON string, quotes left off
// ([EndingsMaxBytes]). A Go string always encodes (invalid UTF-8 becomes
// U+FFFD), so the error is ignored.
func encodedLen(s string) int {
	encoded, _ := json.Marshal(s)
	return len(encoded) - len(`""`)
}

// leftOut is the line counting the n ended turns the block does not show.
func (w endingWords) leftOut(n int) string {
	if n == 1 {
		return "1 " + w.one + " not shown here."
	}
	return fmt.Sprintf("%d %s not shown here.", n, w.many)
}

// renderEnding is one ended turn: the engine's facts, defused under b, then,
// where prose is set, every account fenced under b; without prose the
// accounts are left out, saying so.
func renderEnding(b provenance.Boundary, seat string, words endingWords, t TurnEnding,
	prose bool) string {

	var w strings.Builder
	w.WriteString("### " + words.turn + " " + provenance.Neutralise(b, t.Turn))
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

// account is one fenced field of an ending, its lead line and its label.
type account struct {
	lead  string
	label provenance.Label
	text  string
}

// endingAccounts is every non-empty account of t, in the order told.
//
// Each is at the least trust whoever wrote it, since each quotes what it read
// (a box's output, a store's refusal). The writer is named where the record
// keeps one; the ask's and a stop reason's are not kept.
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
		{"Its executor's session could not go on:", engine, t.Spent},
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
		// Not "with its box or host": lost means no frame saw it end
		// (internal/executorkeep's OutcomeLost), and its box may have been read
		// whole. What ended it is the Failure account.
		line = "It was lost: the engine ended it, not the turn itself, and it is not carried on."
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
