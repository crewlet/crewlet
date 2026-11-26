package statelog

import (
	"context"
	"slices"
)

// Spent is what ONE READ has already paid for on the data nodes asked for it
// before this one, carried to the next. The estate's router asks the next node
// a read one refused for a reason about itself ([ReadRefusal.Elsewhere]), and a
// node asked afresh pays for the whole read again: a barrier appended to the
// log, then up to a whole [ReadBudget] waited. Carried, the walk pays for each
// ONCE however many nodes it asks, rather than a barrier and a budget per node
// on a fleet all `behind` on the very log those barriers go to.
//
// Every term is keyed by its LOG, because a position is a coordinate on one
// log ([Position.Stream]). The zero value has paid for nothing, which is a
// read asked of its first node.
type Spent struct {
	// Index is the read index established on each log, at most one a log: the
	// position of the FIRST committed barrier a walk appended, which every node
	// it asks after is held to. A linearizable read handed one WAITS FOR THE
	// RECORDS BEFORE IT AND APPENDS NOTHING ([waitsThrough], [Runner.Reached]):
	// every data node applies every node's barriers. Equally linearizable,
	// because [ReadIndex.Read] takes only a barrier started after its reader
	// arrived, and that reader is this same request, so every write
	// acknowledged before it began is below the barrier on any node.
	Index []Position `json:"index,omitempty"`

	// Waited is, on each log, the furthest position a node waited its whole
	// bound for and did not reach. A node waits for no position at or below
	// it: it serves the read if it holds it and refuses `behind` at once if
	// not, because the walk's wait is one bound wherever it is spent: the
	// position is by then at least a bound old, and only a copy lagging by
	// more than a whole read budget, which the first wait found, lacks it.
	Waited []Position `json:"waited,omitempty"`

	// Unconfirmed is every log on which a node's barrier ran its whole budget
	// ([ElectionBarrierBudget], [SoloBarrierBudget]) without committing. A read on it appends NO
	// SECOND barrier: it is refused at once, `no_quorum` where several members
	// store the log and `broker_stalled` where one does
	// ([ReadIndex.unconfirmed]). Every node's barrier goes to the one log's
	// leader, so through an election or behind a stalled write each would wait
	// out its budget alike. The one case a second barrier would commit (a
	// member alone cut off from the leader) gets the refusal's retry hint
	// ([ElectionRetryHint]) rather than every election's reads paying a budget
	// per node. Named for what is known, not why: on one member it is no
	// quorum that was missed.
	Unconfirmed []string `json:"unconfirmed,omitempty"`
}

// IsZero reports whether s has paid for nothing.
func (s Spent) IsZero() bool {
	return len(s.Index) == 0 && len(s.Waited) == 0 && len(s.Unconfirmed) == 0
}

// Merge is what a walk has paid for once a node has also paid for o: the read
// index it already held kept on each log (a later node never establishes
// another while one is held, and every node asked so far was held to the
// first), the further of two waited positions on each, and every log a
// barrier ran out on.
func (s Spent) Merge(o Spent) Spent {
	out := Spent{
		Index:       slices.Clone(s.Index),
		Waited:      slices.Clone(s.Waited),
		Unconfirmed: slices.Clone(s.Unconfirmed),
	}
	for _, at := range o.Index {
		if _, held := out.IndexOn(at.Stream); !held && !at.IsZero() {
			out.Index = append(out.Index, at)
		}
	}
	for _, at := range o.Waited {
		if at.IsZero() {
			continue
		}
		i := slices.IndexFunc(out.Waited, func(p Position) bool { return p.Stream == at.Stream })
		switch {
		case i < 0:
			out.Waited = append(out.Waited, at)
		case at.Packed() > out.Waited[i].Packed():
			out.Waited[i] = at
		}
	}
	for _, stream := range o.Unconfirmed {
		if stream != "" && !slices.Contains(out.Unconfirmed, stream) {
			out.Unconfirmed = append(out.Unconfirmed, stream)
		}
	}
	return out
}

// IndexOn is the read index held on stream, and false where none is.
func (s Spent) IndexOn(stream string) (Position, bool) {
	for _, at := range s.Index {
		if at.Stream == stream {
			return at, true
		}
	}
	return Position{}, false
}

// WaitedFor reports whether a node already waited its whole bound for p, or
// for a position past it on p's log.
func (s Spent) WaitedFor(p Position) bool {
	for _, at := range s.Waited {
		if at.Stream == p.Stream && at.Packed() >= p.Packed() {
			return true
		}
	}
	return false
}

// UnconfirmedOn reports whether a node's barrier on stream ran its whole
// budget without committing.
func (s Spent) UnconfirmedOn(stream string) bool { return slices.Contains(s.Unconfirmed, stream) }

// spentKey carries what a read has paid for on the nodes asked before this one
// ([WithSpent]).
type spentKey struct{}

// WithSpent is ctx carrying what the read it serves has already paid for on
// the nodes asked for it before this one ([Spent]), for every read made on it
// ([Reader.Read]). A zero s comes back as ctx itself.
//
// A CONTEXT VALUE, as [WithReplyMargin]'s mark is, because it describes the
// REQUEST rather than one read: every read an operation makes on the request's
// behalf, through whichever domain's reader, is held to it without each
// operation's arguments carrying it.
func WithSpent(ctx context.Context, s Spent) context.Context {
	if s.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, spentKey{}, s)
}

// spentOf is what ctx says its read has already paid for, or nothing.
func spentOf(ctx context.Context) Spent {
	s, _ := ctx.Value(spentKey{}).(Spent)
	return s
}
