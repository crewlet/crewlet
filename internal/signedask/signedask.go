// Package signedask signs and binds what one node of a fleet ASKS another to
// carry out over the queue's ephemeral verbs — the queue's Ask and Serve, a
// scatter with no stream behind it — and what the asked node answers with.
//
// # Why a request is an instruction
//
// The fleet's broker vouches for a CONNECTION, never a message (ADR-0031): an
// embedded broker admits only a route, a leaf link or an operator presenting a
// key derived from the keyring (ADR-0094), but a route's password crosses the
// fleet network in clear once per connection, and an external cluster admits
// whom its operator chooses — and every connection it admits is bound to the
// one account the engine's own subjects live in. So a message on a subject a
// node SERVES is whatever anything the broker admitted chose to write. Where
// the node only answers a question about itself, that is a forged question and
// an answer nobody needed. Where the node CARRIES THE REQUEST OUT, the message
// is an instruction — and an unsigned instruction is exactly what ADR-0031
// refuses on a state log, one hop before the log.
//
// These channels carry instructions, and each is a [Channel]:
//
//   - the ESTATE SERVICE (internal/estate): a data node runs a tracker, a
//     knowledge-base or a chat write AS THE ACTOR THE REQUEST NAMES, and
//     answers a chat read FOR THE VIEWER IT NAMES, so unsigned, anybody on the
//     broker could file, edit, post or purge as any seat in the company — and
//     the log would carry it signed by the data node that obeyed — or read any
//     private room as anybody;
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
//   - a READ OF A RUN'S BOX asked of the node whose host it is on
//     (internal/sandbox, `sandbox.box_read`): the host reads a box no other
//     node can reach and pauses it, as the collection it is part of does —
//     so unsigned, anybody on the broker could have a run's box read back
//     and paused;
//   - the BOX BRIDGE'S RELAY (internal/httprelay, which internal/api/mcpbridge
//     joins), on two channels: an OPEN has the node holding a box's session
//     run its HTTP request AS WHOEVER ITS TOKEN NAMES, and each frame of the
//     answer is written to the box as that node's. So unsigned, anybody on the
//     broker could call a running seat's tools as that seat, or answer a box in
//     the holder's place;
//   - a TEST OF AN MCP SERVER (internal/engine, `mcp.check`): the node asked
//     starts the server's process or dials its address as the company, with
//     the credentials it is started with, and a draft's launch travels in the
//     request. So unsigned, anybody on the broker could have every node run a
//     command a draft names;
//   - the ASSISTANT'S REPLAY AND STOP (internal/assistant/live), on two
//     channels: the node running a person's turn hands over the frames another
//     node's client missed, or stops the turn, on the asking node's word that
//     the person asked. So unsigned, anybody on the broker could stop anybody's
//     turn, or end one by answering for the node running it;
//   - a CODE SEARCH of several repositories (internal/codesearch): every Gitlet
//     node reads the repositories a request assigns it AS THE ASKER IT NAMES,
//     judging that asker's grant to each — so unsigned, anybody on the broker
//     could read any repository as any seat or person in the company.
//
// # One frame, the state log's
//
// The frame is [recordseal.Signer] and [recordseal.Verifier]: the key id in the
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
// an id exactly as long as its deadline could still admit it, after which the
// deadline refuses it anyway.
//
// THE DEADLINE IS THE SENDING NODE'S INSTANT, JUDGED ON THE TAKING NODE'S
// CLOCK, and two machines' clocks stand apart. A channel says how far it lets
// them ([ClockAllowance]): its taking node admits a request until the deadline
// plus that allowance by its own clock, and remembers the id as long — the
// memory moves with the admission, or a replay arriving after the id was
// forgotten, but inside the allowance, would be taken again. A channel built
// without one judges the deadline on the taking node's clock alone. It is the
// channel's to state, and none unless it does, because what a wider window
// costs is a longer replay window, and what a replay does is the channel's
// receiver's to weigh. A [Guard] is one per serving node and channel, never
// shared: a scatter hands one request to every node, and each must take it
// once.
//
// THE WAIT CROSSES AS A DURATION, never as the deadline: read on the taking
// node's clock, the asker's instant cut the work it bounds short by the
// distance between two clocks where that node stood ahead, and let it run on
// past its asker where it stood behind. So a channel whose taking node bounds
// its work by its asker's wait carries that wait beside the deadline
// ([WaitMS]), and the taking node counts it from the request's arrival on its
// own clock ([Due]); the deadline only binds and admits the request. The wait
// is REQUIRED: a request carrying none runs nothing, since an older unreleased
// build gets no compatibility. The field is additive, so an older taking node
// ignores it and serves a newer asker as it always did.
//
// And a reply names the request it answers ([Reply]): an asker believes only a
// reply that verifies AND is bound to the id it sent, so a reply to another
// question — a captured success, a refusal saying nothing ran — cannot be
// passed off as this one's. The id is drawn from crypto/rand ([NewID]), which
// is what makes it unique across a fleet without a coordinator and
// unguessable by a member of the fleet that did not see the request.
//
// # A write's id outlives a restart
//
// A node that restarted inside a request's window would have forgotten an id
// it held only in memory, and a captured request replayed at it would be taken
// again: harmless for a read, and for a write the write runs again as the
// actor it names. So every request says whether it writes
// ([Request.Writes]), and a guard keeps a write's id in the node's own file
// ([Channel.KeptGuard], [Ledger]), committed BEFORE the request is admitted,
// for exactly as long as it holds it in memory. The FILE HAS THE LAST WORD: a
// write whose id it holds already is a replay, whatever the memory says, so a
// restart forgets nothing and the guard reads nothing back at boot. A write
// whose id the file refuses runs nothing
// ([Unrecorded]), and a guard without a file refuses every write: nothing
// that writes is ever taken unrecorded. Reads stay in memory, where a restart
// costs nothing. Each channel's request says whether it writes, and why a
// note to a turn and the box bridge's relay need no file: each is taken only
// by the process it names, which a restart replaces.
//
// The file holds no id past its window: a sweep of the memory ([Guard])
// deletes every row past its own, and so does a guard's start, so its rows are
// bounded as the memory is, by the writes a channel admits inside one window.
//
// BOTH INSTANTS ARE THIS NODE'S WALL CLOCK: admission judges the deadline on
// it, and an id is kept until the deadline plus the allowance on it, so the
// two move together. A clock stepped back only keeps an id longer; one
// stepped forward forgets an id only once it refuses the request anyway.
//
// # What arrives is one of seven, and each is handled once
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
//     deadline, its deadline and the allowance its channel gives two nodes'
//     clocks had passed when it arrived, or this node has already taken its
//     id. Runs nothing.
//   - UNRECORDED: the fleet's, a write, and this node could not keep its id
//     in its own file. A fact about THIS NODE: it runs nothing, and the asker
//     may ask another.
//   - NOT ADDRESSED: the fleet's, and for another node of a scatter
//     ([Channel.AdmitTo]). Takes nothing, so only the node it names keeps it.
//
// # And one way, for a frame a node shows to people
//
// Two channels carry no instruction and expect no reply, and are signed all
// the same. A PRESENCE frame (internal/presence) is broadcast to every node
// listening, and what a node does with it is show a person in a room: a forged
// one would put a colleague somewhere they are not. An ASSISTANT FRAME
// (internal/assistant/live) is broadcast to every node following a person's
// turn, and what a node does with it is stream it to that person: a forged one
// would put words in their own conversation. So each is sealed one way
// ([OneWay]): the same frame, a key domain of its own, and an expiry inside the
// signed body in place of the id and deadline a reply would be bound to. An
// assistant frame is encrypted under the fleet keyring before it is sealed as
// well, because it carries the person's conversation and a seal hides nothing.
//
// # And a read whose answer is shown
//
// One request-and-reply channel carries no instruction either: the LIVE RELAY
// (internal/engine, liverelay.go), on which a data node whose own copy is out
// of service asks a sound peer what that peer's appliers committed. It only
// reads, and it is a [Channel] all the same, because what the asker does with
// the answer is PUSH it to every socket it holds as the peer's work — a chat
// batch's excerpts and mentions, a datastore batch's documents — so a forged
// answer is words shown under somebody's name. Its asks are bound like any
// request, so the answer a follower believes is the one to the ask it just
// sent; a one-way seal would let a captured answer be believed by any follower
// asking inside its validity.
//
// # What it does not cover
//
// The scatters whose answer is READ — the fleet's history, a search slice, a
// held seat's memory, a sandbox's live tail, each handed back to the one
// caller that asked as the answer to its question — carry no instruction a
// node carries out and nothing a node shows as another's content, so they are
// not channels here; and the queue's own event envelopes, custody batches
// among them, carry what a node observed rather than what it must do
// (ADR-0031). A broker that anybody can reach can read and forge on those as
// it can on any subject: keeping both ports private is the operator's half of
// the rule (SECURITY.md).
package signedask

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/words"
)

var log = logging.Get("signedask")

// Request is a channel's request as the envelope binds it.
//
// THE ID AND THE DEADLINE ARE FIELDS OF THE CHANNEL'S OWN BODY, so they are
// under the signature like every other field: there is no part of an envelope
// a sender can set that the MAC does not cover.
type Request interface {
	// Binding is the id the request was minted under ([NewID]) and the
	// instant, by the sending node's clock, after which nobody is waiting
	// for its answer.
	Binding() (id string, deadline time.Time)

	// Writes reports whether carrying the request out changes anything a
	// second run would change again: a write as the actor it names, a voter
	// removed, a note offered, a turn stopped. Its id is then kept in the
	// node's own file until its window ends ([Channel.KeptGuard]).
	Writes() bool
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
	name           string
	requestDomain  string
	replyDomain    string
	requests       *recordseal.Signer
	requestsOpened *recordseal.Verifier
	replies        *recordseal.Signer
	repliesOpened  *recordseal.Verifier

	// apart is how far this channel lets the sending and the taking node's
	// clocks stand apart ([ClockAllowance]); zero unless it was built with
	// one.
	apart time.Duration
}

// Option is a choice a channel is built with ([New]).
type Option func(*options)

// options is what the [Option]s a channel was built with chose.
type options struct {
	apart time.Duration
}

// ClockAllowance is how far apart, at most, the clocks of the node that sends a
// request and the node that takes it may stand: the taking node admits a request
// until its deadline plus apart by its own clock, and remembers its id exactly as
// long — or a replay arriving after it forgot the id, inside the allowance, would
// be taken again. Each channel states its own, because a wider window keeps every
// id longer, a write's in the node's file too, and lets a read be taken again at
// a node that restarted inside it. A channel built without it judges the
// sender's deadline on its own clock alone. [New] refuses a negative one.
func ClockAllowance(apart time.Duration) Option {
	return func(o *options) { o.apart = apart }
}

// New builds a node's half of the channel name from the fleet keyring — Tier A
// `secrets.keys` and `secrets.active_key_id`, the ring every state-log record
// is signed under — with the choices opts make ([ClockAllowance]). It refuses
// a keyring that names no active key or holds no key at all, for the state
// log's reason: a node that signed under nothing would be refused by every
// peer, one request at a time, for ever. And it refuses a negative clock
// allowance, which would refuse a request before its own deadline and is a
// wiring fault in the channel that states it.
func New[Q Request, R Reply](name string, ring recordseal.Keyring, opts ...Option) (*Channel[Q, R], error) {
	if name == "" {
		return nil, errors.New("signedask: a channel needs a name — it is the key domain " +
			"its envelopes are signed under")
	}
	var chosen options
	for _, opt := range opts {
		opt(&chosen)
	}
	if chosen.apart < 0 {
		return nil, fmt.Errorf("signedask: %s: a clock allowance of %v is negative — it is how "+
			"far two nodes' clocks may stand apart, zero or more", name, chosen.apart)
	}
	c := &Channel[Q, R]{name: name, requestDomain: name + ".request", replyDomain: name + ".reply",
		apart: chosen.apart}
	var err error
	if c.requests, err = recordseal.NewSigner(c.requestDomain, ring); err != nil {
		return nil, fmt.Errorf("signedask: %s: sign the requests this node sends: %w", name, err)
	}
	if c.requestsOpened, err = recordseal.NewVerifier(c.requestDomain, ring); err != nil {
		return nil, fmt.Errorf("signedask: %s: verify the requests this node answers: %w", name, err)
	}
	if c.replies, err = recordseal.NewSigner(c.replyDomain, ring); err != nil {
		return nil, fmt.Errorf("signedask: %s: sign the replies this node answers with: %w", name, err)
	}
	if c.repliesOpened, err = recordseal.NewVerifier(c.replyDomain, ring); err != nil {
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
// A NAMED TYPE WITH SEVEN VALUES rather than a bool, for the state log's own
// [recordseal.Verdict] reason: the failures have different answers — a key this
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
	// Unrecorded is a write the fleet signed whose id this node could not
	// keep in its own file, so it runs nothing here.
	Unrecorded Disposition = "unrecorded"
	// NotAddressed is a request the fleet signed for another node
	// ([Channel.AdmitTo]): nothing taken, nothing kept.
	NotAddressed Disposition = "not_addressed"
)

// Admission is what [Channel.Admit] concluded about one request.
type Admission[Q Request] struct {
	Disposition Disposition

	// Request is the request. VERIFIED where the disposition is [Admitted],
	// [Stale] or [Unrecorded]; UNTRUSTED where it is [KeyUnknown] — decoded so the
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
// with it, judging its deadline on now — this node's clock — within the
// allowance the channel gives two nodes' clocks ([ClockAllowance]), and its id
// against what seen has taken. An id is TAKEN only when the request is
// admitted — and taken whether or not the caller then runs it, because every
// asker mints a fresh id for every ask: a second arrival of one id is a replay
// whatever the first was answered. A write is admitted only once its id is
// committed to the node's file; ctx bounds that write.
func (c *Channel[Q, R]) Admit(ctx context.Context, sealed []byte, seen *Guard, now time.Time) Admission[Q] {
	return c.AdmitTo(ctx, sealed, seen, now, nil)
}

// AdmitTo is [Channel.Admit] on a channel whose requests each name the node
// they are for, scattered to every node: addressed says whether this node is
// the one. It is asked only of a request the fleet signed, after it decoded,
// so nothing unsigned learns which node holds what; and BEFORE the take, so a
// request for another node is [NotAddressed], taking no id and writing no row.
// Nil addresses every request.
func (c *Channel[Q, R]) AdmitTo(ctx context.Context, sealed []byte, seen *Guard, now time.Time,
	addressed func(Q) bool) Admission[Q] {

	var out Admission[Q]
	body, verdict := c.requestsOpened.Open(sealed)
	switch verdict {
	case recordseal.Tampered:
		out.Disposition, out.Why = Tampered, "the request is not signed by this fleet"
		return out
	case recordseal.KeyUnknown:
		out.Disposition = KeyUnknown
		out.Why = fmt.Sprintf("the request is signed under a key this node does not hold "+
			"(it holds %s) — add the key to secrets.keys", words.And(c.HeldKeys()))
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
	if addressed != nil && !addressed(req) {
		out.Disposition, out.Why = NotAddressed, "the request is for another node"
		return out
	}
	if refused, why := seen.take(ctx, id, deadline, c.apart, now, req.Writes()); why != "" {
		out.Disposition, out.Why = refused, why
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
	case recordseal.Tampered:
		return zero, "its reply is not signed by this fleet", nil
	case recordseal.KeyUnknown:
		return zero, fmt.Sprintf("its reply is signed under a key this node does not "+
			"hold (it holds %s) — add the key to secrets.keys", words.And(c.repliesOpened.KeyIDs())), nil
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
// channel would refuse it anyway — its deadline plus the allowance the channel
// gives two nodes' clocks ([ClockAllowance]): a request whose id it holds is a
// replay, and one past that instant is refused before it is looked up — so an
// id forgotten then is refused either way, and the set never holds more than
// the requests its channel could still admit. Each entry keeps its own instant,
// and a guard serves one channel, so one allowance. A write's id is kept in
// the node's file as well (package doc, "A write's id outlives a restart").
//
// ONE PER SERVING NODE AND CHANNEL, never shared between nodes: a scatter
// hands the same request to every node, and each of them takes it once.
type Guard struct {
	mu   sync.Mutex
	seen map[string]time.Time

	// sweepAt is the size at which the next admission sweeps out the ids
	// their channel would refuse now — twice what survived the last sweep, so
	// the cost of sweeping is spread across as many admissions as it has
	// entries to visit.
	sweepAt int

	// file keeps the ids of the writes this guard takes under channel, its
	// channel's name; nil on a guard that keeps nothing past a restart,
	// which refuses every write ([NewGuard]).
	file    Ledger
	channel string
}

// Ledger is the node's own file as a guard keeps the ids of the writes it
// takes in it (internal/store's TakenRequests). Every row names its channel,
// so one file holds every channel's apart.
type Ledger interface {
	// Keep records id as taken on channel until until, committed durably
	// before it returns. fresh is false, and nothing changed, where the file
	// holds the id already: a replay.
	Keep(ctx context.Context, channel, id string, until time.Time) (fresh bool, err error)

	// Forget deletes channel's ids whose until is not after now.
	Forget(ctx context.Context, channel string, now time.Time) error
}

// sweepFloor is the size below which a guard never sweeps: small enough that
// an idle node holds a few kilobytes of ids, large enough that a busy one does
// not sweep on every request.
const sweepFloor = 1024

// NewGuard is an empty guard that keeps nothing past a restart, for a channel
// none of whose requests writes ([Request.Writes]): it refuses a write as
// [Unrecorded], so a channel wired without its file runs no write at all.
func NewGuard() *Guard {
	return &Guard{seen: map[string]time.Time{}, sweepAt: sweepFloor}
}

// KeptGuard is this channel's guard over the node's own file: every write it
// takes is kept there until its window ends, and one the file holds already,
// from before a restart, is a replay. It starts by deleting the ids past their
// window, judged on now, and refuses to start where the file refuses that: a
// file it cannot write is one it could keep no write in.
func (c *Channel[Q, R]) KeptGuard(ctx context.Context, file Ledger, now time.Time) (*Guard, error) {
	if file == nil {
		return nil, fmt.Errorf("signedask: %s: a guard that keeps its writes needs the node's file", c.name)
	}
	if err := file.Forget(ctx, c.name, now); err != nil {
		return nil, fmt.Errorf("signedask: %s: clear the requests this node took past their window "+
			"(the node file's signed_request_taken table): %w", c.name, err)
	}
	return &Guard{seen: map[string]time.Time{}, sweepAt: sweepFloor, file: file, channel: c.name}, nil
}

// take records one request's id until its deadline plus apart — the last
// instant its channel admits it, by this node's clock — answering why it may
// not be taken now, and as what: empty where it was taken. The id is
// remembered exactly as long as it is admitted, never only to the deadline, or
// a replay arriving after a sweep forgot it but inside the allowance would be
// taken again. A write's id is held in memory first, so a concurrent replay
// meets it, and committed to the file before it counts as taken.
func (g *Guard) take(ctx context.Context, id string, deadline time.Time, apart time.Duration,
	now time.Time, writes bool) (Disposition, string) {

	until := deadline.Add(apart)
	switch {
	case g == nil:
		// A WIRING FAULT, answered as refusals rather than a panic in a
		// handler that serves the fleet: without a guard a replay could
		// not be told from the request it repeats.
		return Stale, "this node keeps no record of the requests it has taken, so it could " +
			"not tell a replay from the request it repeats"
	case id == "":
		return Stale, "the request names no id, so a replay of it could not be told from it"
	case deadline.IsZero():
		return Stale, "the request names no deadline, so nothing bounds how long its id " +
			"would have to be remembered"
	case !now.Before(until):
		allowed := ""
		if apart > 0 {
			allowed = fmt.Sprintf(", and the %v this channel allows two nodes' clocks to stand apart,", apart)
		}
		return Stale, fmt.Sprintf("the request's deadline %s%s had passed when it arrived — "+
			"nobody is waiting for its answer", deadline.UTC().Format(time.RFC3339Nano), allowed)
	case writes && g.file == nil:
		return Unrecorded, "this node keeps the ids it takes on this channel in memory only, " +
			"and a write's id must outlive a restart; nothing ran"
	}
	swept, why := g.hold(id, until, now)
	if why != "" {
		return Stale, why
	}
	if swept && g.file != nil {
		// A FAILED SWEEP LEAVES ROWS THE NEXT ONE OR THE NEXT BOOT DELETES,
		// so it refuses nothing.
		if err := g.file.Forget(ctx, g.channel, now); err != nil {
			log.WarnContext(ctx, "signed_requests_not_forgotten", "channel", g.channel,
				"error", err.Error(), "detail", "the next sweep or boot deletes them")
		}
	}
	if !writes {
		return "", ""
	}
	fresh, err := g.file.Keep(ctx, g.channel, id, until)
	switch {
	case err != nil:
		g.release(id)
		log.ErrorContext(ctx, "signed_request_unrecorded", "channel", g.channel, "request", id,
			"error", err.Error(), "detail", "the node file did not keep the id of a write, so "+
				"it ran nothing here; the asker may ask another node")
		return Unrecorded, fmt.Sprintf("this node could not keep request %s in its own file (%v), "+
			"so it ran nothing", id, err)
	case !fresh:
		return Stale, fmt.Sprintf("request %s was taken already, so this is a replay of it", id)
	}
	return "", ""
}

// hold enters id in memory until until, sweeping first where the guard has
// grown to its sweep size; swept says it did.
func (g *Guard) hold(id string, until, now time.Time) (swept bool, why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.seen) >= g.sweepAt {
		for held, forgetAt := range g.seen {
			if !now.Before(forgetAt) {
				delete(g.seen, held)
			}
		}
		g.sweepAt = max(2*len(g.seen), sweepFloor)
		swept = true
	}
	if _, held := g.seen[id]; held {
		return swept, fmt.Sprintf("request %s was taken already, so this is a replay of it", id)
	}
	g.seen[id] = until
	return swept, ""
}

// release forgets id, a write the file would not keep: it ran nothing, so it
// may be taken again.
func (g *Guard) release(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.seen, id)
}
