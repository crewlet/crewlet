package estate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/signedask"
)

// Every request and reply on the estate's subjects is signed under the fleet
// keyring by internal/signedask's rule, never a copy of it (ADR-0008); why:
// package doc, "Every envelope is signed, and bound to its question". ADMITTED
// is dispatched as the actor (or for the viewer) in the verified body; TAMPERED
// is [unservedUnauthenticated], bound to no request, logged at error; KEY
// UNKNOWN the same but bound to the asker's id, so the router asks a node with
// the key; UNDECODABLE is an error (a build that disagrees, not an outsider);
// STALE is [unservedStale], bound to the request it repeats; UNRECORDED is
// [unservedUnrecorded]. The domains `estate.request` and `estate.reply`
// ([channelName]) keep other channels' envelopes and state-log records from
// verifying here. Custody batches are event envelopes, unsigned: observations,
// not an instruction run as somebody (the event spine's call).

// channelName is the estate's channel, and so the stem of its two key domains.
const channelName = "estate"

// Envelopes signs and verifies the estate's envelopes for one node: the
// requests its router sends and the replies its server answers, and the
// opposite halves of both. One value serves both halves, since a node both asks
// and answers, and one that could sign a request but not verify its answer
// would believe nothing. It also holds the verifier keys a machine token's
// answer is MACed under ([TokenFacts]), formed by the answering half and
// checked by the asking one, so both derive from this ring and no separately
// wired value can come from another.
type Envelopes struct {
	channel   *signedask.Channel[request, reply]
	verifiers verifierKeys
}

// NewEnvelopes builds a node's envelopes from the fleet keyring — Tier A
// `secrets.keys` and `secrets.active_key_id`, the ring every state-log record
// is signed under. It refuses a keyring that names no active key or holds no
// key at all, for the state log's reason: a node that signed under nothing
// would be refused by every peer, one request at a time, for ever.
func NewEnvelopes(ring recordseal.Keyring) (*Envelopes, error) {
	channel, err := signedask.New[request, reply](channelName, ring)
	if err != nil {
		return nil, fmt.Errorf("estate: %w", err)
	}
	// signedask.New has held the ring to a signing shape (an id on every
	// entry, the active one among them), so every key derives.
	return &Envelopes{channel: channel, verifiers: newVerifierKeys(ring)}, nil
}

// errNoEnvelopes is a router or a server built without envelopes.
var errNoEnvelopes = errors.New("estate: no envelopes — every request and reply on the " +
	"estate's subjects is signed under the fleet keyring (Tier A secrets.keys), so " +
	"a node without them can neither ask nor answer")

// sealRequest encodes and signs one request, which must carry its id and its
// deadline ([signedask.Channel.SealRequest]).
func (e *Envelopes) sealRequest(req request) ([]byte, error) {
	return e.channel.SealRequest(req)
}

// admit opens one request a data node was sent and decides what it may do with
// it — see [signedask.Channel.Admit].
func (e *Envelopes) admit(ctx context.Context, sealed []byte, seen *signedask.Guard,
	now time.Time) signedask.Admission[request] {
	return e.channel.Admit(ctx, sealed, seen, now)
}

// keptGuard is a serving node's guard over its own file
// ([signedask.Channel.KeptGuard]).
func (e *Envelopes) keptGuard(ctx context.Context, file signedask.Ledger, now time.Time) (*signedask.Guard, error) {
	return e.channel.KeptGuard(ctx, file, now)
}

// sealReply encodes and signs one reply. It never fails: every field of a reply
// is plain data, and a node that could not answer would be the silent node an
// answer exists to avoid — so a reply that will not encode is answered by one
// saying so.
func (e *Envelopes) sealReply(r reply) []byte {
	sealed, err := e.channel.SealReply(r)
	if err == nil {
		return sealed
	}
	// The fallback is three plain strings, which always encode.
	sealed, _ = e.channel.SealReply(reply{Node: r.Node, Request: r.Request, Err: &wireError{
		Message: fmt.Sprintf("estate: %s could not encode its reply: %v", r.Node, err),
	}})
	return sealed
}

// openReply verifies one reply and decodes it, believing it only where it is
// bound to the request it answers — see [signedask.Channel.OpenReply].
func (e *Envelopes) openReply(sealed []byte, asked string) (rep reply, why string, err error) {
	return e.channel.OpenReply(sealed, asked)
}

// Domains is the two key domains the estate's envelopes are signed under, for
// the gate that holds every channel's apart from every other's and from every
// state log's.
func (e *Envelopes) Domains() (request, reply string) { return e.channel.Domains() }

// heldKeys is every key id this node verifies requests under.
func (e *Envelopes) heldKeys() []string { return e.channel.HeldKeys() }
