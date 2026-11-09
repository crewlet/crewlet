package accessreview

import (
	"context"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
)

var log = logging.Get("accessreview")

// Parser turns one review request into the wake of the seat it names; a seat
// the roster does not hold as an agent wakes nobody.
type Parser struct{}

var _ notify.Parser = Parser{}

// Source implements [notify.Parser].
func (Parser) Source() string { return Source }

// Parse implements [notify.Parser]: one wake, its id from [WakeID] and its
// instant the baseline told record's, the same on every copy. No sender: the
// turn is the company's own (ADR-0109).
func (Parser) Parse(_ context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	r, err := Decode(w)
	if err != nil {
		return nil, err
	}
	if reg != nil {
		party, ok := reg.ByHandle(r.Handle)
		if !ok || party.Human {
			log.Info("access_review_reaches_nobody", "agent", r.Handle,
				"detail", "the company no longer employs this handle as an agent seat")
			return nil, nil
		}
	}
	return []notify.Routed{{
		Inbound: notify.Inbound{
			Source:    Source,
			EventType: EventType,
			Subject:   Subject,
			Metadata: map[string]string{
				BaselineField: r.Baseline,
				DigestField:   r.Digest,
				ChangesField:  strconv.Itoa(len(r.Lines)),
				OpenField:     strconv.Itoa(r.Open),
			},
		},
		To:     notify.Recipient{Handle: r.Handle},
		WakeID: WakeID(r),
		WakeAt: r.BaselineAt,
	}}, nil
}

// Subject is the one line a review's wake is named by: the same words on every
// review, so a label rather than content ([Prompt.SubjectIsLabel]).
const Subject = "Your access changed: review your open work"

// Prompt is what a review asks of its seat: to review the open work listed at
// its dispatch ([Work.Section]) and nothing else. Never addressed, so the turn
// may end having only looked.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt]: always; the task points at the
// seat's own work, which it reads with its own tools.
func (Prompt) RequiresRecon(notify.Inbound) bool { return true }

// SubjectIsLabel implements [notify.Prompt]: always, the subject is [Subject].
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return true }

// Addressed implements [notify.Prompt]: never. See the type's doc.
func (Prompt) Addressed(notify.Inbound) bool { return false }

// PartitionKey implements [notify.Prompt]: one conversation per seat, so
// reviews waiting together merge into one turn.
func (Prompt) PartitionKey(map[string]string, string) string { return conversation }

// ConversationIdentity implements [notify.Prompt]: the same.
func (Prompt) ConversationIdentity(map[string]string, string) string { return conversation }

// conversation is a seat's reviews' one conversation.
const conversation = "review"

// DigestBody implements [notify.Prompt]: nothing; every review asks the same.
func (Prompt) DigestBody(string, string) string { return "" }

// WakesActor implements [notify.Prompt]: never; a review names no actor.
func (Prompt) WakesActor(notify.Inbound) bool { return false }

// Provenance implements [notify.Prompt]: a note about a change somebody made.
// The parser writes no body, so nothing is fenced under it.
func (Prompt) Provenance(notify.Inbound, notify.Parties) notify.Provenance {
	return notify.Provenance{Source: provenance.SourceChangeNote}
}

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, _ notify.Parties) string {
	out := Task
	if n.Body != "" {
		out += "\n\n" + n.Body
	}
	return out
}

// Task is a review turn's task: the review of what [Work.Section] lists, and
// nothing else.
var Task = "**Access review.** Your access changed, and some of your open work was " +
	"raised or started under your earlier access. This turn is for reviewing that work " +
	"alone. What changed is under \"" + changesTitle + "\" above, and the work under \"" +
	SectionTitle + "\" beside it. Check each item against the change, update or withdraw " +
	"each one the change affects as that section says, and change nothing else: start no " +
	"new work, and end the turn once every item is checked."

// changesTitle is the heading the turn is told its change under, as the
// capability package renders it.
var changesTitle = strings.TrimPrefix(capability.ChangesHeading, "## ")
