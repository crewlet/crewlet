package jetstream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsinflight"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// DomainLog is one state-log domain's append surface: a conditional publish
// that returns the acknowledgement the ordinary publish path discards, and
// the last-message query a rejection is discriminated with.
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
	return &DomainLog{js: q.js, stream: s, name: stream, q: q, crossing: q.recordCrossing(s),
		info: func(ctx context.Context) (*jetstream.StreamInfo, error) {
			// A HANDLE OF THIS REQUEST'S OWN — see [DomainLog.info].
			fresh, err := q.js.Stream(ctx, stream)
			if err != nil {
				return nil, err
			}
			return fresh.CachedInfo(), nil
		}}, nil
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
	return jsprovision.ReadWith(ctx, l.q.provisioning(), l.info, nil)
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
