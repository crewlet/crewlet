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
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// DomainLog is one state-log domain's append surface: a conditional publish
// that returns the acknowledgement the ordinary publish path discards, and
// the last-message query a rejection is discriminated with — and the WATCH
// ([DomainLog.Watch]) a reader takes when what it needs from the log is which
// objects moved, never what the records say.
//
// # Why this is not Queue.Publish
//
// [Queue.Publish] throws the PubAck away, and for its own traffic that is
// right: an event's sequence is nobody's business. For a state log the
// sequence inside it is the object's new version, the applier's checkpoint
// and the next writer's expectation, all at once — so the framework
// publishes through here instead, on the same connection and with the same
// credentials.
type DomainLog struct {
	js     jetstream.JetStream
	stream jetstream.Stream
	name   string

	// q is the queue this log was opened on, held for the ONE thing a
	// bare JetStream context cannot do: ensure a durable consumer in a
	// way that survives a peer creating the same name at the same instant.
	// See [Queue.ensureDurableConsumer].
	q *Queue

	// crossing is how long the reply to a read of one of this log's
	// records by its position is given beyond the read term to cross this
	// node's connection to the stream's leader: nothing on a member's own
	// connection, and across a link the crossing of the largest record the
	// log stores, at the object store's floor ([DomainLog.At]). Fixed at
	// open.
	crossing time.Duration

	// info reads the stream's info — its ends, its size, its ceiling, its
	// identity — through A HANDLE OF ITS OWN EVERY TIME it is asked
	// ([DomainLog.stateInfo]), and is what [DomainLog.Stats] and
	// [DomainLog.SetMaxBytes] read; nothing else here reads the info.
	//
	// # Why a state read never goes through a handle another read holds
	//
	// The vendored stream handle is not safe for concurrent use, and the
	// shared mutable state is exactly one field: `Info` caches its reply
	// into `s.info` with no lock (stream.go:498), and `getMsg` — which is
	// what `GetMsg` and `GetLastMsgForSubject` are — READS `s.info.Config`
	// on the same handle (stream.go:577). So one goroutine asking for the
	// log's size on the handle above would race every goroutine reading a
	// record by sequence or by subject, on a DomainLog that is shared by
	// design: the applier's readiness check, the publisher's expectation
	// read and the retention tick all hold this one. Nothing calls `Info`
	// on that handle, so its `s.info` is written once at open and read
	// immutably thereafter, and `LastSeq` — on the WRITE path, every
	// conditional append that loses its expectation reads it — takes no
	// lock.
	//
	// AND NOT A SECOND SHARED HANDLE EITHER, which is what this was: a
	// second handle needed its callers to take turns, and a state read is
	// asked as a hedged read ([jsprovision.ReadWith]) whose requests may be
	// out two at a time, which on one handle is the same race. A handle a
	// request opens for itself (one STREAM.INFO, the same request `Info`
	// sends) is shared with nobody, so state reads take no turn and wait
	// for nobody else's: the readiness checks, the trim tick, the operator
	// report — and the GATE RESERVE'S USAGE READING, which is on the
	// hottest path the log has: every ordinary append and every barrier,
	// which is to say every linearizable read, is admitted through it
	// ([statelog.Reserve.Admit]) and waits for a state round trip that
	// starts after it arrives. NOT A READ'S READINESS: a read is served
	// the log's ends from memory, refreshed from here at most once a
	// second whatever the read rate.
	info func(ctx context.Context) (*jetstream.StreamInfo, error)

	// members is how many members store the stream, as the broker last
	// reported it: in the info its open read, and then in every state read
	// ([DomainLog.stateInfo]) — so a stream an operator re-replicates is seen
	// within the second a read's ends are re-read in, and asking it costs no
	// round trip ([DomainLog.Members]).
	members atomic.Int32

	// watchTiming is every duration a watch on this log waits on, and the
	// ZERO VALUE IS PRODUCTION: [DomainLog.watching] answers the constants
	// beside [DomainLog.Watch] whenever it is unset, which is always
	// outside this package's own tests — for [Queue.timing]'s reason: the
	// branches worth proving are a reconnect noticed and a create asked
	// again, and at the production numbers a case proving one waits them
	// out. Set only before the log's first Watch.
	watchTiming watchTiming
}

// DomainLog opens the append surface for a stream this node has provisioned.
//
// It resolves the stream ONCE, at open, rather than per call: the
// last-message query needs a stream handle, and looking one up on every
// rejection would put an extra round trip on the path that is already the
// slowest one the write authority has.
func (q *Queue) DomainLog(ctx context.Context, stream string) (*DomainLog, error) {
	s, err := q.openProvisioned(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("jetstream: open the log %q: %w", stream, err)
	}
	l := &DomainLog{js: q.js, stream: s, name: stream, q: q, crossing: q.recordCrossing(s),
		info: func(ctx context.Context) (*jetstream.StreamInfo, error) {
			// A HANDLE OF THIS REQUEST'S OWN — see [DomainLog.info].
			fresh, err := q.js.Stream(ctx, stream)
			if err != nil {
				return nil, err
			}
			return fresh.CachedInfo(), nil
		}}
	l.noteMembers(s.CachedInfo())
	return l, nil
}

// Members is how many members store this log — its replication factor as the
// broker last reported it — and at least one.
//
// It is what a barrier that did not commit MEANS ([statelog.NewReadIndex]):
// on several members, a majority that did not agree in time; on one, never
// that, since the sole member's acknowledgement waits on nobody else's — the
// member's own write did not complete. Read from the stream's info rather
// than from Tier A's stream.replicas, which is a floor the running stream may
// exceed ([Queue.observeStream]): a node configured for one copy starts
// against a stream that keeps three.
func (l *DomainLog) Members() int { return int(max(l.members.Load(), 1)) }

// noteMembers records the replication factor info states, where it states
// one.
func (l *DomainLog) noteMembers(info *jetstream.StreamInfo) {
	if info != nil && info.Config.Replicas > 0 {
		l.members.Store(int32(info.Config.Replicas))
	}
}

// recordCrossing is how long the reply to a read of one of s's records by its
// position is given beyond the read term to cross this queue's connection to
// the stream's leader — see [DomainLog.At]: nothing where the connection is to
// the node's own member, in its own process, and where it crosses a link the
// crossing of the largest message s stores ([jsapi.Crossing]) at the object
// store's floor, [objstore.MiBPace], the one pace the engine allows a link to
// be as slow as — the pace internal/objstore/natsobj sizes its piece reads
// and its puts' window by on this same connection. s is the handle just
// opened, whose info is its open's and is read by nothing that writes it.
func (q *Queue) recordCrossing(s jetstream.Stream) time.Duration {
	if !q.acrossLink() {
		return 0
	}
	largest := q.nc.MaxPayload()
	if info := s.CachedInfo(); info != nil && info.Config.MaxMsgSize > 0 {
		largest = min(largest, int64(info.Config.MaxMsgSize))
	}
	return jsapi.Crossing(int(largest), objstore.MiBPace)
}

// acrossLink reports whether this queue's connection reaches JetStream across
// a LINK — a dial to a broker somebody else runs, or a leaf's link to the
// members — rather than reaching the node's own member in its own process:
// the fact the engine hands the object store as natsobj.Config.AcrossLink
// (every broker but a member's), read here off how the connection was made.
func (q *Queue) acrossLink() bool { return q.embedded == nil || q.embedded.leaf }

// stateInfo is the stream's info, read through a handle of its own
// ([DomainLog.info]).
//
// ASKED AS A READ ([jsprovision.ReadWith]): each request presumed lost after
// the read term, and another sent while nobody answers. A stream's info is
// answered by its LEADER, and a member that still believes in a leader the
// cluster has just lost stays silent rather than answer — so a request sent in
// the window after a leader died is never answered at all, the new leader's
// election notwithstanding, and one request held for its caller's whole
// context waited out the committer's thirty-second budget for a reply nobody
// was going to send, where asked again it was answered by the new leader a
// second or two later.
//
// AND EVERY REQUEST STAYS OUTSTANDING while the next is asked, because the
// leader queues each for a pool of workers and a backlog answers it LATE
// rather than never. Thrown away at the read term, a reading from a leader
// slower than a second was never taken at all — and the reserve that admits
// every append and every barrier through this reading failed each of them at
// its own budget, every push batch and every linearizable read on the node
// at once, where waiting would only have slowed them.
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
// # Why a bare lookup was wrong here
//
// Every caller of [Queue.DomainLog] reaches it immediately after
// [Queue.EnsureDomainStream] returned nil for the same name — that is the
// contract, and the two are consecutive statements in the state log's
// provision step. On one node the lookup after a successful create cannot
// fail. On a CLUSTER it can: the create is committed by the metadata leader
// and the asking member learns of it on the next metadata update, so there is
// a window in which the member that made the stream is told it does not
// exist.
//
// That is not a hypothetical. It failed a clustered boot outright —
// `open the log "CREWLET_TRACKER_VECTORS": nats: API error: code=404
// err_code=10059 description=stream not found`, reported by the node whose own
// create of that stream had just succeeded. Everything else on this path
// already tolerates the window: [Queue.createOrObserveStream] re-asks after a
// lost race and so does [openBucket] in [internal/coord/kv]. This was the one
// lookup with no tolerance at all, which is why it was the one that broke.
//
// # Why it is bounded by ReadBack rather than the provisioning budget
//
// The create has already returned successfully, so what is being waited out is
// metadata propagation and not placement — a round trip, not a raft election.
// A not-found that outlives [jsprovision.ReadBack] is a stream that really is
// gone, and reporting that promptly is what the caller needs. A placement
// failure is deliberately NOT waited out here: nothing is being placed, so
// waiting for peers would be waiting for something nobody is going to do.
func (q *Queue) openProvisioned(ctx context.Context, stream string) (jetstream.Stream, error) {
	// THE CEILING IS REAL, which means it bounds the LOOKUPS and not only
	// the gaps between them — and that is [jsprovision.Settle]'s to own
	// rather than this caller's: it hands each attempt a context carrying
	// the window, so a single metadata request that blocked cannot outlive
	// the window this function documents and hang until the boot context
	// expired. ctx goes in undiminished, which is what lets an operator who
	// cancels a boot be answered at once.
	return jsprovision.SettleWith(ctx, q.provisioning(),
		func(ctx context.Context) (jetstream.Stream, error) { return q.js.Stream(ctx, stream) })
}

// Append publishes one record.
//
// expect is the per-subject last sequence the append is conditional on, and
// NIL means no expectation header at all — which is correct only where the
// records commute. A pointer rather than a sentinel because zero is a real
// expectation with a specific meaning, "this subject holds nothing", and it
// is the one branch where being wrong costs a silent lost update.
//
// msgID empty sends no message id, which the read barrier requires: a
// repeated id inside the duplicate window is served out of the window with no
// quorum round trip, so an acknowledgement carrying one would prove nothing
// about the log's end.
func (l *DomainLog) Append(ctx context.Context, subject, msgID string, expect *uint64, body []byte) (uint64, bool, error) {
	opts := make([]jetstream.PublishOpt, 0, 2)
	if msgID != "" {
		opts = append(opts, jetstream.WithMsgID(msgID))
	}
	if expect != nil {
		opts = append(opts, jetstream.WithExpectLastSequencePerSubject(*expect))
	}
	// DECIDED BEFORE IT IS ANSWERED. A conditional append the leader refuses
	// because another write to the subject is still in flight has not been
	// compared with anything — the write ahead may be this node's own, already
	// acknowledged — and handed up as a rejection it sent the publisher round
	// the discriminator to re-decide against the very anchor it had, for as
	// many rounds as that write took to clear. [jsinflight.Decide] waits it out
	// within the append's own budget; the leader stored nothing of a refused
	// attempt, its message id included, so the retry is the same append. One
	// still in flight at the deadline is [jsinflight.Undecided], which carries
	// no API error, so the state log reads it as the no-answer it is.
	var ack *jetstream.PubAck
	err := jsinflight.Decide(ctx, l.js.Options().DefaultTimeout, func(ctx context.Context) error {
		var err error
		ack, err = l.js.Publish(ctx, subject, body, opts...)
		return err
	})
	if err != nil {
		// THE SIZES TRAVEL WITH A RECORD TOO LARGE TO SEND, because the
		// client's refusal carries neither and they are the whole of what
		// its remedy turns on: how far over the record is, and which
		// server's max_payload it was measured against. Wrapped, so the
		// state log still classifies it by the sentinel.
		if errors.Is(err, nats.ErrMaxPayload) {
			return 0, false, fmt.Errorf("%s: %w",
				tooLarge(l.js.Conn(), "append to", subject, len(body)), err)
		}
		return 0, false, err
	}
	return ack.Sequence, ack.Duplicate, nil
}

// LastSeq answers the last sequence on a subject, reporting false with a nil
// error when the subject holds no message.
//
// "No message here" is a FACT and not a failure: it is what tells a trimmed
// anchor from a lost race, and the two have opposite remedies. Returning the
// broker's not-found as an error would make every caller re-derive that
// distinction, and the first one to forget it would take the trimmed-anchor
// branch on a broker that was merely unreachable.
//
// ASKED AS A READ, for [DomainLog.stateInfo]'s reasons: a domain log allows no
// direct get, so this is answered by the stream's leader or by nobody — and
// it is on the write path, discriminating every rejected append.
//
// # At the read term, even across a link
//
// Its reply is a whole record too — the subject's newest, base64-encoded, as
// large as the log's largest — so across a link a re-ask of a large one puts
// another on the link before the first has landed, which [DomainLog.At]'s
// term is sized to prevent. This read keeps the read term anyway, because a
// writer is waiting on it: every create asks it, a subject never written
// answering "no message here" in a few hundred bytes, and so does every
// append that lost its expectation or its answer — and a request a member
// dropped across an election has to be asked again in seconds there, where a
// term sized for the largest record would hold the write for its whole
// budget. So across a link slower than its reply — for a subject whose newest
// record is the largest a log holds, below about 45 Mbit/s — a re-ask puts a
// second reply on the link, and the ceiling ends a read whose reply cannot
// land inside it.
func (l *DomainLog) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
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
// SEPARATE FROM [DomainLog.LastSeq], which is per subject, because the two
// answer different questions: a writer forming an expectation wants its own
// subject's last message, and a reader establishing how far the log goes wants
// the stream's. Reading one for the other is how a lag figure comes back as
// zero on a busy company because the subject a node happened to ask about is
// quiet.
func (l *DomainLog) End(ctx context.Context) (uint64, error) {
	_, last, err := l.Bounds(ctx)
	return last, err
}

// Bounds is BOTH ends of the log: the first sequence still held and the last
// one written.
//
// # Why the first end is not a detail
//
// A consumer whose position falls below the stream's first sequence is CLAMPED
// UPWARD by the server with no error at all, and then reports itself perfectly
// caught up — over a hole. The only thing that can notice is a comparison
// against this number, so a readiness check that read only the end would call
// such a node established for ever.
//
// ONE ROUND TRIP for both, because they are one answer: read separately, a
// caller can pair a first sequence from before a trim with a last sequence
// from after it and compute a window neither describes.
func (l *DomainLog) Bounds(ctx context.Context) (first, last uint64, err error) {
	// THROUGH [DomainLog.Stats], so the pairing rule above has one
	// implementation: a second read of the same stream info is a second
	// place for a caller to end up holding two ends from two instants.
	stats, err := l.Stats(ctx)
	if err != nil {
		return 0, 0, err
	}
	return stats.FirstSeq, stats.LastSeq, nil
}

// At reads one record back by sequence.
//
// # Why a domain needs this at all
//
// Almost nothing reads a log by position: a consumer is how records are
// delivered, and a consumer is what the apply loop uses. What needs this is
// the small set of paths that address the log directly rather than following
// it — a reanchor establishing what the old stream's head was, a snapshot's
// verification, and any tool that has a position and needs the record at it.
//
// It reports (false, nil) for a sequence the stream no longer holds, which is
// an ordinary answer rather than an error: a trimmed record is exactly what
// the retention gate exists to produce, and a caller distinguishing "gone" from
// "the broker is unreachable" is the whole reason it is not one error value.
// Asked as a read, for [DomainLog.LastSeq]'s reason.
//
// # Its term covers the record's crossing where the record crosses a link
//
// A record may be as large as its log's largest message — the code log's is
// the transport's whole [statelog.MaxTransportRecordBytes], 8 MiB less 4 KiB —
// and what carries it back is the leader's `STREAM.MSG.GET` reply (a domain
// log allows no direct get): JSON, the record base64-encoded inside it beside
// its envelope, about 11.2 MB for the largest record, heard only once its last
// byte is in. The hedge asks a read nobody has answered again two seconds
// after it was sent and every two seconds after that, every request still
// outstanding, so a reply still crossing a link when a re-ask goes out is a
// second whole reply on that link, queued ahead of everything else the node's
// connection carries, its lease renewals among them: for the largest record,
// a second reply below about 45 Mbit/s, and another every two seconds below
// about 22. And the ceiling's thirty seconds end a read whose reply needs
// longer than that to land — the largest record's, below about 3 Mbit/s.
//
// On a member's own connection, in its own process, the leader's reply
// crosses at most the fleet's cluster routes: read back on a member, the
// largest record took 46 ms (BenchmarkADomainLogReadOfItsLargestRecord, 20
// reads on four cores), inside the read term twentyfold, and a gigabit route
// adds about 90 ms — so a member asks at the read term. A node whose
// connection crosses a LINK to the stream — a client of an external cluster,
// a leaf — reaches the leader over a link the engine allows to be as slow as
// the object store's floor ([Queue.recordCrossing]), which is what
// internal/objstore/natsobj gives its piece reads on the same connection
// (internal/jsapi's Leader.Carrying); there a read is presumed lost only once
// its log's largest reply could have crossed at that floor. For a log whose
// records may be megabytes that is longer than the ceiling — five minutes
// and more for the code log's — so across a link such a read is ONE request,
// held to its caller's deadline or the ceiling: a request a member dropped
// across an election costs its caller that wait rather than two seconds, and
// a slow link never carries one record two and three times ahead of the
// node's leases. The reads that ask this can afford it — the retention's age
// floor and the intake rate a log is measured by, a checkpoint's verification
// at the applier's start, whether the log still holds a peer's checkpoint, a
// reanchor's walk back along the log, a standing read, and an ambiguous
// append's read of its own record after its answer was already lost — where
// [DomainLog.LastSeq], which every create and every rejected append asks,
// keeps the read term (see there).
//
// The ceiling is the same thirty seconds across a link: a record whose reply
// cannot land inside it is not read back by position there — the largest
// record below about 3 Mbit/s. Its own append, raw rather than base64, is
// acknowledged inside a committer's thirty seconds down to about 2.2 Mbit/s,
// so a node on a link between the two can hold in its log a record this read
// cannot reach.
func (l *DomainLog) At(ctx context.Context, seq uint64) (subject string,
	payload []byte, storedAt time.Time, ok bool, err error) {

	timing := l.q.provisioning()
	timing.ReadTerm += l.crossing
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

// LogStats is everything the retention gate reads about a log, in ONE answer.
//
// # Why it is one call and not four
//
// [DomainLog.Bounds] already gives the reason for two of them, and the trim
// needs four: it divides bytes by the ceiling to publish headroom, compares
// the first sequence against what it is about to remove, and reports the last
// one as the fleet's head. Read separately, a tick can pair a byte count from
// before a purge with a first sequence from after it and publish a headroom
// figure describing no state the stream was ever in — which is the number an
// operator watches approach a threshold.
type LogStats struct {
	// FirstSeq and LastSeq are the log's two ends.
	FirstSeq, LastSeq uint64

	// Messages is how many records survive between them, which is not
	// LastSeq − FirstSeq once anything has been purged.
	Messages uint64

	// Bytes is what the stream holds and MaxBytes the ceiling the BROKER
	// is enforcing, read from the stream's own configuration rather than
	// from the Tier A field, because Tier A is per node and is only the
	// value a stream is created with: once the stream exists an edit to the
	// field names a ceiling nothing is applying, restart or not, and this
	// is the number an operator divides by.
	//
	// MaxBytes is zero when the stream declares no ceiling, which is a
	// real setting rather than a full log.
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
	// THE STREAM'S IDENTITY, read the one way every consumer reads it —
	// the instant stamped at its creation, which a snapshot restore keeps
	// and the broker's own instant does not ([statelog.StreamCreatedAt]).
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

// Purge removes every record BELOW upTo, which is the exclusive sequence the
// retention gate concluded may go.
//
// # Why nothing here re-decides
//
// The six terms are a minimum taken somewhere else, deliberately: the gate is
// a policy with an inversion in it that three readers got backwards, and a
// policy that can only be exercised through a live broker is one nobody
// checks. This call is the effect, and its only judgement is the one thing the
// arithmetic cannot know — that removing nothing is not worth a round trip.
//
// The broker's own semantics are what make the exclusive bound safe: a purge
// at sequence N leaves N itself, so a caller that has established every node
// committed THROUGH N − 1 can pass N without arithmetic of its own.
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
// # The one stream-configuration write in the engine
//
// Every other stream call here creates, reads or purges. This one UPDATES a
// running stream's configuration, and it is deliberately the only one: a
// second site could raise a ceiling a capacity operation was in the middle of
// lowering, and neither would know.
//
// It is safe to call only from a process that has established the exclusion —
// see internal/engine/capacity.go, which is its one caller — because the
// usage a resize is decided against has to be a quantity nothing can move.
// Nothing here checks that: the exclusion is structural, established by every
// node of the fleet running a mode that starts no publisher, and a check here
// would be an observation of the thing the mode makes impossible.
//
// The update carries the stream's CURRENT configuration with one field
// changed, read in the same call, because a JetStream update replaces the
// whole configuration: sending a config built from this node's own spec would
// silently reset every field an operator had set, or that the node which
// created the stream set from its own Tier A.
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
		// NAMED, exactly as a refused CREATE is. The broker answers a
		// raise it cannot reserve with the same two codes and the same
		// numberless sentence, and a caller that reported it as "the
		// outcome is unknown" sent an operator to a fleet-wide seal for
		// evidence the broker had already given: a refusal wrote
		// nothing and nothing is in flight.
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
// The two are called ONE AT A TIME, never concurrently, and never once
// [DomainWatch.Stop] has returned, so a caller keeps what the two share without
// a lock of its own. They run on the delivery path of the queue's connection,
// so each returns promptly — a nudge schedules work and does not do it — and
// neither may call Stop.
type WatchHandler struct {
	// Subject is called once for each record appended under the watch's
	// filter after the watch began, in log order, with the record's subject
	// and its sequence on the log.
	//
	// THE SEQUENCE TRAVELS WITH THE SUBJECT because the watch hears of a
	// record when the broker stores it, which is before any applier has
	// applied it: a reader about to read the rows the record wrote waits for
	// its copy to reach that position first, or it reads the rows from
	// before the record and finds nothing to do.
	Subject func(subject string, seq uint64)

	// Gap is called whenever the watch may have missed a record — it lost its
	// consumer and began again at the log's end, a delivery its consumer
	// counted never reached it, a delivery came whose position it could not
	// read, or the queue's connection to the broker went away and came back —
	// with through, a position on the log at or past every record it may
	// have missed. The caller waits for its copy to reach through, then reads
	// its rows, which hold what the watch did not tell it.
	//
	// THE POSITION TRAVELS WITH THE GAP for Subject's reason. A Gap is told
	// when the watch notices it, which says nothing of how far this node's
	// copy has come, and what cost the watch its records — a member
	// restarting, a connection lost — can hold the copy's applier back as
	// well: rows read before the copy reaches through can predate the very
	// records the Gap is about, and the caller would find them only at its
	// next pass over its rows. Every record appended under the filter after
	// the watch began is told to Subject or lies at or below the through of a
	// Gap told after the watch may have missed it — so a Gap can follow
	// Subjects of records past it, and a record can be both told and covered:
	// a nudge twice is still a nudge.
	Gap func(through uint64)
}

// Watch tells h the subject of every record appended to the log under filter
// from now on, without taking the records: the nudge a reader that keeps its
// work in rows takes as "look at this object now", where its own periodic
// pass over the rows would find the record a cadence later. filter is a
// subject pattern that meets the log's own subject space; one that meets none
// of it is refused ([DomainLog.carries]).
//
// It returns once the watch has begun — every record appended after it
// returns is told to h.Subject or lies within a Gap told after it — and the
// watch is its caller's to end with [DomainWatch.Stop]. A watch that could
// not begin is an error and leaves nothing running. One that has begun
// outlives its consumer and its connection's reconnects on its own, saying so
// through h.Gap, until it is stopped or the queue's connection is closed for
// good, which takes the node down with it ([Queue.Lost]).
//
// # Subjects and never records
//
// The consumer is HEADERS ONLY: the broker sends each record's subject, its
// headers and its position, and never its body. The records a watch looks at
// are a domain's own — the code log's as large as the transport's whole
// [statelog.MaxTransportRecordBytes], 8 MiB less 4 KiB — and a watcher wants
// to know WHICH object moved, which it reads from the rows its own copy
// derives. Carrying every record to every watching node as well would cost
// each of them the log's whole volume a second time, over a leaf's link as
// much as over a member's routes.
//
// # From the log's end, and acknowledging nothing
//
// It begins at the log's END (DeliverNew): what the log held before the watch
// began is what the caller's rows already say, read by the pass the caller
// makes once the watch has begun. And it acknowledges NOTHING (AckNone), so
// no delivery is ever held, redelivered or counted against anybody: a nudge
// is worth something only while it is early, and the rows decide every
// question it raises.
//
// # One replica, in memory, replaced rather than repaired
//
// The consumer is ONE replica held IN MEMORY by one member of the stream's
// group: its whole state is how far it has delivered, which nothing needs to
// survive. A replicated consumer would be a raft group per watching node and
// a proposal per delivery, for state a new consumer at the log's end replaces
// for the price of one create. So a consumer that goes — the member holding it
// stopped or was cut off, the broker reaped it while this node was away
// ([ephemeralInactive]), somebody deleted it — is not waited for: the watch
// creates another at the log's end, under a name of its own, and calls h.Gap
// through the position the new consumer begins after, and the caller reads
// its rows for what the gap held once its copy is there.
//
// # How a loss is known, and how far its Gap reaches
//
// The broker sends an idle heartbeat to a standing pull every
// [watchHeartbeat] while it has nothing to deliver, so silence for two of
// them is a consumer that is not serving this watch whatever the reason — and
// so is a pull nobody serves (no responders: no member holds the consumer),
// and the broker's word that it deleted the consumer or that the member
// holding it is shutting down ([lostConsumer]). Each is a loss, answered with
// a new consumer, and its Gap reaches the log's end when the broker made that
// consumer, because the new consumer delivers everything past it — a position
// the create's own reply carries, so telling it costs nothing more.
//
// Two things are not a loss, and each is a Gap with the consumer kept. One is
// a delivery the consumer counted and this node never received: every
// delivery carries the consumer's own count, which must rise by one each
// time, and the Gap reaches the position of the delivery that revealed the
// miss, since the consumer delivers in log order. The other is a reconnect of
// the queue's connection, across which a delivery may have gone to an inbox
// nobody held: the connection's reconnect count is read every heartbeat, so
// the caller hears of one within one, and the Gap reaches the log's end, read
// once the reconnect is noticed — at or past every record the consumer can
// have delivered by then, since a consumer delivers only what the log already
// holds. A delivery whose position cannot be read is told as a reconnect is.
// One the broker still makes to the old interest after that read, in the
// instant before this node's resubscription reaches the consumer's member, the
// consumer's count reveals at the next delivery.
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
	// REFUSED HERE, because the broker does not: it makes a consumer under
	// any filter at all (the subset check its error 10093 names is gone
	// from it), and one outside the stream's subjects delivers nothing, for
	// ever, with nothing saying why. A filter that is not a subject pattern
	// at all the client library and the broker both refuse, so its syntax
	// stays theirs.
	if server.IsValidSubject(filter) && !l.carries(filter) {
		return nil, fmt.Errorf("jetstream: a watch on %q under %s would hear "+
			"nothing — the log carries %v, and no subject there matches it; "+
			"name subjects inside them", l.name, filter, l.subjects())
	}
	// DETACHED from the call that begins the watch, as the live feed's
	// consumer is: ctx bounds the first create, and the watch lives until
	// Stop.
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w := &DomainWatch{
		l: l, filter: filter, h: h, timing: l.watching(), logger: l.q.log,
		prefix: "watch_" + nuid.Next() + "_",
		life:   life, cancel: cancel,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if err := w.begin(ctx, 1); err != nil {
		cancel()
		return nil, fmt.Errorf("jetstream: watch %q under %s: %w", l.name, filter, err)
	}
	go w.supervise()
	return w, nil
}

// watchHeartbeat is how often the broker tells an idle watch that its
// consumer is still serving it — and so, at two of them missed, how long a
// consumer that stopped is still taken to be serving.
//
// FIVE SECONDS, the client library's own heartbeat for its ordered consumer,
// the one other reader it builds to replace a consumer it lost rather than
// wait for it (nats.go's jetstream/pull.go): a watch notices a member that
// went within ten seconds, for one heartbeat every five to a watch with
// nothing to deliver, where the library's default for a plain pull, fifteen,
// left a watch deaf for thirty. The pull's other two settings are the
// library's defaults and hold here: a request that stands thirty seconds and
// is asked again as it ends, and five hundred records a request, which as
// headers is a few hundred kilobytes in flight at most.
const watchHeartbeat = 5 * time.Second

// watchRetry and watchRetryCeiling space the attempts to replace a lost
// consumer — the first at once, then a second later, doubling to ten seconds
// between attempts — spread by watchRetryJitter so the watching nodes of a
// fleet that lost a member together do not ask its metadata group in step.
//
// THE CLIENT LIBRARY'S NUMBERS for replacing its ordered consumer (nats.go's
// jetstream/ordered.go), for the same kind of reader. A member's re-election
// or restart takes seconds, so the first attempts meet its return; a broker
// that stays away is asked every ten seconds, one metadata proposal a
// watching node, which a recovering group can afford. And a watch that cannot
// begin again costs its caller latency and nothing else, since the caller's
// rows are its record.
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
	// attempt bounds one create of a replacement consumer: in production
	// the client's own default for a request with no deadline, the five
	// seconds every request this node sends without one is given. The
	// reply to a create comes once the metadata group has committed it and
	// the consumer's one member leads it — milliseconds on a healthy
	// fleet — and an attempt that outlasts it, its request lost with a
	// member going down, is asked again under the SAME name, which the
	// broker answers with the consumer an earlier attempt made rather than
	// a second one. Not the provisioning's write term ([jsprovision.AskTerm],
	// fifteen seconds clustered and thirty solo): that is sized for a
	// replicated group's election, which a one-member consumer has none of,
	// and a create lost to a restarting member would hold the watch deaf
	// for all of it. It bounds the read of the log's end a Gap with no
	// position of its own waits for too ([DomainWatch.placeGap]): one
	// request the stream's leader answers as fast, asked again inside the
	// bound as every read of the log's state is ([DomainLog.stateInfo]).
	attempt time.Duration
	// retry and retryCeiling space the attempts to replace a lost consumer.
	retry, retryCeiling time.Duration
}

// carries reports whether filter can match a subject the log carries.
//
// OVERLAP, not containment: a filter wider than the log (the code log's
// `crewlet.code.>`, say) watches every record the log holds, which is a
// watch its caller can mean; one that meets none of its subjects can only be
// a mistake. The subjects are the handle's, read at open: a safety field no
// node changes on a running stream ([Queue.observeStream]).
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
// ONE GOROUTINE OF ITS OWN, the supervisor, which replaces a lost consumer,
// reads the connection's reconnect count and reads the log's end for a Gap
// that has no position of its own, and which [DomainWatch.Stop] ends and
// waits for — so no request the watch sends is made on the delivery path.
// Deliveries and the client's reports about its pull arrive on the client's
// own goroutines; all three meet under mu, where every call to the handler is
// made, which is what makes the calls one at a time and none after Stop.
type DomainWatch struct {
	l      *DomainLog
	filter string
	h      WatchHandler
	timing watchTiming
	logger *slog.Logger

	// prefix begins the name of each of this watch's consumers, one a
	// GENERATION, the generation's number after it — so a create asked
	// again after a reply that never came is the same create, which the
	// broker answers with the consumer it already made, rather than a second
	// consumer beside it.
	prefix string

	// life is the watch's own context, cancelled by Stop: every create and
	// read the supervisor asks runs under it.
	life   context.Context
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
	// gapPending is the Gap a loss owes, called once its replacement has
	// begun — by the replacement's first delivery or by the supervisor,
	// whichever comes first — through the position the replacement began
	// after, and never before, or the records appended between the call and
	// the new consumer would be covered by nothing.
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

// config is the consumer one generation reads through — see [DomainLog.Watch]
// for why each setting is what it is.
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
	// READ BEFORE THE CONSUMER EXISTS, so a reconnect while it is being
	// made is one the watch notices at its next recheck rather than one it
	// starts past. One before it is answered without a Gap of its own:
	// whatever a delivery lost to it carried lies at or below the position
	// this consumer begins after, which the Gap a loss owes reaches, or this
	// consumer delivers it.
	reconnects := w.l.js.Conn().Stats().Reconnects
	w.l.q.noteConsumerProposal()
	cons, err := w.l.js.CreateConsumer(ctx, w.l.name, w.config(name))
	if err != nil {
		return fmt.Errorf("create the consumer %s: %w", name, err)
	}
	// WHERE IT BEGINS, from the create's own reply: a consumer of new
	// records reports everything up to the log's end when the broker made
	// it as delivered, and delivers every record after. So that end is
	// exactly how far a caller's rows must reach to hold what the watch
	// missed before this consumer, told for nothing beyond the create —
	// where a read of the log's end would be a second request saying less.
	// A create asked again is answered with the consumer an earlier attempt
	// made, which reports where IT was made: the position that holds, since
	// that consumer delivers everything after it.
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
			meta, err := msg.Metadata()
			w.take(gen, msg.Subject(), meta, err)
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
		// NO COUNT, so nothing says whether a delivery went missing before
		// this one, and NO POSITION, so nothing says how far the record it
		// carried lies: a Gap through the log's end, which the supervisor
		// reads — a request, which the delivery path never waits on.
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
// IT ANSWERS FOR A GAP STILL WAITING FOR THE LOG'S END ([DomainWatch.gapUnread])
// as well: whatever a delivery the lost consumer made and this node never took
// carried lies at or below that position, or the replacement delivers it.
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
// watch is stopped or its connection closed for good. What it acts on is the
// watch's state, so a wake that arrives while it is busy is answered by the
// pass it is in or the next.
func (w *DomainWatch) supervise() {
	defer close(w.done)
	recheck := time.NewTicker(w.timing.recheck)
	defer recheck.Stop()
	for {
		select {
		case <-w.life.Done():
			return
		case <-recheck.C:
			w.recheckConnection()
		case <-w.wake:
		}
		if !w.replace() {
			return
		}
		w.placeGap()
	}
}

// recheckConnection owes a Gap when the queue's connection reconnected since
// it was last read: a delivery made while it was away may have gone to an
// inbox nobody held, and nothing would say so until the next one. It reaches
// the log's end, which [DomainWatch.placeGap] reads next.
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
// — a reconnect, a delivery whose position could not be read — through the
// log's end, once it has read it: every record the consumer delivered before
// the read lies at or below it, since a consumer delivers only what the log
// already holds. Read here and not where the Gap was noticed, because a read
// is a request and the delivery path never waits on one.
//
// THE LOG'S END, NOT THE CONSUMER'S OWN DELIVERED POSITION, though the
// consumer's info would name a tighter one: a member answers for a one-replica
// consumer it holds before the consumer has chosen where it begins — on a
// cluster that choice waits for it to lead, as after the member restarted and
// made it again — and reports it as having delivered nothing, a position
// below everything its predecessor delivered. The log's end has no such
// moment: no consumer delivers past it.
//
// A read that fails leaves the Gap owed, asked again at the next recheck. A
// loss meanwhile owes a Gap of its own, which answers for this one
// ([DomainWatch.flushGap]).
func (w *DomainWatch) placeGap() {
	w.mu.Lock()
	owed := w.gapUnread && w.live && !w.stopped
	w.mu.Unlock()
	if !owed {
		return
	}
	ctx, cancel := context.WithTimeout(w.life, w.timing.attempt)
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
// once and then again, spaced, until a consumer begins or the watch ends,
// and reports whether the watch goes on.
func (w *DomainWatch) replace() bool {
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
		ctx, cancel := context.WithTimeout(w.life, w.timing.attempt)
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
		if errors.Is(err, errWatchStopped) || w.life.Err() != nil {
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
		case <-w.life.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// Stop ends the watch: when it returns, the handler is not being called and
// never will be again. Safe to call more than once and from more than one
// goroutine.
//
// THE CONSUMER IS LEFT for the broker to reap ([ephemeralInactive]) rather
// than deleted: a delete is a metadata proposal on the stopping path, which
// is where a node is least able to wait for one, and a consumer nothing pulls
// from costs its member nothing a minute does not end.
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
