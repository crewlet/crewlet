package jetstream

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// THE METADATA GROUP'S MEMBERSHIP, read and changed from inside a member. A
// member gone for good stays a voter of the metadata group (the raft group
// placing every stream and consumer) until removed, so a fleet that lost two
// of three has no quorum left to create anything with. nats-server removes one
// on `$JS.API.SERVER.REMOVE`, answered ONLY on the system account
// (server/jetstream_api.go, jsLeaderServerRemoveRequest). So a CLUSTERED member
// declares it and [brokerAuth] admits one key to it: minted at start, accepted
// only over the in-memory pipe, held by nothing outside this process (package
// doc, "Every connection presents a key"). An nkey hands the server no secret.
// The account keeps its default name, `$SYS`: the group's state is stored under
// it, and a rename would forget every stream across an upgrade. A solo member
// has no metadata group and declares nothing.

// processUser is an identity this process alone holds on its own broker: the
// public key the authenticator accepts and the key pair that signs for it. The
// engine's own clients carry one ([brokerAuth.engine]) and the membership code
// another ([brokerAuth.system]).
type processUser struct {
	public string
	keys   nkeys.KeyPair
}

// newProcessUser mints one identity for one broker start; role names it in
// the error a failed mint reports.
func newProcessUser(role string) (processUser, error) {
	keys, err := nkeys.CreateUser()
	if err != nil {
		return processUser{}, fmt.Errorf("jetstream: mint the %s's key: %w", role, err)
	}
	public, err := keys.PublicKey()
	if err != nil {
		return processUser{}, fmt.Errorf("jetstream: read the %s's key: %w", role, err)
	}
	return processUser{public: public, keys: keys}, nil
}

// declared reports whether this identity was minted at all: a member that
// declares no system account mints no key for it.
func (u processUser) declared() bool { return u.keys != nil }

// sign signs the server's nonce with this identity's key.
func (u processUser) sign(nonce []byte) ([]byte, error) { return u.keys.Sign(nonce) }

// option presents this identity on a connection.
func (u processUser) option() nats.Option { return nats.Nkey(u.public, u.sign) }

// declareSystemAccount puts the system account into a clustered member's
// options. It declares no user: the authenticator decides who reaches it.
func declareSystemAccount(opts *server.Options) {
	opts.Accounts = append(opts.Accounts, server.NewAccount(server.DEFAULT_SYSTEM_ACCOUNT))
	opts.SystemAccount = server.DEFAULT_SYSTEM_ACCOUNT
}

// connect opens an in-process connection on the system account, as this
// identity — the system one, which the authenticator binds there.
func (u processUser) connect(ns *server.Server) (*nats.Conn, error) {
	return nats.Connect("", nats.InProcessServer(ns), u.option(),
		nats.Name("crewlet-membership"))
}

// ErrNoMetaGroup is a broker configured with no metadata group: a solo member
// (one named into a cluster with no route port too, which nats-server runs
// standalone) or a leaf. Nothing about a fleet's membership can be read from it
// or changed through it, and a solo member has no peers to read.
var ErrNoMetaGroup = errors.New("jetstream: this broker is not a member of a " +
	"clustered JetStream, so it has no metadata group")

// ErrMetaGroupDown is a member configured with a metadata group that can't
// report it now: its JetStream is off (nats-server shuts it down, without
// ending the process, when a store runs out of space or can't be written) or
// the group isn't up yet. NEVER [ErrNoMetaGroup]: the fleet has peers this
// member can't see, so a reader asks another member or reports it unread.
var ErrMetaGroupDown = errors.New("jetstream: this member is clustered, but its " +
	"JetStream is off or its metadata group isn't up, so it can't report the group")

// ErrNotAMetaPeer is a removal naming a voter the metadata group does not
// count — never a member, or already removed.
var ErrNotAMetaPeer = errors.New("jetstream: the metadata group counts no such voter")

// ErrMembershipChanging is a removal refused because another membership change
// is still being committed. The group takes one at a time; asking again once
// it has landed is the remedy.
var ErrMembershipChanging = errors.New("jetstream: another change to the " +
	"metadata group's membership is still being committed")

// ErrNoMetaLeader is a removal nobody answered for the whole wait. Only the
// leader answers, and [Server.RemovePeer] asks again every [removeResend], so
// an election is waited out: a group silent that long has lost its quorum and
// can change nothing about itself.
var ErrNoMetaLeader = errors.New("jetstream: the metadata group has no leader " +
	"to commit a membership change — it has lost its quorum, or has been " +
	"electing a leader for longer than the removal waits")

// ErrRemovalUnconfirmed is a removal this member proposed and then couldn't read
// its own view of the group to settle: the group may have committed it. NEVER
// the view's own sentinel ([ErrMetaGroupDown]), which says nothing was
// proposed and would send the removal to another member to propose again.
var ErrRemovalUnconfirmed = errors.New("jetstream: this member proposed the " +
	"removal and then couldn't read its own view of the metadata group to confirm it")

// ErrRemovingSelf is a member asked to carry its own removal. nats-server
// turns its JetStream off the moment the change commits, so the answer would
// be lost with the member that owed it. Another member carries it.
var ErrRemovingSelf = errors.New("jetstream: a member cannot carry its own " +
	"removal from the metadata group — ask another member")

// peerIDAlphabet and peerIDLength are how nats-server spells a raft peer id:
// the first eight bytes of a SHA-256, each mapped onto these 62 symbols
// (server/accounts.go digits and base, server/events.go getHash, and idLen in
// server/raft.go).
const (
	peerIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	peerIDLength   = 8
)

// PeerIDOf is the raft peer id the metadata group counts the server of this
// name by, derived exactly as nats-server hashes a server's name into its raft
// node id.
//
// DERIVED, NOT LOOKED UP: a member learns another server's name only from that
// server (server/events.go, processNewServer), so once a dead member's
// survivors have restarted none can name it — the group lists it as "Server
// name unknown at this time", and a removal by name finds no such server while
// every election goes on counting it. The membership suites pin the derivation
// against running servers, so a nats-server upgrade that changed it fails there.
func PeerIDOf(name string) string {
	sum := sha256.Sum256([]byte(name))
	out := make([]byte, peerIDLength)
	for i := range out {
		out[i] = peerIDAlphabet[int(sum[i])%len(peerIDAlphabet)]
	}
	return string(out)
}

// ValidPeerID reports whether s is spelled as a raft peer id, the check an
// operator's gesture naming a voter is held to before the group is asked.
func ValidPeerID(s string) bool {
	if len(s) != peerIDLength {
		return false
	}
	for i := range len(s) {
		if !strings.ContainsRune(peerIDAlphabet, rune(s[i])) {
			return false
		}
	}
	return true
}

// MetaPeer is one voter of the metadata group, as this member's broker sees it.
type MetaPeer struct {
	// Name is the server's name, the engine's node id; EMPTY for a voter this
	// member has not heard from since it started ([PeerIDOf]), which is
	// still counted and named by Peer.
	Name string `json:"name"`
	// Peer is the raft peer id: what the group counts the voter by, and
	// what a removal names ([PeerIDOf]).
	Peer string `json:"peer"`
	// Self is the member that answered.
	Self bool `json:"self,omitempty"`
	// Leader is the group's current leader.
	Leader bool `json:"leader,omitempty"`
	// Current is a peer seen recently and caught up with the group.
	Current bool `json:"current"`
	// Offline is a peer not seen recently.
	Offline bool `json:"offline,omitempty"`
	// Active is how long since this broker last heard from the peer.
	Active time.Duration `json:"active"`
}

// MetaGroup is the metadata group as one member reports it.
type MetaGroup struct {
	// Cluster is the cluster's name.
	Cluster string `json:"cluster"`
	// Leader names the group's leader, empty while it has none.
	Leader string `json:"leader,omitempty"`
	// Peers are every voter, this member included while the group counts
	// it, sorted by name then peer id, so a nameless voter sorts first.
	Peers []MetaPeer `json:"peers"`
}

// Counts reports whether the group counts the voter with this peer id.
func (g MetaGroup) Counts(peer string) bool {
	return slices.ContainsFunc(g.Peers, func(p MetaPeer) bool { return p.Peer == peer })
}

// MetaGroup reads the metadata group as this member's broker sees it.
//
// IN PROCESS, from the server's own monitoring, not over a request: it is the
// view a removal through this member acts on, and a group without a leader
// still reports its voters, which is when an operator needs them.
func (s *Server) MetaGroup() (MetaGroup, error) {
	e := s.embedded
	if e == nil || e.leaf || !e.metaGroup {
		return MetaGroup{}, ErrNoMetaGroup
	}
	jsz, err := e.ns.Jsz(&server.JSzOptions{})
	if err != nil {
		return MetaGroup{}, fmt.Errorf("jetstream: read the metadata group: %w", err)
	}
	if jsz.Disabled || jsz.Meta == nil {
		return MetaGroup{}, ErrMetaGroupDown
	}
	return metaGroupOf(jsz.Meta, e.ns.Name(), e.ns.Node()), nil
}

// metaGroupOf reads nats-server's monitoring report of the metadata group into
// this package's terms.
//
// THIS MEMBER IS A VOTER ONLY WHILE THE GROUP COUNTS IT. With a leader, the
// report leaves the answering server out of its peer list, and a removed
// member answers until its JetStream turns off; so it counts while the peers
// beside it fall exactly one short of the group's size. Leaderless, it is
// listed like any other peer. A name that does not hash to its peer id is
// nats-server's placeholder for a voter it has not heard from, and is dropped.
//
// A PURE FUNCTION, because a cluster reaches both states only through a
// removal or a restart (TestTheGroupCountsThisMemberOnlyWhileItDoes holds them).
func metaGroupOf(meta *server.MetaClusterInfo, selfName, selfPeer string) MetaGroup {
	group := MetaGroup{Cluster: meta.Name, Leader: meta.Leader}
	others := 0
	listedSelf := false
	for _, p := range meta.Replicas {
		if p == nil {
			continue
		}
		name := p.Name
		if PeerIDOf(name) != p.Peer {
			name = ""
		}
		self := p.Peer == selfPeer
		if self {
			listedSelf, name = true, selfName
		} else {
			others++
		}
		group.Peers = append(group.Peers, MetaPeer{
			Name: name, Peer: p.Peer, Self: self, Leader: meta.Peer != "" && p.Peer == meta.Peer,
			Current: p.Current || self, Offline: p.Offline && !self, Active: p.Active,
		})
	}
	if !listedSelf && meta.Leader != "" && others < meta.Size {
		group.Peers = append(group.Peers, MetaPeer{
			Name: selfName, Peer: selfPeer, Self: true, Leader: selfPeer == meta.Peer,
			Current: true,
		})
	}
	slices.SortFunc(group.Peers, func(a, b MetaPeer) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Peer, b.Peer)
	})
	return group
}

// removeResend is how long [Server.RemovePeer] waits on one ask before asking
// again: nats-server's one-second raft heartbeat (server/raft.go, hbInterval),
// borrowed from [jsprovision.ReAsk], which re-sends for the same reason, so two
// copies of one server constant cannot drift. A request reaching no leader is
// dropped, and a new leader announces itself on its first heartbeat, so one
// ask a heartbeat reaches it within one heartbeat of its election. Shorter
// only draws more "in flight" answers; longer adds that much to every removal
// made across an election.
const removeResend = jsprovision.ReAsk

// appliedPoll is how often [Server.RemovePeer] reads this member's own view
// while waiting for it to apply a committed removal: a tenth of the heartbeat
// that carries the commit, adding at most that to the answer for ten
// in-process reads per heartbeat of lag.
const appliedPoll = removeResend / 10

// RemovePeer removes a voter from the metadata group by its raft peer id, never
// its name ([PeerIDOf]), and returns once the group committed the change AND
// this member applied it ([awaitApplied]); the leader answers, on the system
// account. It refuses only its own removal ([ErrRemovingSelf]): a removed voter
// still running rejoins on restart, so whether one may go is the caller's call.
//
// A non-leader DROPS the ask and a deposed leader FORGETS the answers it owed
// (server/jetstream_api.go, jsLeaderServerRemoveRequest; jetstream_cluster.go,
// processLeaderChange), so it asks again every [removeResend] on ONE reply
// inbox, where an earlier ask's late success lands too ([judgeRemoval]). The
// wait is [jsprovision.Budget] of a clustered metadata change, or ctx's if
// sooner; either abandons the answer, never the change ([ErrRemovalUnconfirmed]).
func (s *Server) RemovePeer(ctx context.Context, peer string) error {
	e := s.embedded
	if e == nil || e.leaf || !e.metaGroup || e.auth == nil || !e.auth.system.declared() {
		return ErrNoMetaGroup
	}
	if !ValidPeerID(peer) {
		return fmt.Errorf("jetstream: %q is not a raft peer id — name the voter to "+
			"remove by the id the metadata group counts it by", peer)
	}
	if peer == e.ns.Node() {
		return fmt.Errorf("%w: %s is this member (%s)", ErrRemovingSelf, peer, e.ns.Name())
	}
	ctx, cancel := context.WithTimeout(ctx, jsprovision.Budget(true))
	defer cancel()
	listed, err := counts(s.MetaGroup, peer)
	if err != nil {
		return err
	}
	nc, err := e.auth.system.connect(e.ns)
	if err != nil {
		return fmt.Errorf("jetstream: connect to this member's system account: %w", err)
	}
	defer nc.Close()
	body, err := json.Marshal(server.JSApiMetaServerRemoveRequest{Peer: peer})
	if err != nil {
		return err
	}
	answers, err := nc.SubscribeSync(nc.NewInbox())
	if err != nil {
		return fmt.Errorf("jetstream: open the inbox the removal of %s is answered on: %w", peer, err)
	}
	defer func() { _ = answers.Unsubscribe() }()

	asks := 0
	// pending is the last answer that settled nothing — why the removal is
	// not yet known to have landed — and nil while nobody has answered.
	var pending error
	for {
		if err := nc.PublishRequest(server.JSApiRemoveServer, answers.Subject, body); err != nil {
			return fmt.Errorf("jetstream: ask the metadata group to remove %s: %w", peer, err)
		}
		asks++
		window, stop := context.WithTimeout(ctx, removeResend)
		for {
			msg, err := answers.NextMsgWithContext(window)
			if err != nil {
				stop()
				switch {
				case ctx.Err() != nil:
					return removalUnsettled(peer, asks, pending, ctx.Err())
				case window.Err() != nil:
					// This ask's window is over: ask again.
				default:
					return fmt.Errorf("jetstream: wait for the answer to the removal "+
						"of %s: %w", peer, err)
				}
				break
			}
			settled, why := judgeRemoval(peer, msg.Data, removalAsked{
				asks: asks, listed: listed,
				counted: func() (bool, error) { return counts(s.MetaGroup, peer) },
			})
			switch {
			case settled && why == nil:
				stop()
				return awaitApplied(ctx, s.MetaGroup, peer)
			case settled:
				stop()
				return why
			}
			pending = why
		}
	}
}

// removalAsked is what [judgeRemoval] needs to know about the removal an
// answer belongs to.
type removalAsked struct {
	// asks is how many times this call has asked, the answer's own ask
	// included: from the second on, an answer may be about an earlier one.
	asks int
	// listed is whether this member counted the voter before anything was
	// asked.
	listed bool
	// counted reads whether this member counts the voter now.
	counted func() (bool, error)
}

// judgeRemoval decides what one answer to a removal settles: settled with no
// error is a committed removal, settled with one is the caller's answer, and
// unsettled carries the answer reported if the wait runs out first.
//
// From the second ask on an answer may be about an earlier one: "in flight" is
// most likely this call's own proposal and is waited out, and "not a member",
// for a voter this member counted before asking, is a success once this member
// no longer counts it. A voter never counted is never reported removed. A PURE
// DECISION, because a commit outliving its leader or the resend is a race no
// cluster stages on demand.
func judgeRemoval(peer string, data []byte, asked removalAsked) (settled bool, why error) {
	var resp server.JSApiMetaServerRemoveResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return true, fmt.Errorf("jetstream: the metadata group answered the removal of "+
			"%s with something unreadable: %w", peer, err)
	}
	if apiErr := resp.Error; apiErr != nil {
		switch server.ErrorIdentifier(apiErr.ErrCode) {
		case server.JSClusterServerNotMemberErr:
			notMember := fmt.Errorf("%w: %s", ErrNotAMetaPeer, peer)
			if asked.asks == 1 || !asked.listed {
				return true, notMember
			}
			still, err := asked.counted()
			switch {
			case err != nil:
				return true, fmt.Errorf("%w: the group answered that %s is not a member, and "+
					"an earlier ask may have removed it: %w", ErrRemovalUnconfirmed, peer, err)
			case !still:
				// An earlier ask's commit, or somebody else's: the voter is
				// gone either way.
				return true, nil
			}
			return false, fmt.Errorf("%w, though this member still counts it", notMember)
		case server.JSClusterServerMemberChangeInflightErr:
			changing := fmt.Errorf("%w: %s was not removed", ErrMembershipChanging, peer)
			return asked.asks == 1, changing
		}
		return true, fmt.Errorf("jetstream: the metadata group refused to remove %s: %s "+
			"(code %d)", peer, apiErr.Description, apiErr.ErrCode)
	}
	if !resp.Success {
		return true, fmt.Errorf("jetstream: the metadata group answered the removal of %s "+
			"without saying it succeeded", peer)
	}
	return true, nil
}

// removalUnsettled is a removal whose wait ran out, with no answer (a group
// with no leader) or one that settled nothing. Either way it may yet commit,
// which the operator must be told before asking again.
func removalUnsettled(peer string, asks int, pending, cause error) error {
	if pending == nil {
		return fmt.Errorf("%w: nobody answered the removal of %s, asked %d times, "+
			"within the time allowed; if the group has a leader again, the removal "+
			"may still have been committed — read the metadata group before asking "+
			"again: %w", ErrNoMetaLeader, peer, asks, cause)
	}
	return fmt.Errorf("%w when the time allowed ran out — read the metadata group "+
		"before asking again: %w", pending, cause)
}

// counts reports whether a view of the metadata group — this member's own, in
// [Server.RemovePeer] — counts the voter with this peer id.
func counts(view func() (MetaGroup, error), peer string) (bool, error) {
	group, err := view()
	if err != nil {
		return false, err
	}
	return group.Counts(peer), nil
}

// awaitApplied waits for this member's own view of the group to stop counting a
// voter whose removal the group has committed.
//
// A member drops a removed peer when it stores the change (server/raft.go,
// processAppendEntry), and the leader answers once a QUORUM has, which need
// not include the member that carried the removal; without this wait the group
// it reports next could still count the voter it just removed.
func awaitApplied(ctx context.Context, view func() (MetaGroup, error), peer string) error {
	for {
		still, err := counts(view, peer)
		if err != nil {
			return fmt.Errorf("%w: the group committed the removal of %s: %w",
				ErrRemovalUnconfirmed, peer, err)
		}
		if !still {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("jetstream: the metadata group committed the removal of "+
				"%s, and this member had not applied it when the time allowed ran "+
				"out — it is behind its own group: %w", peer, ctx.Err())
		case <-time.After(appliedPoll):
		}
	}
}
