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

// THE METADATA GROUP'S MEMBERSHIP, read and changed from inside a member.
//
// A clustered broker's members are the voters of one raft group, the metadata
// group, which places every stream and consumer. A member that is gone for
// good stays a voter until something removes it: every election and every
// create goes on counting it, and a fleet that lost two of three members for
// good has no quorum left to create anything with. nats-server removes one on
// `$JS.API.SERVER.REMOVE` — and answers that request ONLY when it arrives on
// the SYSTEM account (server/jetstream_api.go, jsLeaderServerRemoveRequest).
//
// # How this process reaches its own system account
//
// Every connection to the embedded broker presents a key, and the broker's one
// authenticator ([brokerAuth]) decides which account each is bound to: this
// process's own clients to the global account, a fleet member's routes to
// none, a leaf's link and an operator's tool to the global account. So a
// CLUSTERED member declares the system account and the authenticator admits
// ONE key to it: a key pair this process mints at start, whose private half
// never leaves it, and which it accepts only over the in-memory pipe — never
// over a socket. No file, no log line and no peer ever holds anything that
// authenticates as it.
//
// NO USER LIST BESIDE IT. The server's own user table could hold the system
// key as well, but it is consulted only where no custom authenticator is set,
// so an entry there would be a second list that nothing reads. And the no-auth
// user nats-server adds to the global account when exactly the global and the
// system account are declared (server/server.go, configureAccounts) — which is
// how every anonymous client used to land in the global account — is still
// added and is unreachable: with a custom authenticator the server never
// consults it, and a client is bound to the global account it starts in only
// until the authenticator binds or refuses it.
//
// AN NKEY RATHER THAN A PASSWORD: what the pipe carries is a public key and a
// signature over the server's nonce, so the server is handed no secret, and
// nothing about it warns on every start the way a plaintext password does
// ("Plaintext passwords detected").
//
// THE SYSTEM ACCOUNT KEEPS ITS DEFAULT NAME, `$SYS`. The metadata group's own
// state is stored under the system account's name, so a member that renamed
// it would come back from an upgrade having forgotten which streams exist.
//
// A SOLO MEMBER DECLARES NOTHING. It has no metadata group to read or change.

// processUser is an identity this process alone holds on its own broker: its
// public key, which the authenticator accepts, and the key pair that signs for
// it, which nothing outside this process ever sees. The engine's own clients
// carry one ([brokerAuth.engine]) and the membership code another
// ([brokerAuth.system]).
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

// ErrNoMetaGroup is a broker with no metadata group: a solo member, a leaf, or
// a server that has shut down. Nothing about a fleet's membership can be read
// from it or changed through it.
var ErrNoMetaGroup = errors.New("jetstream: this broker is not a member of a " +
	"clustered JetStream, so it has no metadata group")

// ErrNotAMetaPeer is a removal naming a voter the metadata group does not
// count — never a member, or already removed.
var ErrNotAMetaPeer = errors.New("jetstream: the metadata group counts no such voter")

// ErrMembershipChanging is a removal refused because another membership change
// is still being committed. The group takes one at a time; asking again once
// it has landed is the remedy.
var ErrMembershipChanging = errors.New("jetstream: another change to the " +
	"metadata group's membership is still being committed")

// ErrNoMetaLeader is a removal nobody answered for the whole wait: only the
// metadata group's leader answers one, and [Server.RemovePeer] asks again
// every [removeResend] — so an election is waited out, and a group that
// answers nobody for that long has lost the quorum it would elect a leader
// with, and can change nothing about itself.
var ErrNoMetaLeader = errors.New("jetstream: the metadata group has no leader " +
	"to commit a membership change — it has lost its quorum, or has been " +
	"electing a leader for longer than the removal waits")

// ErrRemovingSelf is a member asked to carry its own removal. It could not see
// the group drop it — its own view goes on counting it until the server turns
// its JetStream off, which nats-server does the moment the change commits — so
// the answer would be lost with the member that owed it. Another member
// carries it.
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
// name by — derived from the name exactly as nats-server derives it, since a
// server's raft node id is the hash of its own name.
//
// DERIVED RATHER THAN LOOKED UP, because the lookup fails exactly when it is
// needed. A member learns another server's NAME only from that server itself
// (server/events.go, processNewServer), so once the survivors of a member lost
// for good have restarted — a rolling upgrade does it, and so do the restarts
// a capacity seal takes — none of them can name the dead one: the group lists
// it as "Server name unknown at this time", and nats-server resolves a removal
// by name through that same lookup and answers that there is no such server,
// while every election goes on counting it. Its id never leaves the group, and
// the node id an operator knows the member by is enough to compute it.
//
// A derivation written down from another project is a claim until something
// holds it: the membership suites pin it against running servers, and an
// upgrade of nats-server that changed it fails there rather than here.
func PeerIDOf(name string) string {
	sum := sha256.Sum256([]byte(name))
	out := make([]byte, peerIDLength)
	for i := range out {
		out[i] = peerIDAlphabet[int(sum[i])%len(peerIDAlphabet)]
	}
	return string(out)
}

// ValidPeerID reports whether s is spelled as a raft peer id — the check an
// operator's gesture naming a voter by its id is held to before anything asks
// the group about it.
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
	// Name is the server's name, which is the engine's node id — and EMPTY
	// for a voter this member has not heard from since it started, whose
	// name nats-server learns only from the server itself. Such a voter is
	// still counted, and is named by Peer.
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
	// it, sorted by name and then by peer id — so a voter nobody can name
	// sorts first.
	Peers []MetaPeer `json:"peers"`
}

// Counts reports whether the group counts the voter with this peer id.
func (g MetaGroup) Counts(peer string) bool {
	return slices.ContainsFunc(g.Peers, func(p MetaPeer) bool { return p.Peer == peer })
}

// MetaGroup reads the metadata group as this member's broker sees it.
//
// IN PROCESS, from the server's own monitoring, rather than over a request: it
// is this member's view, which is the one a removal made through it would act
// on, and every member holds one — a group without a leader still reports its
// voters, which is exactly when an operator needs to see them.
func (s *Server) MetaGroup() (MetaGroup, error) {
	e := s.embedded
	if e == nil || e.leaf || !e.clustered {
		return MetaGroup{}, ErrNoMetaGroup
	}
	jsz, err := e.ns.Jsz(&server.JSzOptions{})
	if err != nil {
		return MetaGroup{}, fmt.Errorf("jetstream: read the metadata group: %w", err)
	}
	if jsz.Disabled || jsz.Meta == nil {
		return MetaGroup{}, ErrNoMetaGroup
	}
	return metaGroupOf(jsz.Meta, e.ns.Name(), e.ns.Node()), nil
}

// metaGroupOf is the metadata group as nats-server's monitoring reports it,
// read into this package's terms.
//
// THIS MEMBER IS A VOTER ONLY WHILE THE GROUP COUNTS IT. The monitoring leaves
// the answering server out of its own peer list whenever the group has a
// leader, and a member removed from the group goes on answering until the
// server turns its JetStream off — so a member that reported itself
// unconditionally would claim a vote the group had already taken away. What
// says whether it is still counted is the group's own size: the peers listed
// beside it are every voter but itself, and fall short of the size by exactly
// one while it is among them. Leaderless, the monitoring lists it like any
// other peer, and it is read from the list.
//
// A NAME THAT DOES NOT HASH TO ITS PEER ID IS NOT A NAME: it is nats-server's
// placeholder for a voter it has not heard from ("Server name unknown at this
// time"), and it is dropped rather than passed on as though some node were
// called that.
//
// A PURE FUNCTION OVER THE REPORT, because the two states it decides — a
// member removed while it still answers, and a voter whose name nobody knows —
// are ones a cluster reaches only through a removal or a restart, and a rule
// exercised only through one of those is a rule nobody re-reads.
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
// again.
//
// THE RAFT HEARTBEAT, and [jsprovision.ReAsk] is where the tree states it —
// nats-server's one second (server/raft.go, hbInterval), the shortest interval
// over which the group's leadership can have changed: a request that reaches no
// leader is dropped rather than answered, and a newly elected leader makes
// itself known on its first heartbeat, so asking once a heartbeat reaches a new
// leader within one heartbeat of its election. Shorter buys nothing but more
// "in flight" answers from a leader still committing an earlier ask; longer
// adds that much to every removal made across an election. Borrowed rather
// than written again, because a removal is re-sent for exactly the reason a
// provisioning request is, and two copies of one server constant drift.
const removeResend = jsprovision.ReAsk

// appliedPoll is how often [Server.RemovePeer] reads this member's own view of
// the group while waiting for it to apply a committed removal.
//
// A TENTH OF THE HEARTBEAT that carries a commit to a follower: at most a tenth
// of a heartbeat added to the answer, for at most ten in-process reads of the
// server's own monitoring per heartbeat of lag.
const appliedPoll = removeResend / 10

// RemovePeer removes a voter from the metadata group, by its raft peer id
// ([PeerIDOf] of the server's name), and returns once the group has committed
// the change AND this member has applied it — so the group this member reports
// next no longer counts the voter.
//
// BY PEER ID, NEVER BY NAME. nats-server resolves a name through the names it
// has heard, and a member whose survivors have restarted since it died is one
// none of them has heard: asked by name, the group answers that it counts no
// such server while every election goes on counting it — see [PeerIDOf].
//
// THROUGH THIS MEMBER'S SYSTEM ACCOUNT — the only account the request is
// answered on — and answered by the group's leader, wherever that is: the
// request is routed to every member and only the leader replies. What it does
// not do is decide whether the voter SHOULD go: a removed server that is still
// running as a member rejoins as a voter when it restarts, and the caller
// decides whether one that is running may be removed at all. It refuses only
// ITSELF ([ErrRemovingSelf]), which it could never see removed.
//
// # Asked until a leader answers, on one inbox
//
// nats-server's leader answers a removal only once a quorum has committed it,
// and it keeps nothing a lost ask could be recovered from
// (server/jetstream_api.go, jsLeaderServerRemoveRequest;
// server/jetstream_cluster.go, processLeaderChange):
//
//   - a member that is not the leader DROPS the request, so an ask that
//     arrives while the group is electing is never answered at all — and the
//     member an operator removes is often the one that WAS the leader, whose
//     death is what started the election;
//   - a leader that loses its leadership FORGETS the answers it owed, although
//     what it proposed may still be committed by its successor.
//
// A single request therefore waited out the whole budget for an answer nobody
// was ever going to send, and was reported as a group without a leader while
// the survivors had long since elected one. So this asks again every
// [removeResend] until an answer comes, and every ask names the SAME reply
// inbox: an earlier ask's success, sent when its commit lands, is read by
// whichever wait is running then. Two answers are only what they say on the
// FIRST ask, because from the second on they may be about this call's own
// earlier one:
//
//   - "in flight" from the second ask on is most likely this call's own
//     proposal still being committed, so it is waited out — the success comes
//     to the same inbox when it lands, and somebody else's change landing
//     first only means this one is proposed after it;
//   - "not a member" from the second ask on, for a voter this member counted
//     before anything was asked, may be this call's own earlier ask already
//     committed: it is a success once this member's own view no longer counts
//     the voter, and asked again until then. A voter this member never
//     counted is not a member, whichever ask says so — the typo is never
//     reported removed.
//
// THE WAIT IS BOUNDED BY THE BUDGET ONE CLUSTERED METADATA CHANGE GETS
// ([jsprovision.Budget]) — a removal is a proposal to the same group a
// replicated create is, committed by the same quorum — or by the caller's
// context if that ends sooner. Either abandons the answer, not the change: once
// the leader has proposed it, the group commits it without anybody listening.
func (s *Server) RemovePeer(ctx context.Context, peer string) error {
	e := s.embedded
	if e == nil || e.leaf || !e.clustered || e.auth == nil || !e.auth.system.declared() {
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

// judgeRemoval decides what one answer to a removal settles. Settled with no
// error is a removal the group has committed, settled with one is the answer
// the caller gets, and unsettled carries the answer that settled nothing yet —
// the one reported if the wait runs out before another comes.
//
// A PURE DECISION OVER THE ANSWER, because the two cases it exists for — an
// earlier ask's commit landing while its answer was lost to a change of
// leader, and a commit slower than the resend — are races no cluster stages on
// demand, and a rule exercised only through one is a rule nobody re-reads. See
// [Server.RemovePeer] for why each answer means what it does here.
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
				return true, err
			case !still:
				// AN EARLIER ASK'S COMMIT, or somebody else's: either way
				// the group no longer counts the voter, which is what was
				// asked for.
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

// removalUnsettled is a removal whose wait ran out: nobody answered at all,
// which is a group with no leader, or the last answer that settled nothing.
// Either way the removal may yet be committed, which is what the operator
// has to be told before asking again.
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
// A MEMBER DROPS A REMOVED PEER WHEN IT STORES THE CHANGE, before the commit
// (server/raft.go, processAppendEntry), and the leader answers once a QUORUM
// has stored it — which need not include the member that carried the removal:
// in a group of five, a follower outside the two that acknowledged first, or
// one still catching up, has not stored it yet. Without this wait the group a
// removal reports, read from the member that carried it, could still count the
// voter it just removed.
func awaitApplied(ctx context.Context, view func() (MetaGroup, error), peer string) error {
	for {
		still, err := counts(view, peer)
		if err != nil {
			return err
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
