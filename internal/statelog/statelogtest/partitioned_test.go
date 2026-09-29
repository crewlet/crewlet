package statelogtest_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE PARTITIONED CONTROL, expected to come back clean.
//
// The control domain divided in two, each widget filed in the partition its id
// names — what a divided domain looks like when its router, its partition
// function and its applier agree. A family that reported a problem here would
// be one nobody could pass, so it is what says the cases can.
func TestThePartitionedControlPasses(t *testing.T) {
	t.Parallel()
	statelogtest.RunPartitioned(t, func(*testing.T) statelogtest.PartitionedCandidate {
		return partitionedCandidate()
	})
}

// A DOMAIN THAT LIES ABOUT ITS PARTITIONS, OR AN APPLIER THAT DROPS THE RELEASE
// GATE, IS CAUGHT — which is what says the family can fail as well as pass.
//
// The two lies are the two directions a divided domain can break the floor
// theorem in without breaking any number: a write whose scope names another
// partition's object files a deferral the partition it names never probes
// (gate 1), and a write routed to a partition its record does not belong to is
// a record every holder of that log must drop (gate 2). The applier's is the
// release: a leaving node's in-flight write applied as though it never left.
func TestThePartitionedFamilyCatchesALyingDomain(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		bend  func(*statelogtest.PartitionedCandidate)
		names string
	}{
		"a write whose scope names another partition's object": {
			bend: func(c *statelogtest.PartitionedCandidate) {
				c.WriteObject = writeWidget(func(id string) []string {
					return []string{widgetPath(otherWidget(id))}
				})
			},
			names: "declares the path",
		},
		"writes routed to a partition their records do not belong to": {
			bend: func(c *statelogtest.PartitionedCandidate) {
				c.Object = func(p statelog.PartitionID, n int) string {
					return otherWidget(widgetID(p, n))
				}
			},
			names: "refused by the partition gates",
		},
		"a domain whose partition function places no scope path": {
			bend: func(c *statelogtest.PartitionedCandidate) {
				// THE LIE THAT DISARMS GATE 1, carrying the crossing it
				// then lets through: every path answered as the log's
				// own term, and every write naming a widget of the other
				// partition beside its own.
				c.Domain = unplacingControl{partitionedControl{}}
				c.WriteObject = writeWidget(func(id string) []string {
					return []string{widgetPath(otherWidget(id))}
				})
			},
			names: "places in no partition",
		},
		"a log term the partition function places": {
			bend: func(c *statelogtest.PartitionedCandidate) {
				c.LogTerms = append(c.LogTerms, widgetPath(widgetID(widgetSpace(0), 0)))
			},
			names: "a term naming its log, and its partition function places it",
		},
		"an applier that drops no record for its writer's release": {
			bend: func(c *statelogtest.PartitionedCandidate) {
				c.Applier = releaseBlindApplier{partitionedApplier{}}
			},
			names: "the release gate did not drop it",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := statelogtest.Partitioned(t, func(*testing.T) statelogtest.PartitionedCandidate {
				c := partitionedCandidate()
				tc.bend(&c)
				return c
			})
			if err == nil || !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the family did not name %q for %s: %v", tc.names, name, err)
			}
		})
	}
}

// partitionedCandidate is the control divided in two by the partition each
// widget's id names.
func partitionedCandidate() statelogtest.PartitionedCandidate {
	c := control()
	c.Domain = partitionedControl{}
	c.Layout = partitionedLayout()
	c.Log = statelog.LogID{Domain: "control", Partition: widgetSpace(0)}
	c.Applier = partitionedApplier{}
	c.Rows = rowsOver(partitionedControl{})
	return statelogtest.PartitionedCandidate{
		Candidate: c,
		Gates: func(db store.PartitionReader) statelog.Gates {
			return controlGates{db: db}
		},
		Object:      widgetID,
		WriteObject: writeWidget(nil),
		Release: func(ctx context.Context, pub *statelog.Publisher, _ store.PartitionReader,
			node, opID string) (statelog.Result, error) {
			return publishControlGate(ctx, pub, opID, node, true, func(stamp statelog.Stamp) controlGate {
				return controlGate{Envelope: controlGateEnvelope(stamp, opID,
					controlReleaseOp, node), Node: node}
			})
		},
		Readmit: func(ctx context.Context, pub *statelog.Publisher, _ store.PartitionReader,
			node, opID string) (statelog.Result, error) {
			return publishControlGate(ctx, pub, opID, node, false, func(stamp statelog.Stamp) controlGate {
				return controlGate{Envelope: controlGateEnvelope(stamp, opID, "readmit", node),
					Node: node, Readmit: true}
			})
		},
		Holds: func(ctx context.Context, db store.PartitionReader, id string) (bool, error) {
			var n int
			err := db.Read(ctx, func(tx *sql.Tx) error {
				return tx.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM control_widgets WHERE id = ?`, id).Scan(&n)
			})
			return n > 0, err
		},
		// THE NODE GATE'S TERM, which every gate record declares and which
		// lies on whichever log the gate is written to.
		LogTerms: []string{controlGateKind},
	}
}

// partitionedLayout is the layout the partitioned control runs on: its log in
// two partitions of one space.
func partitionedLayout() statelog.Layout {
	return statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 2, Domains: []string{"control"}},
	}}
}

// widgetSpace is the partition of the control's space at index.
func widgetSpace(index uint16) statelog.PartitionID {
	return statelog.PartitionID{Space: statelog.SpaceTracker, Index: index}
}

// widgetID is the n-th widget filed in p: `w<index>-<n>`, the partition in the
// id as a divided domain's ids carry their birth partition.
func widgetID(p statelog.PartitionID, n int) string {
	return fmt.Sprintf("w%d-%d", p.Index, n)
}

// otherWidget is id filed in the control's other partition.
func otherWidget(id string) string {
	p, _ := widgetPartition(id)
	return widgetID(widgetSpace(1-p.Index), 0) + "-" + id
}

// widgetPartition is the partition a widget's id names, or false for an id
// that names none — which no partition holds.
func widgetPartition(id string) (statelog.PartitionID, bool) {
	index, _, ok := strings.Cut(strings.TrimPrefix(id, "w"), "-")
	if !ok || !strings.HasPrefix(id, "w") {
		return statelog.PartitionID{}, false
	}
	n, err := strconv.ParseUint(index, 10, 16)
	if err != nil {
		return statelog.PartitionID{}, false
	}
	return widgetSpace(uint16(n)), true
}

// widgetPath is a widget's scope path.
func widgetPath(id string) string { return "widget/" + id }

// partitionedControl is the identity-claiming control, divided: a widget and
// its path lie in the partition the widget's id names, and the node gates —
// the framework's — in none. Its gate records arbitrate on the node's subject,
// as every divided domain's do, so a release and the readmission that follows
// contend with each other and with nothing else.
type partitionedControl struct{ controlDomain }

func (partitionedControl) StreamShape() statelog.StreamShape {
	s := controlDomain{}.StreamShape()
	s.ArbitratedKinds = []string{"widget", controlGateKind}
	return s
}

func (partitionedControl) PartitionOf(_ statelog.Layout, env statelog.Envelope) (statelog.PartitionID, bool) {
	if env.Kind != "widget" {
		return statelog.PartitionID{}, false
	}
	p, _ := widgetPartition(env.Subject.ID)
	return p, true
}

func (partitionedControl) ScopePartition(_ statelog.Layout, path string) (statelog.PartitionID, bool) {
	id, widget := strings.CutPrefix(path, "widget/")
	if !widget {
		return statelog.PartitionID{}, false
	}
	p, _ := widgetPartition(id)
	return p, true
}

// unplacingControl is the partitioned control with a partition function that
// places no scope path — every one answered as the log's own term, which is
// the answer gate 1 never refuses.
type unplacingControl struct{ partitionedControl }

func (unplacingControl) ScopePartition(statelog.Layout, string) (statelog.PartitionID, bool) {
	return statelog.PartitionID{}, false
}

// writeWidget is the partitioned control's write path: one widget, stamped,
// under its own path — and the paths also names, for a write path that lies
// about its scope.
func writeWidget(also func(id string) []string) func(context.Context, *statelog.Publisher,
	store.PartitionReader, string, string) (statelog.Result, error) {

	return func(ctx context.Context, pub *statelog.Publisher, _ store.PartitionReader,
		id, opID string) (statelog.Result, error) {

		subject := statelog.Subject{Kind: "widget", ID: id}
		paths := []string{widgetPath(id)}
		if also != nil {
			paths = append(paths, also(id)...)
		}
		scope := statelog.ScopeSet{Paths: paths}
		return pub.Publish(ctx, statelog.Request{
			Subject: subject, Scope: scope, OpID: opID,
			Pattern: statelog.PatternArbitrated,
			Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				payload, err := json.Marshal(statelog.Envelope{
					V: 1, Kind: "widget", Subject: subject, OpID: opID,
					Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
				})
				return statelog.Decision{Payload: payload}, err
			},
		})
	}
}

// controlGateEnvelope is the envelope of a gate record about node, under op,
// stamped as its decision was.
func controlGateEnvelope(stamp statelog.Stamp, opID, op, node string) statelog.Envelope {
	return statelog.Envelope{
		V: 1, Kind: controlGateKind, Op: op,
		Subject: statelog.Subject{Kind: controlGateKind, ID: node},
		OpID:    opID, Gen: stamp.Gen, Writer: stamp.Writer,
		Scope: statelog.ScopeSet{Paths: []string{controlGateKind}},
	}
}

// publishControlGate publishes one gate record about node — a node gate, and
// a release where release says so — decided from the stamp its round is
// handed, arbitrated on the node's own subject.
func publishControlGate(ctx context.Context, pub *statelog.Publisher, opID, node string,
	release bool, decide func(statelog.Stamp) controlGate) (statelog.Result, error) {

	return pub.Publish(ctx, statelog.Request{
		Subject: statelog.Subject{Kind: controlGateKind, ID: node},
		Scope:   statelog.ScopeSet{Paths: []string{controlGateKind}},
		OpID:    opID, Pattern: statelog.PatternArbitrated,
		NodeGate: true, Release: release,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			payload, err := json.Marshal(decide(stamp))
			return statelog.Decision{Payload: payload}, err
		},
	})
}

// partitionedApplier is the control's applier with its gate: a record whose
// writer an eviction or a release puts out at its position is dropped, under
// the gate's own reason, as every identity-claiming domain's applier drops it.
type partitionedApplier struct{ controlApplier }

func (partitionedApplier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (statelog.Reason, bool, error) {
	return gatedAt(ctx, tx, rec.Writer, rec.Position)
}

// releaseBlindApplier drops no record for its writer's release — the release
// gate gone, every other gate kept.
type releaseBlindApplier struct{ partitionedApplier }

func (a releaseBlindApplier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (statelog.Reason, bool, error) {
	reason, gated, err := a.partitionedApplier.Gated(ctx, tx, rec)
	if reason == statelog.ReasonReleased {
		return "", false, err
	}
	return reason, gated, err
}

// controlGates is the publisher's side of the control's gate, over one
// partition's rows.
type controlGates struct{ db store.PartitionReader }

func (g controlGates) GatedAt(ctx context.Context, _ statelog.Subject, writer, _ string,
	p statelog.Position) (statelog.Reason, bool, error) {

	var reason statelog.Reason
	var gated bool
	err := g.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		reason, gated, err = gatedAt(ctx, tx, writer, p)
		return err
	})
	return reason, gated, err
}

// gatedAt is the control's one window rule, which its applier and its gate
// reader both ask: a writer an eviction or its own release put out below p,
// and no readmission took back at or below it.
func gatedAt(ctx context.Context, tx *sql.Tx, writer string,
	p statelog.Position) (statelog.Reason, bool, error) {

	if writer == "" {
		return "", false, nil
	}
	var from int64
	var readmitted sql.NullInt64
	var kind string
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position, kind
		FROM control_evictions WHERE node_id = ?`, writer).Scan(&from, &readmitted, &kind)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	gate := statelog.EvictionKind(kind)
	if !gate.Valid() {
		return "", false, fmt.Errorf("control: an eviction row records the gate %q", kind)
	}
	at := p.Packed()
	if at > from && (!readmitted.Valid || at < readmitted.Int64) {
		return gate.Reason(), true, nil
	}
	return "", false, nil
}
