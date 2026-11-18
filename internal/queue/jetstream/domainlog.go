package jetstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsinflight"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// DomainLog is one state-log domain's append surface: a conditional publish
// that returns its acknowledgement, the last-message query a rejection is
// discriminated with, and a WATCH ([DomainLog.Watch]) for a reader that needs
// only which objects moved.
//
// Not [Queue.Publish], which discards the PubAck: for a state log the sequence
// is the object's new version, the applier's checkpoint and the next writer's
// expectation at once.
type DomainLog struct {
	js     jetstream.JetStream
	stream jetstream.Stream
	name   string

	// q is the queue this log was opened on, for the one thing a bare
	// JetStream context cannot do: ensure a durable consumer that survives a
	// peer creating the same name at once ([Queue.ensureDurableConsumer]).
	q *Queue

	// arrival is what a leader's read by position is given beyond the read
	// term and the lookup ceiling for its reply to reach this node whole,
	// where the record's size is unknown: the largest record's arrival
	// ([Queue.recordArrival]). A log read through its copies gives each
	// record its own ([DomainLog.replyArrival]). Fixed at open.
	arrival time.Duration

	// copies reads records ([DomainLog.At], [DomainLog.LastSeq]) through
	// consumers on the leader's own copy ([copyRead]) where a leaf's link
	// stands between connection and leader and the leader's reply of the
	// largest record could not cross it ([Queue.leaderCarries]); nil where
	// every reply reaches this node. Fixed at open.
	copies *copyRead

	// info reads the stream's info through A HANDLE OF ITS OWN EVERY TIME
	// ([DomainLog.stateInfo]), for [DomainLog.Stats] and [DomainLog.SetMaxBytes].
	//
	// The vendored handle is not safe for concurrent use: `Info` caches into
	// `s.info` unlocked (stream.go:498) and `getMsg` (GetMsg,
	// GetLastMsgForSubject) reads `s.info.Config` (stream.go:577). So nothing
	// calls `Info` on the shared [DomainLog.stream], and `LastSeq` on the
	// write path takes no lock. Nor a second shared handle: a hedged state
	// read ([jsprovision.ReadWith]) has two requests out at once, and a
	// per-request handle makes no read wait its turn — the reserve's usage
	// reading admits every append and barrier ([statelog.Reserve.Admit]).
	info func(ctx context.Context) (*jetstream.StreamInfo, error)

	// members is how many members store the stream, as its open and every
	// state read ([DomainLog.stateInfo]) last reported it, so a
	// re-replication is seen within a second at no round trip
	// ([DomainLog.Members]).
	members atomic.Int32

	// watchTiming is every duration a watch on this log waits on; the ZERO
	// VALUE IS PRODUCTION ([DomainLog.watching]). Only this package's tests
	// set it, for [Queue.timing]'s reason, before the log's first Watch.
	watchTiming watchTiming
}

// DomainLog opens the append surface for a stream this node has provisioned.
//
// It resolves the stream once, at open: a lookup on every rejection would add
// a round trip to the write authority's slowest path.
func (q *Queue) DomainLog(ctx context.Context, stream string) (*DomainLog, error) {
	s, err := q.openProvisioned(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("jetstream: open the log %q: %w", stream, err)
	}
	l := &DomainLog{js: q.js, stream: s, name: stream, q: q, arrival: q.recordArrival(s),
		info: func(ctx context.Context) (*jetstream.StreamInfo, error) {
			// A HANDLE OF THIS REQUEST'S OWN — see [DomainLog.info].
			fresh, err := q.js.Stream(ctx, stream)
			if err != nil {
				return nil, err
			}
			return fresh.CachedInfo(), nil
		}}
	if !q.leaderCarries(q.largestMessage(s)) {
		leader, err := jsapi.NewLeader(q.js, stream)
		if err != nil {
			return nil, fmt.Errorf("jetstream: open the log %q: %w", stream, err)
		}
		l.copies = q.copiesOf(stream, leader)
	}
	l.noteMembers(s.CachedInfo())
	return l, nil
}

// Members is how many members store this log — its replication factor as the
// broker last reported it — and at least one.
//
// It is what a barrier that did not commit MEANS ([statelog.NewReadIndex]): on
// several members, no majority agreed in time; on one, the member's own write
// did not complete. Read from the stream's info, not Tier A's stream.replicas,
// a floor the running stream may exceed ([Queue.observeStream]).
func (l *DomainLog) Members() int { return int(max(l.members.Load(), 1)) }

// noteMembers records the replication factor info states, where it states
// one.
func (l *DomainLog) noteMembers(info *jetstream.StreamInfo) {
	if info != nil && info.Config.Replicas > 0 {
		l.members.Store(int32(info.Config.Replicas))
	}
}

// recordArrival is what a leader's read of one of s's records by position is
// given beyond the read term and the lookup ceiling ([DomainLog.At]), where the
// record's size is unknown: the arrival of the largest message s stores
// ([jsapi.Arrival]) — its handling on every connection, plus, across a link,
// its crossing at the connection's pace ([link.pace]), the one pace this
// connection's writes and internal/objstore/natsobj's reads and puts use too.
// s is the handle just opened, whose info nothing writes.
func (q *Queue) recordArrival(s jetstream.Stream) time.Duration {
	return jsapi.Arrival(q.largestMessage(s), q.link.pace)
}

// replyArrival is how long the reply to a leader's read of a record of
// recordBytes (header block and payload, as [copyRead.at] counted them) takes
// to reach this connection whole at its pace ([jsapi.Arrival]): what
// [DomainLog.At] gives in place of [DomainLog.arrival].
func (l *DomainLog) replyArrival(recordBytes int) time.Duration {
	return jsapi.Arrival(recordBytes, l.q.link.pace)
}

// largestMessage is the largest message s may store, its header block and
// its payload: the largest it states, where it states one, and never more
// than the transport carries. s is a handle just opened, as
// [Queue.recordArrival]'s is.
func (q *Queue) largestMessage(s jetstream.Stream) int {
	largest := q.nc.MaxPayload()
	if info := s.CachedInfo(); info != nil && info.Config.MaxMsgSize > 0 {
		largest = min(largest, int64(info.Config.MaxMsgSize))
	}
	return int(largest)
}

// stateInfo is the stream's info, read through a handle of its own
// ([DomainLog.info]).
//
// ASKED AS A READ ([jsprovision.ReadWith]), each request presumed lost after
// the read term and another sent: a member still believing in a leader the
// cluster just lost stays silent, so one request held for the whole context
// waits out the committer's thirty-second budget where a re-ask is answered
// by the new leader in seconds. EVERY REQUEST STAYS OUTSTANDING meanwhile: a
// backlogged leader answers late, not never, and the reserve admitting every
// append and barrier through this reading must not fail them all for a reply
// that was only slow.
func (l *DomainLog) stateInfo(ctx context.Context) (*jetstream.StreamInfo, error) {
	info, err := jsprovision.ReadWith(ctx, l.q.provisioning(), l.info, nil)
	if err == nil {
		l.noteMembers(info)
	}
	return info, err
}

// openProvisioned resolves a stream THIS NODE HAS JUST PROVISIONED, waiting
// out the window in which its own create has committed but is not yet visible
// to it.
//
// Every caller reaches it right after [Queue.EnsureDomainStream] returned nil.
// On a cluster the metadata leader commits the create and the asking member
// learns of it on the next metadata update, so meanwhile a bare lookup is
// told "stream not found" (err_code=10059) and fails the boot. Bounded by
// [jsprovision.ReadBack], not the provisioning budget: what is waited out is
// propagation, a round trip, so a longer not-found is a stream really gone;
// and a placement failure is not waited out, since nothing is being placed.
func (q *Queue) openProvisioned(ctx context.Context, stream string) (jetstream.Stream, error) {
	// [jsprovision.SettleWith] hands each attempt a context carrying the
	// window, so a blocked lookup cannot outlive it; ctx goes in
	// undiminished, so a cancelled boot is answered at once.
	return jsprovision.SettleWith(ctx, q.provisioning(),
		func(ctx context.Context) (jetstream.Stream, error) { return q.js.Stream(ctx, stream) })
}

// Append publishes one record.
//
// expect is the per-subject last sequence the append is conditional on; NIL
// sends no expectation, correct only where the records commute. A pointer
// because zero is a real expectation, "this subject holds nothing", and
// mistaking it for none costs a silent lost update.
//
// msgID empty sends no message id, which the read barrier requires: a repeated
// id inside the duplicate window is answered without a quorum round trip, so
// its acknowledgement proves nothing about the log's end.
func (l *DomainLog) Append(ctx context.Context, subject, msgID string, expect *uint64, body []byte) (uint64, bool, error) {
	msg := appendMsg(subject, msgID, expect, body)
	// DECIDED BEFORE IT IS ANSWERED: an append the leader refuses because
	// another write to the subject (maybe this node's own) is in flight was
	// compared with nothing, and handed up as a rejection it sent the
	// publisher round the discriminator against its own anchor.
	// [jsinflight.Decide] waits it out within the append's budget (a refused
	// attempt stored nothing, its id included, so the retry is the same
	// append); one still in flight at the deadline is [jsinflight.Undecided],
	// which the state log reads as no answer.
	//
	// Each of its first sends is given the time its bytes take to cross
	// ([Queue.writing]; package doc, "A write across a link is given the time
	// its bytes take to cross", which also says what that leaves unbounded).
	var ack *jetstream.PubAck
	sends := l.q.writing(msg)
	err := jsinflight.Decide(ctx, sends, func(ctx context.Context) error {
		var err error
		ack, err = sendWithin(ctx, sends, l.js, msg)
		return err
	})
	if err != nil {
		// THE SIZES TRAVEL WITH A RECORD TOO LARGE TO SEND: the client's
		// refusal carries neither the record's excess nor the server's
		// max_payload, and the remedy turns on both. Wrapped, so the state
		// log still classifies it by the sentinel.
		if errors.Is(err, nats.ErrMaxPayload) {
			return 0, false, fmt.Errorf("%s: %w",
				tooLarge(l.js.Conn(), "append to", subject, len(body)), err)
		}
		return 0, false, err
	}
	return ack.Sequence, ack.Duplicate, nil
}

// appendMsg is the message an append publishes: the record as payload, and
// the headers the client's WithMsgID and WithExpectLastSequencePerSubject
// write, set on the message so it is all the append sends and its budget
// counts what crosses ([carried]).
func appendMsg(subject, msgID string, expect *uint64, body []byte) *nats.Msg {
	msg := &nats.Msg{Subject: subject, Data: body, Header: nats.Header{}}
	if msgID != "" {
		msg.Header.Set(jetstream.MsgIDHeader, msgID)
	}
	if expect != nil {
		msg.Header.Set(jetstream.ExpectedLastSubjSeqHeader, strconv.FormatUint(*expect, 10))
	}
	return msg
}

// LastSeq answers the last sequence on a subject, reporting false with a nil
// error when the subject holds no message.
//
// "No message here" is a FACT, not a failure: it tells a trimmed anchor from a
// lost race, and as an error a caller that forgot to check would take the
// trimmed-anchor branch on a broker merely unreachable. Asked as a read, for
// [DomainLog.stateInfo]'s reasons, and at the read term even where its reply
// (the subject's newest record) crosses a slow link, unlike [DomainLog.At]:
// every create and every rejected append waits on it, and a request dropped
// across an election must be re-asked in seconds, a second reply on a slow
// link the price. Across a leaf's link (package doc, "A message too large for
// a leader's reply is read raw") it reads the newest's headers ([copyRead.last]).
func (l *DomainLog) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	if l.copies != nil {
		seq, found, err := l.copies.last(ctx, subject)
		if err != nil {
			return 0, false, fmt.Errorf("jetstream: last message on %q: %w", subject, err)
		}
		return seq, found, nil
	}
	msg, err := jsprovision.ReadWith(ctx, l.q.provisioning(),
		func(ctx context.Context) (*jetstream.RawStreamMsg, error) {
			return l.stream.GetLastMsgForSubject(ctx, subject)
		}, nil)
	if err != nil {
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("jetstream: last message on %q: %w", subject, err)
	}
	return msg.Sequence, true, nil
}

// End is the stream's own last sequence, across every subject.
//
// Not [DomainLog.LastSeq], which is per subject: a reader measuring how far
// the log goes from a quiet subject's end reports zero lag on a busy company.
func (l *DomainLog) End(ctx context.Context) (uint64, error) {
	_, last, err := l.Bounds(ctx)
	return last, err
}

// Bounds is BOTH ends of the log: the first sequence still held and the last
// one written.
//
// The first end matters: the server CLAMPS a consumer whose position falls
// below it upward with no error, and the consumer then reports itself caught
// up over a hole, which only a comparison against this number catches. ONE
// ROUND TRIP for both, so a caller never pairs ends from either side of a trim.
func (l *DomainLog) Bounds(ctx context.Context) (first, last uint64, err error) {
	// THROUGH [DomainLog.Stats], so the pairing rule has one implementation.
	stats, err := l.Stats(ctx)
	if err != nil {
		return 0, 0, err
	}
	return stats.FirstSeq, stats.LastSeq, nil
}

// At reads one record back by sequence, reporting (false, nil) for one the
// stream no longer holds: a trim is the retention gate's ordinary work, and
// "gone" must not read as "the broker is unreachable".
//
// The leader's read is given its reply's ARRIVAL beyond the read term and the
// lookup ceiling (jsprovision.Timing.Arriving): the record's own where
// [copyRead.at] sized it, else [DomainLog.arrival], whose handling
// BenchmarkADomainLogReadOfItsLargestRecord measured. So a reply still being
// built or crossing is never re-asked into a second one queued ahead of the
// node's lease renewals; its callers can afford a slower re-ask after an
// election, where [DomainLog.LastSeq] cannot. Across a leaf's link: package
// doc, "A message too large for a leader's reply is read raw".
func (l *DomainLog) At(ctx context.Context, seq uint64) (subject string,
	payload []byte, storedAt time.Time, ok bool, err error) {

	arrival := l.arrival
	if l.copies != nil {
		at, atErr := l.copies.at(ctx, seq)
		switch {
		case atErr != nil:
			return "", nil, time.Time{}, false, fmt.Errorf("jetstream: read %q at %d: %w", l.name, seq, atErr)
		case !at.held:
			return "", nil, time.Time{}, false, nil
		case !l.q.leaderCarries(at.bytes):
			msg, held, readErr := l.copies.message(ctx, seq, at)
			if readErr != nil {
				return "", nil, time.Time{}, false, fmt.Errorf("jetstream: read %q at %d: %w", l.name, seq, readErr)
			}
			return msg.subject, msg.data, msg.time, held, nil
		}
		arrival = l.replyArrival(at.bytes)
	}
	timing := l.q.provisioning().Arriving(arrival)
	msg, err := jsprovision.ReadWith(ctx, timing,
		func(ctx context.Context) (*jetstream.RawStreamMsg, error) {
			return l.stream.GetMsg(ctx, seq)
		}, nil)
	switch {
	case errors.Is(err, jetstream.ErrMsgNotFound):
		return "", nil, time.Time{}, false, nil
	case err != nil:
		return "", nil, time.Time{}, false,
			fmt.Errorf("jetstream: read %q at %d: %w", l.name, seq, err)
	}
	return msg.Subject, msg.Data, msg.Time, true, nil
}

// LogStats is everything the retention gate reads about a log, in ONE answer,
// for [DomainLog.Bounds]'s reason: a byte count from before a purge paired
// with a first sequence from after it publishes a headroom figure for a state
// the stream was never in, the number an operator watches near a threshold.
type LogStats struct {
	// FirstSeq and LastSeq are the log's two ends.
	FirstSeq, LastSeq uint64

	// Messages is how many records survive between them, which is not
	// LastSeq − FirstSeq once anything has been purged.
	Messages uint64

	// Bytes is what the stream holds and MaxBytes the ceiling the BROKER
	// enforces, from the stream's own configuration: Tier A's field only
	// creates a stream, so once it exists an edit to it names a ceiling
	// nothing applies. MaxBytes is zero for no ceiling, not a full log.
	Bytes, MaxBytes uint64

	// CreatedAt is the creation instant the stream's identity is keyed to
	// ([statelog.StreamCreatedAt]), which is what detects a recreated one.
	CreatedAt time.Time
}

// Stats reads the log's ends, its size and its ceiling in one round trip.
func (l *DomainLog) Stats(ctx context.Context) (LogStats, error) {
	info, err := l.stateInfo(ctx)
	if err != nil {
		return LogStats{}, fmt.Errorf("jetstream: read %q's state: %w", l.name, err)
	}
	// THE STREAM'S IDENTITY, read as every consumer reads it: the instant
	// stamped at creation, which a snapshot restore keeps and the broker's
	// own does not ([statelog.StreamCreatedAt]).
	created, err := statelog.StreamCreatedAt(info)
	if err != nil {
		return LogStats{}, fmt.Errorf("jetstream: read %q's identity: %w", l.name, err)
	}
	stats := LogStats{
		FirstSeq:  info.State.FirstSeq,
		LastSeq:   info.State.LastSeq,
		Messages:  info.State.Msgs,
		Bytes:     info.State.Bytes,
		CreatedAt: created,
	}
	if info.Config.MaxBytes > 0 {
		stats.MaxBytes = uint64(info.Config.MaxBytes)
	}
	return stats, nil
}

// Purge removes every record BELOW upTo, the exclusive sequence the retention
// gate concluded may go.
//
// Nothing here re-decides: the gate's minimum is taken where it can be tested
// without a broker, and the only judgement here is that removing nothing is
// not worth a round trip. A purge at N leaves N itself, so a caller that
// established every node committed THROUGH N − 1 passes N as it is.
func (l *DomainLog) Purge(ctx context.Context, upTo uint64) error {
	if upTo == 0 {
		return nil
	}
	if err := l.stream.Purge(ctx, jetstream.WithPurgeSequence(upTo)); err != nil {
		return fmt.Errorf("jetstream: purge %q below %d: %w", l.name, upTo, err)
	}
	return nil
}

// SetMaxBytes changes the log's byte ceiling.
//
// It is the engine's ONE write of a running stream's configuration
// (TestOnlyOnePlaceWritesARunningStreamsConfiguration): a second could raise a
// ceiling a capacity operation was lowering. Its one caller,
// internal/engine/capacity.go, runs only once every node runs a mode that
// starts no publisher, so the usage a resize is decided against cannot move;
// nothing here checks it. The update carries the stream's CURRENT config with
// one field changed, since an update replaces the whole configuration and one
// built from this node's spec would reset what others had set.
func (l *DomainLog) SetMaxBytes(ctx context.Context, maxBytes uint64) error {
	info, err := l.stateInfo(ctx)
	if err != nil {
		return fmt.Errorf("jetstream: read %q's configuration before changing "+
			"its ceiling: %w", l.name, err)
	}
	config := info.Config
	config.MaxBytes = int64(maxBytes)
	_, err = l.js.UpdateStream(ctx, config)
	if jsprovision.OutOfCapacity(err) {
		// NAMED, as a refused create is: the broker answers a raise it
		// cannot reserve with the same codes, and the refusal wrote nothing,
		// so "outcome unknown" would send an operator to a needless seal.
		return fmt.Errorf("jetstream: set %q's ceiling to %d: %w: %w",
			l.name, maxBytes, ErrInsufficientStorage, err)
	}
	if err != nil {
		return fmt.Errorf("jetstream: set %q's ceiling to %d: %w",
			l.name, maxBytes, err)
	}
	return nil
}

// WatchHandler is what a [DomainWatch] tells its caller: the subject of each
// record appended under its filter, and every moment it may have missed one,
// with how far the caller's rows must reach to hold what it missed.
//
// The two are called ONE AT A TIME, never once [DomainWatch.Stop] has
// returned, so a caller needs no lock for what they share. They run on the
// connection's delivery path: each returns promptly (a nudge schedules work,
// it does not do it) and neither calls Stop.
type WatchHandler struct {
	// Subject is called for each record appended under the filter after the
	// watch began, in log order, with its subject and sequence. THE SEQUENCE
	// TRAVELS WITH IT because the watch hears of a record when the broker
	// stores it, before any applier has: a reader waits for its copy to reach
	// that position, or it reads the rows from before the record.
	Subject func(subject string, seq uint64)

	// Gap is called whenever the watch may have missed a record — its
	// consumer lost and begun again at the log's end, a delivery counted and
	// never received, a delivery whose position it could not read, or a
	// reconnect — with through, a position at or past every record it may
	// have missed. The caller waits for its copy to reach through, for
	// Subject's reason, then reads its rows.
	//
	// Every record appended under the filter after the watch began is told to
	// Subject or lies at or below the through of a Gap told after it may have
	// been missed, so a Gap may follow Subjects of records past it, and a
	// record may be both told and covered.
	Gap func(through uint64)
}

// Watch tells h the subject of every record appended to the log under filter
// from now on, without taking the records (package doc, "A watch is told which
// records moved, and never carries them"). filter must meet the log's subject
// space ([DomainLog.carries]).
//
// It returns once the watch has begun: every record appended after is told to
// h.Subject or lies within a later Gap, and the caller then reads its rows for
// what came before. A watch that could not begin is an
// error and leaves nothing running; one that began outlives lost consumers and
// reconnects, saying so through h.Gap, until [DomainWatch.Stop] or the
// connection closes for good, which takes the node down ([Queue.Lost]).
func (l *DomainLog) Watch(ctx context.Context, filter string, h WatchHandler) (*DomainWatch, error) {
	if l.q.isClosed() {
		return nil, ErrClosed
	}
	if filter == "" {
		return nil, fmt.Errorf("jetstream: a watch on %q names no subjects — "+
			"an empty filter is every record the log carries, which no "+
			"watcher means; name the subjects, or the log's own wildcard", l.name)
	}
	if h.Subject == nil || h.Gap == nil {
		return nil, fmt.Errorf("jetstream: a watch on %q under %s needs both "+
			"handlers — a watch without Gap would miss records and say nothing",
			l.name, filter)
	}
	// REFUSED HERE, because the broker makes a consumer under any filter
	// (error 10093's subset check is gone from it) and one outside the
	// stream's subjects delivers nothing, silently. A filter that is no
	// subject pattern the client and the broker refuse themselves.
	if server.IsValidSubject(filter) && !l.carries(filter) {
		return nil, fmt.Errorf("jetstream: a watch on %q under %s would hear "+
			"nothing — the log carries %s, and no subject there matches it; "+
			"name subjects inside them", l.name, filter, words.And(l.subjects()))
	}
	// DETACHED, as the live feed's consumer is: ctx bounds the first create,
	// and the watch lives until Stop.
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w := &DomainWatch{
		l: l, filter: filter, h: h, timing: l.watching(), logger: l.q.log,
		prefix: "watch_" + nuid.Next() + "_",
		cancel: cancel,
		wake:   make(chan struct{}, 1), done: make(chan struct{}),
	}
	if err := w.begin(ctx, 1); err != nil {
		cancel()
		return nil, fmt.Errorf("jetstream: watch %q under %s: %w", l.name, filter, err)
	}
	go w.supervise(life)
	return w, nil
}

// watchHeartbeat is how often the broker tells an idle watch that its consumer
// still serves it; two missed is a loss ([lostConsumer]).
//
// FIVE SECONDS, the client library's heartbeat for its ordered consumer, the
// other reader that replaces a lost consumer rather than wait (nats.go's
// jetstream/pull.go): a lost member is noticed within ten seconds, where a
// plain pull's fifteen left a watch deaf for thirty. The pull's other settings
// are the library's defaults: a request standing thirty seconds, renewed as it
// ends, and five hundred records, a few hundred kilobytes of headers at most.
const watchHeartbeat = 5 * time.Second

// watchRetry and watchRetryCeiling space the attempts to replace a lost
// consumer — at once, then a second later, doubling to ten seconds — spread by
// watchRetryJitter so a fleet's watchers that lost a member together do not
// ask its metadata group in step.
//
// THE CLIENT LIBRARY'S NUMBERS for its ordered consumer (nats.go's
// jetstream/ordered.go): a member's re-election or restart takes seconds, a
// broker that stays away costs one proposal a watcher every ten seconds, and a
// watch that cannot begin costs only latency, since the rows are the record.
const (
	watchRetry        = time.Second
	watchRetryCeiling = 10 * time.Second
	watchRetryJitter  = 0.2
)

// watchTiming is every duration a watch waits on — see
// [DomainLog.watchTiming].
type watchTiming struct {
	// heartbeat is the broker's idle heartbeat to the watch's pull.
	heartbeat time.Duration
	// recheck is how often the watch reads its connection's reconnect
	// count, and how soon a read of the log's end that failed is asked
	// again: every heartbeat, in production.
	recheck time.Duration
	// attempt bounds one create of a replacement consumer: in production the
	// client's default request timeout, five seconds. A create is answered
	// once the metadata group commits it and its one member leads it —
	// milliseconds when healthy — and one lost with a member going down is
	// asked again under the SAME name, answered with the consumer already
	// made. Not the provisioning's write term ([jsprovision.AskTerm]), sized
	// for a replicated group's election this consumer has none of, which
	// would hold the watch deaf. It also bounds [DomainWatch.placeGap]'s read
	// of the log's end, hedged like every state read ([DomainLog.stateInfo]).
	attempt time.Duration
	// retry and retryCeiling space the attempts to replace a lost consumer.
	retry, retryCeiling time.Duration
}

// carries reports whether filter can match a subject the log carries.
//
// OVERLAP, not containment: a filter wider than the log (`crewlet.code.>` on
// the code log) watches every record, which a caller can mean; one meeting
// none of its subjects can only be a mistake. The subjects are the handle's,
// read at open: no node changes them on a running stream ([Queue.observeStream]).
func (l *DomainLog) carries(filter string) bool {
	for _, subject := range l.subjects() {
		if server.SubjectsCollide(filter, subject) {
			return true
		}
	}
	return false
}

// subjects is the log's subject space, as its stream declared it when the log
// was opened.
func (l *DomainLog) subjects() []string {
	info := l.stream.CachedInfo()
	if info == nil {
		return nil
	}
	return info.Config.Subjects
}

// watching is the timing a watch on this log runs at: production unless this
// package's tests set [DomainLog.watchTiming].
func (l *DomainLog) watching() watchTiming {
	if l.watchTiming != (watchTiming{}) {
		return l.watchTiming
	}
	return watchTiming{heartbeat: watchHeartbeat, recheck: watchHeartbeat,
		attempt: l.js.Options().DefaultTimeout,
		retry:   watchRetry, retryCeiling: watchRetryCeiling}
}

// errWatchStopped is a create or a pull that finished after its watch was
// stopped, which the watch drops rather than reports.
var errWatchStopped = errors.New("jetstream: the watch was stopped")

// DomainWatch is one running [DomainLog.Watch].
//
// Its one goroutine, the supervisor, sends every request the watch makes —
// replacing a lost consumer, reading the reconnect count and the log's end —
// so none is made on the delivery path; [DomainWatch.Stop] ends it and waits.
// Deliveries and the client's reports arrive on the client's goroutines; all
// three meet under mu, where every handler call is made, which makes the calls
// one at a time and none after Stop.
type DomainWatch struct {
	l      *DomainLog
	filter string
	h      WatchHandler
	timing watchTiming
	logger *slog.Logger

	// prefix begins each GENERATION's consumer name, its number after it, so
	// a create asked again after a lost reply is answered with the consumer
	// already made rather than a second beside it.
	prefix string

	// cancel ends the watch's own context, under which the supervisor asks
	// every create and read; Stop calls it.
	cancel context.CancelFunc
	// wake tells the supervisor a loss was reported or a Gap waits for the
	// log's end. Capacity one, so a report is never lost and its sender
	// never blocks: the supervisor reads the watch's state, not the reports.
	wake chan struct{}
	// done is closed when the supervisor has returned.
	done chan struct{}
	stop sync.Once

	mu sync.Mutex
	// stopped is Stop having begun: nothing calls the handler after it.
	stopped bool
	// gen is the generation whose deliveries are taken, and live whether
	// it still serves: between a loss and the consumer replacing it,
	// none does.
	gen  uint64
	live bool
	// name and consume are gen's consumer and its standing pull, and began
	// the position that consumer began after: the log's end when the broker
	// made it ([DomainWatch.begin]).
	name    string
	consume jetstream.ConsumeContext
	began   uint64
	// delivered is the consumer's own count of the last delivery taken.
	delivered uint64
	// gapPending is the Gap a loss owes, called through the position its
	// replacement began after once it has begun (by its first delivery or the
	// supervisor, whichever is first), never before, or records appended in
	// between would be covered by nothing.
	gapPending bool
	// gapUnread is a Gap owed within gen that has no position of its own — a
	// reconnect, a delivery whose position could not be read — which the
	// supervisor calls once it has read the log's end ([DomainWatch.placeGap]).
	gapUnread bool
	// lost is what the client reported when gen stopped serving; closed is
	// the queue's connection closed for good, which ends the watch.
	lost   error
	closed bool
	// reconnects is the connection's reconnect count as last read.
	reconnects uint64
}

// config is the consumer one generation reads through (package doc, "A watch
// is told which records moved, and never carries them"). HEADERS ONLY: a
// record may be the transport's whole 8 MiB, and carrying each to every
// watcher would cost each the log's volume again. From the END (DeliverNew),
// since the caller's rows hold the rest, and acknowledging NOTHING (AckNone),
// since a nudge is worth something only early. ONE REPLICA IN MEMORY: its only
// state is how far it delivered, and a replicated one would be a raft group a
// watcher and a proposal a delivery for state one create replaces — so a lost
// consumer is replaced, never waited for ([DomainWatch.replace]).
func (w *DomainWatch) config(name string) jetstream.ConsumerConfig {
	description := "a watch on " + w.filter
	if node := w.l.q.contract.Node; node != "" {
		description = "node " + node + "'s watch on " + w.filter
	}
	return jetstream.ConsumerConfig{
		Name:              name,
		Description:       description,
		FilterSubject:     w.filter,
		DeliverPolicy:     jetstream.DeliverNewPolicy,
		AckPolicy:         jetstream.AckNonePolicy,
		HeadersOnly:       true,
		Replicas:          1,
		MemoryStorage:     true,
		InactiveThreshold: ephemeralInactive,
	}
}

// begin creates generation gen's consumer at the log's end, under ctx, and
// starts its standing pull.
func (w *DomainWatch) begin(ctx context.Context, gen uint64) error {
	name := w.prefix + strconv.FormatUint(gen, 10)
	// READ BEFORE THE CONSUMER EXISTS, so a reconnect during the create is
	// noticed at the next recheck. One before it needs no Gap of its own:
	// what it lost lies at or below where this consumer begins, which the
	// loss's Gap reaches, or this consumer delivers it.
	reconnects := w.l.js.Conn().Stats().Reconnects
	w.l.q.noteConsumerProposal()
	cons, err := w.l.js.CreateConsumer(ctx, w.l.name, w.config(name))
	if err != nil {
		return fmt.Errorf("create the consumer %s: %w", name, err)
	}
	// WHERE IT BEGINS, from the create's own reply: a DeliverNew consumer
	// reports the log's end at its creation as delivered and delivers every
	// record after, so that end is how far the rows must reach, at no extra
	// request. A create asked again returns the earlier attempt's consumer,
	// whose own beginning is the one that holds.
	began := cons.CachedInfo().Delivered.Stream
	w.mu.Lock()
	if w.stopped {
		// The consumer stays behind, and the broker reaps it within
		// [ephemeralInactive]: nothing will ever pull from it.
		w.mu.Unlock()
		return errWatchStopped
	}
	w.gen, w.live, w.name, w.delivered, w.began = gen, true, name, 0, began
	w.reconnects = reconnects
	w.mu.Unlock()

	cc, err := cons.Consume(
		func(msg jetstream.Msg) {
			meta, metaErr := msg.Metadata()
			w.take(gen, msg.Subject(), meta, metaErr)
		},
		jetstream.PullHeartbeat(w.timing.heartbeat),
		jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
			w.failed(gen, err)
		}))
	if err != nil {
		w.mu.Lock()
		if w.gen == gen {
			w.live = false
		}
		w.mu.Unlock()
		return fmt.Errorf("pull from the consumer %s: %w", name, err)
	}
	w.mu.Lock()
	stopped := w.stopped
	if !stopped {
		w.consume = cc
	}
	w.mu.Unlock()
	if stopped {
		// STOPPED WHILE THE PULL WAS STARTING: Stop saw no pull to end, so
		// this one is ended here.
		cc.Stop()
		return errWatchStopped
	}
	return nil
}

// take hands one delivery of generation gen to the handler — its subject and
// its sequence on the log, after the Gap anything before it owes.
func (w *DomainWatch) take(gen uint64, subject string, meta *jetstream.MsgMetadata, metaErr error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || !w.live || gen != w.gen {
		return
	}
	w.flushGap()
	if metaErr != nil {
		// NO COUNT and NO POSITION: a Gap through the log's end, which the
		// supervisor reads, since the delivery path never waits on a request.
		w.logger.Warn("jetstream_watch_unreadable", "stream", w.l.name,
			"filter", w.filter, "consumer", w.name, "subject", subject,
			"error", metaErr.Error(),
			"detail", "a delivery carried no position this build can read; "+
				"its caller is told to read its rows once its copy reaches "+
				"the log's end")
		w.oweUnread()
		return
	}
	if count := meta.Sequence.Consumer; count != w.delivered+1 {
		w.logger.Info("jetstream_watch_missed", "stream", w.l.name,
			"filter", w.filter, "consumer", w.name, "last_taken", w.delivered,
			"delivered", count, "through", meta.Sequence.Stream,
			"detail", "the consumer counted deliveries this node never "+
				"received — sent to an inbox nobody held; its caller is told "+
				"to read its rows once its copy reaches this delivery")
		// THROUGH THIS DELIVERY'S OWN POSITION: the consumer delivers in
		// log order, so every delivery it counted and this node missed
		// carried a record before this one.
		w.h.Gap(meta.Sequence.Stream)
	}
	w.delivered = meta.Sequence.Consumer
	w.h.Subject(subject, meta.Sequence.Stream)
}

// flushGap calls the Gap a loss owes, once, through the position its
// replacement began after. Under mu.
//
// It answers for a pending [DomainWatch.gapUnread] too: what the lost consumer
// delivered unseen lies at or below that position, or the replacement
// delivers it.
func (w *DomainWatch) flushGap() {
	if w.gapPending {
		w.gapPending, w.gapUnread = false, false
		w.h.Gap(w.began)
	}
}

// oweUnread owes a Gap within the current generation that has no position of
// its own, and wakes the supervisor to read the log's end for it. Under mu.
func (w *DomainWatch) oweUnread() {
	w.gapUnread = true
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// failed is the client's report about generation gen's standing pull. A loss
// stops the generation serving and wakes the supervisor to replace it; the
// queue's connection closed for good ends the watch; anything else the client
// recovers from by itself.
func (w *DomainWatch) failed(gen uint64, err error) {
	closed := connectionClosed(err)
	if !closed && !lostConsumer(err) {
		return
	}
	w.mu.Lock()
	if w.stopped || !w.live || gen != w.gen {
		w.mu.Unlock()
		return
	}
	// NOT SERVING FROM HERE, before the supervisor has done anything: a
	// delivery still in flight from the lost consumer would otherwise take
	// the owed Gap ahead of the replacement it has to follow.
	w.live, w.gapPending = false, true
	w.lost, w.closed = err, closed
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// lostConsumer reports whether err, which the client reported about a watch's
// standing pull, says the broker no longer serves that consumer: two
// heartbeats of silence, a pull nobody serves, the broker's word that it
// deleted the consumer or that the member holding it is shutting down, or a
// request the broker refused, after which the client stops pulling.
func lostConsumer(err error) bool {
	for _, lost := range []error{
		jetstream.ErrNoHeartbeat,
		nats.ErrNoResponders,
		jetstream.ErrConsumerDeleted,
		jetstream.ErrServerShutdown,
		jetstream.ErrBadRequest,
	} {
		if errors.Is(err, lost) {
			return true
		}
	}
	return false
}

// connectionClosed reports whether err is the queue's connection closed for
// good, in either of the client's two spellings: its pull's, and a request's.
func connectionClosed(err error) bool {
	return errors.Is(err, jetstream.ErrConnectionClosed) ||
		errors.Is(err, nats.ErrConnectionClosed)
}

// supervise replaces a lost consumer, reads the connection's reconnect count
// every recheck and reads the log's end for a Gap that waits for it, until the
// watch is stopped — life, the watch's own context, ended — or its connection
// closed for good. What it acts on is the watch's state, so a wake that
// arrives while it is busy is answered by the pass it is in or the next.
func (w *DomainWatch) supervise(life context.Context) {
	defer close(w.done)
	recheck := time.NewTicker(w.timing.recheck)
	defer recheck.Stop()
	for {
		select {
		case <-life.Done():
			return
		case <-recheck.C:
			w.recheckConnection()
		case <-w.wake:
		}
		if !w.replace(life) {
			return
		}
		w.placeGap(life)
	}
}

// recheckConnection owes a Gap when the queue's connection reconnected since
// it was last read: a delivery made while it was away may have gone to an
// inbox nobody held, and nothing would say so until the next one. It reaches
// the log's end, which [DomainWatch.placeGap] reads next. One sent to the old
// inbox after this read, before the resubscription lands, the consumer's
// count reveals ([DomainWatch.take]).
func (w *DomainWatch) recheckConnection() {
	reconnects := w.l.js.Conn().Stats().Reconnects
	w.mu.Lock()
	defer w.mu.Unlock()
	if reconnects == w.reconnects {
		return
	}
	w.reconnects = reconnects
	if w.stopped || !w.live {
		// A LOSS ALREADY OWES ONE, called once its replacement has begun and
		// reaching past whatever this reconnect cost ([DomainWatch.flushGap]).
		return
	}
	w.logger.Info("jetstream_watch_reconnected", "stream", w.l.name,
		"filter", w.filter, "consumer", w.name,
		"detail", "the connection to the broker went away and came back, and a "+
			"delivery made meanwhile may have gone to nobody; the consumer is "+
			"kept and its caller is told to read its rows once its copy reaches "+
			"the log's end")
	w.gapUnread = true
}

// placeGap calls a Gap the current generation owes with no position of its own
// — a reconnect, an unreadable delivery — through the log's end once it has
// read it: no consumer delivers past the log's end. Read here, not where the
// Gap was noticed, because the delivery path never waits on a request.
//
// NOT THE CONSUMER'S DELIVERED POSITION, though tighter: a member answers for
// a one-replica consumer before it has chosen where it begins (on a cluster,
// once it leads, as after a restart) and reports nothing delivered, below all
// its predecessor delivered. A failed read leaves the Gap owed for the next
// recheck; a loss meanwhile owes its own, which answers for this one
// ([DomainWatch.flushGap]).
func (w *DomainWatch) placeGap(life context.Context) {
	w.mu.Lock()
	owed := w.gapUnread && w.live && !w.stopped
	w.mu.Unlock()
	if !owed {
		return
	}
	ctx, cancel := context.WithTimeout(life, w.timing.attempt)
	through, err := w.l.End(ctx)
	cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || !w.live || !w.gapUnread {
		return
	}
	if err != nil {
		w.logger.Warn("jetstream_watch_end_unread", "stream", w.l.name,
			"filter", w.filter, "consumer", w.name, "error", err.Error(),
			"retry_in", w.timing.recheck.String(),
			"detail", "the watch owes its caller a Gap through the log's end and "+
				"could not read the end; it asks again, and its caller's reads of "+
				"its rows carry its work meanwhile")
		return
	}
	w.gapUnread = false
	w.h.Gap(through)
}

// replace begins a generation in place of one that stopped serving, asking at
// once and then again, spaced, until a consumer begins or life — the watch's
// own context — ends, and reports whether the watch goes on.
func (w *DomainWatch) replace(life context.Context) bool {
	w.mu.Lock()
	if w.stopped || w.closed {
		// CLOSED FOR GOOD: the queue's connection is gone and the node
		// stops with it ([Queue.Lost]), so there is nothing to begin again
		// on and no caller left to tell.
		w.mu.Unlock()
		return false
	}
	if w.live {
		w.mu.Unlock()
		return true
	}
	gen, name, cause, cc := w.gen, w.name, w.lost, w.consume
	w.consume = nil
	w.mu.Unlock()
	if cc != nil {
		cc.Stop()
	}
	w.logger.Info("jetstream_watch_lost", "stream", w.l.name, "filter", w.filter,
		"consumer", name, "error", fmt.Sprint(cause),
		"detail", "the consumer is not serving this watch; it begins again at "+
			"the log's end with a consumer of its own, and its caller is told "+
			"to read its rows for what it missed once its copy reaches where "+
			"that consumer begins")
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(life, w.timing.attempt)
		err := w.begin(ctx, gen+1)
		cancel()
		if err == nil {
			w.mu.Lock()
			// ONLY WHILE THE NEW CONSUMER STILL SERVES: one lost the moment
			// it began owes its own Gap, after ITS replacement, and calling
			// it here would leave that replacement owing none.
			if w.live && w.gen == gen+1 {
				w.flushGap()
			}
			name, began := w.name, w.began
			w.mu.Unlock()
			w.logger.Info("jetstream_watch_resumed", "stream", w.l.name,
				"filter", w.filter, "consumer", name, "attempts", attempt,
				"through", began)
			return true
		}
		if errors.Is(err, errWatchStopped) || life.Err() != nil {
			return false
		}
		if connectionClosed(err) {
			return false
		}
		wait := backoff.Jitter(backoff.Doubling(attempt, w.timing.retry,
			w.timing.retryCeiling), watchRetryJitter)
		w.logger.Warn("jetstream_watch_resume_failed", "stream", w.l.name,
			"filter", w.filter, "attempt", attempt, "error", err.Error(),
			"retry_in", wait.String(),
			"detail", "the broker refused or did not answer the create of the "+
				"watch's next consumer; it asks again, and its caller's reads "+
				"of its rows carry its work meanwhile")
		timer := time.NewTimer(wait)
		select {
		case <-life.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// Stop ends the watch: when it returns, the handler is not being called and
// never will be again. Safe to call more than once, concurrently.
//
// THE CONSUMER IS LEFT for the broker to reap ([ephemeralInactive]): a delete
// is a metadata proposal on the stopping path, where a node can least wait for
// one, and an unpulled consumer costs its member little for a minute.
func (w *DomainWatch) Stop() {
	w.stop.Do(func() {
		w.mu.Lock()
		w.stopped = true
		cc := w.consume
		w.consume = nil
		w.mu.Unlock()
		w.cancel()
		if cc != nil {
			cc.Stop()
		}
		<-w.done
	})
}

// consumer is the watch's current consumer and whether it serves the watch
// with no Gap still owed, for this package's tests.
func (w *DomainWatch) consumer() (name string, serving bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.name, w.live && !w.gapPending
}
