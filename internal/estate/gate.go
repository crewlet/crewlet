package estate

import (
	"errors"
	"fmt"

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
// # What is declared here, and what is not yet
//
// The operation's NAME, its ARGUMENTS and its PARTITION FUNCTION — the one
// partition a gate record goes to, which is the log's own. The router that
// sends an operation to a serving holder of the partition its function names is
// the estate's next shape (contract §G1, step 6), and the operation is
// registered on it there, with the node gate sending each log it does not serve
// through it. Until then the gate reports such a log as not written here, with
// the remedy of running the gesture through a node that serves the partition
// under the same operation id — which this operation automates. Registering it
// is also what lets a READMISSION put the node back in the estate map from any
// node: the in is made only by a gesture that finds every log has taken the
// node back, so until one gesture reaches every log it is made only on a node
// that serves every partition (engine.MapAwaitsLogs) — and on a fleet where NO
// node serves every partition it is not made at all. Nothing else lifts the
// bar: the operator's own in refuses a barred node
// (membership.ErrBarredMember), because an in sees no log. So until this
// operation is registered, such a fleet keeps a readmitted node barred from
// the map, and registering it is what lets that readmission land.
//
// Its CLASS is an idempotent write (§G6): a gate record carries the operation
// id the gesture derived for that log and node, and the log's own ledger
// answers a repeat with the record the first copy landed, so an unanswered
// request moves on to the partition's next serving holder under the same id.

// OpStatelogGate is the operation's name on the wire.
const OpStatelogGate = "statelog.gate"

// GateArgs is one log's gate record, as the node an operator asked sends it to
// a serving holder of the log's partition.
type GateArgs struct {
	// Layout is the number of the layout the log is of, and Domain and
	// Partition name the log — a log of the partition, never the key alone,
	// which does not carry the layout.
	Layout    int    `json:"layout"`
	Domain    string `json:"domain"`
	Partition string `json:"partition"`

	// Node is the node the gesture evicts or readmits, By the operator who
	// ran it, and OpID the operation the record is published under — the
	// gesture's own id derived for this log and node, so a retry anywhere
	// is the same operation on this log.
	Node string `json:"node"`
	By   string `json:"by"`
	OpID string `json:"op_id"`

	// Readmit is a readmission; false is an eviction.
	Readmit bool `json:"readmit,omitempty"`
}

// ErrGateArgs reports a gate record that names no log of the running layout.
var ErrGateArgs = errors.New("estate: the gate record names no log of the running layout")

// GatePartitions is the partition a gate record goes to — exactly one, the
// log's own — or why the record names none of layout: another layout's log,
// a partition the layout does not have, or a domain that has no log there.
func GatePartitions(l statelog.Layout, a GateArgs) ([]statelog.PartitionID, error) {
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
