package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FLEET'S BROKER MEMBERSHIP, as an operator reads and changes it.
//
// Two records say who the broker's members are, and they can disagree. Every
// node ADVERTISES its broker kind on its presence lease ([placement.BrokerKind]),
// and the broker's METADATA GROUP counts its voters for itself. A member that is
// gone for good is the disagreement that matters: its presence lapses with its
// process, and the metadata group goes on counting it in every election and
// every create until it is removed — so a three-member fleet that lost two for
// good has no quorum left to create anything with, and nothing on the presence
// side says why.
//
// # Read through a member, from any node
//
// Only a member holds the metadata group, and its view is read in process
// ([jetstream.Server.MetaGroup]). A node that is not a member — a leaf, or a
// member lent no broker handle — asks a live member over its own core subject,
// `crewlet.fleet.broker.<member>`, one member at a time. The same subject
// carries a removal, because nats-server answers one only on the system
// account, which a member reaches in its own process and nothing else can.
//
// A fleet on an EXTERNAL cluster has no members of its own: the cluster's
// membership is its operator's, and both gestures say so rather than
// answering about a broker this fleet does not run.
//
// # Every request and every reply is signed
//
// A removal on that subject is an INSTRUCTION a member carries out through its
// own system account — the one action the design keeps off the global account
// every node's connections, a leaf's link included, are bound to — and it
// changes the quorum every election and every create runs on. The broker's
// admission vouches for a CONNECTION, never a message: an embedded broker
// admits only a key derived from the keyring (ADR-0064), but a route's
// password crosses the fleet network in clear, and an external cluster admits
// whom its operator chooses. So unsigned, anything the broker admitted could
// remove a voter, past the grant, the step-up, the confirmation and the
// live-member refusal the operator's route puts in front of the gesture — all
// of which run on the ASKING node. So the subject is a channel of
// internal/signedask's, in domains of its own
// ([fleetBrokerChannel]): a member carries out only a request the fleet
// signed, bound to an id it has not taken and a deadline still ahead
// ([signedask.Channel.Admit]), and the asker believes only a reply the fleet
// signed for the request it sent. What arrives otherwise runs nothing:
// a request the fleet did not sign is refused and logged at error; one under
// a key this member does not hold is answered "did not run", so the asker
// asks the next member, as it does a member with no metadata group; and the
// reads go through the same channel, because a forged listing is what an
// operator removes a voter on.
//
// WHO ASKED travels inside the signature as the author, kind and credential
// [iam.ActorFor] makes of the operator's principal, so the carrying member's
// `fleet_broker_member_removed` line names the credential the removal was
// pressed through, and nothing on the broker can choose it.
//
// # A voter is its raft peer id
//
// The metadata group counts a voter by its peer id, which nats-server derives
// from the server's name ([jetstream.PeerIDOf]); the NAME is only what the
// answering member happens to have heard. A member lost for good whose
// survivors have since restarted is one none of them has heard, so the group
// lists it nameless. Both records are therefore held against each other by
// PEER ID — a live node's id is computed from its node id, which also names a
// nameless voter whenever its node is alive under another kind — and a removal
// names the voter by id: the operator's node id, hashed, or the id the listing
// shows for a voter nobody can name.

// fleetBrokerSubjectPrefix prefixes every member's broker-membership subject.
const fleetBrokerSubjectPrefix = "crewlet.fleet.broker"

// fleetBrokerChannel is the broker-membership channel, and so the stem of the
// two key domains its envelopes are signed under — its own, so no estate
// envelope, steer note or state-log record verifies as one.
const fleetBrokerChannel = "fleet.broker"

// brokerWire is the broker-membership channel's envelopes: one per node, both
// halves, since a member both asks and answers.
type brokerWire = signedask.Channel[brokerRequest, brokerReply]

// newBrokerWire builds this node's broker-membership envelopes over the fleet
// keyring every state-log record is signed under. The keyring is required on
// every node, so this fails only on a bootstrap [config.Bootstrap.Validate]
// would already have refused.
func newBrokerWire(ring statelog.Keyring) (*brokerWire, error) {
	wire, err := signedask.New[brokerRequest, brokerReply](fleetBrokerChannel, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the fleet broker's envelopes: %w", err)
	}
	return wire, nil
}

// errNoBrokerWire is a broker-membership gesture on a node built without its
// envelopes: it can neither ask a member nor answer one.
var errNoBrokerWire = errors.New("engine: no envelopes for the fleet broker's " +
	"membership — every request and reply on its subjects is signed under the fleet " +
	"keyring (Tier A secrets.keys)")

// fleetBrokerSubject is the one subject a member answers the fleet's broker
// questions on. THE NODE ID IS ESCAPED as a subject token, for the estate
// subject's reason: an id may carry a dot, which would split it in two.
func fleetBrokerSubject(node string) string {
	return fleetBrokerSubjectPrefix + "." + coord.DocumentKey(node)
}

// brokerReadWait bounds a read of the metadata group through the members —
// every member asked at once, the first to report the group answering.
//
// FIVE SECONDS, and what fixes it is who waits on the listing: the command
// line's call and the dashboard's socket query each give a node ten seconds
// for the whole answer ([BrokerReadWait] is held under both), and the node
// also lists the fleet's presence before it asks anybody. Half of that wait
// leaves the other half for the presence read and the answer's way back. A
// live member answers out of its own memory in one round trip — a leaf link
// included — so one silent for five seconds is gone rather than slow, and
// asking every member at once means a silent one costs the listing nothing
// while another answers.
const brokerReadWait = 5 * time.Second

// BrokerReadWait is [brokerReadWait], for the gates that hold the callers'
// waits above it.
func BrokerReadWait() time.Duration { return brokerReadWait }

// BrokerRemoveWait bounds a whole removal: the carrying member waits for the
// metadata group to commit it within the budget one clustered metadata change
// gets ([jsprovision.Budget]), and its answer takes a fleet round trip back —
// bounded as a read of the group is ([brokerReadWait]). ONE DEADLINE FOR THE
// GESTURE, never one per member asked: a member that cannot carry it says so
// at once and the next is asked inside what is left, and a member that does
// not answer ends the gesture ([ErrRemovalOutcomeUnknown]). Exported for the
// surfaces that wait on the gesture, which have to wait longer still.
func BrokerRemoveWait() time.Duration { return jsprovision.Budget(true) + brokerReadWait }

// brokerMembership is what the engine asks of its own embedded broker.
// Declared here, by the consumer; a leaf's and a solo member's answer
// [jetstream.ErrNoMetaGroup], and name no client listener.
type brokerMembership interface {
	MetaGroup() (jetstream.MetaGroup, error)
	RemovePeer(ctx context.Context, peer string) error
	// ClientListener is the member's loopback client listener, where an
	// operator's nats tool reaches its broker with the operator key
	// ([jetstream.Server.ClientListener]), and "" where it opens none.
	ClientListener() string
}

// BrokerFindingKind is one way the two records of the broker's membership
// disagree.
type BrokerFindingKind string

const (
	// BrokerDeadMember is a voter of the metadata group that no live node
	// advertises as a member: its process is gone, or it came back as
	// something else. It is counted in every election until it is removed.
	BrokerDeadMember BrokerFindingKind = "dead_member"

	// BrokerNotInGroup is a live node advertising a member that the
	// metadata group does not list: still joining, or removed while it ran,
	// in which case it rejoins as a voter at its next restart.
	BrokerNotInGroup BrokerFindingKind = "not_in_group"

	// BrokerUnknownKind is a live node whose presence advertises no kind
	// this build knows — a kind a newer build added, or a profile nobody
	// derived a kind for. It is counted as a member wherever that is the
	// safe reading.
	BrokerUnknownKind BrokerFindingKind = "unknown_kind"
)

// brokerFindingKinds is every kind, in the order a surface lists them.
var brokerFindingKinds = []BrokerFindingKind{BrokerDeadMember, BrokerNotInGroup, BrokerUnknownKind}

// BrokerFindingKinds is every kind — a fresh slice per call, so the
// dashboard's copy can be held against it.
func BrokerFindingKinds() []BrokerFindingKind { return slices.Clone(brokerFindingKinds) }

// Valid reports whether a kind off the wire is one this build knows.
func (k BrokerFindingKind) Valid() bool { return slices.Contains(brokerFindingKinds, k) }

// BrokerFinding is one disagreement, about one node.
type BrokerFinding struct {
	Kind BrokerFindingKind `json:"kind"`
	// Node is the node id, and empty for a voter no live node is and whose
	// name the member that answered has never heard.
	Node string `json:"node"`
	// Peer is the voter's raft peer id, on a finding about a voter: the
	// name a removal of one nobody can name goes by.
	Peer   string `json:"peer,omitempty"`
	Detail string `json:"detail"`
}

// BrokerNode is one live node's broker, as its presence advertises it.
type BrokerNode struct {
	Node string `json:"node"`
	// Peer is the raft peer id the metadata group counts this node by when
	// it is a voter ([jetstream.PeerIDOf] of its node id) — what a client
	// matches a voter to its node by, since the voter's name is only what
	// the answering member has heard. Carried rather than left to each
	// client to derive, because the derivation is nats-server's and a
	// browser has no synchronous hash to repeat it with.
	Peer string `json:"peer"`
	// Kind is the advertised kind as an operator reads it — "unknown" for
	// a presence that does not say.
	Kind  string   `json:"kind"`
	Roles []string `json:"roles"`
}

// BrokerView is the fleet's broker membership as one node answers it.
type BrokerView struct {
	// Node is the node that answered, and Kind its own broker's kind.
	Node string `json:"node"`
	Kind string `json:"kind"`

	// Client is the ANSWERING node's own loopback client listener,
	// host:port — where an operator on that node's host reaches its broker
	// with the nats tool and the operator key `crewlet fleet broker
	// credential` writes (ADR-0064) — and absent where it opens none: a solo
	// member, a member with only a leaf listener, a leaf, an external
	// cluster's client. The node's, as Node and Kind are, never the fleet's:
	// a client listener is reachable from its own host alone.
	Client string `json:"client,omitempty"`

	// External is a fleet whose broker is an external cluster: its
	// membership is that cluster's operator's, and nothing below lists it.
	External bool `json:"external"`

	// Nodes is every live node and the kind it advertises, by id.
	Nodes []BrokerNode `json:"nodes"`

	// Group is the metadata group as the member GroupFrom reports it, and
	// absent when no member could be asked or none answered — GroupError
	// then says why. An unread group is never an empty one.
	Group      *jetstream.MetaGroup `json:"group,omitempty"`
	GroupFrom  string               `json:"group_from,omitempty"`
	GroupError string               `json:"group_error,omitempty"`

	// Findings are the disagreements between the two records, and between
	// the presence rows and this build. Empty when there are none.
	Findings []BrokerFinding `json:"findings"`
}

// BrokerRemoval is an operator's removal of a voter from the metadata group,
// named by EXACTLY ONE of Node and Peer.
type BrokerRemoval struct {
	// Node is the member to remove, by node id — the server name, whose
	// peer id the group counts it by ([jetstream.PeerIDOf]).
	Node string
	// Peer is the voter to remove, by the raft peer id the listing shows —
	// for a voter whose name no member has heard, which has no node id to
	// give.
	Peer string
	// Force removes it although its node holds a live presence lease as a
	// member.
	Force bool
	// By is who asked, as [iam.ActorFor] writes the operator's principal —
	// the author, the kind and the credential — for the carrying member's
	// log. It travels inside the signed request, so the member names the
	// credential the asking node resolved and nothing a sender chose.
	By iam.Actor
}

// target is the voter a removal names, by peer id, and its node id where the
// removal gave one.
func (r BrokerRemoval) target() (peer, node string, err error) {
	switch {
	case r.Node != "" && r.Peer != "":
		return "", "", errors.New("engine: name the voter to remove by its node id " +
			"or by its peer id, not both")
	case r.Node != "":
		if !config.ValidNodeID(r.Node) {
			return "", "", fmt.Errorf("engine: %q is not a node id", r.Node)
		}
		return jetstream.PeerIDOf(r.Node), r.Node, nil
	case r.Peer != "":
		if !jetstream.ValidPeerID(r.Peer) {
			return "", "", fmt.Errorf("engine: %q is not a raft peer id", r.Peer)
		}
		return r.Peer, "", nil
	}
	return "", "", errors.New("engine: name the voter to remove, by its node id or " +
		"by the peer id the listing shows")
}

// BrokerRemoved is a removal the metadata group committed.
type BrokerRemoved struct {
	// Node is the removed voter's node id, empty where the removal named
	// only its peer id and no live node is it.
	Node string `json:"node"`
	// Peer is the removed voter's raft peer id.
	Peer string `json:"peer"`
	// By is the member whose system account carried it.
	By string `json:"by"`
	// Group is the metadata group as that member reads it after the
	// commit, absent if it could not be read.
	Group *jetstream.MetaGroup `json:"group,omitempty"`
}

// ErrExternalBroker is a gesture on a fleet whose broker is an external
// cluster, whose membership this fleet does not run.
var ErrExternalBroker = errors.New("engine: this fleet's broker is an external " +
	"NATS cluster (stream.type: nats), whose membership belongs to whoever runs " +
	"it — change it with that cluster's own tools")

// ErrNoBrokerMember is a gesture with no live member to carry it: every member
// is gone, every one that answered could not carry it, or the one named is the
// only one.
var ErrNoBrokerMember = errors.New("engine: no live member of the fleet's broker " +
	"could carry it")

// ErrRemovalOutcomeUnknown is a removal handed to a member that did not answer
// within the gesture's wait. It may have proposed the change before it went
// silent, and the group commits a proposal whoever is listening — so whether
// the voter is gone is unknown, and the metadata group is what says.
var ErrRemovalOutcomeUnknown = errors.New("engine: the member carrying the " +
	"removal did not answer, so whether the metadata group committed it is unknown")

// BrokerMemberLive is a removal refused because the named voter's node holds a
// live presence lease AS A MEMBER (or does not say what its broker is): it is
// running, and a running member removed from the group rejoins as a voter the
// next time it restarts. A node that is alive as a leaf or a client under the
// voter's name is not refused — its broker runs no JetStream and never rejoins.
type BrokerMemberLive struct {
	Node string
	Kind placement.BrokerKind
}

func (e *BrokerMemberLive) Error() string {
	return fmt.Sprintf("engine: %s holds a live presence lease and its broker is %s, "+
		"so it is still a running member: stop it first — or remove it with force, "+
		"knowing a running member removed from the metadata group rejoins it as a "+
		"voter at its next restart", e.Node, e.Kind)
}

// FleetBroker is the fleet's broker membership, for the operator surfaces.
type FleetBroker struct{ e *Engine }

// FleetBroker is this engine's broker-membership surface. Every node has one:
// any node answers the read, asking a member where it is not one.
func (e *Engine) FleetBroker() *FleetBroker { return &FleetBroker{e: e} }

// brokerRequest is one question to a member, as it crosses the wire — signed
// whole, so every field is one the fleet wrote. A removal names the voter by
// Peer; Node is the operator's name for it, for the log, and empty where the
// removal gave none.
type brokerRequest struct {
	// ID and Deadline bind the request ([signedask.Request]): minted per
	// ask, so one gesture asked of two members is two requests.
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`

	Op   string `json:"op"`
	Peer string `json:"peer,omitempty"`
	Node string `json:"node,omitempty"`

	// By, ByKind and OperatorID are who asked, as [BrokerRemoval.By] — the
	// author, the kind and the credential — and empty on a read.
	By         string        `json:"by,omitempty"`
	ByKind     iam.ActorKind `json:"by_kind,omitempty"`
	OperatorID string        `json:"operator_id,omitempty"`

	// From is the asking node, for the member's log.
	From string `json:"from,omitempty"`
}

// Binding is the request's id and deadline ([signedask.Request]).
func (r brokerRequest) Binding() (string, time.Time) { return r.ID, r.Deadline }

const (
	brokerOpGroup  = "group"
	brokerOpRemove = "remove"
)

// brokerReply is a member's answer — signed whole, like the request. Code
// names a refusal a caller branches on.
type brokerReply struct {
	Node string `json:"node"`
	// Request is the id of the request this answers ([signedask.Reply]),
	// empty only where the request could not be read at all.
	Request string               `json:"request,omitempty"`
	Group   *jetstream.MetaGroup `json:"group,omitempty"`
	Code    string               `json:"code,omitempty"`
	Detail  string               `json:"detail,omitempty"`
}

// Answers is the id of the request this reply answers ([signedask.Reply]).
func (r brokerReply) Answers() string { return r.Request }

const (
	brokerCodeNoGroup   = "no_meta_group"
	brokerCodeNotPeer   = "not_a_peer"
	brokerCodeChanging  = "membership_changing"
	brokerCodeNoLeader  = "no_leader"
	brokerCodeSelf      = "removing_self"
	brokerCodeFailed    = "failed"
	brokerCodeUnknownOp = "unknown_op"

	// brokerCodeUnauthenticated is a request the member did not run because
	// it could not verify it — signed under a key it does not hold, or not
	// by the fleet at all. Nothing ran, so the asker asks the next member.
	brokerCodeUnauthenticated = "unauthenticated"
	// brokerCodeStale is a request the member did not run because it was
	// not one to take now: past its deadline, unbound, or already taken.
	brokerCodeStale = "stale"
)

// errUnverifiedRequest is a member's answer that it ran nothing because it
// could not verify what it was sent ([brokerCodeUnauthenticated]) or was not to
// take it now ([brokerCodeStale]): nothing ran, so another member may carry it.
var errUnverifiedRequest = errors.New("engine: the member ran nothing it was sent")

// errorOf turns a member's refusal back into the sentinel it answered from,
// so a caller's errors.Is holds across the wire.
func (r brokerReply) errorOf() error {
	switch r.Code {
	case "":
		return nil
	case brokerCodeUnauthenticated, brokerCodeStale:
		return fmt.Errorf("%w (%s)", errUnverifiedRequest, r.Detail)
	case brokerCodeNoGroup:
		return fmt.Errorf("%w (%s)", jetstream.ErrNoMetaGroup, r.Detail)
	case brokerCodeNotPeer:
		return fmt.Errorf("%w (%s)", jetstream.ErrNotAMetaPeer, r.Detail)
	case brokerCodeChanging:
		return fmt.Errorf("%w (%s)", jetstream.ErrMembershipChanging, r.Detail)
	case brokerCodeNoLeader:
		return fmt.Errorf("%w (%s)", jetstream.ErrNoMetaLeader, r.Detail)
	case brokerCodeSelf:
		return fmt.Errorf("%w (%s)", jetstream.ErrRemovingSelf, r.Detail)
	}
	return fmt.Errorf("engine: %s refused: %s", r.Node, r.Detail)
}

// replyFor answers one member-side error with the code the asker branches on.
func replyFor(node string, err error) brokerReply {
	out := brokerReply{Node: node, Detail: err.Error()}
	switch {
	case errors.Is(err, jetstream.ErrNoMetaGroup):
		out.Code = brokerCodeNoGroup
	case errors.Is(err, jetstream.ErrNotAMetaPeer):
		out.Code = brokerCodeNotPeer
	case errors.Is(err, jetstream.ErrMembershipChanging):
		out.Code = brokerCodeChanging
	case errors.Is(err, jetstream.ErrNoMetaLeader):
		out.Code = brokerCodeNoLeader
	case errors.Is(err, jetstream.ErrRemovingSelf):
		out.Code = brokerCodeSelf
	default:
		out.Code = brokerCodeFailed
	}
	return out
}

// serveFleetBroker makes this member answer the fleet's broker questions on its
// own subject. Only a MEMBER serves — a leaf and a client hold no metadata
// group — and every member does, so a node that is not one always has somebody
// to ask.
//
// A MEMBER WITHOUT ITS ENVELOPES IS REFUSED rather than serving unsigned: the
// subject carries an instruction, and a member that answered it unsigned is
// the hole the envelopes close.
func (e *Engine) serveFleetBroker(ctx context.Context) error {
	if e.profile.Broker != placement.BrokerMember || e.backends == nil ||
		e.backends.Queue == nil || e.backends.broker == nil {
		return nil
	}
	if e.brokerWire == nil {
		return errNoBrokerWire
	}
	// THE IDS THIS MEMBER HAS TAKEN, for as long as it serves.
	taken := signedask.NewGuard()
	stop, err := e.backends.Queue.Serve(ctx, fleetBrokerSubject(e.id), queue.ConcurrentAnswers,
		func(ctx context.Context, raw []byte) ([]byte, error) {
			return e.brokerWire.SealReply(e.answerBroker(ctx, raw, taken, time.Now()))
		})
	if err != nil {
		return fmt.Errorf("engine: serve the fleet's broker membership: %w", err)
	}
	e.stopFleetBroker = stop
	return nil
}

// stopServingFleetBroker withdraws this member's subject. Nil-safe.
func (e *Engine) stopServingFleetBroker(ctx context.Context) {
	if e.stopFleetBroker == nil {
		return
	}
	if err := e.stopFleetBroker(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "fleet_broker_not_withdrawn", "error", err.Error())
	}
	e.stopFleetBroker = nil
}

// answerBroker runs one request and ALWAYS answers, for the estate server's
// reason: a member that stayed silent would be indistinguishable from one that
// is down, and the asker would wait out its budget before asking the next.
//
// IT RUNS ONLY WHAT IT ADMITS ([signedask.Channel.Admit]), judged on now and
// against the ids taken: every other request is answered with what it is and
// runs nothing — and a removal above all, which is an instruction to this
// member's system account.
func (e *Engine) answerBroker(ctx context.Context, raw []byte, taken *signedask.Guard,
	now time.Time) brokerReply {

	admitted := e.brokerWire.Admit(raw, taken, now)
	switch admitted.Disposition {
	case signedask.Tampered:
		// NOT THE FLEET'S: nothing in it is believed, not even the id a
		// reply would name. At ERROR — no member will ever take it, so
		// whoever sent it reached the broker and is not the fleet.
		log.ErrorContext(ctx, "fleet_broker_request_refused", "node", e.id,
			"disposition", string(admitted.Disposition), "detail", admitted.Why+"; nothing in it ran")
		return brokerReply{Node: e.id, Code: brokerCodeUnauthenticated,
			Detail: fmt.Sprintf("%s refused a request that is not signed by this fleet", e.id)}
	case signedask.KeyUnknown:
		// A FACT ABOUT THIS MEMBER, which an operator changes by adding the
		// key: "did not run", bound to the id the asker sent, so it asks
		// the next member.
		log.WarnContext(ctx, "fleet_broker_request_refused", "node", e.id,
			"disposition", string(admitted.Disposition), "claimed_from", admitted.Request.From,
			"detail", admitted.Why+"; nothing in it ran")
		return brokerReply{Node: e.id, Request: admitted.ID, Code: brokerCodeUnauthenticated,
			Detail: fmt.Sprintf("%s does not hold the key the request is signed under "+
				"(it holds %v)", e.id, e.brokerWire.HeldKeys())}
	case signedask.Undecodable:
		return brokerReply{Node: e.id, Code: brokerCodeFailed,
			Detail: fmt.Sprintf("%s could not decode a request the fleet signed: %s", e.id, admitted.Why)}
	case signedask.Stale:
		log.WarnContext(ctx, "fleet_broker_request_refused", "node", e.id,
			"disposition", string(admitted.Disposition), "op", admitted.Request.Op,
			"from", admitted.Request.From, "detail", admitted.Why)
		return brokerReply{Node: e.id, Request: admitted.ID, Code: brokerCodeStale,
			Detail: fmt.Sprintf("%s: %s", e.id, admitted.Why)}
	}
	req := admitted.Request
	out := e.runBroker(ctx, req)
	out.Request = req.ID
	return out
}

// runBroker runs one admitted request.
func (e *Engine) runBroker(ctx context.Context, req brokerRequest) brokerReply {
	switch req.Op {
	case brokerOpGroup:
		group, err := e.backends.broker.MetaGroup()
		if err != nil {
			return replyFor(e.id, err)
		}
		return brokerReply{Node: e.id, Group: &group}
	case brokerOpRemove:
		return e.removeHere(ctx, req.Peer, req.Node,
			iam.Actor{Name: req.By, Kind: req.ByKind, OperatorID: req.OperatorID}, req.From)
	}
	// A NEWER PEER'S QUESTION, which this build cannot answer: said so,
	// so the asker moves on rather than waiting.
	return brokerReply{Node: e.id, Code: brokerCodeUnknownOp,
		Detail: fmt.Sprintf("%s does not know the operation %q", e.id, req.Op)}
}

// removeHere removes a voter through THIS member's system account. node is
// the operator's name for it, and by who asked — the author, the kind and the
// credential — each for the log alone.
//
// ON ITS OWN BUDGET, detached from the request that asked: once the leader has
// proposed the change the group commits it whoever is listening, so an asker
// that gave up changes nothing but who hears the answer.
func (e *Engine) removeHere(ctx context.Context, peer, node string, by iam.Actor, from string) brokerReply {
	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jsprovision.Budget(true))
	defer cancel()
	if err := e.backends.broker.RemovePeer(removeCtx, peer); err != nil {
		log.WarnContext(ctx, "fleet_broker_remove_refused", "peer", peer, "node", node,
			"by", by.Name, "by_kind", string(by.Kind), "operator_id", by.OperatorID,
			"asked_by", from, "error", err.Error())
		return replyFor(e.id, err)
	}
	log.WarnContext(ctx, "fleet_broker_member_removed", "peer", peer, "node", node,
		"by", by.Name, "by_kind", string(by.Kind), "operator_id", by.OperatorID,
		"asked_by", from, "detail", "the metadata group no longer counts "+
			"this voter in its elections or its creates; a member that restarts "+
			"under its name joins it again as a new voter")
	out := brokerReply{Node: e.id}
	if group, err := e.backends.broker.MetaGroup(); err == nil {
		out.Group = &group
	}
	return out
}

// livePresence is every live node's profile, by id — listed from the store now,
// because both gestures decide something from it: who to ask, and whether the
// node an operator named is still running.
func (f *FleetBroker) livePresence(ctx context.Context) (map[string]placement.NodeProfile, error) {
	out := map[string]placement.NodeProfile{}
	if f.e.backends == nil || f.e.backends.Coord == nil {
		return out, nil
	}
	held, err := f.e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, fmt.Errorf("engine: list the live nodes: %w", err)
	}
	for _, lease := range held {
		if profile, ok := placement.FromLease(lease); ok {
			out[profile.ID] = profile
		}
	}
	// THIS NODE AS IT IS, whether or not its presence has landed yet and
	// whatever an earlier incarnation's row says — the rule
	// [placement.Compute] follows for the same reason.
	out[f.e.id] = f.e.profile
	return out, nil
}

// askOrder is who a gesture asks, in order: this node first when it is a
// member it can reach in process, then every other node advertising a member,
// by id — leaving out the node whose peer id is skip, which is never asked to
// carry its own removal ([jetstream.ErrRemovingSelf]).
func (f *FleetBroker) askOrder(live map[string]placement.NodeProfile, skip string) (local bool, remote []string) {
	local = f.e.profile.Broker == placement.BrokerMember && f.e.backends != nil &&
		f.e.backends.broker != nil && jetstream.PeerIDOf(f.e.id) != skip
	for id, profile := range live {
		if id == f.e.id || jetstream.PeerIDOf(id) == skip || profile.Broker != placement.BrokerMember {
			continue
		}
		remote = append(remote, id)
	}
	slices.Sort(remote)
	return local, remote
}

// asked is how one member's ask ended.
type asked int

const (
	// askNotSent is a request that never left this node: nobody can have
	// acted on it.
	askNotSent asked = iota
	// askSilent is a request sent that nobody answered before the wait ran
	// out: the member may have acted on it.
	askSilent
	// askAnswered is a member's answer, readable or not.
	askAnswered
)

// ask puts one request to one member and reads its answer, within ctx —
// signed, bound to an id of its own and to ctx's deadline, and believed only
// where the reply is the fleet's answer to it.
//
// A REPLY THAT IS NOT BELIEVED IS NO ANSWER, and it is reported as answered
// with an error: the member heard the request and may have acted on it, so
// what it did is not known — which is what a removal reports for it.
func (f *FleetBroker) ask(ctx context.Context, member string, req brokerRequest) (brokerReply, asked, error) {
	wire := f.e.brokerWire
	if wire == nil {
		return brokerReply{}, askNotSent, errNoBrokerWire
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return brokerReply{}, askNotSent, errors.New("engine: a question to a member is " +
			"asked within a deadline, which is how long the member remembers it")
	}
	req.ID, req.Deadline = signedask.NewID(), deadline
	body, err := wire.SealRequest(req)
	if err != nil {
		return brokerReply{}, askNotSent, err
	}
	replies, err := f.e.backends.Queue.Ask(ctx, fleetBrokerSubject(member), body, 1)
	if err != nil {
		return brokerReply{}, askNotSent, fmt.Errorf("engine: ask %s: %w", member, err)
	}
	if len(replies) == 0 {
		return brokerReply{}, askSilent, nil
	}
	reply, why, err := wire.OpenReply(replies[0], req.ID)
	switch {
	case err != nil:
		return brokerReply{}, askAnswered, fmt.Errorf("engine: %s answered with "+
			"something unreadable: %w", member, err)
	case why != "":
		return brokerReply{}, askAnswered, fmt.Errorf("engine: %s answered, and %s", member, why)
	}
	return reply, askAnswered, nil
}

// List is the fleet's broker membership: what every live node advertises, the
// metadata group as a member reports it, and where the two disagree.
func (f *FleetBroker) List(ctx context.Context) (BrokerView, error) {
	view := BrokerView{Node: f.e.id, Kind: f.e.profile.Broker.String(), Findings: []BrokerFinding{}}
	if broker := f.e.backends.broker; broker != nil {
		view.Client = broker.ClientListener()
	}
	live, err := f.livePresence(ctx)
	if err != nil {
		return BrokerView{}, err
	}
	for _, id := range sortedProfileIDs(live) {
		profile := live[id]
		view.Nodes = append(view.Nodes, BrokerNode{
			Node: id, Peer: jetstream.PeerIDOf(id), Kind: profile.Broker.String(),
			Roles: profile.Roles.Names(),
		})
	}
	if f.e.profile.Broker == placement.BrokerClient {
		view.External = true
		return view, nil
	}
	view.Group, view.GroupFrom, view.GroupError = f.readGroup(ctx, live)
	view.Findings = brokerFindings(live, view.Group)
	return view, nil
}

// readGroup reads the metadata group from this member in process, or else
// from whichever other member reports it first.
//
// EVERY OTHER MEMBER AT ONCE, within one [brokerReadWait]: asked one after
// another, a member that died with its presence lease still live would spend
// the whole wait of everybody reading the listing before the next was asked.
// The first report of the group answers; the reasons the others gave, or their
// silence, are what the listing says when nobody reports one.
func (f *FleetBroker) readGroup(ctx context.Context, live map[string]placement.NodeProfile) (
	*jetstream.MetaGroup, string, string) {

	local, remote := f.askOrder(live, "")
	var why []string
	if local {
		group, err := f.e.backends.broker.MetaGroup()
		if err == nil {
			return &group, f.e.id, ""
		}
		why = append(why, fmt.Sprintf("%s: %v", f.e.id, err))
	}
	if len(remote) == 0 {
		if len(why) == 0 {
			return nil, "", "no live node advertises a member of the fleet's broker, so " +
				"there is nobody to read the metadata group from"
		}
		return nil, "", "no member reported the metadata group: " + joinReasons(why)
	}
	readCtx, cancel := context.WithTimeout(ctx, brokerReadWait)
	defer cancel()
	type answer struct {
		member string
		reply  brokerReply
		how    asked
		err    error
	}
	answers := make(chan answer, len(remote))
	for _, member := range remote {
		go func() {
			reply, how, err := f.ask(readCtx, member,
				brokerRequest{Op: brokerOpGroup, From: f.e.id})
			answers <- answer{member: member, reply: reply, how: how, err: err}
		}()
	}
	for range remote {
		a := <-answers
		switch {
		case a.err != nil:
			why = append(why, a.err.Error())
		case a.how == askSilent:
			why = append(why, a.member+" did not answer")
		case a.reply.Group != nil:
			// THE REST ARE ABANDONED, not awaited: the deferred cancel
			// ends their asks, and the channel holds every answer, so
			// none of them blocks on a reader that has gone.
			return a.reply.Group, a.reply.Node, ""
		default:
			why = append(why, fmt.Sprintf("%s: %v", a.member, a.reply.errorOf()))
		}
	}
	slices.Sort(why)
	return nil, "", "no member reported the metadata group: " + joinReasons(why)
}

// brokerFindings holds what the nodes advertise against what the metadata
// group counts. A group that could not be read yields only the findings the
// presence rows carry on their own.
//
// BY PEER ID, both ways: a voter is the node whose id hashes to it, whatever
// name — or none — the member that answered has for it. So a voter nobody can
// name is still recognised as the live node it is, and a live member the group
// counts under an unknown name is not reported uncounted.
func brokerFindings(live map[string]placement.NodeProfile, group *jetstream.MetaGroup) []BrokerFinding {
	out := []BrokerFinding{}
	nodeOf := map[string]string{}
	for id := range live {
		nodeOf[jetstream.PeerIDOf(id)] = id
	}
	if group != nil {
		for _, peer := range group.Peers {
			id, running := nodeOf[peer.Peer]
			profile := live[id]
			switch {
			case !running && peer.Name == "":
				out = append(out, BrokerFinding{Kind: BrokerDeadMember, Peer: peer.Peer,
					Detail: "the metadata group counts it as a voter, no live node is " +
						"it, and the member that answered has not heard its name since " +
						"it started: its process is gone, and every election and create " +
						"goes on counting it until it comes back or is removed by its " +
						"peer id"})
			case !running:
				out = append(out, BrokerFinding{Kind: BrokerDeadMember, Node: peer.Name,
					Peer: peer.Peer,
					Detail: "the metadata group counts it as a voter and no live node " +
						"is it: its process is gone, and every election and create " +
						"goes on counting it until it comes back or is removed"})
			case profile.Broker == placement.BrokerLeaf || profile.Broker == placement.BrokerClient:
				out = append(out, BrokerFinding{Kind: BrokerDeadMember, Node: id,
					Peer: peer.Peer,
					Detail: fmt.Sprintf("the metadata group counts it as a voter, and "+
						"its node now runs as a %s: the member it was is gone and is "+
						"counted in every election until it is removed", profile.Broker)})
			}
		}
	}
	for _, id := range sortedProfileIDs(live) {
		profile := live[id]
		switch {
		case profile.Broker == placement.BrokerUnknown:
			out = append(out, BrokerFinding{Kind: BrokerUnknownKind, Node: id,
				Detail: "its presence advertises no broker kind this build knows — " +
					"a kind a newer build added — so it is counted as a member wherever " +
					"that is the safe reading, a capacity seal included"})
		case group != nil && profile.Broker == placement.BrokerMember &&
			!group.Counts(jetstream.PeerIDOf(id)):
			out = append(out, BrokerFinding{Kind: BrokerNotInGroup, Node: id,
				Detail: "it advertises a member and the metadata group does not " +
					"count it: it is still joining, or it was removed while it ran " +
					"and joins again as a voter at its next restart"})
		}
	}
	return out
}

// Remove removes a voter from the metadata group, through a live member's
// system account.
//
// REFUSED WHILE THE VOTER'S NODE HOLDS A LIVE PRESENCE LEASE AS A MEMBER, or
// without saying what its broker is, unless forced: a running member removed
// from the group rejoins it as a voter at its next restart, so the gesture is
// for a member that is gone for good, and the lease is the fleet's own evidence
// that this one is not. A node alive under the voter's name as a LEAF or a
// CLIENT is the member it was gone for good — its broker runs no JetStream and
// never rejoins — so it is removed without force.
//
// THE VOTER NEVER CARRIES ITS OWN REMOVAL ([jetstream.ErrRemovingSelf]). With
// no other member to carry it the gesture is refused, naming why.
//
// ONE DEADLINE, [BrokerRemoveWait], FOR THE WHOLE GESTURE: members are asked
// one after another, in [FleetBroker.askOrder]; one that cannot carry it
// answers at once and the next is asked inside what is left, and one that does
// not answer ENDS the gesture — it may have proposed the change before it went
// silent, and a second proposal through another member would answer as a
// membership change in flight at best. What the operator is told then is that
// the outcome is unknown ([ErrRemovalOutcomeUnknown]).
func (f *FleetBroker) Remove(ctx context.Context, req BrokerRemoval) (BrokerRemoved, error) {
	peer, node, err := req.target()
	if err != nil {
		return BrokerRemoved{}, err
	}
	if f.e.profile.Broker == placement.BrokerClient {
		return BrokerRemoved{}, ErrExternalBroker
	}
	ctx, cancel := context.WithTimeout(ctx, BrokerRemoveWait())
	defer cancel()
	live, err := f.livePresence(ctx)
	if err != nil {
		return BrokerRemoved{}, err
	}
	for id, profile := range live {
		if jetstream.PeerIDOf(id) != peer {
			continue
		}
		node = id
		switch profile.Broker {
		case placement.BrokerMember, placement.BrokerUnknown:
			if !req.Force {
				return BrokerRemoved{}, &BrokerMemberLive{Node: id, Kind: profile.Broker}
			}
		}
	}
	done := func(reply brokerReply) BrokerRemoved {
		return BrokerRemoved{Node: node, Peer: peer, By: reply.Node, Group: reply.Group}
	}
	// cannotCarry is an answer that says nothing about the group: this member
	// holds no metadata group to carry it through, does not know the
	// operation, or ran nothing it was sent because it could not verify it.
	// The next member is asked.
	cannotCarry := func(reply brokerReply, err error) bool {
		return errors.Is(err, jetstream.ErrNoMetaGroup) || errors.Is(err, errUnverifiedRequest) ||
			reply.Code == brokerCodeUnknownOp
	}

	local, remote := f.askOrder(live, peer)
	var why []string
	if local {
		reply := f.e.removeHere(ctx, peer, node, req.By, f.e.id)
		err := reply.errorOf()
		switch {
		case err == nil:
			return done(reply), nil
		case !cannotCarry(reply, err):
			return BrokerRemoved{}, err
		}
		why = append(why, fmt.Sprintf("%s: %v", f.e.id, err))
	}
	for _, member := range remote {
		reply, how, err := f.ask(ctx, member, brokerRequest{
			Op: brokerOpRemove, Peer: peer, Node: node, From: f.e.id,
			By: req.By.Name, ByKind: req.By.Kind, OperatorID: req.By.OperatorID,
		})
		switch {
		case how == askNotSent:
			why = append(why, err.Error())
			continue
		case how == askSilent:
			return BrokerRemoved{}, fmt.Errorf("%w: %s was asked to carry the removal "+
				"of %s and did not answer within %s — read the group "+
				"(`crewlet fleet broker list`) before asking again",
				ErrRemovalOutcomeUnknown, member, voterName(peer, node), BrokerRemoveWait())
		case err != nil:
			// AN ANSWER NOTHING HERE CAN READ OR BELIEVE from a member
			// that heard the request: what it did is not known either.
			return BrokerRemoved{}, fmt.Errorf("%w: %w", ErrRemovalOutcomeUnknown, err)
		}
		if err := reply.errorOf(); err != nil {
			if cannotCarry(reply, err) {
				why = append(why, err.Error())
				continue
			}
			return BrokerRemoved{}, err
		}
		return done(reply), nil
	}
	if len(why) > 0 {
		return BrokerRemoved{}, fmt.Errorf("%w: %s", ErrNoBrokerMember, joinReasons(why))
	}
	if profile, running := live[node]; running && profile.Broker == placement.BrokerMember {
		return BrokerRemoved{}, fmt.Errorf("%w: the only live member is %s, the one "+
			"to be removed, and a member cannot carry its own removal — it could not "+
			"see the group drop it, and loses JetStream the moment the change "+
			"commits. Start another member to carry it, or stop %s and remove it "+
			"once another is running", ErrNoBrokerMember, node, node)
	}
	return BrokerRemoved{}, fmt.Errorf("%w: no live node advertises a member, so "+
		"there is nobody whose system account can carry the removal of %s",
		ErrNoBrokerMember, voterName(peer, node))
}

// voterName is how a removal's voter reads in a sentence: its node id and peer
// id where it has both, its peer id alone where it has only that.
func voterName(peer, node string) string {
	if node == "" {
		return "the voter " + peer
	}
	return fmt.Sprintf("%s (peer %s)", node, peer)
}

// sortedProfileIDs is a profile map's ids, sorted.
func sortedProfileIDs(profiles map[string]placement.NodeProfile) []string {
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// joinReasons is several members' reasons as one clause.
func joinReasons(why []string) string { return strings.Join(why, "; ") }
