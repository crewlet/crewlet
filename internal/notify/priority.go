package notify

import (
	"strings"

	"github.com/crewlet/crewlet/internal/iam"
)

// A PERSON WAITING ON A SEAT IS TAKEN BEFORE THE SEAT'S BACKGROUND WORK.
//
// A wake rides its seat's priority lane (queue.Prioritize) exactly when two
// things hold, and both are answers this package already gives elsewhere:
//
//   - THE SEAT OWES AN ANSWER, by the backend's own [AddressRule]: a direct
//     message, a mention, a question it was asked, a thread it is in for one
//     of those reasons. The same value the working indicator and the delivery
//     check read ([Prompt.Addressed]), so the three cannot disagree about
//     whether somebody is waiting.
//   - A PERSON WROTE IT ([WrittenByPerson]). An agent's mention of a colleague
//     is work between seats and waits its turn with the rest of it; a
//     person's is somebody watching a screen for the reply.
//
// Everything else (a tracker notice, a room's news, a colleague's ask, a
// schedule's fire) is background, and the order among background work is
// what it always was. [Service] applies the rule as it publishes each wake.

// WrittenByPerson reports whether the text a notification carries was written
// by a person rather than by an agent or by the engine itself.
//
// THE KIND THE RECORD STATED FIRST ([BodyAuthorKindField]), because a source
// the engine hosts names its writer's kind on every record, a person whose
// seat has since gone included. Then the colleague the sender resolves to,
// in the source's own namespace ([Parties.ByExternalID]): a seat is a person
// or an agent. A sender nobody in the company holds is somebody outside it,
// which on every surface a seat is reached on is a person; no sender at all
// is nobody.
func WrittenByPerson(reg Parties, n Inbound) bool {
	switch iam.ActorKind(n.Metadata[BodyAuthorKindField]) {
	case iam.ActorHuman, iam.ActorOperator:
		return true
	case iam.ActorAgent, iam.ActorSystem:
		return false
	}
	sender := strings.TrimSpace(n.Sender)
	if sender == "" {
		return false
	}
	if reg != nil {
		if party, ok := reg.ByExternalID(n.Source, sender); ok {
			return party.Human
		}
	}
	return true
}
