package assistant

import (
	"time"

	"github.com/crewlet/crewlet/internal/assistant/live"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// THE TURN'S BOUNDS. What a company sets (the step cap, the turn's wall clock,
// a proposal's lifetime, the retention) is read from its `assistant:` block
// through internal/config's accessors and is not restated here; what is
// stored is bounded by internal/assistantdomain, whose writer refuses past it,
// and is referenced rather than restated; the frame log's bounds are
// internal/assistant/live's. What is left is the turn's own.
const (
	// PromptVersion names the system prompt's text, stamped on every turn
	// row and audit record, so a change in behaviour can be read against a
	// change in the prompt. A reworded section is a new version.
	PromptVersion = "assistant.prompt.v6"

	// ModelResultBytes is the most of one tool result the model reads:
	// sixteen kibibytes, a quarter of what a call reads at most
	// ([exec.MaxResponseBytes]), so a turn's forty steps at most
	// (internal/config's max_steps ceiling) fit a 200,000 token context
	// beside the prompt. A card keeps up to its own bound
	// separately; the model is handed a summary of it.
	ModelResultBytes = 16 << 10

	// HistoryMessages is how many earlier messages of the conversation a
	// turn reads: twelve, six exchanges, which carry a follow-up chain
	// ("now only engineering", "save that") at a few thousand tokens.
	// Older turns are named by count only.
	HistoryMessages = 12

	// RunningTurnsPerPerson is how many turns one person may have running
	// at once across their conversations: two, half of the four slots the
	// recommended assistant entry runs, so one person never holds the
	// whole drawer for everybody else. Past it: 429 assistant_busy.
	RunningTurnsPerPerson = 2

	// TurnsPerMinute and TurnBurst are one person's turn rate on this node:
	// twenty a minute with five at once, which nobody typing questions
	// reaches and a script looping on the route does at once.
	TurnsPerMinute = 20
	TurnBurst      = 5

	// StreamsPerPerson is how many turn streams one person may hold open on
	// a node at once: four, so two tabs each following a turn and a
	// reconnect overlapping the stream it replaces fit, while a script
	// opening thousands cannot hold a goroutine and a frame buffer for
	// each. Past it: 429 assistant_busy.
	StreamsPerPerson = 4

	// StreamOpensPerMinute and StreamOpenBurst are one person's stream
	// opens: sixty a minute with ten at once, on this node and across the
	// fleet's counter alike. A drawer opens one per turn and reconnects with
	// backoff; the bound is what keeps a loop on the route from unsealing an
	// ended turn's whole transcript on every request.
	StreamOpensPerMinute = 60
	StreamOpenBurst      = 10

	// NodeStreamOpensPerSecond and NodeStreamOpenBurst are every person's
	// stream opens on this node together.
	NodeStreamOpensPerSecond = 20
	NodeStreamOpenBurst      = 100

	// NodeTurnsPerSecond and NodeTurnBurst are every person's turns on this
	// node together: two a second with twenty at once. The model entry's
	// own slots are the real bound on what runs; this only stops a stampede
	// of starts from queueing more turns than those slots could serve in a
	// turn's lifetime.
	NodeTurnsPerSecond = 2
	NodeTurnBurst      = 20

	// GrantMargin is how long before its read grant expires a turn stops
	// sending requests: thirty seconds, so a call is never sent on a grant
	// that lapses while the route serves it, which would read as a refusal.
	GrantMargin = 30 * time.Second

	// TitleTimeout bounds the one model call that names a new
	// conversation: it is best effort and runs after the answer, so a slow
	// model costs a title, never the turn.
	TitleTimeout = 30 * time.Second

	// TitleMaxRunes bounds a conversation's title as the model writes it.
	TitleMaxRunes = 60

	// The stored bounds, from internal/assistantdomain.
	MessageMaxBytes = assistantdomain.MessageMaxBytes
	CardMaxBytes    = assistantdomain.CardMaxBytes
	ParkedMaxBytes  = assistantdomain.ParkedMaxBytes

	// CardMaxRows is the most rows one table card carries: a thousand, which
	// a person can still scroll and export, and well inside [CardMaxBytes]
	// for the narrow tables the catalogue builds.
	CardMaxRows = 1000

	// SubscriberFrames is the frame log's: how far a stream may fall behind.
	SubscriberFrames = live.SubscriberFrames

	// PersonBudgetScope is the scope a refusal of a person's OWN ceiling
	// (`assistant.token_budget`) names, beside "org" for the company's: the
	// engine's meter names it, the turn tells the person it is theirs, and
	// `budget_exhausted`'s detail carries it.
	PersonBudgetScope = "person"
)

// assistantHome is the per-person home a harness-backed model runs each of the
// person's rounds in (llm.WithSeat): `assistant:` and the first twelve hex
// digits of their id. A COLON, which no seat handle may carry (a seat handle
// is one segment, internal/iam), so a person's assistant home can never be a
// seat's, whatever a company names its seats; and per person rather than the
// shared home, so nothing one person's rounds leave on disk is in another's.
func assistantHome(person [16]byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len("assistant:")+12)
	out = append(out, "assistant:"...)
	for _, b := range person[:6] {
		out = append(out, hexdigits[b>>4], hexdigits[b&0x0f])
	}
	return string(out)
}
