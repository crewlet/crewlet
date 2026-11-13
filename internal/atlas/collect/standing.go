package collect

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
)

// State is how a source's last read went, in one word.
type State string

// The states of a source.
const (
	// StateNever is a source the duty has not read yet.
	StateNever State = "never"
	// StateRead is a source whose last read finished and read every part.
	StateRead State = "read"
	// StatePartial is a source whose last read finished and was refused
	// some part ([Finding]).
	StatePartial State = "partial"
	// StateFailed is a source whose last read did not finish.
	StateFailed State = "failed"
)

// States is every state, in the order a reader meets them.
var States = []State{StateNever, StateRead, StatePartial, StateFailed}

// Valid reports whether s is one of [States].
func (s State) Valid() bool { return slices.Contains(States, s) }

// Standing is one source as a reader is shown it: its recorded standing
// ([Status]) without the read's digest or the ids of what it stopped seeing,
// the ask that waits on it, and when it is read next.
type Standing struct {
	Kind  integration.Kind `json:"kind"`
	State State            `json:"state"`

	// LastAttemptAt is when the source was last read, and LastCompleteAt
	// when it was last read whole; absent where it never was.
	LastAttemptAt  time.Time `json:"last_attempt_at,omitzero"`
	LastCompleteAt time.Time `json:"last_complete_at,omitzero"`

	// Entries, Calls and Withheld are the last complete read's: the entries
	// it held, the requests it made at the vendor, and the values it left
	// out for their shape, counted and never shown.
	Entries  int `json:"entries"`
	Calls    int `json:"calls"`
	Withheld int `json:"withheld"`

	// Recorded, Retired and Purged are what the last attempt wrote.
	Recorded int `json:"recorded"`
	Retired  int `json:"retired"`
	Purged   int `json:"purged"`

	// Missing is how many entries complete reads have stopped seeing and
	// have not retired yet.
	Missing int `json:"missing"`

	// LastError is the fault of the last attempt, for an administrator;
	// Failed says that there was one to every reader.
	LastError string `json:"last_error,omitempty"`

	// Unread is each part the last complete read was refused, and
	// UnreadOverflow how many more there were.
	Unread         []Finding `json:"unread"`
	UnreadOverflow int       `json:"unread_overflow"`

	// Scopes is each part of the source with a standing of its own: each
	// server the SSH source reads.
	Scopes []Scope `json:"scopes"`

	// NextReadAt is when the source is next read on its own; absent where
	// it is due now.
	NextReadAt time.Time `json:"next_read_at,omitzero"`

	// RefreshRequestedAt and RefreshRequestedBy are the ask that waits on
	// the source, absent where none does; RefreshableAt is the first
	// instant an ask is honoured, absent where that is now.
	RefreshRequestedAt time.Time `json:"refresh_requested_at,omitzero"`
	RefreshRequestedBy string    `json:"refresh_requested_by,omitempty"`
	RefreshableAt      time.Time `json:"refreshable_at,omitzero"`
}

// Report is every source the company connects, as a reader is shown them,
// and the cadence they are read on.
type Report struct {
	Sources []Standing `json:"sources"`

	// RefreshIntervalSeconds is how often each source is read on its own,
	// and RefreshFloorSeconds the least time between two reads a person
	// can ask for.
	RefreshIntervalSeconds int `json:"refresh_interval_seconds"`
	RefreshFloorSeconds    int `json:"refresh_floor_seconds"`
}

// Describe is the report of kinds, the sources this build reads that the
// company connects, in the order of their names. A kind with no recorded
// standing is one never read. PURE.
func Describe(kinds []integration.Kind, standings map[integration.Kind]Status,
	asked map[integration.Kind]Request, interval time.Duration, now time.Time) Report {

	kinds = slices.Clone(kinds)
	slices.Sort(kinds)
	out := Report{
		Sources:                make([]Standing, 0, len(kinds)),
		RefreshIntervalSeconds: int(interval / time.Second),
		RefreshFloorSeconds:    int(RefreshFloor / time.Second),
	}
	for _, kind := range slices.Compact(kinds) {
		out.Sources = append(out.Sources, describe(kind, standings[kind], asked[kind], interval, now))
	}
	return out
}

// describe is one source's standing as a reader is shown it.
func describe(kind integration.Kind, st Status, req Request, interval time.Duration, now time.Time) Standing {
	missing := st.MissingOverflow
	for _, g := range st.Missing {
		missing += len(g.Entries)
	}
	out := Standing{
		Kind: kind, State: st.state(),
		LastAttemptAt: st.LastAttemptAt, LastCompleteAt: st.LastCompleteAt,
		Entries: st.Entries, Calls: st.Calls, Withheld: st.Withheld,
		Recorded: st.Recorded, Retired: st.Retired, Purged: st.Purged,
		Missing: missing, LastError: st.LastError,
		Unread: slices.Clone(st.Unread), UnreadOverflow: st.UnreadOverflow,
		Scopes: slices.Clone(st.Scopes),
	}
	if out.Unread == nil {
		out.Unread = []Finding{}
	}
	if out.Scopes == nil {
		out.Scopes = []Scope{}
	}
	if next := st.NextReadAt(interval); next.After(now) {
		out.NextReadAt = next
	}
	if req.Waits(st) {
		out.RefreshRequestedAt, out.RefreshRequestedBy = req.RequestedAt, req.By
	}
	if at := st.RefreshableAt(); at.After(now) {
		out.RefreshableAt = at
	}
	return out
}

// state is the standing in one word.
func (s Status) state() State {
	switch {
	case s.LastAttemptAt.IsZero():
		return StateNever
	case !s.Completed:
		return StateFailed
	case len(s.Unread) > 0:
		return StatePartial
	}
	return StateRead
}
