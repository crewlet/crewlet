package coord

import "context"

// GrantRequests is the agents' requests for one grant each beside their level
// (internal/seataccess), kept from the ask until a person grants or declines
// it, or it lapses.
//
// Fleet-wide: an agent asks on its seat's node and a person answers on
// whichever node serves the dashboard, which builds the grant from the stored
// request, never from anything the agent wrote. The value is OPAQUE,
// internal/seataccess's vocabulary as [SeatAccess]'s is. Each request is its
// own key, written once by the asker and deleted once by whoever settles it,
// so no write can lose another and no lease is needed. No age ([PositionKey]):
// the engine sweeps a request past its own stamp (seataccess.RequestTTL).
type GrantRequests interface {
	// GrantRequest is one request, and whether there is one. RAISES on an
	// unreachable store.
	GrantRequest(ctx context.Context, id string) ([]byte, bool, error)

	// GrantRequests is every request, by id, for the sweep.
	GrantRequests(ctx context.Context) (map[string][]byte, error)

	// PutGrantRequest stores one request, replacing any prior one.
	PutGrantRequest(ctx context.Context, id string, value []byte) error

	// DeleteGrantRequest drops one request. Deleting one that is not there
	// is the outcome asked for.
	DeleteGrantRequest(ctx context.Context, id string) error
}

// GrantRequestClass is the key class a request is filed under in the
// positions register.
const GrantRequestClass = "grant_request"

// MaxGrantRequestBytes bounds a grant request's record as stored, a small
// value read at the term (ADR-0175): internal/seataccess refuses a request
// past it, and the tool that files one tells the agent to ask for less.
const MaxGrantRequestBytes = SmallValueBytes

// GrantRequestKey is one request's key.
func GrantRequestKey(id string) string { return DocumentKey(GrantRequestClass, id) }

// GrantRequestFilter selects every request.
func GrantRequestFilter() string { return DocumentFilter(GrantRequestClass) }

// GrantRequestID recovers a request's id from its key, false for a key of
// another class or one this grammar did not write.
func GrantRequestID(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != GrantRequestClass || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}
