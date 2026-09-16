package search

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE FAN-OUT'S WIRE, and why it is its own encoding rather than an event.
//
// A scattered slice request is not an event: nothing subscribes to it, nothing
// replays it, no node needs to know it happened, and it must leave no trace at
// all — see [queue.EventQueue.Ask]. So it carries its own small JSON payload
// on the ephemeral verbs rather than an [events.Event] on the durable ones.
//
// EVOLUTION IS ADDITIVE, on the event envelope's terms and for the same
// reason: a rolling upgrade puts two builds on one broker, and a peer that
// cannot decode a request answers nothing — which the coordinator reports as a
// missing assignment rather than as a failure, so an upgrade degrades a
// ranking instead of breaking a search.

// slicePayload is one scattered request.
type slicePayload struct {
	// Version is what the ASKER is speaking. A peer that does not know it
	// answers nothing, which is exactly a missing assignment.
	Version int `json:"version"`

	Text       string   `json:"text"`
	Containers []string `json:"containers,omitempty"`
	Sources    []string `json:"sources,omitempty"`
	Vector     []byte   `json:"vector,omitempty"`
	Model      string   `json:"model,omitempty"`
	Dim        int      `json:"dim,omitempty"`

	// Table is the whole assignment, so a node reads its OWN row and
	// answers only for that. Sent whole rather than per node because a
	// scatter reaches every node on one subject, and a node not named here
	// stays silent.
	Table []Assigned `json:"table"`
}

// sliceReply is one participant's answer on the wire.
type sliceReply struct {
	Version int   `json:"version"`
	Slice   Slice `json:"slice"`
}

// sliceProtocol is the fan-out's payload version.
//
// ONE, and it moves only for a reshape. A new FIELD needs no bump: an older
// peer ignores what it does not know and answers over the buckets it was
// given, which is a slightly worse ranking rather than a missing slice.
const sliceProtocol = 1

// SliceSubject is where a slice request is scattered.
//
// ONE SUBJECT FOR THE WHOLE FLEET, not one per node: every node serves it,
// every node receives every request, and the assignment table inside decides
// who answers. A subject per node would make the coordinator's roster the
// thing that has to be right rather than the thing it merely proposes.
const SliceSubject = topics.SearchSlice

// Broker is [Peers] over the queue's ephemeral scatter.
type Broker struct{ Queue queue.EventQueue }

// Scatter asks the fleet for its assignments and returns what arrived.
func (b Broker) Scatter(ctx context.Context, q FanQuery, table []Assigned) ([]Slice, error) {
	if b.Queue == nil || len(table) == 0 {
		return nil, nil
	}
	request, err := json.Marshal(slicePayload{
		Version: sliceProtocol,
		Text:    q.Text, Containers: q.Containers, Sources: q.Sources,
		Vector: q.Vector, Model: q.Model, Dim: q.Dim,
		Table: table,
	})
	if err != nil {
		return nil, fmt.Errorf("search: encode a slice request: %w", err)
	}
	replies, err := b.Queue.Ask(ctx, SliceSubject, request, len(table))
	if err != nil {
		// NOT A SEARCH FAILURE. The coordinator holds the whole corpus
		// and has already scanned its own assignment; a broker that
		// could not be asked costs the answer the peers' buckets, which
		// is what the partial report is for.
		return nil, err
	}
	out := make([]Slice, 0, len(replies))
	for _, raw := range replies {
		var reply sliceReply
		if err := json.Unmarshal(raw, &reply); err != nil || reply.Version != sliceProtocol {
			// AN UNREADABLE REPLY IS A MISSING ONE. It came from a
			// build this one does not speak, and guessing at its
			// fields would put a ranking nobody can reproduce into
			// an answer.
			continue
		}
		out = append(out, reply.Slice)
	}
	return out, nil
}

// ServeSlices makes this node an answerer for the fleet's slice requests.
//
// It answers ONLY for its own row in the table. A node that is up but not
// named — one that joined after the coordinator read its roster — stays
// silent rather than volunteering a range somebody else also holds, because
// two answers over one range would count those buckets twice and merge one
// document's score against itself.
func ServeSlices(ctx context.Context, q queue.EventQueue, self string, scan Scanner) (queue.Unsubscribe, error) {
	if q == nil || scan == nil {
		return nil, fmt.Errorf("search: serve slices with no queue or no scanner")
	}
	return q.Serve(ctx, SliceSubject, func(ctx context.Context, raw []byte) ([]byte, error) {
		var req slicePayload
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("search: decode a slice request: %w", err)
		}
		if req.Version != sliceProtocol {
			return nil, fmt.Errorf("search: slice request at version %d, this "+
				"build speaks %d", req.Version, sliceProtocol)
		}
		mine, named := Assignment{}, false
		for _, a := range req.Table {
			if a.Node == self {
				mine, named = a.Shards, true
				break
			}
		}
		if !named {
			return nil, errNotMine
		}
		slice, err := scan.Scan(ctx, FanQuery{
			Text: req.Text, Containers: req.Containers, Sources: req.Sources,
			Vector: req.Vector, Model: req.Model, Dim: req.Dim,
		}, mine)
		if err != nil {
			return nil, err
		}
		slice.Node, slice.Shards = self, mine
		return json.Marshal(sliceReply{Version: sliceProtocol, Slice: slice})
	})
}

// errNotMine is how a node declines a request that named somebody else.
//
// AN ERROR RATHER THAN AN EMPTY SLICE, because those two are different facts
// and the coordinator counts them differently: an empty slice is a range that
// was scanned and matched nothing, while this is a range this node was never
// asked about.
var errNotMine = fmt.Errorf("search: this node is not in the assignment table")
