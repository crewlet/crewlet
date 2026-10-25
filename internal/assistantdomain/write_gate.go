package assistantdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ---- node gates --------------------------------------------------------- //

// GateWriter publishes this log's node gates: a node's eviction and its
// readmission. ITS OWN TYPE beside [Writer], because a gate names a machine and
// carries nothing a person said, so it is built with no keyring: the node gate
// writes every identity-claiming log at once, and none of its writers should
// need more than the log.
type GateWriter struct {
	publisher *statelog.Publisher
	now       func() time.Time
}

// NewGateWriter builds the node gate's writer over the domain's publisher.
func NewGateWriter(publisher *statelog.Publisher) (*GateWriter, error) {
	if publisher == nil {
		return nil, errors.New("assistantdomain: a gate writer needs the log's write authority")
	}
	return &GateWriter{publisher: publisher, now: func() time.Time { return time.Now().UTC() }}, nil
}

// EvictNode installs the gate that drops a node's records on this log, AS the
// actor who ran the gesture, under the operation id the engine's node gate
// derived. UNJUDGED HERE: whether the node may be evicted is asked once, by the
// node gate, before the first log is written.
func (w *GateWriter) EvictNode(ctx context.Context, by iam.Actor, opID, node string) (statelog.Result, error) {
	return w.gateNode(ctx, by, opID, node, false)
}

// ReadmitNode is the eviction's INVERSE COMMIT rather than a delete.
func (w *GateWriter) ReadmitNode(ctx context.Context, by iam.Actor, opID, node string) (statelog.Result, error) {
	return w.gateNode(ctx, by, opID, node, true)
}

// gateNode publishes one eviction record on the node's own subject. A retry is
// answered by the framework from this node's ledger, judged by whether the
// record its operation landed is still in force ([statelog.GateStanding]).
func (w *GateWriter) gateNode(ctx context.Context, by iam.Actor, opID, node string,
	readmit bool) (statelog.Result, error) {

	switch {
	case node == "":
		return statelog.Result{}, invalid("an eviction names no node")
	case opID == "":
		return statelog.Result{}, invalid("the eviction of %s has no operation id: a "+
			"retry under a fresh one would write the gate twice", node)
	case strings.TrimSpace(by.Name) == "" || !by.Kind.Valid():
		return statelog.Result{}, invalid("an eviction names who ran it")
	}
	subject, scope := EvictionSubject(node), ScopeSet{Subject: true}
	at := w.now()
	return publish(ctx, w.publisher, statelog.Request{
		Subject: subject.framework(), Scope: scope.Resolve(subject), OpID: opID,
		Pattern: statelog.PatternArbitrated, NodeGate: true,
		Standing: func(tx *sql.Tx, held statelog.Position) error {
			row, found, err := standingIn(ctx, tx, node)
			if err != nil {
				return err
			}
			return statelog.GateStanding(opID, node, readmit, held, row, found)
		},
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return decision(stamp, by, subject, OpEviction, scope, opID, Eviction{
				V: GateRecordVersion, NodeID: node, EvictedBy: by.Name, EvictedAt: at,
				Readmitted: readmit,
			}, at)
		},
	})
}

// standingIn is node's eviction row on this log, read in the transaction the
// caller holds.
func standingIn(ctx context.Context, tx *sql.Tx, node string) (statelog.EvictionRow, bool, error) {
	var from int64
	var readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM assistant_evictions
		WHERE node_id = ?`, node).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return statelog.EvictionRow{}, false, nil
	case err != nil:
		return statelog.EvictionRow{}, false, fmt.Errorf("assistantdomain: read %s's "+
			"eviction row: %w", node, err)
	}
	return statelog.EvictionRow{
		NodeID: node, From: uint64(from), Readmitted: uint64(readmitted.Int64),
		Back: readmitted.Valid && readmitted.Int64 > from,
	}, true, nil
}
