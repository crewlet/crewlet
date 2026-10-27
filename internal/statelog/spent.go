package statelog

import (
	"context"
	"slices"
)

// Spent is what ONE READ has already paid for on the data nodes asked for it
// before this one, carried to the next.
//
// # Why a read carries it
//
// The estate's router asks the next data node a read one refused for a reason
// about itself ([ReadRefusal.Elsewhere]), and a node asked afresh pays for the
// whole read again: a barrier appended to the log, then up to a whole
// [ReadBudget] waited for its position. On a fleet that one burst put behind
// together, every node refuses `behind` — so the one final refusal cost a
// barrier per node, each applied by every node's applier on the very log they
// are all behind on, and a read budget per node: six seconds at three data
// nodes and ten at five, where one node's refusal had cost two. Carried, the
// walk pays for each ONCE, however many nodes it asks:
//
//   - Index is the READ INDEX a node established for the read on a log: the
//     position of a barrier it appended, which the broker committed. A
//     linearizable read handed one on its own log WAITS FOR THE RECORDS
//     BEFORE IT AND APPENDS NOTHING — every node derives the same position to
//     wait for from it ([waitsThrough]). That is equally linearizable: the
//     read index takes only a barrier that started after its reader arrived
//     ([ReadIndex.Read]), and that reader is this same request, reaching a
//     node after it began — so every write acknowledged before the request
//     began is below the barrier, whichever node's applier the request is
//     then served from.
//   - Waited is the furthest position on a log a node already waited its
//     whole bound for and did not reach. A node waits for no position at or
//     below it — it serves the read if it already holds it, and refuses
//     `behind` at once if it does not — because the walk's wait for a
//     position is one bound wherever it is spent: the position is by then at
//     least that bound old, and a copy that answers requests holds a
//     position that old unless it lags by more than a whole read budget,
//     which is the copy the first wait has already found.
//   - NoQuorum is every log a node spent a barrier's whole budget on without
//     the barrier committing — a log without a quorum rather than a slow one
//     ([DefaultBarrierBudget]) — and a read on it appends NO SECOND barrier:
//     it is refused `no_quorum` at once. Every node's barrier goes to the one
//     log's leader, so through an election every node's waits out its whole
//     budget alike; the one case a second barrier commits where the first did
//     not — a member cut off from the log's leader while its peers and its
//     asker still reach it — is answered with the refusal and its retry hint
//     ([ElectionRetryHint]) rather than every read of every election paying
//     a barrier's budget per data node.
//
// Every term is keyed by its LOG, because a position is a coordinate on one
// log ([Position.Stream]) and what was paid for on one says nothing about
// another. The zero value has paid for nothing, which is a read asked of its
// first node.
type Spent struct {
	// Index is the read index established on each log, at most one a log:
	// the FIRST one a walk established, which every node it asks after is
	// held to.
	Index []Position `json:"index,omitempty"`

	// Waited is, on each log, the furthest position a node waited its whole
	// bound for and did not reach.
	Waited []Position `json:"waited,omitempty"`

	// NoQuorum is every log on which a node's barrier ran its whole budget
	// without committing.
	NoQuorum []string `json:"no_quorum,omitempty"`
}

// IsZero reports whether s has paid for nothing.
func (s Spent) IsZero() bool {
	return len(s.Index) == 0 && len(s.Waited) == 0 && len(s.NoQuorum) == 0
}

// Merge is what a walk has paid for once a node has also paid for o: the read
// index it already held kept on each log (a later node never establishes
// another while one is held, and every node asked so far was held to the
// first), the further of two waited positions on each, and every log a
// barrier ran out on.
func (s Spent) Merge(o Spent) Spent {
	out := Spent{
		Index:    slices.Clone(s.Index),
		Waited:   slices.Clone(s.Waited),
		NoQuorum: slices.Clone(s.NoQuorum),
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
	for _, stream := range o.NoQuorum {
		if stream != "" && !slices.Contains(out.NoQuorum, stream) {
			out.NoQuorum = append(out.NoQuorum, stream)
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

// NoQuorumOn reports whether a node's barrier on stream ran its whole budget
// without committing.
func (s Spent) NoQuorumOn(stream string) bool { return slices.Contains(s.NoQuorum, stream) }

// spentKey carries what a read has paid for on the nodes asked before this one
// ([WithSpent]).
type spentKey struct{}

// WithSpent is ctx carrying what the read it serves has already paid for on
// the nodes asked for it before this one ([Spent]), for every read made on it
// ([Reader.Read]). A zero s comes back as ctx itself: a read asked of its
// first node has paid for nothing.
//
// A CONTEXT VALUE, as [WithReplyMargin]'s mark is, because both describe the
// REQUEST a read is made for rather than the read: the serving node's estate
// marks the request it answers, and every read the operation makes on that
// request's behalf — however many, through whichever domain's reader — is
// held to it without each operation's arguments carrying it.
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
