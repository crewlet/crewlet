package notify

import (
	"maps"
	"slices"
	"strings"
)

// Generic is the prompt for a source nobody has written one for.
//
// An extension's own events, or a third-party app added to config before its
// prompt exists. Its answers are the CONSERVATIVE reading of each question,
// which is not the same as the empty one:
//
//   - It renders the body in full, because for a source the spine knows
//     nothing about, the body is all the context there is.
//   - It therefore reports no recon needed: the trigger IS the context, so
//     the turn-start filters have something real to filter against.
//   - It derives no partition key, so its events are never merged. Merging
//     two triggers the spine cannot tell apart loses one of them.
//   - It supersedes nothing, because it has no idea which of this
//     third-party app's bodies are state and which are messages.
type Generic struct{}

var _ Prompt = Generic{}

// Source is empty, which keeps Generic out of a registry built from a list: it
// is the FALLBACK, reached through For, not an entry that could shadow a real
// third-party app whose name somebody left blank.
func (Generic) Source() string { return "" }

// RequiresRecon is false: an unrecognised source has no API to read a
// conversation's membership from, so the text has to decide alone.
func (Generic) RequiresRecon(Inbound) bool { return false }

// SubjectIsLabel is false: for a source the spine knows nothing about, the
// subject is part of what it sent, and dropping it could drop the topic — the
// conservative half, see [Prompt.SubjectIsLabel].
func (Generic) SubjectIsLabel(Inbound) bool { return false }

// Addressed is false: an unrecognised source has no routing vocabulary to
// read an ask out of, and the trigger's own evaluation block already tells
// the seat to decide from the text. False is the safe half — see
// [Prompt.Addressed].
func (Generic) Addressed(Inbound) bool { return false }

// PartitionKey is empty for an unrecognised source, which coalesces
// nothing — the honest answer when the backend cannot say what a thread is.
func (Generic) PartitionKey(map[string]string, string) string { return "" }

// ConversationIdentity is empty for the SAME reason, and the two halves are
// both load-bearing: an unrecognised source's events merge with nothing AND
// record nothing, because a conversation the spine cannot name is one no
// later message could ever read back.
func (Generic) ConversationIdentity(map[string]string, string) string { return "" }

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
// THE EVALUATION BLOCK IS THE POINT, and it is here rather than in each
// third-party app because it is a fact about being an agent in a company,
// not about the third-party app: a seat that replies to everything it is
// mentioned in is noise, and an unaddressed wake — which every generic one is
// ([Generic.Addressed]) — has nothing but this text keeping it quiet.
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

// evaluationBlock is the shared "were you actually asked?" guidance, for a
// wake its source does NOT read as addressed.
//
// Exported through Build rather than duplicated per integration: every
// source's prompt for activity a seat merely follows wants it, and a copy per
// integration is how five of them come to say slightly different things about
// when silence is correct.
//
// NEVER ON AN ADDRESSED WAKE. A mention or an assignment owes an answer, and
// the delivery check refuses a silent end to it — so telling that seat it may
// stay silent buys a corrective round and nothing else. What a seat does when
// it declines a direct ask is the executor's contract, stated once there.
const evaluationBlock = `
## Evaluate Before Acting
Act only if this asks something of you — read the verb, not just whether your name appears — or changes work you have in hand. Otherwise stay silent.
`

// EvaluationBlock is that guidance, for a third-party app's prompt that
// builds its own body and wants the same ending on an unaddressed wake.
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
