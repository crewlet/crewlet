package estate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The `statelog.gate` operation: an eviction or a readmission of a node,
// published on ONE LOG by a node that serves the log's partition, on behalf of
// the node the operator ran the gesture on (contract §F6, §G6).
//
// # Why a gesture needs another node to write some of its logs
//
// A node gate is judged once and written on every log the node it names is
// counted on, and only a node that SERVES a log's partition writes that log —
// the write authority's gate 3 refuses anybody else. Under layout 0 every data
// node serves the one partition, so the node an operator asks writes every log
// itself. Under a divided layout it serves some partitions and not others, and
// each log it does not serve is written by one that does: the gesture's record
// for that log is this operation, sent to a serving holder of its partition.
//
// # Routed like every other operation, to the log's own partition
//
// The operation's PARTITION FUNCTION is the one partition a gate record goes
// to, the log's own ([GatePartitions]), so the router sends it exactly where it
// sends every other operation on that partition: this node first where it
// serves it, then the partition's serving holders in order ([Router.Gate]).
// The holder that takes it publishes the record through its own write
// authority on that log ([Backend.Gates]) — the judgement was made once, on
// the node the operator asked, and is not made again — and the router reports
// WHICH node wrote it, because what a refusal says (evicted, released, behind,
// unvouched) is about the node whose authority published, and an operator told
// "this node" of a refusal another node gave is sent to the wrong machine.
//
// That is also what lets a READMISSION put the node back in the estate map
// from any node: the in is made only by a gesture that finds every log has
// taken the node back, and only one gesture that reaches every log can find
// that — on a fleet where no node serves every partition, nothing else could
// lift the bar, since the operator's own in refuses a barred node
// (membership.ErrBarredMember) because an in sees no log.
//
// Its CLASS is an idempotent write (§G6): a gate record carries the operation
// id the gesture derived for that log and node, and the log's own ledger
// answers a repeat with the record the first copy landed, so an unanswered
// request moves on to the partition's next serving holder under the same id —
// and so does one a holder answered unvouched, whose ledger cannot say whether
// it landed while another holder's may. It is ONE APPEND ([AppendAttempt]), so
// a silent holder is passed after an attempt rather than after the gesture's
// whole budget, and it carries no session floor: the write authority decides
// from its own snapshot and the broker arbitrates, so nothing the asking node
// wrote has to be visible first. And every request NAMES its partition, since
// no build from before partitions sends it.
//
// # And the readmission's judgement reads each log where it is written
//
// A readmission is JUDGED ONCE, before any log is written, against every log
// the node would be counted on — refused if the node is below a trim floor it
// would be counted against ([statelog.PermitReadmission]) — and each log's
// bound is what the write fence of the node that writes it reads: the floor
// published at the generation that node runs the log at, and the log's first
// surviving sequence. A log the asking node does not write is written by a
// holder of its partition, so its bound is read there too, by the companion
// operation `statelog.readmission_bound` ([Router.ReadmissionBound]), routed to
// the same partition by the same function. Judged only on the logs the asking
// node runs, a readmission was refused on none of the others and then written
// on all of them through their holders — putting back, unannounced, the very
// pin the eviction was run to lift.

// OpStatelogGate is the operation's name on the wire.
const OpStatelogGate = "statelog.gate"

// OpReadmissionBound is the name on the wire of the read a readmission's
// judgement makes of one log it does not write itself.
const OpReadmissionBound = "statelog.readmission_bound"

// LogRef names one log of a layout: the log a gate record is published on, or
// whose readmission bound is read.
//
// THE LAYOUT, THE DOMAIN AND THE PARTITION, never the log's key alone, which
// does not carry the layout (`tracker@tracker.007` is the same key in layouts 1
// and 2).
type LogRef struct {
	Layout    int    `json:"layout"`
	Domain    string `json:"domain"`
	Partition string `json:"partition"`
}

// GateArgs is one log's gate record, as the node an operator asked sends it to
// a serving holder of the log's partition.
type GateArgs struct {
	// LogRef is the log the record is published on.
	LogRef

	// Node is the node the gesture evicts or readmits, By the operator who
	// ran it, and OpID the operation the record is published under — the
	// gesture's own id derived for this log and node, so a retry anywhere
	// is the same operation on this log.
	Node string `json:"node"`
	By   string `json:"by"`
	OpID string `json:"op_id"`

	// Kind is which gate the record is — REQUIRED, and refused by the
	// serving node when it is not one this build knows ([GateKind.Valid]).
	Kind GateKind `json:"kind"`
}

// GateKind is which node gate a `statelog.gate` record is: an eviction, or the
// readmission that undoes one.
//
// A NAMED KIND, NEVER A FLAG, because of what the zero value of a flag would
// mean here. Carried as `readmit bool`, every request that was not a
// readmission — an empty field, a sender that forgot it, and above all a kind
// a later build adds, which this build would decode to false — was published
// as an EVICTION: the most destructive record a node gate writes, dropping
// every record the named node publishes on that log, on every applier. A kind
// this build does not know is refused instead ([ErrGateKind]), with nothing
// written, so a rolling upgrade's newer gesture reaches a node that can write
// it rather than one that writes the wrong one. A RELEASE never travels here:
// a leaving node publishes its own (contract §F8).
type GateKind string

const (
	// GateEvict is an operator's eviction: the node's records above it
	// apply nowhere on this log, and the trim stops counting it here.
	GateEvict GateKind = "evict"

	// GateReadmit is a readmission: the eviction before it lifted, and the
	// node counted on this log again.
	GateReadmit GateKind = "readmit"
)

// GateKinds is every kind this build writes, for validation and for a test
// that walks them.
var GateKinds = []GateKind{GateEvict, GateReadmit}

// Valid reports whether k is a kind this build writes.
func (k GateKind) Valid() bool { return slices.Contains(GateKinds, k) }

// ErrGateArgs reports a gate record that names no log of the running layout.
var ErrGateArgs = errors.New("estate: the gate record names no log of the running layout")

// ErrGateKind reports a gate record whose kind the serving node does not write
// — none at all, or one a later build added. Nothing was published.
var ErrGateKind = errors.New("estate: the gate record is of a kind this node does not write")

// GatePartitions is the partition a request about one log goes to — exactly
// one, the log's own — or why it names no log of layout: another layout's log,
// a partition the layout does not have, or a domain that has no log there. It
// is both node-gate operations' partition function, so a log's bound is read
// where its record is written.
func GatePartitions(l statelog.Layout, a LogRef) ([]statelog.PartitionID, error) {
	p, err := statelog.ParsePartitionID(a.Partition)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrGateArgs, err)
	case a.Layout != l.Number:
		return nil, fmt.Errorf("%w: it names a log of layout %d, and this node runs layout %d",
			ErrGateArgs, a.Layout, l.Number)
	}
	log := statelog.LogID{Domain: a.Domain, Partition: p}
	for _, have := range l.Logs(p) {
		if have == log {
			return []statelog.PartitionID{p}, nil
		}
	}
	return nil, fmt.Errorf("%w: layout %d has no log %s", ErrGateArgs, l.Number, log)
}

// GateWriter publishes one log's gate record through the serving node's own
// write authority on it: the `statelog.gate` operation's server half for one
// log ([Backend.Gates]).
type GateWriter func(ctx context.Context, a GateArgs) (statelog.Result, error)

// AppendAttempt bounds one attempt, on one holder, at an operation that is ONE
// APPEND on one log ([opSpec.oneAppend]) — `statelog.gate` — whatever the
// caller's own deadline.
//
// A SILENCE THRESHOLD, NOT A BOUND ON THE HOLDER'S WORK. What a holder
// ordinarily does for one append is the write authority's two waits, each
// bounded by [statelog.DefaultResolveBudget] (five seconds) — its applier
// reaching a peer's record the decision must see, and the resolution of its
// own append — plus the request's transit and the snapshot the decision reads:
// fifteen seconds. A write CAN take longer — a decision that loses its
// compare-and-set is taken again, up to the publisher's sixteen rounds, and a
// round may wait behind a peer again — but a holder still working past fifteen
// seconds is one racing a write storm on the node's own gate subject, and the
// next serving holder is asked under the SAME operation id. That is safe
// because of the class, not the clock: the record carries the id the gesture
// derived for the log, so an append the slow holder does land is collapsed
// onto by the broker inside the log's duplicate window and answered from the
// ledger after it, and applies once. Bounded by the caller's deadline alone,
// as a seat's longer writes are ([writeAttempt]), one silent holder took the
// whole gesture with it.
const AppendAttempt = 2*statelog.DefaultResolveBudget + 5*time.Second

// opStatelogGate is the operation — see the file's doc.
var opStatelogGate = define(OpStatelogGate, opIdempotentWrite,
	address[GateArgs]{partitions: func(_ context.Context, l statelog.Layout, _ Resolver,
		a GateArgs) ([]statelog.PartitionID, error) {
		return GatePartitions(l, a.LogRef)
	}}, false,
	func(ctx context.Context, b Backend, _ *Actor, a GateArgs) (statelog.Result, error) {
		// THE KIND FIRST, before any backend sees the record: a kind this
		// node does not write is refused whatever it runs, and never read
		// as the eviction a missing flag would have been.
		if !a.Kind.Valid() {
			return statelog.Result{}, fmt.Errorf("%w: %q (this node writes %v) — the "+
				"record for %s@%s on %s", ErrGateKind, a.Kind, GateKinds, a.Domain,
				a.Partition, a.Node)
		}
		if b.Gates == nil {
			return statelog.Result{}, errNoHalf
		}
		write := b.Gates(a.Domain)
		if write == nil {
			return statelog.Result{}, errNoHalf
		}
		return write(ctx, a)
	}).floorless().appends().named()

// Gate publishes one log's gate record on a node that serves the log's
// partition — this one, where it serves it and runs the log — and answers the
// write's own three-valued outcome and the node whose write authority gave the
// answer: this node's own id where it did, and empty where no holder answered
// at all ([ErrPartitionUnserved], or a request that could not be made).
//
// The caller has judged the gesture already; the holder publishes what it is
// given, under the operation id in a, and a repeat of the id anywhere is the
// same operation on that log.
func (r *Router) Gate(ctx context.Context, a GateArgs) (statelog.Result, string, error) {
	var writer string
	res, err := callFrom(ctx, r, opStatelogGate, nil, a, func(node string) { writer = node })
	return res, writer, err
}

// BoundReader reads one log's readmission bound on the serving node's own copy:
// the `statelog.readmission_bound` operation's server half for one log
// ([Backend.ReadmissionBounds]).
type BoundReader func(ctx context.Context) (statelog.ReadmissionBound, error)

// opReadmissionBound is the read a readmission's judgement makes of one log it
// does not write itself — see the file's doc.
//
// A READ OF THE LOG'S STANDING, never of its rows at a position, so it carries
// no session floor; and like the record it is judged for, every request names
// its partition. A holder that runs no such log right now answers "no native
// backend here", and the request moves on.
var opReadmissionBound = define(OpReadmissionBound, opRead,
	address[LogRef]{partitions: func(_ context.Context, l statelog.Layout, _ Resolver,
		a LogRef) ([]statelog.PartitionID, error) {
		return GatePartitions(l, a)
	}}, false,
	func(ctx context.Context, b Backend, _ *Actor, a LogRef) (statelog.ReadmissionBound, error) {
		if b.ReadmissionBounds == nil {
			return statelog.ReadmissionBound{}, errNoHalf
		}
		read := b.ReadmissionBounds(a.Domain)
		if read == nil {
			return statelog.ReadmissionBound{}, errNoHalf
		}
		return read(ctx)
	}).floorless().named()

// ReadmissionBound reads one log's readmission bound on a node that serves the
// log's partition — this one, where it serves it and runs the log: the bound
// the write fence of that node reads, which is the one a readmission written
// there is held to ([statelog.ReadmissionBound]).
func (r *Router) ReadmissionBound(ctx context.Context, l LogRef) (statelog.ReadmissionBound, error) {
	return call(ctx, r, opReadmissionBound, nil, l)
}
