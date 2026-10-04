package queries

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/objstore"
)

// The fleet view's object-store block: where the company's files are kept,
// and what the collector last found (ADR-0026).
//
// # One typed value, rendered in one place
//
// The block has two readers — the dashboard's file-storage card and `crewlet
// objects status` — so the shape is [FleetObjects] and the rendering is
// [RenderObjects]: the fleet question answers through it, the command line's
// tests serve what it returns, and the dashboard's suite reads the golden
// internal/api writes from it (`internal/api/testdata/objects_answer.json`).
//
// # Read from the fleet's record, never this node
//
// The collector is one duty that moves between data nodes, and it records each
// pass in the coordination store. Every node answers from that record, so the
// card says the same thing whichever node a request reached — and names the
// node that ran the pass.

// ObjectsState is which of three things the fleet's record was when the view
// read it.
type ObjectsState string

const (
	// ObjectsUnavailable is a coordination store that would not give the
	// record up. The files are where they were; this answer cannot say
	// what the collector last found.
	ObjectsUnavailable ObjectsState = "unavailable"

	// ObjectsNotYet is a fleet whose collector has not finished a pass —
	// a new fleet, or one with no data node running the duty.
	ObjectsNotYet ObjectsState = "not_yet"

	// ObjectsReported is a record read and rendered: every
	// [ReportedObjects] field is present.
	ObjectsReported ObjectsState = "reported"
)

// ObjectsStates is every state this build renders, in the order the
// dashboard's copy lists them.
func ObjectsStates() []ObjectsState {
	return []ObjectsState{ObjectsUnavailable, ObjectsNotYet, ObjectsReported}
}

// Valid reports whether s is a state this build renders.
func (s ObjectsState) Valid() bool { return slices.Contains(ObjectsStates(), s) }

// FleetObjects is the fleet view's object-store block.
type FleetObjects struct {
	State ObjectsState `json:"state"`
	*ReportedObjects
}

// ReportedObjects is what the collector's last record says.
type ReportedObjects struct {
	// Backend names the store the files are in, as the fleet recorded it:
	// `nats`, or `s3:<endpoint>/<bucket>/<prefix>`.
	Backend string `json:"backend"`

	// Node is the data node that ran the passes below.
	Node string `json:"node"`

	// Collect is the last collection, absent before one has ended; Audit
	// the last audit, absent before one has ended.
	Collect *ObjectCollect `json:"collect,omitempty"`
	Audit   *ObjectAudit   `json:"audit,omitempty"`
}

// ObjectCollect is one collection pass, rendered.
type ObjectCollect struct {
	At         time.Time `json:"at"`
	Completed  bool      `json:"completed"`
	Listed     int       `json:"listed"`
	Aged       int       `json:"aged"`
	Deleted    int       `json:"deleted"`
	Referenced int       `json:"referenced"`
	Refreshed  int       `json:"refreshed"`
	Skipped    string    `json:"skipped,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// ObjectAudit is one audit, rendered.
type ObjectAudit struct {
	At            time.Time       `json:"at"`
	Completed     bool            `json:"completed"`
	Referenced    int             `json:"referenced"`
	Missing       int             `json:"missing"`
	MissingChunks []objstore.Hash `json:"missing_chunks,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// RenderObjects is the one rendering of the collector's record.
func RenderObjects(r engine.CollectionReport) FleetObjects {
	out := &ReportedObjects{Backend: r.Backend, Node: r.Node}
	if c := r.Status.Collect; !c.At.IsZero() {
		out.Collect = &ObjectCollect{
			At: c.At, Completed: c.Completed, Listed: c.Listed, Aged: c.Aged,
			Deleted: c.Deleted, Referenced: c.Referenced, Refreshed: c.Refreshed,
			Skipped: c.Skipped, Error: c.Error,
		}
	}
	if a := r.Status.Audit; !a.At.IsZero() {
		out.Audit = &ObjectAudit{
			At: a.At, Completed: a.Completed, Referenced: a.Referenced,
			Missing: a.Missing, MissingChunks: a.MissingChunks, Error: a.Error,
		}
	}
	return FleetObjects{State: ObjectsReported, ReportedObjects: out}
}

// ObjectsReader is the fleet's record of the object store, read as every node
// reads it.
type ObjectsReader = engine.CollectionRecords

// objects is the fleet's object store as the fleet view shows it.
//
// THREE STATES NAMED APART, never folded into an empty report: a record the
// store would not give up, a fleet whose collector has not reported yet, and
// a report — because each sends an operator somewhere different.
func (s Sources) objects(ctx context.Context) FleetObjects {
	r, found, err := engine.ObjectCollection(ctx, s.Objects)
	switch {
	case err != nil:
		log.WarnContext(ctx, "fleet_objects_unavailable", "error", err)
		return FleetObjects{State: ObjectsUnavailable}
	case !found:
		return FleetObjects{State: ObjectsNotYet}
	}
	return RenderObjects(r)
}
