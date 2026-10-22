package statelog

import "errors"

// THREE REFUSALS AS A PERSON READS THEM.
//
// Three of this framework's refusals mean one thing whatever the domain and
// whatever the object, and say what to do next on their own: a write that lost
// its compare-and-set rounds ([ErrConflict]), a create of what is already there
// ([ErrExists]) and a write about what a deletion marker keeps deleted
// ([ReasonDeleted]). What the error itself says about each is the subject it
// met, a broker path nobody can act on, so every surface that tells one of
// them tells it in the words below: the agent's tools, to a model, and every
// surface that answers a person in words of its own, the identity directory
// first among them. A person's act route and the human write surface answer
// each in its code's own sentence instead (ADR-0060).
//
// HERE, BESIDE THE SENTINELS, because this is the one package every surface
// that meets them already imports. The words lived in internal/agent/builtin,
// which the API's surfaces outside the operator's do not import, so the
// identity directory answered a lost race with the framework's own text, the
// subject it met included. A second copy there would have been the same
// sentence kept in step by nothing.
//
// Every other refusal of this package is a condition of the node that met it,
// whose [Unavailable.Detail] and [Refused.Detail] are composed for whoever
// receives them; this function answers none of them.

// LostRace is a write that lost its compare-and-set rounds ([ErrConflict]): what
// happened, that nothing was written, and that the same change may be made
// again once its writer has looked at what the object says now. A seat pause
// that lost its compare-and-set on the coordination store is told in these
// words too, since to the person who asked it is the same thing.
const LostRace = "Someone else changed this at the same moment, so nothing " +
	"was written. Look at it again, then make the change again if it still " +
	"needs making."

// AlreadyThere is a create the framework found already made ([ErrExists]) —
// most often the same create sent again after its operation was forgotten.
const AlreadyThere = "What this creates is already there, so nothing was " +
	"written. Look at what's there rather than making it again."

// Deleted is a write about something a deletion marker keeps deleted
// ([ReasonDeleted]): nothing will ever write it again, on any node.
const Deleted = "This was deleted for good, so nothing was written."

// Sentence is one of the three refusals above as a person reads it, and false
// for any other error, a condition of the node among them.
//
// A DOMAIN'S OWN WRAP IS THE SAME REFUSAL: a domain that wraps [ErrConflict]
// in a sentinel of its own ("the page kept changing under this write") lost
// the same race, and is told in the same words.
func Sentence(err error) (string, bool) {
	var refused *Unavailable
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, ErrConflict):
		return LostRace, true
	case errors.Is(err, ErrExists):
		return AlreadyThere, true
	case errors.As(err, &refused) && refused.Reason == ReasonDeleted:
		return Deleted, true
	}
	return "", false
}
