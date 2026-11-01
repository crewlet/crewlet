// Package signedask signs and binds what one node of a fleet ASKS another to
// carry out over the queue's ephemeral verbs — the queue's Ask and Serve, a
// scatter with no stream behind it — and what the asked node answers with.
//
// # Why a request is an instruction
//
// The fleet's broker vouches for a CONNECTION, never a message (ADR-0031): an
// embedded broker admits only a route, a leaf link or an operator presenting a
// key derived from the keyring (ADR-0064), but a route's password crosses the
// fleet network in clear once per connection, and an external cluster admits
// whom its operator chooses — and every connection it admits is bound to the
// one account the engine's own subjects live in. So a message on a subject a
// node SERVES is whatever anything the broker admitted chose to write. Where
// the node only answers a question about itself, that is a forged question and
// an answer nobody needed. Where the node CARRIES THE REQUEST OUT, the message
// is an instruction — and an unsigned instruction is exactly what ADR-0031
// refuses on a state log, one hop before the log.
//
// Four channels are instructions, and each is a [Channel]:
//
//   - the ESTATE SERVICE (internal/estate): a data node runs a tracker or a
//     knowledge-base write AS THE ACTOR THE REQUEST NAMES, so unsigned, anybody
//     on the broker could file, edit or purge as any seat in the company, and
//     the log would carry it signed by the data node that obeyed;
//   - the FLEET BROKER'S MEMBERSHIP (internal/engine, fleetbroker.go): a member
//     removes a voter from the broker's metadata group through its own system
//     account, the one action the design keeps off the global account — so
//     unsigned, anybody on the broker could change the quorum every election
//     runs on, past the grant, the step-up and the live-member refusal the
//     operator's route puts in front of it;
//   - a NOTE TO A RUNNING TURN (internal/agent/steer): the running node offers
//     a person's note to the turn AS THAT PERSON, on the asking node's word
//     that they may steer it — so unsigned, anybody on the broker could put
//     words in front of a running agent under anybody's name;
//   - a CODE SEARCH of several repositories (internal/codesearch): every git
//     node reads the repositories a request assigns it AS THE ASKER IT NAMES,
//     judging that asker's grant to each — so unsigned, anybody on the broker
//     could read any repository as any seat or person in the company.
//
// # One frame, the state log's
//
// The frame is [statelog.Signer] and [statelog.Verifier]: the key id in the
// clear, an HMAC over the domain, the id and the body under a key derived from
// the keyring per domain. A second signature format here would be a second
// implementation of a rule nothing compares (ADR-0008), and the rotation story
// is the one an operator already runs — add, restart, flip, restart, drop —
// because the frame names the key that signed it.
//
// EVERY CHANNEL SIGNS IN DOMAINS OF ITS OWN, two of them: `<name>.request` and
// `<name>.reply` ([Channel.Domains]). The domain is inside the derived key and
// inside the MAC, so no state-log record verifies as an envelope, no envelope
// of one channel verifies on another, and no reply captured off the wire can
// be sent back as a request.
//
// # A signature can be replayed, so it is bound
//
// A signature says the fleet wrote these bytes, not that it wrote them NOW or
// for THIS question. So a request carries an id and a deadline inside its
// signed body ([Request]), and the node that takes it refuses one whose
// deadline has passed or whose id it has already taken ([Guard]) — remembering
// an id only until its deadline, after which the deadline refuses it anyway.
// And a reply names the request it answers ([Reply]): an asker believes only a
// reply that verifies AND is bound to the id it sent, so a reply to another
// question — a captured success, a refusal saying nothing ran — cannot be
// passed off as this one's. The id is drawn from crypto/rand ([NewID]), which
// is what makes it unique across a fleet without a coordinator and
// unguessable by a member of the fleet that did not see the request.
//
// # What arrives is one of five, and each is handled once
//
// [Channel.Admit] decides what a serving node may do with what it was sent:
//
//   - ADMITTED: the fleet's, bound, and not seen before — carried out.
//   - TAMPERED: not a frame, or a MAC that fails under a key this node holds.
//     A fact about the MESSAGE: nothing in it is believed, not even the id a
//     reply would name, it runs nothing and it is logged at error.
//   - KEY UNKNOWN: signed under a key this node does not hold. A fact about
//     THIS NODE, which an operator changes by adding the key — so the request
//     runs nothing and is answered "did not run", and the asker moves on to a
//     node that holds the key. The body is decoded UNTRUSTED for the one thing
//     such an answer needs: the id (and where a channel routes by it, the
//     object) it is about.
//   - UNDECODABLE: the fleet signed it and this build cannot read it — a peer
//     on another build. Runs nothing.
//   - STALE: the fleet's, and not to be taken now: it names no id or no
//     deadline, its deadline had passed when it arrived, or this node has
//     already taken its id. Runs nothing.
//
// # And one way, for a frame that names a person
//
// One channel carries no instruction and expects no reply, and is signed all
// the same: a PRESENCE frame (internal/presence) is broadcast to every node
// listening, and what a node does with it is show a person in a room. A forged
// one would put a colleague somewhere they are not, so it is sealed one way
// ([OneWay]): the same frame, a key domain of its own, and an expiry inside the
// signed body in place of the id and deadline a reply would be bound to.
//
// # What it does not cover
//
// The scatters that only READ — the fleet's history, a search slice, a held
// seat's memory, a sandbox's live tail — carry no instruction a node carries
// out, so they are not channels here; and the queue's own event envelopes,
// custody batches among them, carry what a node observed rather than what it
// must do (ADR-0031). A broker that anybody can reach can read and forge on
// those as it can on any subject: keeping both ports private is the operator's
// half of the rule (SECURITY.md).
package signedask

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Request is a channel's request as the envelope binds it.
//
// THE ID AND THE DEADLINE ARE FIELDS OF THE CHANNEL'S OWN BODY, so they are
// under the signature like every other field: there is no part of an envelope
// a sender can set that the MAC does not cover.
type Request interface {
	// Binding is the id the request was minted under ([NewID]) and the
	// instant after which nobody is waiting for its answer.
	Binding() (id string, deadline time.Time)
}

// Reply is a channel's reply as the envelope binds it.
type Reply interface {
	// Answers is the id of the request this reply answers, empty only
	// where the request could not be read at all.
	Answers() string
}

// Channel signs and verifies one channel's envelopes for one node: the
// requests it sends and the replies it answers with, and the opposite halves
// of both.
//
// ONE VALUE FOR BOTH HALVES, built once from the node's keyring and handed to
// the asking and the serving side alike, because a node is usually both, and a
// node that could sign a request it could not verify the answer to would
// believe nothing it was told. Immutable and safe for concurrent use; the ids a
// serving node has taken live in its [Guard], not here.
type Channel[Q Request, R Reply] struct {
	requestDomain  string
	replyDomain    string
	requests       *statelog.Signer
	requestsOpened *statelog.Verifier
	replies        *statelog.Signer
	repliesOpened  *statelog.Verifier
}

// New builds a node's half of the channel name from the fleet keyring — Tier A
// `secrets.keys` and `secrets.active_key_id`, the ring every state-log record
// is signed under. It refuses a keyring that names no active key or holds no
// key at all, for the state log's reason: a node that signed under nothing
// would be refused by every peer, one request at a time, for ever.
func New[Q Request, R Reply](name string, ring statelog.Keyring) (*Channel[Q, R], error) {
	if name == "" {
		return nil, errors.New("signedask: a channel needs a name — it is the key domain " +
			"its envelopes are signed under")
	}
	c := &Channel[Q, R]{requestDomain: name + ".request", replyDomain: name + ".reply"}
	var err error
	if c.requests, err = statelog.NewSigner(c.requestDomain, ring); err != nil {
		return nil, fmt.Errorf("signedask: %s: sign the requests this node sends: %w", name, err)
	}
	if c.requestsOpened, err = statelog.NewVerifier(c.requestDomain, ring); err != nil {
		return nil, fmt.Errorf("signedask: %s: verify the requests this node answers: %w", name, err)
	}
	if c.replies, err = statelog.NewSigner(c.replyDomain, ring); err != nil {
		return nil, fmt.Errorf("signedask: %s: sign the replies this node answers with: %w", name, err)
	}
	if c.repliesOpened, err = statelog.NewVerifier(c.replyDomain, ring); err != nil {
		return nil, fmt.Errorf("signedask: %s: verify the replies this node is sent: %w", name, err)
	}
	return c, nil
}

// Domains is the two key domains this channel signs under, for the gate that
// holds every channel's apart from every other's and from every state log's.
func (c *Channel[Q, R]) Domains() (request, reply string) {
	return c.requestDomain, c.replyDomain
}

// HeldKeys is every key id this node verifies under, sorted, for the sentence
// that says why a request signed under another was not run.
func (c *Channel[Q, R]) HeldKeys() []string { return c.requestsOpened.KeyIDs() }

// NewID is a request's id: 128 bits from crypto/rand. See the package doc.
func NewID() string { return rand.Text() }

// SealRequest encodes and signs one request.
//
// IT REFUSES A REQUEST THAT IS NOT BOUND — no id, or no deadline — because
// every serving node would refuse it as stale, and a sender that minted one is
// a bug that should fail here, on the node that has it, rather than as a
// refusal on every peer.
func (c *Channel[Q, R]) SealRequest(req Q) ([]byte, error) {
	if id, deadline := req.Binding(); id == "" || deadline.IsZero() {
		return nil, errors.New("signedask: a request is sealed with an id and a deadline, " +
			"or every node refuses it as one whose replay it could not tell from it")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("signedask: encode the request: %w", err)
	}
	return c.requests.Seal(raw), nil
}

// Disposition is what a serving node may do with one request it was sent.
//
// A NAMED TYPE WITH FIVE VALUES rather than a bool, for the state log's own
// [statelog.Verdict] reason: the failures have different answers — a key this
// node does not hold is "ask another node", a tampered frame is "ask nobody,
// believe nothing" — and a caller handed `false` for all of them would have to
// guess which.
type Disposition string

const (
	// Admitted is a request this node may carry out.
	Admitted Disposition = "admitted"
	// Tampered is a request that is not the fleet's.
	Tampered Disposition = "tampered"
	// KeyUnknown is a request signed under a key this node does not hold.
	KeyUnknown Disposition = "key_unknown"
	// Undecodable is a request the fleet signed that this build cannot read.
	Undecodable Disposition = "undecodable"
	// Stale is a request the fleet signed that is not to be taken now.
	Stale Disposition = "stale"
)

// Admission is what [Channel.Admit] concluded about one request.
type Admission[Q Request] struct {
	Disposition Disposition

	// Request is the request. VERIFIED where the disposition is [Admitted]
	// or [Stale]; UNTRUSTED where it is [KeyUnknown] — decoded so the
	// answer can say which id and which object it is about, and never
	// carried out — and the zero value otherwise.
	Request Q

	// ID is the id a reply to it names: the verified id, or for
	// [KeyUnknown] the untrusted one, and empty for [Tampered] and
	// [Undecodable], which no reply can be bound to.
	ID string

	// Why is the sentence a refusal says, for every disposition but
	// [Admitted].
	Why string
}

// Admit opens one request a serving node was sent and decides what it may do
// with it, judging its deadline on now and its id against what seen has
// taken. An id is TAKEN only when the request is admitted — and taken whether
// or not the caller then runs it, because every asker mints a fresh id for
// every ask: a second arrival of one id is a replay whatever the first was
// answered.
func (c *Channel[Q, R]) Admit(sealed []byte, seen *Guard, now time.Time) Admission[Q] {
	var out Admission[Q]
	body, verdict := c.requestsOpened.Open(sealed)
	switch verdict {
	case statelog.Tampered:
		out.Disposition, out.Why = Tampered, "the request is not signed by this fleet"
		return out
	case statelog.KeyUnknown:
		out.Disposition = KeyUnknown
		out.Why = fmt.Sprintf("the request is signed under a key this node does not hold "+
			"(it holds %v) — add the key to secrets.keys", c.HeldKeys())
		var claimed Q
		if err := json.Unmarshal(body, &claimed); err == nil {
			out.Request = claimed
			out.ID, _ = claimed.Binding()
		}
		return out
	}
	var req Q
	if err := json.Unmarshal(body, &req); err != nil {
		out.Disposition, out.Why = Undecodable, fmt.Sprintf("the fleet signed a request "+
			"this build cannot decode: %v", err)
		return out
	}
	out.Request = req
	id, deadline := req.Binding()
	out.ID = id
	if why := seen.take(id, deadline, now); why != "" {
		out.Disposition, out.Why = Stale, why
		return out
	}
	out.Disposition = Admitted
	return out
}

// SealReply encodes and signs one reply. A serving node answers EVERY request
// it was sent, a refusal included: a node that stayed silent would be
// indistinguishable from one that is down, and the asker would wait out its
// budget before asking the next.
func (c *Channel[Q, R]) SealReply(rep R) ([]byte, error) {
	raw, err := json.Marshal(rep)
	if err != nil {
		return nil, fmt.Errorf("signedask: encode the reply: %w", err)
	}
	return c.replies.Seal(raw), nil
}

// OpenReply verifies one reply and decodes it, believing it only where it is
// bound to asked, the id of the request this node sent. why is set, and the
// reply must not be believed, where it is not the fleet's, is signed under a
// key this node does not hold, or answers another request. err is set for a
// reply the fleet signed that this build cannot decode.
//
// A REPLY THAT IS NOT BELIEVED IS NO ANSWER — never an answer of "did not
// run": a refusal the fleet signed for another request is exactly what
// somebody replaying one would send, so an asker that read it as "nothing ran"
// would repeat a write that may have.
func (c *Channel[Q, R]) OpenReply(sealed []byte, asked string) (rep R, why string, err error) {
	var zero R
	if asked == "" {
		return zero, "no request was named to bind the reply to", nil
	}
	body, verdict := c.repliesOpened.Open(sealed)
	switch verdict {
	case statelog.Tampered:
		return zero, "its reply is not signed by this fleet", nil
	case statelog.KeyUnknown:
		return zero, fmt.Sprintf("its reply is signed under a key this node does not "+
			"hold (it holds %v) — add the key to secrets.keys", c.repliesOpened.KeyIDs()), nil
	}
	if err := json.Unmarshal(body, &rep); err != nil {
		return zero, "", fmt.Errorf("signedask: decode the reply: %w", err)
	}
	if answers := rep.Answers(); answers != asked {
		return zero, fmt.Sprintf("its reply answers request %q, not this one", answers), nil
	}
	return rep, "", nil
}

// Guard is the ids of the requests one serving node has taken, each until its
// deadline: a request whose id it holds is a replay, and one whose deadline
// has passed is refused before it is looked up — so an id forgotten at its
// deadline is refused either way, and the set never holds more than the
// requests still in flight.
//
// ONE PER SERVING NODE AND CHANNEL, never shared between nodes: a scatter
// hands the same request to every node, and each of them takes it once.
type Guard struct {
	mu   sync.Mutex
	seen map[string]time.Time

	// sweepAt is the size at which the next admission sweeps out the ids
	// whose deadlines have passed — twice what survived the last sweep, so
	// the cost of sweeping is spread across as many admissions as it has
	// entries to visit.
	sweepAt int
}

// sweepFloor is the size below which a guard never sweeps: small enough that
// an idle node holds a few kilobytes of ids, large enough that a busy one does
// not sweep on every request.
const sweepFloor = 1024

// NewGuard is an empty guard.
func NewGuard() *Guard {
	return &Guard{seen: map[string]time.Time{}, sweepAt: sweepFloor}
}

// take records one request's id until its deadline, answering why it may not
// be taken now — empty where it was taken.
func (g *Guard) take(id string, deadline, now time.Time) string {
	switch {
	case g == nil:
		// A WIRING FAULT, answered as refusals rather than a panic in a
		// handler that serves the fleet: without a guard a replay could
		// not be told from the request it repeats.
		return "this node keeps no record of the requests it has taken, so it could " +
			"not tell a replay from the request it repeats"
	case id == "":
		return "the request names no id, so a replay of it could not be told from it"
	case deadline.IsZero():
		return "the request names no deadline, so nothing bounds how long its id " +
			"would have to be remembered"
	case !now.Before(deadline):
		return fmt.Sprintf("the request's deadline %s had passed when it arrived — "+
			"nobody is waiting for its answer", deadline.UTC().Format(time.RFC3339Nano))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.seen) >= g.sweepAt {
		for held, until := range g.seen {
			if !now.Before(until) {
				delete(g.seen, held)
			}
		}
		g.sweepAt = max(2*len(g.seen), sweepFloor)
	}
	if _, held := g.seen[id]; held {
		return fmt.Sprintf("request %s was taken already, so this is a replay of it", id)
	}
	g.seen[id] = deadline
	return ""
}
