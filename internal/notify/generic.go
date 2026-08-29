package notify

import (
	"maps"
	"slices"
	"strings"
)

// Generic is the prompt for a source nobody has written one for.
//
// An extension's own events, or a vendor added to config before its prompt
// exists. Its answers are the CONSERVATIVE reading of each question, which is
// not the same as the empty one:
//
//   - It renders the body in full, because for a source the spine knows
//     nothing about, the body is all the context there is.
//   - It therefore reports no recon needed: the trigger IS the context, so
//     the Plan-phase filters have something real to filter against.
//   - It derives no conversation key, so its events are never merged. Merging
//     two triggers the spine cannot tell apart loses one of them.
//   - It supersedes nothing, because it has no idea which of this vendor's
//     bodies are state and which are messages.
type Generic struct{}

var _ Prompt = Generic{}

// Source is empty, which keeps Generic out of a registry built from a list: it
// is the FALLBACK, reached through For, not an entry that could shadow a real
// vendor whose name somebody left blank.
func (Generic) Source() string { return "" }

// RequiresRecon is false: an unrecognised source has no API to read a
// conversation's membership from, so the text has to decide alone.
func (Generic) RequiresRecon(Inbound) bool { return false }

// ConversationKey is empty for an unrecognised source, which coalesces
// nothing — the honest answer when the backend cannot say what a thread is.
func (Generic) ConversationKey(map[string]string, string) string { return "" }

// WakesActor is false for an unrecognised source: an event type nobody has
// classified that turns out to loop takes the company down with it, while
// one that goes unheard costs a single notification.
func (Generic) WakesActor(string) bool { return false }

// DigestBody passes the body through unchanged: a backend with no message
// format of its own has nothing to render a digest into.
func (Generic) DigestBody(_, body string) string { return body }

// Build renders the notification and then tells the seat how to decide whether
// it was asked for anything.
//
// THE EVALUATION BLOCK IS THE POINT, and it is here rather than in each vendor
// because it is a fact about being an agent in a company, not about the
// vendor: a seat that replies to everything it is mentioned in is noise, and a
// seat that silently declines a direct ask looks like the message was lost.
// Both failures are the same root cause — reading "my name appeared" as "I was
// asked" — and both are cheap to fix in the trigger.
func (Generic) Build(n Inbound, _ Parties) string {
	subject := n.Subject
	if subject == "" {
		subject = n.Source + " notification"
	}
	var b strings.Builder
	b.WriteString("You received a notification from " + n.Source + ": " + subject + "\n\n")
	b.WriteString("**Message:**\n" + n.Body + "\n")
	if meta := renderMetadata(n.Metadata); meta != "" {
		b.WriteString("\n**Notification metadata:**\n" + meta + "\n")
	}
	b.WriteString(evaluationBlock)
	return b.String()
}

// evaluationBlock is the shared "were you actually asked?" guidance.
//
// Exported through Build rather than duplicated per vendor: every source's
// prompt wants it, and a copy per vendor is how five of them come to say
// slightly different things about when silence is correct.
const evaluationBlock = `
## Evaluate Before Acting
1. **Were you actually asked to do something?** Read the verb, not just whether your name appears. Being mentioned in passing (e.g. "check with @you on this") or named as the subject of a message addressed to someone else is NOT a request directed at you.
2. **If you were not asked** (informational, FYI, addressee is someone else, passing reference) → stay silent. Silence beats noise.
3. **If you were asked but have decided not to act** (out of scope, already handled, wrong person, declining) → do NOT skip silently. Post a brief reply on the originating channel explaining why — leaving a direct ask unanswered looks like the message was lost.
`

// EvaluationBlock is that guidance, for a vendor prompt that builds its own
// body and wants the same ending.
func EvaluationBlock() string { return evaluationBlock }

// renderMetadata lists the metadata a seat can act on.
//
// SORTED, because map order is randomised and a trigger that reorders itself
// between two identical events makes every diff of a turn's prompt useless.
// Empty values are dropped: a key with nothing behind it tells the seat
// nothing and costs a line it has to read.
func renderMetadata(metadata map[string]string) string {
	var lines []string
	for _, key := range slices.Sorted(maps.Keys(metadata)) {
		if value := metadata[key]; value != "" {
			lines = append(lines, "  "+key+": "+value)
		}
	}
	return strings.Join(lines, "\n")
}
