package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DIRECTORY A NODE THAT RUNS NO IDENTITY DOMAIN READS: the fleet's.
//
// # Why a seats-only node needs one at all
//
// A seats-only satellite does not apply the identity log, so its copy of the
// directory is empty — and it still runs everything the party registry feeds.
// It consumes inbound deliveries (the inbound group is fleet-wide, so the node
// that wins a delivery is whichever node asked first), it runs seats whose
// turns render a roster and resolve colleagues, and its transports mention
// people by the accounts the registry holds. Built from the chart alone, its
// registry went on attributing a suspended person's Slack messages to their
// seat for every delivery it happened to consume, and its agents went on
// addressing them, while every ingress node had withdrawn them within one
// apply. A suspension's promise held on some nodes of a fleet and not others,
// which is the same as not holding.
//
// Reading its own empty copy as "nobody holds any seat" is the other wrong
// answer — it is right only when the company genuinely has nobody bound — so
// the node ASKS: every node that runs the identity domain serves who holds each
// seat, from its own replicated rows, over the broker's ephemeral scatter
// ([queue.EventQueue.Ask]) — a query, not an event, so nothing is retained and
// nothing is audited per read. The satellite takes the answer from the
// most-caught-up node that replied, which is the reading closest to the log.
//
// # Three answers, and the third is not a guess
//
//   - A node running the domain answers: that is the reading.
//   - Nobody answers, and the identity LOG has never carried a record: the
//     company has bound nobody to anything, so the empty reading is a FACT
//     rather than a fallback — every fleet provisions every domain's stream,
//     and a log with no record in it is a directory with no row in it anywhere.
//     This is what an all-seats fleet reads, which is a deployment with no
//     sign-in surface and therefore nobody to suspend.
//   - Nobody answers and the log HAS records: some node ran the domain and
//     none is answering now. That is UNKNOWN, and the registry treats it as
//     every unreadable directory is treated — the last reading carried
//     forward, or [notify.Unread] if there has never been one.
//
// # How fast a suspension reaches it
//
// There is no committed hook here — the node applies nothing on this log — so
// the reading is taken on the directory trigger's periodic net,
// [DirectoryRefresh], and on every published company. A suspension therefore
// reaches a satellite within one refresh interval rather than within one
// apply, which is the stall threshold the alarm table already uses.
//
// # An answer the fleet did not write is not an answer
//
// The broker has no authentication of its own (ADR-0018), and the scatter is a
// subject anything that can reach the cluster port may serve. An unsigned
// answer is therefore an INSTRUCTION anyone could write: a reply stating a
// position no honest node has reached, holding nobody, would be chosen as the
// most caught up by every satellite, and route every suspended, retired and
// removed person's accounts back to their seats. So an answer is signed under
// the fleet's Tier A keyring exactly as a state-log record is — under this
// subject's own label, so a reply never verifies as a record and no record as a
// reply — and a satellite reads only an answer that verifies. Three more checks
// close what a signature alone leaves open:
//
//   - THE ASKER'S NONCE IS ECHOED inside the signed body, so an answer captured
//     from an earlier scatter cannot be replayed into a later one.
//   - A POSITION PAST THE LOG'S HEAD is refused: the satellite reads its own
//     view of the identity log's last sequence, and an answer claiming to have
//     applied records the log does not hold is not one any node can give.
//   - A READING NEVER GOES BACKWARD. A satellite remembers the position of the
//     last reading it accepted, and an answer below it is the unknown arm —
//     the last reading carried forward — never a rebuild. The most caught-up
//     reply of ONE scatter is only the best of whoever answered in time: the
//     node that had applied a suspension can restart or run slow past the
//     budget, and the one that answered instead, still behind it, would have
//     routed the suspended person again until the first came back. An
//     applier's own position only moves forward, and so does this.

// holdersProtocol is the directory scatter's payload version.
//
// TWO, since an answer became a signed frame echoing the asker's nonce: a peer
// at one could neither open the frame nor produce one. It moves only for a
// reshape: a new field needs no bump, because an older peer ignores it. A peer
// that does not speak the asker's version answers nothing, which the asker
// counts as a node that did not reply.
const holdersProtocol = 2

// holdersSignatureLabel is what an answer is signed under — the scatter's own
// subject, standing where a state log's domain name stands in a record's key
// derivation. A subject is never a domain's name, so an answer can never verify
// as a record on any log, nor a record as an answer.
const holdersSignatureLabel = topics.IamHolders

// holdersNonceBytes is the size of the asker's nonce: sixteen random bytes,
// the length of a uuid — enough that two scatters never share one for the life
// of the fleet, which is the whole of what it is for.
const holdersNonceBytes = 16

// fleetDirectoryBudget bounds one scatter.
//
// [statelog.ReadBudget], the two seconds a linearizable read is promised, and
// for the same composition: a broker round trip plus one local read on the
// answering node — here an index range over the seat bindings and the removal
// tombstones. A scatter that has not heard back inside it is taken with
// whatever did arrive, and one that heard nothing is the unknown arm, which the
// next [DirectoryRefresh] retries.
const fleetDirectoryBudget = statelog.ReadBudget

// holdersRequest is one scattered question: always "who holds every seat", at
// a version, with the nonce an answer must echo.
type holdersRequest struct {
	Version int    `json:"version"`
	Nonce   string `json:"nonce"`
}

// holdersReply is one node's answer.
type holdersReply struct {
	Version int    `json:"version"`
	Node    string `json:"node"`

	// At is the answering node's identity checkpoint, PACKED
	// ([statelog.Position.Packed]), so two answers compare across a
	// reanchor: the generation is in the high bits. It is read BEFORE the
	// rows, so it is a floor under what the answer saw.
	At int64 `json:"at"`

	// Nonce is the request's, echoed INSIDE the signed body, which is what
	// binds this answer to the scatter that asked for it.
	Nonce string `json:"nonce"`

	Holders []holderWire `json:"holders"`
}

// holderWire is one binding on the wire — the FACT, never the verdict, and
// never the person: which seat the binding names, by the seat's IDENTITY (the
// handle it was created under, ADR-0020), and at what stage. Who holds it stays
// on the nodes that hold the directory.
type holderWire struct {
	Seat    string `json:"seat"`
	Stage   string `json:"stage,omitempty"`
	Removed bool   `json:"removed,omitempty"`
}

// holdersSource is the identity estate as the scatter's answerer reads it.
type holdersSource interface {
	SeatHolders(ctx context.Context) ([]iamdomain.SeatHolder, error)
	At() statelog.Position
}

// serveHolders makes this node one of the fleet's directory answerers.
//
// Every node that runs the identity domain serves, whether or not a satellite
// ever asks: a scatter has no roster, and the node that happens to be up is
// the one a satellite needs. Every answer is SIGNED — see the notes above — and
// a node with no signer serves nothing rather than answering unsigned.
func serveHolders(ctx context.Context, q queue.EventQueue, node string,
	src holdersSource, signer *statelog.Signer) (queue.Unsubscribe, error) {

	if q == nil || src == nil {
		return nil, errors.New("engine: serve the directory with no broker or " +
			"no directory")
	}
	if signer == nil {
		return nil, fmt.Errorf("%w: the directory's answers are signed under "+
			"secrets.keys, and this node holds no signer", statelog.ErrUnsigned)
	}
	return q.Serve(ctx, topics.IamHolders, func(ctx context.Context, raw []byte) ([]byte, error) {
		var req holdersRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("engine: decode a directory request: %w", err)
		}
		if req.Version != holdersProtocol {
			return nil, fmt.Errorf("engine: directory request at version %d, "+
				"this build speaks %d", req.Version, holdersProtocol)
		}
		at := src.At()
		holders, err := src.SeatHolders(ctx)
		if err != nil {
			// ANSWERS NOTHING, which the asker reads as a node that did
			// not reply — never as an empty directory.
			log.WarnContext(ctx, "directory_answer_failed", "error", err)
			return nil, err
		}
		reply := holdersReply{
			Version: holdersProtocol, Node: node, At: at.Packed(),
			Nonce:   req.Nonce,
			Holders: make([]holderWire, 0, len(holders)),
		}
		for _, h := range holders {
			reply.Holders = append(reply.Holders, holderWire{
				Seat: h.Seat, Stage: string(h.Stage), Removed: h.Removed,
			})
		}
		body, err := json.Marshal(reply)
		if err != nil {
			return nil, fmt.Errorf("engine: encode a directory answer: %w", err)
		}
		return signer.Seal(body), nil
	})
}

// errNoDirectoryAnswer is the unknown arm of a satellite's reading.
var errNoDirectoryAnswer = errors.New("engine: no node running the identity " +
	"domain answered, and its log has records, so this node cannot say who " +
	"holds which seat")

// errDirectoryBehind is the unknown arm for an answer read BELOW the reading
// this node already took — see "A reading never goes backward" above.
var errDirectoryBehind = errors.New("engine: every node that answered for " +
	"the identity directory is behind the reading this node already has, so " +
	"it keeps that one")

// fleetDirectory is [notify.Directory] for a node that runs no identity domain.
type fleetDirectory struct {
	// ask is the broker's scatter.
	ask queue.EventQueue

	// answerers counts the live nodes that run the identity domain, off the
	// presence leases — how many replies are worth waiting for.
	answerers func(ctx context.Context) (int, error)

	// head is the identity log's last sequence as THIS node reads it, zero
	// for a log that has never carried a record: the ceiling no honest
	// answer's position can exceed, and the fact an empty directory rests on.
	head func(ctx context.Context) (uint64, error)

	// verifier opens an answer's frame. Nil opens nothing, so every answer
	// is refused — never read unverified.
	verifier *statelog.Verifier

	// mu guards floor.
	mu sync.Mutex

	// floor is the packed position of the last reading this node accepted.
	// PER NODE AND IN MEMORY, because it is this node's observation of the
	// fleet and nobody else has to agree on it; a restart begins again from
	// whatever the fleet answers first, as a boot always has.
	floor int64
}

var _ notify.Directory = (*fleetDirectory)(nil)

// SeatHolders asks the fleet, and answers with the most-caught-up reply it can
// trust, never one behind the reading it already took.
func (d *fleetDirectory) SeatHolders(ctx context.Context) ([]notify.Holder, error) {
	n, err := d.answerers(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: count the nodes that hold the directory: %w", err)
	}
	var answers scattered
	if n > 0 {
		if answers, err = d.scatter(ctx, n); err != nil {
			return nil, err
		}
	}
	// THE HEAD IS READ AFTER THE SCATTER, so it covers every position an
	// honest answer could have been read at.
	head, err := d.head(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: read the identity log's state: %w", err)
	}
	best := d.best(ctx, answers, head)

	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case best != nil && best.At < d.floor:
		return nil, fmt.Errorf("%w: the best answer was read at %s and this "+
			"node's reading at %s", errDirectoryBehind,
			statelog.Unpack(topics.IamLogStream, best.At),
			statelog.Unpack(topics.IamLogStream, d.floor))
	case best != nil:
		d.floor = best.At
		out := make([]notify.Holder, 0, len(best.Holders))
		for _, h := range best.Holders {
			out = append(out, notify.Holder{
				Seat: h.Seat, Stage: iam.Stage(h.Stage), Removed: h.Removed,
			})
		}
		return out, nil
	case head == 0 && d.floor == 0:
		// A FACT, not a fallback — see the notes above. Only while this
		// node has never taken a reading from the log: an empty log
		// under a reading taken from a full one is a log that was
		// recreated or purged, and nothing about who holds which seat
		// can be read off that.
		return nil, nil
	}
	return nil, errNoDirectoryAnswer
}

// scattered is one scatter: the nonce it asked under, and the replies as they
// arrived, unopened.
type scattered struct {
	nonce   string
	replies [][]byte
}

// scatter asks up to want answerers under a fresh nonce, and answers what
// came back as it arrived — opened, checked and chosen by [fleetDirectory.best].
func (d *fleetDirectory) scatter(ctx context.Context, want int) (scattered, error) {
	if d.ask == nil {
		return scattered{}, errors.New("engine: no broker to ask the fleet's " +
			"directory through")
	}
	nonce, err := holdersNonce()
	if err != nil {
		return scattered{}, err
	}
	request, err := json.Marshal(holdersRequest{Version: holdersProtocol, Nonce: nonce})
	if err != nil {
		return scattered{}, fmt.Errorf("engine: encode a directory request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, fleetDirectoryBudget)
	defer cancel()
	replies, err := d.ask.Ask(ctx, topics.IamHolders, request, want)
	if err != nil {
		return scattered{}, fmt.Errorf("engine: ask the fleet's directory: %w", err)
	}
	return scattered{nonce: nonce, replies: replies}, nil
}

// best is the answer read at the highest position among those this node can
// TRUST: signed by the fleet, echoing the scatter's nonce, at this protocol's
// version, and at a position the log actually holds. Nil when there is none —
// nobody asked, nobody answered, or nobody answered in a way that can be read.
func (d *fleetDirectory) best(ctx context.Context, answers scattered, head uint64) *holdersReply {
	var best *holdersReply
	for _, raw := range answers.replies {
		reply, ok := d.open(ctx, raw, answers.nonce, head)
		if !ok {
			continue
		}
		if best == nil || reply.At > best.At ||
			(reply.At == best.At && reply.Node < best.Node) {
			// THE MOST-CAUGHT-UP NODE, and the lowest node id between
			// two at one position, so two readings of the same fleet
			// pick the same answer rather than whichever arrived first.
			best = &reply
		}
	}
	return best
}

// open is one answer, if it may be read at all.
//
// A REPLY THIS NODE CANNOT TRUST IS A MISSING ONE, never an error: the rest of
// the fleet's answers are still worth reading, and an attacker who could fail
// the whole scatter by answering garbage would have a way to hold every
// satellite on its last reading for ever.
func (d *fleetDirectory) open(ctx context.Context, raw []byte, nonce string,
	head uint64) (holdersReply, bool) {

	if d.verifier == nil {
		return holdersReply{}, false
	}
	body, verdict := d.verifier.Open(raw)
	switch verdict {
	case statelog.Verified:
	case statelog.KeyUnknown:
		// A KEY THIS NODE DOES NOT HOLD is a rotation in progress, not a
		// forgery: this node cannot read the answer yet, and says so quietly.
		log.DebugContext(ctx, "directory_answer_unverifiable",
			"held_keys", d.verifier.KeyIDs())
		return holdersReply{}, false
	default:
		log.WarnContext(ctx, "directory_answer_refused", "verdict", string(verdict),
			"detail", "an answer on the directory's subject that the fleet's "+
				"keyring does not open — something that is not this fleet is "+
				"serving it")
		return holdersReply{}, false
	}
	var reply holdersReply
	if err := json.Unmarshal(body, &reply); err != nil ||
		reply.Version != holdersProtocol || reply.Nonce != nonce {
		// A reply this build cannot read, or one written for another
		// scatter — a replay.
		return holdersReply{}, false
	}
	if reply.At < 0 || statelog.Unpack(topics.IamLogStream, reply.At).Seq > head {
		log.WarnContext(ctx, "directory_answer_implausible", "node", reply.Node,
			"at", statelog.Unpack(topics.IamLogStream, reply.At).String(),
			"head", head,
			"detail", "a signed answer claims records the identity log does "+
				"not hold, so it is not a reading any node applied")
		return holdersReply{}, false
	}
	return reply, true
}

// holdersNonce is one scatter's nonce.
func holdersNonce() (string, error) {
	var b [holdersNonceBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("engine: mint a directory nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// directoryAnswerers counts the live nodes whose roles run the identity
// domain.
//
// OFF THE PRESENCE LEASES, through the same predicate a node applies to its own
// roles — the retention report's own derivation, so the two can never
// disagree about who runs the domain.
func directoryAnswerers(ctx context.Context, leases coord.Backend) (int, error) {
	if leases == nil {
		return 0, nil
	}
	live, err := leases.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return 0, err
	}
	name := iamdomain.Domain{}.Name()
	n := 0
	for _, lease := range live {
		profile, ok := placement.FromLease(lease)
		if ok && participationOf(profile.Roles).Runs(name) {
			n++
		}
	}
	return n, nil
}
