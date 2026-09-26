package transfer

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/queue"
)

var log = logging.Get("objstore")

// Chunks is a node's own chunk store.
type Chunks interface {
	Put(h objstore.Hash, data []byte) error
	Get(h objstore.Hash) ([]byte, error)
	Has(h objstore.Hash) bool
}

// Maps answers the placement map this node currently places by, and false
// when it has none yet.
type Maps func() (placement.Map, bool)

// Server is what registering an answerer needs from the queue.
type Server interface {
	Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// Serve makes this node answer for its own chunks.
//
// EVERY DATA NODE SERVES, from boot, whether or not the map names it yet: a
// node that is about to be added is where writers will be sending within one
// cache refresh, and a copy a displaced write left here is one a reader may
// come looking for.
func Serve(ctx context.Context, q Server, nodeID string, chunks Chunks, current Maps) (queue.Unsubscribe, error) {
	if q == nil || chunks == nil || current == nil {
		return nil, errors.New("objstore/transfer: serve needs a queue, a chunk store and a map")
	}
	if nodeID == "" {
		return nil, errors.New("objstore/transfer: serve needs this node's id — it is the subject a writer asks")
	}
	return q.Serve(ctx, Subject(nodeID), func(ctx context.Context, raw []byte) ([]byte, error) {
		return answer(ctx, nodeID, chunks, current, raw), nil
	})
}

// answer runs one request and ALWAYS answers: silence reads as a node that is
// down, and the asker would wait out its whole attempt before the next member.
func answer(ctx context.Context, nodeID string, chunks Chunks, current Maps, raw []byte) []byte {
	out := reply{Node: nodeID, Status: statusOK}
	var req request
	body, err := unframe(raw, &req)
	if err != nil {
		return encodeReply(refuse(out, fmt.Sprintf("%s could not read the request: %v", nodeID, err)), nil)
	}
	switch req.Op {
	case opPut:
		if len(body) > objstore.ChunkSize {
			return encodeReply(refuse(out, fmt.Sprintf("a %d-byte chunk is over the %d-byte chunk size",
				len(body), objstore.ChunkSize)), nil)
		}
		if err := chunks.Put(req.Hash, body); err != nil {
			log.WarnContext(ctx, "object_put_refused", "chunk", string(req.Hash), "from", req.From, "error", err)
			return encodeReply(refuse(out, err.Error()), nil)
		}
		return encodeReply(out, nil)
	case opGet:
		data, err := chunks.Get(req.Hash)
		if err != nil {
			out.Status, out.Detail = statusMissing, err.Error()
			return encodeReply(out, nil)
		}
		return encodeReply(out, data)
	case opHas:
		if len(req.Hashes) > MaxHas {
			return encodeReply(refuse(out, fmt.Sprintf("%d hashes in one request, over %d",
				len(req.Hashes), MaxHas)), nil)
		}
		m, placed := current()
		if placed {
			out.Epoch = m.Epoch
		}
		out.Held = make([]bool, len(req.Hashes))
		out.Placed = make([]bool, len(req.Hashes))
		for i, h := range req.Hashes {
			out.Held[i] = chunks.Has(h)
			out.Placed[i] = placed && h.Valid() && slices.Contains(m.Up(h.PG()), nodeID)
		}
		return encodeReply(out, nil)
	}
	// A NEWER PEER'S OPERATION. Refused by name, so the asker's error
	// says which build to look at rather than that this node is down.
	return encodeReply(refuse(out, fmt.Sprintf("%s does not know the operation %q", nodeID, req.Op)), nil)
}

func refuse(out reply, detail string) reply {
	out.Status, out.Detail = statusRefused, detail
	return out
}

// encodeReply frames a reply. A header that cannot be encoded is a bug in
// this file, so the fallback is a refusal that says so rather than silence.
func encodeReply(out reply, body []byte) []byte {
	raw, err := frame(out, body)
	if err != nil {
		raw, _ = frame(reply{Node: out.Node, Status: statusRefused, Detail: err.Error()}, nil)
	}
	return raw
}
