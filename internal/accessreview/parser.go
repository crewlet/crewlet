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

// Parser turns one review request into the wake of the seat it names.
//
// IT TAKES NOTHING OF THE COMPANY: the request names its seat, and the roster
// the delivery is routed against says whether the company still employs it as
// an agent. A seat the company removed, or a handle that names a person, wakes
// nobody.
type Parser struct{}

var _ notify.Parser = Parser{}

// Source implements [notify.Parser].
func (Parser) Source() string { return Source }

// Parse implements [notify.Parser]: one wake, with the id [WakeID] derives and
// the instant the node measured the change, so every copy of one request
// routes identically.
//
// NO SENDER AND NO WRITER: the engine asks for the review, and a turn nobody
// woke is held to the seat's own authority alone (ADR-0109), so a requester
// ceiling such as SSH's treats it as the company's own turn.
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
		WakeAt: r.At,
	}}, nil
}

// Subject is the one line a review's wake is named by: the same words on every
// review, so a label rather than content ([Prompt.SubjectIsLabel]).
const Subject = "Your access changed: review your open work"

// Prompt is what a review asks of its seat.
//
// # The turn reviews, and does nothing else
//
// The change and the open work are rendered at the turn's dispatch from the
// access log and the seat's own records ([Work.Section]), above the task,
// which is why the task names them rather than repeating them: what the node
// measured when it asked may be older than what the turn is told.
//
// # Nobody is waiting on an answer
//
// The review may find nothing the change affects, and a turn obliged to post
// would post that. So no copy is addressed, and the turn may end having only
// looked.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt]: always. The task is a pointer at
// the seat's own work, which the turn reads with its own tools, so the
// turn-start relevance filters have nothing to filter against.
func (Prompt) RequiresRecon(notify.Inbound) bool { return true }

// SubjectIsLabel implements [notify.Prompt]: always, the subject is [Subject].
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return true }

// Addressed implements [notify.Prompt]: never. See the type's doc.
func (Prompt) Addressed(notify.Inbound) bool { return false }

// PartitionKey implements [notify.Prompt]: ONE CONVERSATION PER SEAT, so the
// reviews a seat's inbox holds pending together merge into one turn, which is
// told the newest difference whatever each request measured.
func (Prompt) PartitionKey(map[string]string, string) string { return conversation }

// ConversationIdentity implements [notify.Prompt]: the same, the thread a
// seat's reviews follow over time.
func (Prompt) ConversationIdentity(map[string]string, string) string { return conversation }

// conversation is a seat's reviews' one conversation.
const conversation = "review"

// DigestBody implements [notify.Prompt]: nothing of an earlier copy. Every
// review asks the same, and the turn is told the change at its dispatch.
func (Prompt) DigestBody(string, string) string { return "" }

// WakesActor implements [notify.Prompt]: never, and the question does not
// arise, since a review names no actor.
func (Prompt) WakesActor(notify.Inbound) bool { return false }

// Provenance implements [notify.Prompt]: a note about a change somebody made,
// by nobody the engine names. The parser writes no body, so nothing is fenced
// under it; a body a later build writes is read at that trust.
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
