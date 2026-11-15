package coord

import "context"

// GrantRequests is the agents' requests for one grant each beside their level
// (internal/seataccess): what an agent asked, kept from the moment it asks
// until a person grants or declines it, or it lapses.
//
// # Who has to agree on it
//
// The whole company, now. An agent asks on the node holding its seat, and a
// person answers on whichever node serves the dashboard, which builds the
// grant from the stored request rather than from anything the agent wrote.
//
// # The value is OPAQUE, as [SeatAccess]'s is
//
// A record is internal/seataccess's vocabulary. Each request is its own key,
// written once by the asking node and deleted once by whoever settles it, so
// no write can lose another and no lease is needed.
//
// # No age
//
// It lives in the positions register under its own key class. The engine
// sweeps a request past its own stamp (seataccess.RequestTTL); an age would
// restart at nothing and is a retention nobody chose.
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
