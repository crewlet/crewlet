package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat/placement"
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

// fleetBrokerSubjectPrefix prefixes every member's broker-membership subject.
const fleetBrokerSubjectPrefix = "crewlet.fleet.broker"

// fleetBrokerSubject is the one subject a member answers the fleet's broker
// questions on. THE NODE ID IS ESCAPED as a subject token, for the estate
// subject's reason: an id may carry a dot, which would split it in two.
func fleetBrokerSubject(node string) string {
	return fleetBrokerSubjectPrefix + "." + coord.DocumentKey(node)
}

// brokerReadAsk bounds one member's answer to a read of the metadata group.
//
// TEN SECONDS, the estate's own per-node read attempt: the member answers out
// of its own memory, so this is a request across the fleet — a leaf link
// included — and a member silent for many times that is one that is gone
// rather than slow, and the next one answers sooner.
const brokerReadAsk = 10 * time.Second

// BrokerRemoveWait bounds one member's answer to a removal: the member waits
// for the metadata group to commit the change within the budget one clustered
// metadata change gets ([jsprovision.Budget]), and the asker waits that and the
// read attempt's round trip on top, so a member that answers at the edge of its
// own budget is still heard. Exported for the surfaces that wait on the
// gesture, which have to wait longer still.
func BrokerRemoveWait() time.Duration { return jsprovision.Budget(true) + brokerReadAsk }

// brokerMembership is what the engine asks of its own embedded broker.
// Declared here, by the consumer; a leaf's and a solo member's answer
// [jetstream.ErrNoMetaGroup].
type brokerMembership interface {
	MetaGroup() (jetstream.MetaGroup, error)
	RemovePeer(ctx context.Context, name string) error
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

	// BrokerUnknownKind is a live node whose presence does not say what its
	// broker is — a build older than the field. It is counted as a member
	// wherever that is the safe reading.
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
	Kind   BrokerFindingKind `json:"kind"`
	Node   string            `json:"node"`
	Detail string            `json:"detail"`
}

// BrokerNode is one live node's broker, as its presence advertises it.
type BrokerNode struct {
	Node string `json:"node"`
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

// BrokerRemoval is an operator's removal of a member from the metadata group.
type BrokerRemoval struct {
	// Node is the member to remove, by node id — the server name.
	Node string
	// Force removes it although it holds a live presence lease.
	Force bool
	// By is the operator, for the log.
	By string
}

// BrokerRemoved is a removal the metadata group committed.
type BrokerRemoved struct {
	Node string `json:"node"`
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
// is gone, or the one named is the only one.
var ErrNoBrokerMember = errors.New("engine: no live member of the fleet's broker " +
	"answered")

// BrokerMemberLive is a removal refused because the named node holds a live
// presence lease — it is running, and a running member removed from the group
// rejoins as a voter the next time it restarts.
type BrokerMemberLive struct {
	Node string
	Kind placement.BrokerKind
}

func (e *BrokerMemberLive) Error() string {
	return fmt.Sprintf("engine: %s holds a live presence lease (its broker is %s), "+
		"so it is still running: stop it first — or remove it with force, knowing "+
		"a running member removed from the metadata group rejoins it as a voter "+
		"at its next restart", e.Node, e.Kind)
}

// FleetBroker is the fleet's broker membership, for the operator surfaces.
type FleetBroker struct{ e *Engine }

// FleetBroker is this engine's broker-membership surface. Every node has one:
// any node answers the read, asking a member where it is not one.
func (e *Engine) FleetBroker() *FleetBroker { return &FleetBroker{e: e} }

// brokerRequest is one question to a member, as it crosses the wire.
type brokerRequest struct {
	Op   string `json:"op"`
	Node string `json:"node,omitempty"`
	By   string `json:"by,omitempty"`
	From string `json:"from,omitempty"`
}

const (
	brokerOpGroup  = "group"
	brokerOpRemove = "remove"
)

// brokerReply is a member's answer. Code names a refusal a caller branches on.
type brokerReply struct {
	Node   string               `json:"node"`
	Group  *jetstream.MetaGroup `json:"group,omitempty"`
	Code   string               `json:"code,omitempty"`
	Detail string               `json:"detail,omitempty"`
}

const (
	brokerCodeNoGroup   = "no_meta_group"
	brokerCodeNotPeer   = "not_a_peer"
	brokerCodeChanging  = "membership_changing"
	brokerCodeNoLeader  = "no_leader"
	brokerCodeFailed    = "failed"
	brokerCodeUnknownOp = "unknown_op"
)

// errorOf turns a member's refusal back into the sentinel it answered from,
// so a caller's errors.Is holds across the wire.
func (r brokerReply) errorOf() error {
	switch r.Code {
	case "":
		return nil
	case brokerCodeNoGroup:
		return fmt.Errorf("%w (%s)", jetstream.ErrNoMetaGroup, r.Detail)
	case brokerCodeNotPeer:
		return fmt.Errorf("%w (%s)", jetstream.ErrNotAMetaPeer, r.Detail)
	case brokerCodeChanging:
		return fmt.Errorf("%w (%s)", jetstream.ErrMembershipChanging, r.Detail)
	case brokerCodeNoLeader:
		return fmt.Errorf("%w (%s)", jetstream.ErrNoMetaLeader, r.Detail)
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
	default:
		out.Code = brokerCodeFailed
	}
	return out
}

// serveFleetBroker makes this member answer the fleet's broker questions on its
// own subject. Only a MEMBER serves — a leaf and a client hold no metadata
// group — and every member does, so a node that is not one always has somebody
// to ask.
func (e *Engine) serveFleetBroker(ctx context.Context) error {
	if e.profile.Broker != placement.BrokerMember || e.backends == nil ||
		e.backends.Queue == nil || e.backends.broker == nil {
		return nil
	}
	stop, err := e.backends.Queue.Serve(ctx, fleetBrokerSubject(e.id),
		func(ctx context.Context, raw []byte) ([]byte, error) {
			return json.Marshal(e.answerBroker(ctx, raw))
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
func (e *Engine) answerBroker(ctx context.Context, raw []byte) brokerReply {
	var req brokerRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return brokerReply{Node: e.id, Code: brokerCodeFailed,
			Detail: fmt.Sprintf("%s could not decode the request: %v", e.id, err)}
	}
	switch req.Op {
	case brokerOpGroup:
		group, err := e.backends.broker.MetaGroup()
		if err != nil {
			return replyFor(e.id, err)
		}
		return brokerReply{Node: e.id, Group: &group}
	case brokerOpRemove:
		return e.removeHere(ctx, req.Node, req.By, req.From)
	}
	// A NEWER PEER'S QUESTION, which this build cannot answer: said so,
	// so the asker moves on rather than waiting.
	return brokerReply{Node: e.id, Code: brokerCodeUnknownOp,
		Detail: fmt.Sprintf("%s does not know the operation %q", e.id, req.Op)}
}

// removeHere removes a member through THIS member's system account.
//
// ON ITS OWN BUDGET, detached from the request that asked: once the leader has
// proposed the change the group commits it whoever is listening, so an asker
// that gave up changes nothing but who hears the answer.
func (e *Engine) removeHere(ctx context.Context, node, by, from string) brokerReply {
	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jsprovision.Budget(true))
	defer cancel()
	if err := e.backends.broker.RemovePeer(removeCtx, node); err != nil {
		log.WarnContext(ctx, "fleet_broker_remove_refused", "node", node, "by", by,
			"asked_by", from, "error", err.Error())
		return replyFor(e.id, err)
	}
	log.WarnContext(ctx, "fleet_broker_member_removed", "node", node, "by", by,
		"asked_by", from, "detail", "the metadata group no longer counts this "+
			"member in its elections or its creates; a process that restarts under "+
			"this name joins it again as a new voter")
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
// by id, leaving out skip.
func (f *FleetBroker) askOrder(live map[string]placement.NodeProfile, skip string) (local bool, remote []string) {
	local = f.e.profile.Broker == placement.BrokerMember && f.e.backends != nil &&
		f.e.backends.broker != nil && f.e.id != skip
	for id, profile := range live {
		if id == f.e.id || id == skip || profile.Broker != placement.BrokerMember {
			continue
		}
		remote = append(remote, id)
	}
	slices.Sort(remote)
	return local, remote
}

// ask puts one request to one member and reads its answer. A member that did
// not answer within the budget reports false, and the caller asks the next.
func (f *FleetBroker) ask(ctx context.Context, member string, req brokerRequest,
	budget time.Duration) (brokerReply, bool, error) {

	body, err := json.Marshal(req)
	if err != nil {
		return brokerReply{}, false, err
	}
	askCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	replies, err := f.e.backends.Queue.Ask(askCtx, fleetBrokerSubject(member), body, 1)
	if err != nil {
		return brokerReply{}, false, fmt.Errorf("engine: ask %s: %w", member, err)
	}
	if len(replies) == 0 {
		return brokerReply{}, false, nil
	}
	var reply brokerReply
	if err := json.Unmarshal(replies[0], &reply); err != nil {
		return brokerReply{}, false, fmt.Errorf("engine: %s answered with something "+
			"unreadable: %w", member, err)
	}
	return reply, true, nil
}

// List is the fleet's broker membership: what every live node advertises, the
// metadata group as a member reports it, and where the two disagree.
func (f *FleetBroker) List(ctx context.Context) (BrokerView, error) {
	view := BrokerView{Node: f.e.id, Kind: f.e.profile.Broker.String(), Findings: []BrokerFinding{}}
	live, err := f.livePresence(ctx)
	if err != nil {
		return BrokerView{}, err
	}
	for _, id := range sortedProfileIDs(live) {
		profile := live[id]
		view.Nodes = append(view.Nodes, BrokerNode{
			Node: id, Kind: profile.Broker.String(), Roles: profile.Roles.Names(),
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

// readGroup reads the metadata group from the first member that answers.
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
	for _, member := range remote {
		reply, answered, err := f.ask(ctx, member,
			brokerRequest{Op: brokerOpGroup, From: f.e.id}, brokerReadAsk)
		switch {
		case err != nil:
			why = append(why, err.Error())
		case !answered:
			why = append(why, member+" did not answer")
		case reply.Group != nil:
			return reply.Group, reply.Node, ""
		default:
			why = append(why, fmt.Sprintf("%s: %v", member, reply.errorOf()))
		}
	}
	if len(why) == 0 {
		return nil, "", "no live node advertises a member of the fleet's broker, so " +
			"there is nobody to read the metadata group from"
	}
	return nil, "", "no member reported the metadata group: " + joinReasons(why)
}

// brokerFindings holds what the nodes advertise against what the metadata
// group counts. A group that could not be read yields only the findings the
// presence rows carry on their own.
func brokerFindings(live map[string]placement.NodeProfile, group *jetstream.MetaGroup) []BrokerFinding {
	out := []BrokerFinding{}
	voters := map[string]bool{}
	if group != nil {
		for _, peer := range group.Peers {
			voters[peer.Name] = true
			profile, running := live[peer.Name]
			switch {
			case !running:
				out = append(out, BrokerFinding{Kind: BrokerDeadMember, Node: peer.Name,
					Detail: "the metadata group counts it as a voter and no live node " +
						"is it: its process is gone, and every election and create " +
						"goes on counting it until it comes back or is removed"})
			case profile.Broker == placement.BrokerLeaf || profile.Broker == placement.BrokerClient:
				out = append(out, BrokerFinding{Kind: BrokerDeadMember, Node: peer.Name,
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
				Detail: "its presence does not say what its broker is — a build " +
					"older than the field — so it is counted as a member wherever " +
					"that is the safe reading, a capacity seal included"})
		case group != nil && profile.Broker == placement.BrokerMember && !voters[id]:
			out = append(out, BrokerFinding{Kind: BrokerNotInGroup, Node: id,
				Detail: "it advertises a member and the metadata group does not " +
					"count it: it is still joining, or it was removed while it ran " +
					"and joins again as a voter at its next restart"})
		}
	}
	return out
}

// Remove removes a member from the metadata group, through a live member's
// system account.
//
// REFUSED WHILE THE NAMED NODE HOLDS A LIVE PRESENCE LEASE, unless forced: a
// running member removed from the group rejoins it as a voter at its next
// restart, so the gesture is for a member that is gone for good, and the lease
// is the fleet's own evidence that this one is not. The member that carries it
// is never the one being removed while another will do.
func (f *FleetBroker) Remove(ctx context.Context, req BrokerRemoval) (BrokerRemoved, error) {
	if !config.ValidNodeID(req.Node) {
		return BrokerRemoved{}, fmt.Errorf("engine: %q is not a node id", req.Node)
	}
	if f.e.profile.Broker == placement.BrokerClient {
		return BrokerRemoved{}, ErrExternalBroker
	}
	live, err := f.livePresence(ctx)
	if err != nil {
		return BrokerRemoved{}, err
	}
	if profile, running := live[req.Node]; running && !req.Force {
		return BrokerRemoved{}, &BrokerMemberLive{Node: req.Node, Kind: profile.Broker}
	}
	local, remote := f.askOrder(live, req.Node)
	if local {
		reply := f.e.removeHere(ctx, req.Node, req.By, f.e.id)
		if err := reply.errorOf(); err != nil {
			return BrokerRemoved{}, err
		}
		return BrokerRemoved{Node: req.Node, By: reply.Node, Group: reply.Group}, nil
	}
	// THE MEMBER BEING REMOVED CARRIES IT ONLY AS A LAST RESORT, forced: a
	// member removing itself is still a proposal the leader commits, and
	// with no other member live there is nobody else to ask.
	if len(remote) == 0 && req.Force {
		if profile, running := live[req.Node]; running && profile.Broker == placement.BrokerMember {
			remote = []string{req.Node}
		}
	}
	var why []string
	for _, member := range remote {
		reply, answered, err := f.ask(ctx, member, brokerRequest{
			Op: brokerOpRemove, Node: req.Node, By: req.By, From: f.e.id,
		}, BrokerRemoveWait())
		switch {
		case err != nil:
			why = append(why, err.Error())
			continue
		case !answered:
			why = append(why, member+" did not answer")
			continue
		}
		if err := reply.errorOf(); err != nil {
			if errors.Is(err, jetstream.ErrNoMetaGroup) || reply.Code == brokerCodeUnknownOp {
				// THIS MEMBER CANNOT CARRY IT, which says nothing
				// about the group: ask the next.
				why = append(why, err.Error())
				continue
			}
			return BrokerRemoved{}, err
		}
		return BrokerRemoved{Node: req.Node, By: reply.Node, Group: reply.Group}, nil
	}
	if len(why) == 0 {
		return BrokerRemoved{}, fmt.Errorf("%w: no live node other than %s advertises "+
			"a member, so there is nobody whose system account can carry the removal",
			ErrNoBrokerMember, req.Node)
	}
	return BrokerRemoved{}, fmt.Errorf("%w: %s", ErrNoBrokerMember, joinReasons(why))
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
