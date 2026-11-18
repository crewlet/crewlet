package statelog

import (
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// TransitionChange is what one alarm instance did between two evaluations a
// [Tracker] observed.
type TransitionChange string

const (
	// TransitionFired is an instance up that the tracker's previous
	// evaluation did not have up.
	TransitionFired TransitionChange = "fired"

	// TransitionCleared is an instance the tracker had up that its latest
	// evaluation does not.
	TransitionCleared TransitionChange = "cleared"
)

// TransitionChanges is the closed set, in the order an instance meets them.
func TransitionChanges() []TransitionChange {
	return []TransitionChange{TransitionFired, TransitionCleared}
}

// Valid reports whether c is a change this build makes.
func (c TransitionChange) Valid() bool { return slices.Contains(TransitionChanges(), c) }

// AlarmTransition is one alarm instance changing on one node's tracker — the
// record a wake about it is derived from (ADR-0095). ONE PER [instance], as
// the log lines are: a log or server joining an alarm already up is a
// transition of its own.
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

	// Entry is the configured entry a per-entry alarm stands on, a model
	// entry or an MCP server ([PerEntryKinds]); empty for every other kind.
	Entry string `json:"entry,omitempty"`

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
// kind on node (on log domain, or on configured entry) that was raised at
// since.
//
// DERIVED, never minted: these six name exactly one transition, so a
// republish or a second delivery carries the same id and a wake derived from
// it is recognised as a duplicate. THE ENTRY IS ONE OF THE SIX because two
// servers down in one evaluation share every other part, and the second's wake
// would be dropped as the first's. A UUIDv5, so the id has one length and no
// separator a name could carry.
func TransitionID(node string, kind Kind, domain, entry string, change TransitionChange, since time.Time) string {
	key := node + "\x00" + string(kind) + "\x00" + domain + "\x00" + entry + "\x00" + string(change) + "\x00" +
		strconv.FormatInt(since.UnixNano(), 10)
	return uuid.NewSHA1(transitionNamespace, []byte(key)).String()
}

// transitionNamespace scopes transition ids. Fixed for the life of the
// deployment: a new one would give a republished transition a new id.
var transitionNamespace = uuid.MustParse("2451aa12-6e22-460a-a0cc-769ed90fbbb5")

// Describe is the table's meaning and remedy for kind (the columns
// [AlarmReference] prints), and false for a kind this build does not raise. A
// surface that tells somebody of an alarm, a woken seat's prompt among them,
// reads the table's words here rather than a copy that drifts.
func Describe(kind Kind) (meaning, remedy string, ok bool) {
	for _, r := range table {
		if r.kind == kind {
			return alarmMeaning[kind], r.remedy, true
		}
	}
	return "", "", false
}
