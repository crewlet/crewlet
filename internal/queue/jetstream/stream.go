// Package jetstream implements the EventQueue contract on NATS JetStream,
// embedded in this process by default and reachable as an external cluster
// when configured.
//
// It is the same client code either way — embedded versus external is a
// connection choice, not a second backend — which is what lets a laptop run
// the whole company with no services and a fleet run the same binary against
// a cluster.
//
// The one thing an external server must promise that the embedded one is
// built with is the contract's message size
// ([github.com/crewlet/crewlet/internal/queue.MaxPayloadBytes]): the
// client refuses a message past what its server announces, so a server left
// at nats-server's 1 MiB default takes every small write and refuses the
// first large one. Every connection a queue opens is held to the contract on
// the limit its server announces once it has authenticated — its own
// max_payload, or its account's where that is lower — and one that falls
// short is a refused boot naming the setting rather than a node that serves
// and then cannot write — see carriesTheContract.
//
// # Why JetStream fits
//
// Crewlet's subject grammar IS NATS grammar: dot-separated segments with `*`
// and `>` wildcards. More importantly, a durable consumer can be created
// with nothing attached (measured: 1.7 ms), which is the operation a seat's
// mailbox is built on — an ordinary API call here, where a broker that only
// creates a subscription by joining it needs an out-of-band admin call to
// avoid stealing a live peer's traffic.
//
// Pull consumers are the other half: nothing is pushed into a client-side
// queue, so a wedged node holds no mail it has not fetched, and resuming a
// quiesced attachment does not have to reclaim a prefetch.
//
// # What it costs, measured
//
// Three properties an engine might want are not on offer, and each shaped the
// code above this package. There is no free handoff — every path back to the
// broker increments the delivery count — and redeliveries return BEHIND
// never-delivered messages rather than replaying from the head. The first is
// absorbed by a larger delivery budget (see maxDeliver); the second is why
// conversation order comes from event timestamps rather than from the broker
// (see queue.OrderForDispatch).
//
// The third is newer and cost a red CI to learn: A CONSUMER'S COUNTERS DO NOT
// READ YOUR OWN WRITES. The server stores a message and acks the publisher,
// and only afterwards, on an offloaded goroutine, signals the consumers that
// increment their pending counts — so a consumer that already existed when
// the publish landed can report nothing pending for a message the stream
// demonstrably holds. Measured on the embedded broker at roughly two reads in
// three from a second connection. Anything that has to be synchronous with a
// returned publish therefore reads the STREAM's own state, never a consumer's
// count; see Backlog, which is where the tempting one-round-trip form was and
// what it did.
//
// # What a broker that does not answer looks like from above
//
// The client reports a broker it cannot reach in a dozen words of its own — a
// timeout, no responders, a stream with no leader, a connection reconnecting
// or disconnected, a reconnect buffer that filled — and what the waiting call
// ends on is often only a deadline. Every verb that calls the broker therefore
// marks its failure with the contract's word for it before returning
// ([Queue.brokerFailed]): queue.ErrUnavailable for silence, a condition the
// layers above answer with "try again"; [ErrConnectionLost], which is
// queue.ErrNotLive, for a connection the client closed for good. Unmarked,
// every one of them read above the queue as this node's own fault. The
// domain-log seam (DomainLog, DomainConsumer) is deliberately not marked:
// internal/statelog classifies its own appends and reads into outcomes and
// refusals, and a second classification beneath it would be a second answer
// to one question.
//
// # A write across a link is given the time its bytes take to cross
//
// A write here — a Publish, a dead letter's keep (KeepDeadLetter, or the
// delivery budget's), a state log's Append — returns once its stream has
// acknowledged it, and the stream acknowledges a message only once its last
// byte is in. A member's own connection reaches its broker in its own process,
// but a leaf's crosses its link to the members and an external cluster's
// client crosses the network to it, and the engine allows such a link to be as
// slow as the object store's floor, a mebibyte in thirty seconds
// (objstore.MiBPace). The connection's pace is decided once, where it is made
// (linkOf), and every crossing on it reads it: a read's reply (DomainLog.At,
// the dead letters' frames) and a write's request alike. So a write whose
// caller set no deadline waits the request term (jsapi.RequestTerm, five
// seconds) and, where its connection crosses a link, its bytes at that pace
// for each of its first three sends (jsapi.WriteSends; Queue.writing,
// jsapi.Budget): a write is sent again — the client's own re-sends turned off
// and made here, so each is a send of the queue's (sendWithin) — where nobody
// heard it, and an append again where the leader refused it while another
// write to its subject was in flight, and each send carries the whole message
// across the link again; a send past the third waits in the budget as it
// stands. A
// caller's own deadline is kept, never extended: a caller that cannot wait for
// a slow link says so. On an external broker the client does not check that
// deadline while the request waits for the connection's lock, which a large
// write holds for its whole socket Write, nor while its own write is in the
// socket, so the caller is held behind every large write ahead of it, each up
// to the dial's write deadline (externalWriteDeadline, about eight minutes),
// and then for its own; bounding that needs the write windowed
// (crewlet/crewlet#1035).
//
// Held to the five seconds alone, a write at the transport's eight-mebibyte
// ceiling could not be acknowledged on any link slower than 1.6 MiB/s and
// failed as queue.ErrUnavailable while the link was still carrying it — and a
// keeper retries an unavailable broker for ever, each copy of the entry
// crossing and landing behind the last (crewlet/crewlet#1005).
//
// WHAT THAT GIVES, AND WHAT IT DOES NOT. A write is given its own bytes at the
// floor, so a link that carries it alone at the floor or faster never fails
// it — an external dial's socket write deadline included, which is the
// largest write its client makes at the floor (externalWriteDeadline). A link
// slower than the floor still fails it, and so can a link busy
// with something ahead of it — another large write, an object store put's
// window — whose bytes no budget here counts. And the worst case is long, and
// not only for the write: one at the ceiling across a link at the floor waits
// five seconds and about four minutes a send before it reads as an outage —
// up to three sends, about twelve minutes, for a write whose answer decided
// nothing, the bound its budget is fixed at when it starts (jsapi.WriteSends)
// — and everything the node sends behind it on that link waits as long, its
// lease renewals among them, which at the shipped lease lapse long before it
// is in. That is a defect of its own (crewlet/crewlet#1035), which these
// budgets do not fix.
//
// # What a link carries is read at its socket
//
// A node that crosses a link to the fleet's broker — a leaf, across its link
// to the members; any node, across the network to an external cluster — has
// to tell a link that is carrying something from one that carries nothing,
// because a message on it can be megabytes (the transport's ceiling,
// queue.MaxPayloadBytes) and one takes four minutes to cross at the slowest
// pace the engine allows. No counter the libraries keep can say: nats-server
// counts a connection's bytes in once its read loop has parsed a whole
// message and out when it queues one, and nats.go counts a message once it is
// whole, so for all four minutes every one of them stands still. The kernel's
// do not. So the link's SOCKET is read (LinkMeter): the bytes the node read
// off it, and the bytes the far end's kernel acknowledged of what it wrote
// (internal/sockcount: TCP_INFO's tcpi_bytes_acked on Linux, written less
// SO_NWRITE on darwin), every quarter of a read term, the last
// objstore.Stall of readings kept. A link is MOVING when it carried, in
// either direction, at least seven tenths of what the floor carries over the
// span asked about: midway between the background a quiet link carries and
// the floor a busy one does (movingShare).
//
// WHERE IT SITS. On an external broker, every connection the queue dials is
// metered as one link, reconnects included (infoDialer). On a leaf, the
// link to the members is dialled by nats-server, which gives no embedder its
// socket, so the leaf dials it through a tunnel in the engine's own process
// — an HTTP CONNECT proxy on loopback, nats-server's supported leaf-remote
// Proxy option — whose upstream socket the meter reads (tunnel). A member
// crosses no link and has no meter (Queue.LinkMeter).
//
// WHAT IT CANNOT SEE: a proxy between the node and the broker that
// terminates TCP — a service mesh's sidecar on the leaf port, a load
// balancer that buffers — acknowledges what it has buffered rather than what
// the broker took, so behind one a broker that stopped reading looks like a
// buffer still draining. The deployment guides state it as a requirement:
// the link reaches the broker's own sockets.
//
// WHAT READS IT (ADR-0155): a wait across a link ends for silence only while
// the link carries nothing at the floor. A seat's hydration
// (internal/learning/memsync, handed the meter as its Config.Link) refuses a
// replay that heard nothing for its stall only where the link did not move
// over it, and holds its hedged requests' ceilings past their spans while it
// moves (internal/jsprovision's Timing.Following), each wait capped by one
// message at the transport's bound crossing at the floor.
//
// # A pull allocates for what it receives, never for what it may
//
// The client library's fetches size their delivery channels by the BATCH a
// pull may receive, and its byte-bounded fetch fixes that batch at a million:
// 32 MiB allocated on every call, measured, on a log with nothing in it. A
// state log's applier pulls twice a second per domain for the life of a node,
// so that was 181 MiB/s from three idle consumers and an OOM kill for a
// process running a few hundred. Its count-only fetch costs the same per
// record of batch — ≈ 172 KiB at the applier's four thousand — and cannot
// bound bytes at all. [DomainConsumer.Fetch] therefore makes the server's own
// pull request itself, carrying the count and the byte bound together, and
// every request of a consumer is answered on ONE standing inbox — a callback
// subscription whose queue grows with what arrives, and a buffer of what no
// fetch has taken — so a delivery to a request the client stopped counting
// reaches the next fetch rather than waiting out the ack window, and a fetch
// returns its first burst rather than holding it for the wait. pullrequest.go
// says why that and not the library's Messages iterator, whose prefetch would
// hold records in flight whether or not an applier is reading and needs a
// stop the applier has no call for.
//
// Every other fetch in this package and in internal/learning/memsync is
// count-bounded and small — one record for a mailbox's first wait, the
// configured mailbox batch after a record has arrived, 256 for a peek or a
// memory replay — so the library's per-batch sizing costs them a few
// kilobytes at most, and only the first of them is issued while idle.
//
// Three pulls in this package are the library's own STANDING pulls instead,
// kept asked by the library for as long as their readers live, idle or not.
// The live feed's ([Queue.SubscribeStream]) and a watch's (below) are each a
// Consume, which delivers to a callback subscription no batch sizes. A domain
// group's ([DomainLog.Group], the change feed) is a Messages iterator, whose
// channel the library sizes by its batch — 500 slots at its default: a few
// kilobytes of pointers, and up to that many of the group's records held
// ahead of its reader.
//
// # A linger window ends on a clock the conformance suite can move
//
// A batch drain decides for itself where its linger window ends and whether it
// has: it takes the deadline once its first message is in hand, and reads it
// again after every fetch. That decision, and nothing else, reads the
// [lingerClock] its queue was handed (Config.clock, unexported, resolved by
// one accessor): the wall clock, unless this package's own tests hand another.
// It exists for the conformance suite, whose LingerClock capability builds a
// queue on a clock the case moves, so a case lands a pause, a hold, a stop or
// its next event inside a window and only then ends the window, rather than
// race one on the wall clock.
//
// EACH FETCH INSIDE THE WINDOW KEEPS ITS EXPIRY ON THE BROKER'S AND THE
// CLIENT'S REAL CLOCKS. A test clock decides only where the window ends; it
// says how much of the window is left, and a fetch is sized from that, never
// past a poll. The fetch's expiry is the broker's own timeout for the pull
// request — the 408 the server answers it with once the expiry passes on its
// clock — and the client's wait past it, on the wall clock. A fake clock under
// it would bend the broker's own timing rather than the drain's decision: one
// standing still would have the broker hold a pull request for the whole
// window, in real time, and one moved past the window's end would send requests
// already expired. So a fetch returns within its poll whatever the handed
// clock says, and the drain reads that clock again then.
//
// # A handler runs inline on the goroutine that took its delivery
//
// A Subscribe or SubscribeBatch handler is called inline on its attachment's
// consume loop, so the loop fetches again only once the handler has returned,
// and a listener's handler inline in the callback nats.go calls for its
// subscription. Listen gives a message no goroutine of its own because there is
// nothing to reply to: Serve's answers each run on a goroutine of their own
// because an asker waits on every one, while a broadcast's handler holds nobody
// up but the next broadcast, so nothing about a message is worth a goroutine.
// That rests on the library's own guarantee, which this package does not hold:
// nats.go runs a callback subscription's messages on one goroutine of its own,
// calling the callback for one message after another (at v1.55.0, subscribe
// starts one waitForMsgs per such subscription, which calls the callback in a
// loop). Together they are the contract's one message at a time per listener,
// and the reason a handler that publishes into its own subscription is never
// re-entered.
//
// Crewlet's half of both is held where the code sits, because the conformance
// suite could read either absence here only behind a quiet window:
// TestAHandlerRunsInlineOnTheGoroutineThatTookItsDelivery follows each handler
// from the verb that is handed it to the call that runs it, and refuses a go
// statement, a function literal or a function off that path. A change that
// wants a handler off the goroutine that took its delivery is a production
// decision of its own, made in a change of its own, never one a test makes.
//
// # A watch is told which records moved, and never carries them
//
// A state log is read four ways: by its applier's durable consumer, one a
// node; by a domain group's consumer, one the fleet shares, where whichever
// node reaches a record first handles it ([DomainLog.Group], the change
// feed); by reading one record — by position ([DomainLog.At]) or as its
// subject's newest ([DomainLog.LastSeq]) — or a range from a position
// (internal/backup's replay); and by a watch ([DomainLog.Watch]), the newest.
// A reader that keeps its work in rows — Gitlet's home node is the
// first — needs to know only that an object moved, and sooner than its own
// pass over the rows would find it. So a watch is a consumer of its own that
// takes each new record's SUBJECT and position and never its body: headers
// only, from the log's end, acknowledging nothing, one replica in memory on
// one member. What it may have missed it says it may have missed, and how far
// on the log that reaches — its consumer lost and replaced at the log's end,
// a delivery the consumer counted and this node never received, a reconnect —
// and the caller waits for its copy to reach that position, then reads its
// rows, which are the record; a nudge only ever shortens a wait. Its pull is
// the library's own standing pull, which the applier's is not, because a
// watch acknowledges nothing: what that pull keeps standing ahead of its
// reader holds nothing back from anybody.
//
// # A message too large for a leader's reply is read raw
//
// A stream leader's read of one message (STREAM.MSG.GET, jsapi.Leader; a
// domain log allows no direct get) answers in JSON with the message
// base64-encoded inside it, a third larger than the message
// (jsapi.ReplyBytes): about 11.2 MB for a record of
// statelog.MaxTransportRecordBytes. A leaf's server closes its WHOLE link on
// any message past its max_payload, the transport's eight mebibytes
// (nats-server v2.15.2, leafnode.go's processLeafMsgArgs →
// maxPayloadViolation), cutting off every lease and mailbox the node holds
// across it, and a retry over the redialled link closes it again. So a
// connection across a leaf's link (link.leaf) reads such a message RAW, from
// a consumer's delivery, which carries it as stored ([copyRead]), and only
// such a message: one whose reply the leaf's server carries is the leader's
// read, given its reply's arrival at the link's pace ([Queue.leaderCarries],
// jsapi.Leader.Carrying). Every other connection keeps the leader's read:
// nothing else it crosses holds a message to a max_payload (nats-server
// checks one only on a client's publish and on a leaf's message), and there
// the leader's read is one request where a copy read is a stream's info plus
// consumers the metadata leader makes and deletes — about six metadata
// proposals on a state log's write path ([DomainLog.LastSeq]), failing while
// the metadata group elects a leader.
//
// A DOMAIN LOG decides at open ([DomainLog.copies]), from whether a leaf's
// link stands between and its records may be that large. Its LastSeq reads
// the subject's newest from its headers alone ([copyRead.last]), no record
// crossing; its At finds what the position holds from its headers
// ([copyRead.at]), then takes the leader's read where the leaf's server
// carries that record's reply, given its arrival ([DomainLog.replyArrival]),
// and otherwise reads it raw ([copyRead.message]). Elsewhere the leader's read
// is given its reply's arrival beyond the read term and the lookup ceiling
// ([DomainLog.At], jsprovision.Timing.Arriving). No node opens a state log
// across a leaf's link today: every log is opened by a data node
// (internal/engine's startCore), and a node without `data` is the one that
// joins the members as a leaf.
//
// FROM THE LEADER'S OWN COPY. A replica behind the leader can read an entry
// that exists as gone and one a peer dropped as held, and cannot say how far
// behind it is (filestore.go's LoadNextMsg answers sequence 0 for a match it
// did not find, so "nothing yet" and "nothing any more" look alike). So a
// copy read asks which server leads the stream (jsapi.Leader.Leading) and
// keeps a consumer — one replica, in memory, acknowledging nothing
// ([Queue.throwaway]) — only where its create answer names that server
// ([hosted]; an unclustered server names none and holds the one copy); one
// placed elsewhere is deleted and made again ([copyRead.onLeader]). The
// broker shuffles the live replicas and puts first those its metadata leader
// counts as caught up (raft.go's IsFollowerCaughtUp): uniform in a healthy
// cluster, but just after a metadata election, or while the stream's leader
// lags the metadata leader by three heartbeats, every consumer lands off it.
// So past one draw the read waits a metadata heartbeat before each
// ([placementPause]), and after [placements] of them fails as
// [queue.ErrUnavailable].
//
// EXACTLY THE MESSAGE AT THAT SEQUENCE AND SUBJECT. The consumer's NumPending,
// counted on the leader's copy as it is made, says whether anything is at or
// past the sequence; a delivery before it is refused ([atOrPast]) and only one
// at exactly it is taken. The payload is read from a consumer filtered to the
// subject a headers-only probe found there (a subject never changes), under a
// byte bound the probed message fits and a larger one does not
// ([copyAt.delivered]). Where a peer removed the message since the probe and
// a larger one follows on its subject, the broker answers with the bound
// rather than carry it for nothing, and the sequence is probed again
// ([copyRead.message]).
//
// ONE INBOX, AND A QUIET PULL SETTLED BY THE CONSUMER'S WORD. A read's pulls
// share an inbox held for the whole read (jsapi.Pulls), so a delivery queued
// on the link behind other bytes still lands. A pull quiet for [letterQuiet]
// asks the consumer what it says of itself (jsapi.Pulls.Quiet), given the
// message's crossing time, since that answer crosses behind it. A cluster
// routinely drops a new consumer's first pull: one that holds no pull and
// delivered nothing is made again, up to jsapi.FirstPullRemakes times, as
// internal/objstore/natsobj's reads do; one that holds the pull or has
// delivered is being read however slowly — a whole message at the floor
// takes minutes — and is waited on, on the same inbox.
//
// ONE BOUND FOR THE WHOLE READ. Every request runs under one context
// ([copyRead.bounded]): the caller's, so a caller that goes — a node's
// shutdown among them — stops the read; where it set no deadline, the lookup
// ceiling (jsprovision.LookupBudget) plus the message's raw crossing at the
// link's pace (jsapi.Sending). Not the client's five seconds for one request:
// a copy read is several, and a delivery may wait behind a whole message on
// the link.
//
// # Dead letters
//
// Dead letters live in one stream, CREWLET_DLQ, which every subscription's
// delivery budget writes to, kept by age (the event stream's
// `stream.event_retention_hours`) and never by interest, so an entry stays
// until it ages out whether or not anybody reads it. An entry's keeper header
// is one message header ([letterHeader]) and its body the payload, so a
// reader that needs only what is kept skips the payload: a wake feed's entry
// is a record's whole sealed frame, and the alarm lists the feeds' subjects
// on every data node's beat.
//
// Three reads, each the cheapest that is honest for its reader. A LISTING
// ([Queue.DeadLettersOn]) is one ephemeral headers-only consumer filtered to
// the subject, read until it says nothing is pending and then deleted; the
// broker states the payload's size in its place (nats-server v2.15.2
// consumer.go, convertToHeadersOnly). The pinned server has no headers-only
// read of a single message — STREAM.MSG.GET offers only `no_hdr`
// (jetstream_api.go, JSApiMsgGetRequest) and a direct get always sends the
// payload (stream.go) — so a listing reads the replica the broker placed its
// consumer on (jetstream_cluster.go, createGroupForConsumer), which can be
// behind the leader: it can miss an entry kept a moment ago or show one
// dropped a moment ago, but never hides one that replica holds. THE MARKS
// ([Queue.DeadLetterMarks]) are the leader's: one STREAM.INFO with a subjects
// filter (jsapi.Leader.SubjectsSince) that counts each subject and states the
// last sequence. A reader that needs its listing current holds it to them:
// internal/engine's deadLetterWatch lists again only when the marks move, and
// accepts a listing only when its counts match them. A FRAME
// ([Queue.DeadLetterFrame]) is the one read that moves a payload. Its one
// caller, a feed's replay, acts on it, so it never reports an entry gone
// because a replica had not heard of it: it is the leader's STREAM.MSG.GET
// wherever that reply reaches this node, and across a leaf's link it is found
// from its headers on the leader's own copy ([copyRead.at]) and read as "A
// message too large for a leader's reply is read raw" says.
//
// ONE ENTRY PER IDENTITY. A keep names an identity
// ([queue.EventQueue.KeepDeadLetter]), carried as the message's Nats-Msg-Id
// ([letterID]). The stream's duplicate window is [queue.DeadLetterWindow], set
// where the stream is declared ([engineStreams]); the broker refuses the
// stream if its retention is shorter, and Tier A refuses it first. Within the
// window a second keep of one identity is acknowledged as a duplicate: the
// contract's "already kept". The delivery budget's dead letter
// ([Queue.deadLetter]), on poison's or the unhandled deliveries' subject
// ([queue.RetiredTo]), names one too: the stored message's stream, sequence
// and instant ([letterOf]). A failed keep goes back for the next delivery to
// retry, and the broker's backstop outlasts the budget for that
// ([keepRetries]); a keep that stored its entry but lost its answer counts as
// failed, and the identity makes its retry come to nothing. A copy too large
// to carry the identity header is kept without one, so it may be kept twice
// at worst.
//
// A DROP ([Queue.DropDeadLetter]) checks which subject the sequence is on
// before the leader deletes it. Where the leader's read reaches this node the
// check is that read, moving the one payload being removed, since the server
// has no read of a subject alone; across a leaf's link it reads the entry's
// headers alone on the leader's own copy ([copyRead.at]). Never a replica's
// copy, where an entry the replica had not heard of would read as gone and
// the drop would answer done having removed nothing.
//
// # A domain consumer is held to its node's rows
//
// A node's domain consumer is kept at open only when it hands over exactly
// what a fresh one at the checkpoint's next record would ([driftOf]); it can
// disagree with the SQL checkpoint four ways ([consumerDrift] has their
// causes). ACKNOWLEDGED PAST: the broker never delivers those records again,
// so the loop waits for ever. On a strict log the node stays unhydrated,
// writes on the missing subjects refuse `behind`, and the heartbeat's
// below-floor check never fires because the records are still on the log; on
// a compacted log nothing checks contiguity, so the node applies what comes
// next and reports itself caught up with those rows absent. DELIVERED PAST: a
// strict log waits behind the first of them in its reorder buffer until the
// ack window ends, and the ack floor is only a lower bound, so records
// acknowledged one by one above it may never return. IN FLIGHT at or below
// the checkpoint: harmless records, but a full in-flight ceiling hands over
// nothing new for the ack window. BEHIND: costs the redeliveries between for
// the applier to drop; a node that adopted a snapshot at boot has everything
// from the log's first surviving record up to the artefact's position, and
// nothing else on the boot path moves it.
//
// Every drift is rebuilt (one replicated delete and create, what every node
// pays on its first boot), and the rebuild touches only this node: the
// consumer is named after it, no peer reads through it, and no retention term
// reads its acknowledgements (the trim takes the SQL positions the heartbeat
// publishes). When the rebuild fails, behind and in flight keep whichever
// consumer the broker holds, with a warning, since failing a boot over an
// optimisation's metadata write is worse than not rebuilding; the two drifts
// ahead of the rows fail the open, as does a broker that holds no consumer
// afterwards or cannot say which it holds, because a handle kept on a guess
// fails every fetch while the boot reports success
// ([DomainConsumer.afterFailedRebuild]).
//
// # Every connection presents a key
//
// ADR-0094. Every embedded broker this package starts — a solo member, a
// clustered member, a member with a leaf listener, a leaf — installs ONE
// authenticator (brokerAuth, nats-server's embedding hooks
// CustomClientAuthentication and, on a clustered member,
// CustomRouterAuthentication), and it admits exactly four kinds of connection:
//
//   - a ROUTE presenting the username and password derived from a key of this
//     node's ring under [RouteKeyDomain];
//   - a LEAF LINK presenting the nkey derived from a key of the ring under
//     [LeafKeyDomain], signing the member's nonce;
//   - an OPERATOR on a clustered member's loopback client listener presenting
//     the nkey derived under [ClientKeyDomain] and signing its nonce — the nats
//     tool, for the broker gestures no engine verb performs; bound to the
//     global account, never the system one;
//   - an IN-PROCESS client of this process signing with a key minted at each
//     start that nothing outside the process ever holds: the engine's, bound to
//     the global account, and the membership code's, bound to the system
//     account (membership.go).
//
// Every other connection is refused, counted ([Server.OnPeerRefused]) and
// logged once a minute per class (broker_peer_refused); nats-server's own
// line per refused attempt goes to DEBUG (natsLogger). By topology:
//
//	solo member                   no listener; this process alone
//	member serving leaves only    leaf listener; + leaves presenting a ring key
//	clustered member              route listener, loopback client listener,
//	                              optional leaf listener; + routes and leaves
//	                              presenting a ring key, + an operator on the
//	                              client listener from this host
//	leaf                          no listener; one outbound link presenting
//	                              the active key
//
// THE KEYS ARE THE KEYRING'S, derived per key ([FleetAuthFrom]) by the rule
// every consumer of secrets.keys follows (internal/runtoken), so the keyring's
// rotation runbook moves the broker with no step of its own: every node accepts
// every key of its ring and presents the active one.
//
// WHY THE EMBEDDING HOOKS AND NOT NATS-SERVER'S OWN BLOCKS. A cluster's
// authorization block holds one user, a leaf listener's one nkey or a password
// list, and the client user list cannot refuse anonymity on a member that
// declares a system account — measured: with the system key in the server's own
// nkey list an anonymous client still connected, through the no-auth user the
// server adds to the global account. None of them carries a rotation without a
// secret on the wire or an anonymous path; the hooks carry both.
//
// WHAT IT DOES NOT DO is encrypt. A route's password crosses the fleet network
// in clear once per connection — a NATS route carries a password or a
// certificate, never a signature — and every byte after it crosses in clear,
// as before. So the fleet network stays private, and the state log's records
// and the fleet's instructions stay signed (ADR-0031): the admission is per
// connection and only as private as that network, and a record is believed on
// its own signature, never on the connection it arrived by.
package jetstream

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Stream names. Kept short and uppercase because they appear in every
// JetStream API call and in `nats stream ls` output an operator reads.
const (
	streamAgent         = "CREWLET_AGENT"
	streamEvents        = "CREWLET_EVENTS"
	streamNotifications = "CREWLET_NOTIFICATIONS"
	streamConfig        = "CREWLET_CONFIG"
	streamDeadLetter    = "CREWLET_DLQ"
	streamCustody       = "CREWLET_CUSTODY"
	streamMemory        = topics.MemoryStream

	// derivedPrefix names a stream provisioned for a subject namespace the
	// engine does not itself define.
	derivedPrefix = "CREWLET_NS_"
)

// maxDeliver is the delivery budget before a message is dead-lettered.
//
// It covers poison, node death AND handoff: a deferred delivery returns via
// Nak, which counts, hence 25 rather than the ~10 a free handoff needs. A
// message would have to be in flight across 25 rate-limited seat migrations to
// exhaust it. No cap tells a fast crash-loop from poison.
const maxDeliver = 25

// budgetFor resolves a queue's delivery budget, which a test shrinks so the
// dead-letter assertions do not need dozens of handler invocations each.
func budgetFor(cfg Config) int {
	if cfg.MaxDeliver > 0 {
		return cfg.MaxDeliver
	}
	return maxDeliver
}

// keepRetries is how many deliveries past its budget the broker still makes of
// a subscription's message ([backstopFor]), for a dead letter's keep to retry
// in. The loop retires a message at its budget by keeping its copy, then
// terminating it (attachment.retire); a keep can fail with nothing wrong with
// the message (a stop, a killed node, a leader election), and a backstop AT the
// budget left it no delivery to retry in. A retry keeps under the message's own
// identity, so one after a keep that stored but lost its answer stores nothing.
//
// Five: each returns at the failure path's ~30 s ceiling, so five span 2.5
// minutes, many elections over, plus the one redelivery, at [DefaultAckWait],
// of a message a stopped node held. No retry answers a full stream or an
// external account's limits; the keep at the backstop reports the copy lost.
const keepRetries = 5

// backstopFor is the broker's own MaxDeliver for a subscription's consumer: its
// budget plus [keepRetries]. The consume loop, never the broker, retires a
// message at its budget, so this bounds only retried keeps, and a bug there
// cannot circle a message forever.
func backstopFor(cfg Config) int { return budgetFor(cfg) + keepRetries }

// DefaultAckWait bounds how long a fetched-unacked message stays invisible: a
// wait behind a running turn plus one worst-case turn. A backstop, not the
// handoff path: a seat that loses its lease Naks explicitly.
//
// Exported because it is how long a dead node's delivery stays silent before
// redelivery, which internal/maintenance's ChannelIdleTimeout is sized against
// so it does not close the channel of an ask about to come round again
// (internal/engine holds the order between the two).
const DefaultAckWait = 30 * time.Minute

// defaultEventRetention bounds the event stream. The stream's age limit is the
// authority, and the local materialized index mirrors it.
const defaultEventRetention = 30 * 24 * time.Hour

// streamSpec describes the stream carrying a subject namespace.
//
// Retention differs by purpose: agent inboxes and notification work queues
// use INTEREST, the engine's mailbox semantic (kept while an unacked durable
// consumer exists; a subject no subscription covers drops it, which is why
// EnsureSubscription runs before anything publishes). The event stream uses
// LIMITS with an age bound, for consumers that fall behind and catch up.
//
// A stream another node created, perhaps from a different Tier A file, may
// differ; what a boot does depends on WHICH field differs ([specFieldClass],
// [classifyStreamSpec]), and TestEveryStreamSpecFieldIsClassified fails a
// field nobody classified.
type streamSpec struct {
	name      string
	subjects  []string
	retention jetstream.RetentionPolicy
	maxAge    time.Duration

	// maxPerSubject keeps only the newest message on each subject, turning
	// a log into a keyed table. Zero is unlimited.
	maxPerSubject int

	// maxBytes caps the stream's size on disk; zero is unlimited. CAPACITY:
	// a boot refusing over a disagreement would take a fleet down for a
	// number nobody is losing data to.
	maxBytes int64

	// maxMsgSize is the largest message stored, headers and body, never the
	// subject; zero is unlimited (the server's -1). CAPACITY, for maxBytes'
	// reason: a record past it is refused loudly, never lost.
	maxMsgSize int32

	// discard decides what a full stream does: DiscardNew refuses the
	// append, DiscardOld drops the oldest message. SAFETY, the sharpest
	// here: a log of the company's state refuses loudly and the operator
	// raises the ceiling, rather than silently dropping its oldest record.
	discard jetstream.DiscardPolicy

	// duplicates is the window in which a repeated Nats-Msg-Id is
	// collapsed. CAPACITY: a window shorter than a publisher's retry budget
	// is the PUBLISHER's problem, so the framework derives its budget from
	// this value rather than asserting the stream's.
	duplicates time.Duration

	// denyDelete refuses the delete-message API. SAFETY, true for anything
	// holding durable state: a KV bucket gets it free, a plain stream not.
	denyDelete bool

	// allowRollup lets a publisher replace the whole history with one
	// message. SAFETY, false for a log: a rollup is a delete spelled as a
	// publish.
	allowRollup bool

	// allowDirect and mirrorDirect let a client read from a FOLLOWER.
	// SAFETY, false wherever the framework reads a last-message answer: a
	// follower may be behind an acknowledged write and answer "no message
	// here" for a message the quorum has.
	allowDirect  bool
	mirrorDirect bool
}

// specFieldClass is what a boot does when the running stream's value for a
// field differs from this node's spec.
//
// The replication factor has no class: it is Tier A's, identical for every
// stream on the node, so not a [streamSpec] field; a boot answers for
// durability in [Queue.observeStream].
type specFieldClass int

const (
	// classIdentity names the stream: a difference is a different stream.
	classIdentity specFieldClass = iota

	// classSafety changes what the stream MEANS. A mismatch REFUSES TO RUN,
	// naming the field and the observed and expected values: carrying on
	// would publish durable records into a stream that drops them, replays
	// them at wall-clock speed, or lets any client delete them.
	classSafety

	// classCapacity is a ceiling: REPORTED, not acted on. It is the
	// operator's question, with its own verb, and refusing to boot over it
	// would take a fleet down for a number nobody is losing data to.
	classCapacity
)

// classifyStreamSpec is the one place a spec field's class is decided, keyed
// on the struct field name and held exhaustive by
// TestEveryStreamSpecFieldIsClassified.
func classifyStreamSpec() map[string]specFieldClass {
	return map[string]specFieldClass{
		"name": classIdentity,

		"subjects":      classSafety,
		"retention":     classSafety,
		"maxAge":        classSafety,
		"maxPerSubject": classSafety,
		"discard":       classSafety,
		"denyDelete":    classSafety,
		"allowRollup":   classSafety,
		"allowDirect":   classSafety,
		"mirrorDirect":  classSafety,

		"maxBytes":   classCapacity,
		"maxMsgSize": classCapacity,
		"duplicates": classCapacity,
	}
}

// engineStreams is the topology for the subjects the engine defines. Other
// namespaces are provisioned on demand — see specForSubject.
func engineStreams(eventRetention time.Duration) []streamSpec {
	if eventRetention <= 0 {
		eventRetention = defaultEventRetention
	}
	return []streamSpec{
		{
			name:      streamAgent,
			subjects:  []string{topics.AgentInboxPrefix + ">"},
			retention: jetstream.InterestPolicy,
		},
		{
			name:      streamNotifications,
			subjects:  []string{topics.NotificationsPrefix + ">"},
			retention: jetstream.InterestPolicy,
		},
		{
			name:      streamEvents,
			subjects:  []string{topics.EventsPrefix + ">"},
			retention: jetstream.LimitsPolicy,
			maxAge:    eventRetention,
		},
		{
			// Config nudges are best-effort: a lost one costs a poll
			// interval, never a revision, since the authoritative path
			// polls the epoch pointer. The short age keeps a restarted
			// node from replaying stale activation announcements.
			name:      streamConfig,
			subjects:  []string{topics.ConfigPrefix + ">"},
			retention: jetstream.LimitsPolicy,
			maxAge:    time.Hour,
		},
		{
			// A seat's memory, one subject per row, ONE MESSAGE PER
			// SUBJECT: a keyed table, so a node acquiring a seat reads
			// the memory as it stands, not every write that produced it.
			//
			// No age bound: what a seat still remembers is the learning
			// subsystem's decision, and expiry here would delete memory
			// the seat still holds locally. No byte or count limit
			// either: with discard old, one would drop the oldest row
			// for any message, a hydration's barrier among them
			// (topics.MemoryBarrierToken), where one per subject drops
			// only what a newer message on that subject replaces.
			name:          streamMemory,
			subjects:      []string{topics.MemoryPrefix + ">"},
			retention:     jetstream.LimitsPolicy,
			maxPerSubject: 1,
		},
		{
			// A stateless node's events on their way into one data
			// node's log. INTEREST: a mailbox, kept until the custody
			// group takes it, whether or not a data node is attached.
			// The event log's age, because a batch older than that holds
			// events past the horizon every log reads to.
			name:      streamCustody,
			subjects:  []string{topics.CustodyPrefix + ">"},
			retention: jetstream.InterestPolicy,
			maxAge:    eventRetention,
		},
		{
			// Kept by age, not interest: nothing consumes dead letters
			// automatically, and an operator investigating poison needs
			// them.
			//
			// One identity is one entry: a keep carries its identity as
			// its message id (letterMsg), and this window
			// (queue.DeadLetterWindow, 15 min) outlasts a keep's longest
			// wait across a link at the floor, the request term plus
			// three sends of 8 MiB at 30 s/MiB (5 s + 3 × 240 s, ~12
			// min), so a second copy still crossing is stored as nothing.
			// The broker, and Tier A before it, refuse a retention
			// shorter than the window.
			name:       streamDeadLetter,
			subjects:   []string{topics.DeadLetterPrefix + ">"},
			retention:  jetstream.LimitsPolicy,
			maxAge:     eventRetention,
			duplicates: queue.DeadLetterWindow,
		},
		{
			// Which of a subscription's messages failed as themselves at
			// a delivery: an empty mark per message, ONE PER SUBJECT, so
			// a check is one read of its subject's newest message
			// (failuremarks.go). Aged like the dead letters it decides
			// the subject of, so a mark nobody deleted (its message
			// settled by a member that never wrote it) goes with them.
			name:          streamFailures,
			subjects:      []string{failurePrefix + ">"},
			retention:     jetstream.LimitsPolicy,
			maxAge:        eventRetention,
			maxPerSubject: 1,
		},
	}
}

// specForSubject reports which stream carries a subject: the engine's own
// subjects get their purposeful stream, anything else a stream named for its
// leading segment, provisioned on demand with interest retention (the mailbox
// semantic the contract promises everywhere).
//
// Provisioning, not refusing: the SUBJECT SPACE is the engine's, and an
// extension or test publishing under its own namespace uses the contract
// correctly. A whitelist would make the backend the authority on names.
func specForSubject(subject string, eventRetention time.Duration) (streamSpec, error) {
	if subject == "" {
		return streamSpec{}, fmt.Errorf("%w: empty subject", ErrSubject)
	}
	if strings.ContainsAny(subject, " \t\r\n") {
		return streamSpec{}, fmt.Errorf("%w: %q contains whitespace", ErrSubject, subject)
	}
	if strings.Contains(subject, "..") || strings.HasPrefix(subject, ".") || strings.HasSuffix(subject, ".") {
		// An unroutable handle produces one (crewlet.agent..inbox): a
		// real subject nobody subscribes to, swallowing events silently.
		return streamSpec{}, fmt.Errorf("%w: %q has an empty segment", ErrSubject, subject)
	}

	for _, spec := range engineStreams(eventRetention) {
		for _, pattern := range spec.subjects {
			if topics.Match(pattern, subject) {
				return spec, nil
			}
		}
	}

	ns, _, _ := strings.Cut(subject, ".")
	name, err := streamNameFor(ns)
	if err != nil {
		return streamSpec{}, err
	}
	return streamSpec{
		name:      name,
		subjects:  []string{ns, ns + ".>"},
		retention: jetstream.InterestPolicy,
	}, nil
}

// specForPattern reports which stream a wildcard subscription reads from.
//
// A pattern is resolved through the subject it would match, so a broadcast
// subscription and a publish never disagree about where the messages are.
func specForPattern(pattern string, eventRetention time.Duration) (streamSpec, error) {
	probe := strings.TrimSuffix(strings.TrimSuffix(pattern, ">"), "*")
	probe = strings.TrimSuffix(probe, ".")
	if probe == "" {
		return streamSpec{}, fmt.Errorf("%w: pattern %q spans every namespace", ErrSubject, pattern)
	}
	return specForSubject(probe+".probe", eventRetention)
}

// namespaceName is the shape a subject's first segment must already have to
// be usable as a stream name: lowercase, digits and underscore.
var namespaceName = regexp.MustCompile(`^[a-z0-9_]+$`)

// streamNameFor maps a subject's namespace onto a legal stream name.
//
// It VALIDATES rather than sanitizes: JetStream forbids dots, spaces and
// wildcards in stream names, and a lossy rewrite ALIASES (`a-b` and `a_b`
// would both become `A_B`, and provisioning the second would take the first's
// subjects). The contract lets a backend decline a name it cannot represent,
// and every namespace the engine publishes to already has this shape.
func streamNameFor(ns string) (string, error) {
	if !namespaceName.MatchString(ns) {
		return "", fmt.Errorf(
			"%w: namespace %q must match %s to be a stream name; a rewrite would let "+
				"two namespaces share one stream", ErrSubject, ns, namespaceName)
	}
	return derivedPrefix + strings.ToUpper(ns), nil
}

// consumerNameMax bounds the readable part of a durable name. NATS allows
// more; this leaves room for the hash and keeps a name an operator can read
// in a `nats consumer ls` listing.
const consumerNameMax = 180

// consumerName maps a (topic, group) pair onto a durable consumer name, and it
// must be INJECTIVE. Durable names forbid dots, spaces and wildcards, so the
// readable part is a lossy rewrite, and lossy alone aliases: topics `a.b` and
// `a_b` in one group would share a mailbox (the conformance suite's
// distinct_pairs_never_share_a_subscription holds this).
//
// So the readable part is for operators and the HASH is the identity: over the
// raw pair joined by NUL, which neither half can contain. Forty-eight bits put
// a collision below one in ten million at ten thousand subscriptions, against
// a real ceiling in the hundreds.
func consumerName(topic, group string) string {
	sum := sha256.Sum256([]byte(group + "\x00" + topic))
	id := hex.EncodeToString(sum[:consumerDigestBytes])

	readable := consumerNameSafe(group) + consumerNameSep + consumerNameSafe(topic)
	// Truncation cannot reintroduce an alias: the digest is over the full
	// pair and is appended after it.
	if max := consumerNameMax - len(id) - len(consumerNameSep); len(readable) > max {
		// On a character boundary: a byte slice through a multi-byte
		// group or topic is a consumer name that is not valid UTF-8.
		readable = textcut.Bytes(readable, max)
	}
	return readable + consumerNameSep + id
}

// consumerDigestBytes is how much of the pair's digest ends a durable name, and
// consumerNameSep what joins the three parts.
const (
	consumerDigestBytes = 6
	consumerNameSep     = "__"
)

// consumerNameSafe is the lossy rewrite that makes a name part legal in a
// durable consumer name.
func consumerNameSafe(s string) string {
	return strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_").Replace(s)
}

// DomainStream is the stream a statelog domain declares, in the domain's
// vocabulary rather than the broker's.
//
// Narrow on purpose: a domain says what its log IS (name, subject space, how
// big it may grow), and the safety fields are the framework's, identical for
// every domain, so no domain can build a log that drops its oldest record.
type DomainStream struct {
	// Name is the stream. Conventionally CREWLET_<DOMAIN>_LOG.
	Name string

	// Subjects is the subject space the domain publishes into.
	Subjects []string

	// MaxBytes is the ceiling. Zero is unlimited, which no shipped domain
	// wants: a log with no ceiling fills the volume instead of refusing.
	MaxBytes int64

	// MaxMessageBytes is the largest message the stream stores, headers
	// included. Zero is unlimited, which no shipped domain wants: this
	// build's publisher refuses a larger record before sending, but only
	// the broker's refusal holds a peer on another build to it.
	MaxMessageBytes int64

	// Duplicates is the window a repeated Nats-Msg-Id is collapsed in. It
	// has to outlast a publisher's whole retry budget, or a retry that
	// takes longer than the window appends the record twice.
	Duplicates time.Duration

	// MaxPerSubject keeps only the newest message per subject: zero is a
	// log, one the KEYED TABLE a derived, recomputable domain uses. It
	// changes what replay means: compaction removes INTERIOR sequences, so
	// a loop treating contiguity as an invariant stalls on the first
	// ordinary write; the framework refuses the mismatch rather than
	// inferring the loop.
	MaxPerSubject int

	// MaxAge bounds how long a message is kept, legitimate only on a
	// compacted stream whose rows are durable in SQL on every node, so
	// expiry deletes nothing. ZERO ON A LOG: an age bound deletes records
	// a node has not applied.
	MaxAge time.Duration

	// No replicas field: the replication factor is Tier A's, identical for
	// every stream on the node, so a domain cannot choose its durability.
	//
	// No replay either: it is a CONSUMER policy (jetstream.ReplayPolicy on
	// ConsumerConfig), set on the replication consumer the framework
	// creates, where the zero value is ReplayInstantPolicy and the guard
	// that matters asserts nothing sets ReplayOriginal.
}

// spec renders the domain's declaration as the broker-level spec, with every
// safety field the framework's own.
func (d DomainStream) spec() streamSpec {
	return streamSpec{
		name:      d.Name,
		subjects:  d.Subjects,
		retention: jetstream.LimitsPolicy,
		maxBytes:  d.MaxBytes,

		// Checked at the door ([Queue.EnsureDomainStream]), the only
		// caller, so this conversion never wraps.
		maxMsgSize: int32(d.MaxMessageBytes),

		// Refuse the append, never drop a record (streamSpec.discard).
		discard: jetstream.DiscardNew,

		duplicates: d.Duplicates,

		// Not deletable, not roll-uppable (streamSpec.denyDelete).
		denyDelete:  true,
		allowRollup: false,

		// No direct gets: the framework reads a subject's last message to
		// decide whether a write raced (streamSpec.allowDirect).
		allowDirect:  false,
		mirrorDirect: false,

		// A log keeps every record until the trim moves the floor; a
		// compacted stream may bound its age (DomainStream.MaxAge).
		maxAge:        d.MaxAge,
		maxPerSubject: d.MaxPerSubject,
	}
}
