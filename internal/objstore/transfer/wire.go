package transfer

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
)

// SubjectPrefix is where every data node answers for its chunks.
const SubjectPrefix = "crewlet.objects"

// Subject is one node's object subject. The id is escaped the way every
// subject token carrying an id is, because a node id may hold a byte a token
// cannot.
func Subject(nodeID string) string {
	return SubjectPrefix + "." + coord.DocumentKey(nodeID)
}

// opName is what a request asks for.
type opName string

const (
	// opPut stores the body under the request's hash.
	opPut opName = "put"
	// opGet answers the bytes under the request's hash.
	opGet opName = "get"
	// opHas answers, for each of the request's hashes, whether this node
	// holds it and whether its own map places it here.
	opHas opName = "has"
)

// MaxHas bounds one has request.
//
// ONE THOUSAND AND TWENTY-FOUR: a hash is sixty-four bytes of hex, so the
// request is about seventy kilobytes — far under what the broker carries —
// and the collector confirming a placement group's strays asks in batches of
// this rather than once per chunk.
const MaxHas = 1024

type request struct {
	Op     opName          `json:"op"`
	Hash   objstore.Hash   `json:"hash,omitempty"`
	Hashes []objstore.Hash `json:"hashes,omitempty"`

	// From is the asking node, named so a serving node's log says who it
	// answered.
	From string `json:"from,omitempty"`
}

// status is how a request went.
type status string

const (
	statusOK      status = "ok"
	statusMissing status = "missing"
	statusRefused status = "refused"
)

type reply struct {
	Node   string `json:"node"`
	Status status `json:"status"`
	Detail string `json:"detail,omitempty"`

	// Held and Placed answer a has request, index for index: whether this
	// node holds each chunk, and whether ITS OWN map places the chunk
	// here — the collector deletes a copy beyond its placement only when
	// every member placing the chunk says both.
	Held   []bool `json:"held,omitempty"`
	Placed []bool `json:"placed,omitempty"`

	// Epoch is the map epoch Placed was judged at, 0 when this node has
	// no map yet.
	Epoch uint64 `json:"epoch,omitempty"`
}

// errFrame is bytes that are not a frame.
var errFrame = errors.New("objstore/transfer: not a frame")

// frame writes a header and a body as one message.
func frame(header any, body []byte) ([]byte, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("objstore/transfer: encode a header: %w", err)
	}
	out := make([]byte, 4+len(h)+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(h)))
	copy(out[4:], h)
	copy(out[4+len(h):], body)
	return out, nil
}

// unframe reads a message's header into header and answers its body.
func unframe(raw []byte, header any) ([]byte, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("%w: %d bytes", errFrame, len(raw))
	}
	n := binary.BigEndian.Uint32(raw)
	if uint64(n) > uint64(len(raw)-4) {
		return nil, fmt.Errorf("%w: a %d-byte header in %d bytes", errFrame, n, len(raw))
	}
	if err := json.Unmarshal(raw[4:4+n], header); err != nil {
		return nil, fmt.Errorf("%w: %w", errFrame, err)
	}
	return raw[4+n:], nil
}
