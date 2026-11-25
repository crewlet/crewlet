// Package jetstream implements the EventQueue contract on NATS JetStream,
// embedded in this process by default and reachable as an external cluster
// when configured.
//
// Embedded versus external is a connection choice, not a second backend.
//
// An external server must promise what the embedded one is built with, the
// contract's message size
// ([github.com/crewlet/crewlet/internal/queue.MaxPayloadBytes]): the client
// refuses a message past what its server announces, so a server left at
// nats-server's 1 MiB default takes every small write and refuses the first
// large one. Every connection is held to the contract on the limit its server
// announces once authenticated (its own max_payload, or its account's where
// lower); one that falls short is a refused boot naming the setting, not a node
// that serves and then cannot write (carriesTheContract).
//
// # Why JetStream fits
//
// Crewlet's subject grammar IS NATS grammar: dot-separated segments with `*`
// and `>` wildcards. A durable consumer can be created with nothing attached,
// which a seat's mailbox is built on: an ordinary API call, where a broker that
// creates a subscription only by joining it needs an out-of-band admin call to
// avoid stealing a live peer's traffic. Pull consumers push nothing into a
// client-side queue, so a wedged node holds no mail it has not fetched.
//
// # What it costs, measured
//
// There is no free handoff: every path back to the broker increments the
// delivery count, absorbed by the delivery budget (maxDeliver). Conversation
// order is by event timestamp (queue.OrderForDispatch). A CONSUMER'S COUNTERS
// DO NOT READ YOUR OWN WRITES (the server counts a message after acking its
// publisher), so anything that must be synchronous with a returned publish
// reads the STREAM's own state (Backlog).
//
// A FAILURE OR A PASS GOES BACK ONCE ITS LOOP HOLDS THE NEXT DELIVERY or heard
// none waits ([attachment.sendSpaced]): sent sooner, its backoff races that
// pull, and a due redelivery is served before new mail. A process dying first
// leaves it to its ack window.
//
// # What a broker that does not answer looks like from above
//
// A call waiting on an unreachable broker often ends only on a deadline. Every
// verb that calls the broker therefore marks its failure with the contract's
// word before returning ([Queue.brokerFailed]): queue.ErrUnavailable for
// silence, which layers above answer with "try again"; [ErrConnectionLost],
// which is queue.ErrNotLive, for a connection closed for good. Unmarked, each
// reads above the queue as this node's own fault. The domain-log seam (DomainLog,
// DomainConsumer) is deliberately not marked: internal/statelog classifies its
// own appends and reads, and a second classification beneath it would be a
// second answer to one question.
//
// # A write across a link is given the time its bytes take to cross
//
// A write (a Publish, a dead letter's keep, a state log's Append) returns when
// its stream acknowledges it, once its last byte is in. A leaf's link
// to the members and an external cluster's network may be as slow as the object
// store's floor, a mebibyte in thirty seconds (objstore.MiBPace). The pace is
// decided once, where the connection is made (linkOf), and every crossing on it
// reads it.
//
// A write whose caller set no deadline waits the request term
// (jsapi.RequestTerm, five seconds) plus, across a link, its bytes at that pace
// for each of its first three sends (jsapi.WriteSends; Queue.writing,
// jsapi.Budget). Sends are the queue's own (sendWithin; the client's re-sends
// are off): a write is sent again where nobody heard it, and an append where the
// leader refused it behind another write to its subject, each send carrying the
// whole message again. A caller's own deadline is kept, never extended.
//
// Held to five seconds alone, a write at the transport's eight-mebibyte ceiling
// failed as queue.ErrUnavailable mid-crossing on a link slower than 1.6 MiB/s,
// and a keeper retries an unavailable broker for ever, each copy landing behind
// the last (crewlet/crewlet#1005). A link slower than the floor still fails a
// write, and so can one busy with something ahead of it whose bytes no budget
// counts. The worst case, about twelve minutes over three sends, holds
// everything the node sends behind it on the data link, and on an external
// broker the client checks no deadline while a request waits for the
// connection's lock, which a large write holds for its whole socket Write. A
// node's holds ride neither (below).
//
// # What a link carries is read at its socket
//
// A node crossing a link to the fleet's broker must tell a link that is
// carrying something from one that carries nothing, because a message can take
// four minutes to cross at the slowest pace. No library counter can say: they
// count a message only once it is whole. The kernel's do not stand still. So the
// link's SOCKET is read (LinkMeter): bytes read off it, and bytes the far end's
// kernel acknowledged of what it wrote (internal/sockcount), every quarter of a
// read term, the last objstore.Stall of readings kept. A link is MOVING when it
// carried, in either direction, at least seven tenths of what the floor carries
// over the span asked about, midway between a quiet link's background and a busy
// one's floor (movingShare). An external broker's connections are each metered
// as one link, reconnects included (infoDialer); a leaf's is dialled by
// nats-server, which gives an embedder no socket, so the leaf dials through a
// loopback HTTP CONNECT tunnel whose upstream socket the meter reads (tunnel). A
// member crosses no link and has no meter (Queue.LinkMeter).
//
// It cannot see past a proxy that terminates TCP (a mesh sidecar, a buffering
// load balancer), which acknowledges what it buffered; the deployment guides
// require the link to reach the broker's own sockets.
//
// WHAT READS IT (ADR-0155): a wait across a link ends for silence only while
// the link carries nothing at the floor. A seat's hydration
// (internal/learning/memsync, Config.Link) refuses a replay that heard nothing
// only where the link did not move over it, and internal/jsprovision's
// Timing.Following holds hedged requests' ceilings past their spans while it
// moves.
//
// # A node's leases ride a link of their own
//
// A renewal queued behind a large write crosses after it, at the floor past
// every lease (ADR-0174). Queue.DialLeases opens a watched connection nothing
// else rides: on a leaf a second leaf link in LeaseAccount, past the tunnel and
// ISOLATED from other leaves' interest; on an external broker a second dial
// that gives a dead socket up within a heartbeat of the shortest lease; on a
// member one in process. Each link is read by its account, never by a count.
//
// Two links can land apart, so a hold waits on Queue.DataPath (ADR-0173): a
// probe of the fleet's JetStream refused past a request's term while the data
// link carried nothing, or past the link's own ping tolerance. The lease link
// gets the same probe, unmetered, and is moved off a member that answers
// nothing, which still answers its pings.
//
// # A pull allocates for what it receives, never for what it may
//
// The client library sizes a fetch's delivery channel by the BATCH a pull may
// receive, and its byte-bounded fetch fixes that batch at a million: 32 MiB per
// call even on an empty log, an OOM kill for an applier pulling twice a second. The count-only fetch cannot bound bytes.
// [DomainConsumer.Fetch] therefore makes the server's pull request itself, count
// and byte bound together, and every request of a consumer is answered on ONE
// standing inbox, so a delivery to a request the client stopped counting reaches
// the next fetch rather than waiting out the ack window. pullrequest.go says why
// not the library's Messages iterator. The live feed's and a watch's pulls are
// the library's standing Consume; a domain group's ([DomainLog.Group]) is a
// Messages iterator.
//
// # A linger window ends on a clock the conformance suite can move
//
// A batch drain takes its linger deadline once its first message is in hand and
// reads it again after every fetch. That decision, and nothing else, reads the
// [lingerClock] its queue was handed (Config.clock, unexported): the wall clock,
// unless this package's own tests hand another, so the conformance suite's
// LingerClock capability can land a pause or a stop inside a window and only then
// end it. Each fetch inside the window keeps its expiry on the broker's and the
// client's REAL clocks, since a fake clock under it would bend the broker's
// timing, not the drain's decision.
//
// # A handler runs inline on the goroutine that took its delivery
//
// A Subscribe or SubscribeBatch handler is called inline on its attachment's
// consume loop, so the loop fetches again only once the handler has returned,
// and a listener's handler inline in the callback nats.go calls for its
// subscription. Listen gives a message no goroutine of its own because there is
// nothing to reply to; Serve's answers each run on one because an asker waits on
// every one. That rests on nats.go's guarantee, which this package does not hold:
// it runs a callback subscription's messages on one goroutine, one after another.
// Together they are the contract's one message at a time per listener.
// Crewlet's half is held by
// TestAHandlerRunsInlineOnTheGoroutineThatTookItsDelivery, which follows each
// handler from the verb handed it to the call that runs it and refuses a go
// statement, a function literal or a function off that path; a handler off that
// goroutine is a production decision of its own, never a test's.
//
// # A watch is told which records moved, and never carries them
//
// A reader that keeps its work in rows (Gitlet's home node) needs to know only
// that an object moved, sooner than its pass over the rows would find it. So a
// state log's watch ([DomainLog.Watch]) is a consumer of its own taking each
// new record's SUBJECT and position and never its body: headers only, from the
// log's end, acknowledging nothing, one replica in memory on one member. What it
// may have missed it says it may have missed, and how far on the log; the caller
// waits for its copy to reach that position, then reads its rows, which are the
// record. A nudge only ever shortens a wait.
//
// # A message too large for a leader's reply is read raw
//
// A stream leader's read of one message (STREAM.MSG.GET, jsapi.Leader; a domain
// log allows no direct get) answers in JSON with the message base64-encoded, a
// third larger (jsapi.ReplyBytes). A leaf's server closes its WHOLE link on any
// message past its max_payload, the transport's eight mebibytes, cutting off
// every lease and mailbox the node holds across it, and a retry closes it again.
// So a connection across a leaf's link (link.leaf) reads such a message RAW, from
// a consumer's delivery, which carries it as stored ([copyRead]), and only such a
// message: one whose reply the leaf's server carries is the leader's read, given
// its reply's arrival at the link's pace ([Queue.leaderCarries],
// jsapi.Leader.Carrying). Every other connection keeps the leader's read, one
// request where a copy read is a stream's info plus consumers the metadata
// leader makes and deletes.
//
// A DOMAIN LOG decides at open ([DomainLog.copies]) whether a leaf's link stands
// between it and records that may be that large; no node opens one so today
// (every log is opened by a data node, internal/engine's startCore). Its LastSeq
// and At's lookup read headers alone ([copyRead.last], [copyRead.at]); At then
// takes the leader's read where the leaf's server carries that reply
// ([DomainLog.replyArrival]), and otherwise reads raw ([copyRead.message]).
//
// FROM THE LEADER'S OWN COPY. A replica behind the leader can read an entry that
// exists as gone and one a peer dropped as held. So a copy read keeps a consumer
// (one replica, in memory, acknowledging nothing, [Queue.throwaway]) only where
// its create answer names the stream's leader (jsapi.Leader.Leading, [hosted]);
// one placed elsewhere is deleted and made again ([copyRead.onLeader]), waiting a
// metadata heartbeat before each draw past the first ([placementPause]) and
// failing as [queue.ErrUnavailable] after [placements] of them.
//
// EXACTLY THE MESSAGE AT THAT SEQUENCE AND SUBJECT. The consumer's NumPending,
// counted on the leader's copy as it is made, says whether anything is at or past
// the sequence; a delivery before it is refused ([atOrPast]). The payload is read
// from a consumer filtered to the subject a headers-only probe found there, under
// a byte bound the probed message fits and a larger one does not
// ([copyAt.delivered]), so a message a peer removed since the probe is probed
// again, not carried ([copyRead.message]). A read's pulls share one inbox
// (jsapi.Pulls), a new consumer's dropped first pull is made again
// (jsapi.FirstPullRemakes), and every request runs under one context
// ([copyRead.bounded]): the caller's, or the lookup ceiling
// (jsprovision.LookupBudget) plus the message's raw crossing (jsapi.Sending).
//
// # Dead letters
//
// Dead letters live in one stream, CREWLET_DLQ, which every subscription's
// delivery budget writes to, kept by age (the event stream's
// `stream.event_retention_hours`) whether or not anybody reads them. An entry's
// keeper header is one message header ([letterHeader]), so a reader that needs
// only what is kept skips the payload.
//
// Three reads, each the cheapest that is honest for its reader. A LISTING
// ([Queue.DeadLettersOn]) is one ephemeral headers-only consumer filtered to the
// subject, on the replica the broker placed it on, which can be behind the
// leader: it can miss an entry kept a moment ago or show one dropped a moment
// ago, but never hides one that replica holds. THE MARKS
// ([Queue.DeadLetterMarks]) are the leader's, one STREAM.INFO with a subjects
// filter (jsapi.Leader.SubjectsSince): internal/engine's deadLetterWatch lists
// again only when they move, accepting a listing only when its counts match. A
// FRAME ([Queue.DeadLetterFrame]) is the one read that moves a payload, and its
// one caller, a feed's replay, acts on it, so it is the leader's STREAM.MSG.GET,
// or across a leaf's link a raw read found from headers on the leader's own copy
// ([copyRead.at]), never a replica's, where an entry the replica had not heard
// of would read as gone. A DROP ([Queue.DropDeadLetter]) checks the same way.
//
// ONE ENTRY PER IDENTITY. A keep names an identity
// ([queue.EventQueue.KeepDeadLetter]), carried as the message's Nats-Msg-Id
// ([letterID]); the delivery budget's dead letter ([Queue.deadLetter], on
// poison's or the unhandled deliveries' subject, [queue.RetiredTo]) names one
// from the stored message's stream, sequence and instant ([letterOf]). Within the
// stream's duplicate window, [queue.DeadLetterWindow] (set where the stream is
// declared, [engineStreams]), a second keep of one identity is acknowledged as a
// duplicate: the contract's "already kept". A failed keep goes back for the next
// delivery to retry ([keepRetries]); a keep that stored its entry but lost its
// answer counts as failed, and the identity makes its retry come to nothing.
//
// # A domain consumer is held to its node's rows
//
// A node's domain consumer is kept at open only when it hands over exactly what
// a fresh one at the checkpoint's next record would ([driftOf]); it can disagree
// with the SQL checkpoint four ways ([consumerDrift] has their causes).
// ACKNOWLEDGED PAST: the broker never delivers those records again, so on a
// strict log the node stays unhydrated and writes on the missing subjects refuse
// `behind`, and on a compacted log the node reports itself caught up with those
// rows absent. DELIVERED PAST and IN FLIGHT at or below the checkpoint stall the
// loop for the ack window; BEHIND costs the redeliveries between.
//
// Every drift is rebuilt (one replicated delete and create), touching only this
// node. When the rebuild fails, behind and in flight keep whichever consumer the
// broker holds, with a warning, since failing a boot over an optimisation's
// metadata write is worse than not rebuilding; the two drifts ahead of the rows
// fail the open, as does a broker that cannot say which consumer it holds,
// because a handle kept on a guess fails every fetch
// ([DomainConsumer.afterFailedRebuild]).
//
// # Every connection presents a key
//
// ADR-0094. Every embedded broker this package starts installs ONE authenticator
// (brokerAuth, nats-server's embedding hooks CustomClientAuthentication and, on a
// clustered member, CustomRouterAuthentication), admitting exactly four kinds of
// connection: a ROUTE presenting the username and password derived from a key of
// this node's ring under [RouteKeyDomain]; a LEAF LINK presenting the nkey
// derived under [LeafKeyDomain], signing the member's nonce; an OPERATOR on a
// clustered member's loopback client listener presenting the nkey derived under
// [ClientKeyDomain], bound to the global account, never the system one; and an
// IN-PROCESS client signing with a key minted at each start that nothing outside
// the process holds: the engine's bound to the global account, the lease
// connection's to [LeaseAccount] on a leaf and the global account elsewhere, and
// the membership code's (membership.go) to the system account. Every other
// connection is refused, counted ([Server.OnPeerRefused]) and logged once a
// minute per class (broker_peer_refused).
//
// THE KEYS ARE THE KEYRING'S, derived per key ([FleetAuthFrom]) by the rule
// every consumer of secrets.keys follows (internal/runtoken), so the keyring's
// rotation runbook moves the broker with no step of its own: every node accepts
// every key of its ring and presents the active one. The embedding hooks, not
// nats-server's own blocks, which cannot refuse anonymity on a member that
// declares a system account.
//
// WHAT IT DOES NOT DO is encrypt. A route's password crosses the fleet network
// in clear once per connection and every byte after it crosses in clear. So the
// fleet network stays private, and the state log's records and the fleet's
// instructions stay signed (ADR-0031): a record is believed on its own
// signature, never on the connection it arrived by.
//
// # A pause hold lives on the queue, for one attachment
//
// Holds live on the QUEUE, keyed by the (topic, group) pair, each TAKEN FOR ONE
// ATTACHMENT of the pair. By PAIR rather than topic, because a topic-keyed hold
// also gated every other group on a shared subject, and a second subsystem
// gating one inbox must not release the first's hold. On the QUEUE rather than a
// consumer, because a hold is routinely taken BEFORE anything attaches (an engine
// that knows a seat is busy pauses first), and one stored on the consumer would
// be dropped in that window. FOR AN ATTACHMENT ([queue.Attachment]), because
// owned by the pair alone a hold could not tell the attach it was taken before
// from the one a release had just ended, and would carry onto the next attachment
// of that inbox, which then fetches nothing with nothing left to lift it; a
// hold asked for any other attachment is refused ([Queue.Attachment]).
//
// # The least time a failing message is carried
//
// [LeastTimeToDeadLetter] is the least time this backend carries a failing
// message, at its defaults, from its first delivery to its dead letter: its
// budget's deliveries ([maxDeliver]), every one but the last returned as a
// failure to one attachment, each return spaced by [Queue.nakBackoff] at the
// floor of its jitter ([backoff.Jitter]): 1, 2, 4, 8 and 16 s, then nineteen at
// the 30 s ceiling, about 481 s. A layer above that leans on a failing message
// outliving a wait of its own (internal/sandbox's waiter) holds its wait under
// this in a test. It is the least on the failure path only: a hand-back returns
// at once ([attachment.handBack]), and a failure to an attachment the message had
// not failed on yet is spaced from the first return again
// ([attachment.failures]), so a message handed from node to node, or met by a
// re-attach, is dead-lettered sooner. [Config.MaxDeliver], NakDelay and
// NakCeiling, which only tests set, change it too.
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
