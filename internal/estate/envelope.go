package estate

import (
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY ENVELOPE ON THE ESTATE'S SUBJECTS IS SIGNED, the request and the reply
// alike, under the fleet keyring — and an envelope that does not verify runs
// nothing and decides nothing.
//
// # Why the estate needs it at all
//
// A data node runs a tracker write AS THE ACTOR THE REQUEST NAMES: the history
// row, the wake and the authority a write is decided on are that actor's. Every
// record the write lands on a log is signed (ADR-0031), but a record signed by
// the data node that wrote it proves only that a fleet member wrote it — and
// the member wrote whatever the request asked. Unsigned, a request on
// `crewlet.estate.<node>` was therefore exactly the instruction ADR-0031 exists
// to refuse, one hop earlier: anyone who could reach the broker could file a
// task, edit a remark or purge one as any seat in the company, and the log
// would carry it signed. And it answers a chat read FOR THE VIEWER THE REQUEST
// NAMES: which private rooms and direct conversations the answer covers is that
// seat's membership, so a read here is not a question a node answers about
// itself — unsigned, it was any private room read as anybody. The reply is the
// other half: an asker that believed any bytes arriving on its reply inbox
// could be told a write landed that never ran, or handed somebody else's
// answer.
//
// # One rule, internal/signedask's
//
// The frame, the domains, the binding and the verdicts are not the estate's
// own: they are the rule every channel a node carries out a request on keeps,
// stated once in internal/signedask, which the fleet broker's membership and a
// note to a running turn take too. A second copy here would be ADR-0008's
// failure — two implementations of a rule nothing compares — on the one rule
// whose silent drift is a write as anybody. What the estate states for itself
// is what each disposition MEANS on its subject:
//
//   - ADMITTED: dispatched, as the actor — or for the viewer — inside the
//     verified body.
//   - TAMPERED: answered [unservedUnauthenticated], bound to no request,
//     logged at error, never dispatched.
//   - KEY UNKNOWN: answered [unservedUnauthenticated], bound to the id the
//     asker sent, so the router asks a data node that holds the key.
//   - UNDECODABLE: answered as an error — the fleet's, so the sender is a
//     build that disagrees with this one rather than somebody who is not the
//     fleet.
//   - STALE: answered [unservedStale], bound to the request it repeats.
//
// A request is bound by its [request.ID] and [request.Deadline], a reply by
// [reply.Request]; a data node keeps the ids it has taken in a
// [signedask.Guard] of its own. The domains are `estate.request` and
// `estate.reply` ([channelName]), so no state-log record and no other
// channel's envelope verifies as an estate one.
//
// # What it does not cover
//
// The custody batches a node holding no data hands its audit events to a data
// node through are EVENT envelopes on the queue's durable verbs, not estate
// envelopes, and are not signed here: they carry what a node observed rather
// than an instruction a data node runs as somebody, and bringing them under a
// signature is a decision about the event spine, not about this service.

// channelName is the estate's channel, and so the stem of its two key domains.
const channelName = "estate"

// Envelopes signs and verifies the estate's envelopes for one node: the
// requests its router sends and the replies its server answers, and the
// opposite halves of both.
//
// ONE VALUE FOR BOTH HALVES, built once from the node's keyring and handed to
// the router and the server alike, because a node is both: a data node answers
// its peers and asks them, and a node that could sign a request it could not
// verify the answer to would believe nothing it was told.
//
// AND THE VERIFIER KEYS, the one other thing the estate's wire derives from
// the keyring: a machine token's answer carries a MAC in place of its verifier
// ([TokenFacts]), formed by the half that answers and checked by the half that
// asks. Both need the same keys, and both are handed exactly this value — a
// second value beside it, wired apart, could be wired from another ring.
type Envelopes struct {
	channel   *signedask.Channel[request, reply]
	verifiers verifierKeys
}

// NewEnvelopes builds a node's envelopes from the fleet keyring — Tier A
// `secrets.keys` and `secrets.active_key_id`, the ring every state-log record
// is signed under. It refuses a keyring that names no active key or holds no
// key at all, for the state log's reason: a node that signed under nothing
// would be refused by every peer, one request at a time, for ever.
func NewEnvelopes(ring statelog.Keyring) (*Envelopes, error) {
	channel, err := signedask.New[request, reply](channelName, ring)
	if err != nil {
		return nil, fmt.Errorf("estate: %w", err)
	}
	// THE RING HAS JUST BEEN HELD TO A SIGNING SHAPE — an id on every
	// entry, the active one among them — so every key derives.
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
func (e *Envelopes) admit(sealed []byte, seen *signedask.Guard, now time.Time) signedask.Admission[request] {
	return e.channel.Admit(sealed, seen, now)
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
	// THE FALLBACK IS THREE PLAIN STRINGS, which always encode, so its own
	// error is not a case: what goes out is the reply that says why.
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
