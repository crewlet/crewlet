package statelog

import (
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// TransitionChange is what one alarm instance did between two evaluations a
// [Tracker] observed.
//
// A NAMED STRING, because it travels: on the inbound delivery a transition is
// published as, in the prompt a woken seat reads, and as the attribute of the
// counter the publishes are counted under.
type TransitionChange string

const (
	// TransitionFired is an instance up that the tracker's previous
	// evaluation did not have up.
	TransitionFired TransitionChange = "fired"

	// TransitionCleared is an instance the tracker had up that its latest
	// evaluation does not.
	TransitionCleared TransitionChange = "cleared"

	// TransitionFiringAtBoot is an instance the tracker's FIRST evaluation
	// found up.
	//
	// NOT "fired": the tracker began watching with it already up, so
	// whether it started at that instant or long before the node restarted
	// is not something the tracker can say — and whoever is told reads the
	// two differently. It is also how a clear the node did not live to hand
	// on is answered: whatever is still up is said again, under this.
	TransitionFiringAtBoot TransitionChange = "firing_at_boot"
)

// TransitionChanges is the closed set, in the order an instance meets them.
func TransitionChanges() []TransitionChange {
	return []TransitionChange{TransitionFiringAtBoot, TransitionFired, TransitionCleared}
}

// Valid reports whether c is a change this build makes.
func (c TransitionChange) Valid() bool { return slices.Contains(TransitionChanges(), c) }

// AlarmTransition is one alarm instance changing on one node's tracker — the
// record a wake about it is derived from (ADR-0088).
//
// ONE PER INSTANCE, as the log lines are: a per-log alarm standing on two logs
// is two instances, and the second log joining it is a transition of its own
// ([instance]).
type AlarmTransition struct {
	// ID is the transition's own id ([TransitionID]), which every copy of
	// it carries and every wake derived from it is keyed on.
	ID string `json:"id"`

	// Node is the node whose tracker observed it.
	Node string `json:"node"`

	// Kind is the alarm.
	Kind Kind `json:"kind"`

	// Domain is the log a per-log alarm stands on ([PerLogKinds]); empty
	// for a node-wide one.
	Domain string `json:"domain,omitempty"`

	// Change is what the instance did.
	Change TransitionChange `json:"change"`

	// Detail is the measurement: the raising evaluation's for a fire, the
	// last one seen while it stood for a clear.
	Detail string `json:"detail,omitempty"`

	// Since is when the instance was raised; a fire's is its own instant.
	Since time.Time `json:"since"`

	// At is when the tracker observed the transition.
	At time.Time `json:"at"`
}

// TransitionID is the id of the transition change made by the instance of
// kind on node — on the log domain, for a per-log alarm — that was raised at
// since.
//
// DERIVED from what makes a transition the one it is, never minted at random:
// an instance is raised once at an instant and clears once, so these five
// name exactly one transition, and every copy of it — a republish, a second
// delivery — carries the same id, which is what lets a wake derived from it be
// recognised as a duplicate of itself. A UUIDv5 rather than the joined string,
// so the id has one length and no separator a node or a log name could carry.
func TransitionID(node string, kind Kind, domain string, change TransitionChange, since time.Time) string {
	key := node + "\x00" + string(kind) + "\x00" + domain + "\x00" + string(change) + "\x00" +
		strconv.FormatInt(since.UnixNano(), 10)
	return uuid.NewSHA1(transitionNamespace, []byte(key)).String()
}

// transitionNamespace scopes transition ids. Fixed for the life of the
// deployment: a new one would give a republished transition a new id.
var transitionNamespace = uuid.MustParse("2451aa12-6e22-460a-a0cc-769ed90fbbb5")

// Describe is what the table says of kind — what it means in an operator's
// words and what to do about it, the two columns the published reference
// prints ([AlarmReference]) — and false for a kind this build does not raise.
//
// FOR A SURFACE THAT TELLS SOMEBODY OF AN ALARM away from the reference, a
// woken seat's prompt among them, so the words it reads are the table's own
// rather than a second copy that drifts from it.
func Describe(kind Kind) (meaning, remedy string, ok bool) {
	for _, r := range table {
		if r.kind == kind {
			return alarmMeaning[kind], r.remedy, true
		}
	}
	return "", "", false
}
