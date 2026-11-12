package jetstream

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE WAKE FEED'S DEFERRAL BACKS OFF NO FURTHER THAN THIS CONSUMER'S ACK
// WINDOW: a record handed back for a peer (changefeed.DeferDelay, #838) waits
// at most changefeed.DeferCap, which is domainConsumerAckWait, so it is never
// redelivered more slowly than a record nobody answered at all.
//
// Mutation: raise either constant alone, red.
func TestTheFeedsDeferralCapIsTheDomainAckWait(t *testing.T) {
	if changefeed.DeferCap != domainConsumerAckWait {
		t.Errorf("changefeed.DeferCap is %v and the domain consumer's AckWait %v: a deferred record would "+
			"come back on another schedule than an unanswered one", changefeed.DeferCap, domainConsumerAckWait)
	}
}

// A WAKE FEED'S DEAD LETTER IS KEPT IN CREWLET_DLQ FOR AS LONG AS THE EVENT
// STREAM KEEPS ANYTHING, and says when it goes.
//
// The entry carries the record's sealed frame, so it needs its log for
// nothing; what bounds it is the dead-letter stream's own age, which is the
// event stream's `stream.event_retention_hours` — a safety field every node of
// a fleet holds equal, so no node keeps an entry longer than another reads it
// as kept. The stream keeps by age and never by interest, so the entry is
// there whether or not anybody reads the subject; and the instant a listing
// says it expires is the one that bound gives, which the alarm counts down to.
//
// Mutation: route the feed's subject to another stream, or give the dead
// letters an age of their own, and the stream or its bound is wrong; compute
// the expiry from anything but the stream's bound, and the instant is.
func TestAFeedsDeadLetterIsKeptInTheDeadLetterStreamForTheEventRetention(t *testing.T) {
	t.Parallel()
	subject := changefeed.DeadLetterSubject(changefeed.Source{Name: "work", Group: "crewlet-tracker-feed"})
	for name, tc := range map[string]struct {
		cfg  Config
		keep time.Duration
	}{
		"the default retention":   {cfg: Config{}, keep: defaultEventRetention},
		"an operator's retention": {cfg: Config{EventRetention: 48 * time.Hour}, keep: 48 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := openForTest(t, tc.cfg)
			if _, err := q.KeepDeadLetter(t.Context(), subject, "the record", []byte(`{"v":1}`),
				[]byte("frame")); err != nil {
				t.Fatalf("KeepDeadLetter: %v", err)
			}
			stream, err := q.streamFor(t.Context(), subject)
			if err != nil || stream != streamDeadLetter {
				t.Fatalf("the feed's dead letters land in %q (%v), want %s", stream, err, streamDeadLetter)
			}
			st, err := q.js.Stream(t.Context(), streamDeadLetter)
			if err != nil {
				t.Fatalf("read %s: %v", streamDeadLetter, err)
			}
			cfg := st.CachedInfo().Config
			if cfg.MaxAge != tc.keep || cfg.Retention != jetstream.LimitsPolicy {
				t.Errorf("%s keeps by %v for %v, want by limits for the event retention %v",
					streamDeadLetter, cfg.Retention, cfg.MaxAge, tc.keep)
			}
			if _, err := st.GetLastMsgForSubject(t.Context(), subject); err != nil {
				t.Errorf("the entry is not in %s: %v", streamDeadLetter, err)
			}
			kept, err := q.DeadLettersOn(t.Context(), subject)
			if err != nil || len(kept) != 1 {
				t.Fatalf("the subject lists %d entries (%v), want the one kept", len(kept), err)
			}
			if want := kept[0].StoredAt.Add(tc.keep); !kept[0].Expires.Equal(want) {
				t.Errorf("the entry kept at %v expires at %v, want %v: the stream's own %v later",
					kept[0].StoredAt, kept[0].Expires, want, tc.keep)
			}
		})
	}
}

// A SECOND KEEP OF ONE IDENTITY IS STORED AS NOTHING FOR AS LONG AS A KEEP CAN
// TAKE.
//
// A keep carries its identity as its message id, and the dead-letter stream
// collapses a repeated one within its duplicate window — the contract's
// window, which has to outlast the slowest keep this backend makes: one at the
// transport's ceiling across a link at the floor, the request term and a
// crossing for each of its sends. Left to the broker's default of two minutes,
// a peer's copy of a record whose first keep was still crossing four minutes
// later was a second entry. So the stream is provisioned with the window, and
// the window holds the slowest keep.
//
// Mutation: provision the dead letters without the window, or shorten the
// window below the slowest keep, and it goes red.
func TestTheDeadLettersCollapseASecondKeepForAsLongAsAKeepCanTake(t *testing.T) {
	t.Parallel()
	slowest := jsapi.RequestTerm +
		time.Duration(1+jetstream.DefaultPubRetryAttempts)*jsapi.Sending(queue.MaxPayloadBytes, objstore.MiBPace)
	if queue.DeadLetterWindow < slowest {
		t.Errorf("the dead letters collapse a second keep for %v, and the slowest keep waits %v: "+
			"a copy still crossing past the window is a second entry", queue.DeadLetterWindow, slowest)
	}
	q := openForTest(t, Config{})
	subject := changefeed.DeadLetterSubject(changefeed.Source{Name: "work", Group: "crewlet-tracker-feed"})
	for i, want := range []bool{false, true} {
		already, err := q.KeepDeadLetter(t.Context(), subject, "the record", []byte(`{"v":1}`), []byte("frame"))
		if err != nil || already != want {
			t.Fatalf("keep %d of one identity = (already %t, %v), want already %t", i+1, already, err, want)
		}
	}
	st, err := q.js.Stream(t.Context(), streamDeadLetter)
	if err != nil {
		t.Fatalf("read %s: %v", streamDeadLetter, err)
	}
	if got := st.CachedInfo().Config.Duplicates; got != queue.DeadLetterWindow {
		t.Errorf("%s collapses a repeated keep for %v, want the contract's window, %v",
			streamDeadLetter, got, queue.DeadLetterWindow)
	}
}

// A LISTING MOVES NO FRAME, AND LEAVES NO CONSUMER BEHIND.
//
// The alarm's reading lists the feeds' subjects whenever their marks move, on
// every data node, and an entry's frame is a record as large as the transport
// carries: so a listing asks the broker for headers only, and the node that
// lists — a node of its own here, its connection carrying nothing but what
// the listing receives — takes in less than one of the frames for every entry
// together, while every entry is listed with its header. And the consumer the
// listing read through is deleted once it has answered, where one left for
// the broker to reap would be a consumer on the dead-letter stream for every
// listing of the last half minute.
//
// Mutation: drop HeadersOnly and the frames cross the connection; drop the
// delete, and the listing's consumer is still on the stream.
func TestAListingMovesNoFrameAndLeavesNoConsumer(t *testing.T) {
	t.Parallel()
	q := openForTest(t, Config{})
	lister, err := q.Peer(t.Context())
	if err != nil {
		t.Fatalf("open a second node on the broker: %v", err)
	}
	t.Cleanup(func() {
		if err := lister.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the second node: %v", err)
		}
	})
	subject := changefeed.DeadLetterSubject(changefeed.Source{Name: "work", Group: "crewlet-tracker-feed"})
	frame := bytes.Repeat([]byte("f"), 1<<20)
	headers := []string{`{"v":1,"seq":1}`, `{"v":1,"seq":2}`, `{"v":1,"seq":3}`}
	for _, header := range headers {
		if _, err := q.KeepDeadLetter(t.Context(), subject, header, []byte(header), frame); err != nil {
			t.Fatalf("KeepDeadLetter: %v", err)
		}
	}
	// THE STREAM IS PROVISIONED ON THE LISTER'S CLIENT before the count
	// begins, so what is counted is the listing's own traffic.
	if _, err := lister.DeadLetterMarks(t.Context(), changefeed.DeadLettersUnder()); err != nil {
		t.Fatalf("DeadLetterMarks: %v", err)
	}

	before := lister.Conn().Stats().InBytes
	kept, err := lister.DeadLettersOn(t.Context(), subject)
	received := lister.Conn().Stats().InBytes - before
	if err != nil || len(kept) != len(headers) {
		t.Fatalf("the subject lists %d entries (%v), want the %d kept", len(kept), err, len(headers))
	}
	for i, k := range kept {
		if string(k.Header) != headers[i] {
			t.Errorf("entry %d is listed with the header %q, want %q", i, k.Header, headers[i])
		}
	}
	if received >= uint64(len(frame)) {
		t.Errorf("the listing node took in %d bytes listing %d entries of a %d-byte frame each: "+
			"the frames crossed its connection", received, len(kept), len(frame))
	}

	st, err := q.js.Stream(t.Context(), streamDeadLetter)
	if err != nil {
		t.Fatalf("read %s: %v", streamDeadLetter, err)
	}
	names := st.ListConsumers(t.Context())
	var left []string
	for info := range names.Info() {
		left = append(left, info.Name)
	}
	if err := names.Err(); err != nil {
		t.Fatalf("list the consumers on %s: %v", streamDeadLetter, err)
	}
	if len(left) != 0 {
		t.Errorf("the listing left the consumers %v on %s", left, streamDeadLetter)
	}
}

// THE FRAMING A KEEP IS HELD TO COVERS EVERY BYTE THIS BACKEND ADDS.
//
// A keep is refused once its header and body come within queue.LetterFraming
// of the transport's ceiling, on the twin as here; so the header block this
// backend wraps them in — `NATS/1.0`, the expected stream, the dead-letter
// header's name and the line endings — must fit inside that framing, or an
// entry the contract admits is one the transport refuses.
//
// Mutation: lower queue.LetterFraming under the header block, or add a header
// to the keep's message, and the block outgrows it.
func TestTheFramingAKeepIsHeldToCoversItsHeaderBlock(t *testing.T) {
	t.Parallel()
	header, body := []byte(`{"v":1}`), []byte("frame")
	msg := letterMsg("dlq.changefeed.work.grp.0123456789ab", streamDeadLetter,
		strings.Repeat("an identity of any length ", 64), header, body)
	block := msg.Size() - len(msg.Subject) - len(header) - len(body)
	if block > queue.LetterFraming {
		t.Errorf("a keep's message adds %d bytes to its header and body, past the %d "+
			"queue.LetterFraming holds every backend to", block, queue.LetterFraming)
	}
}

// A WAKE FEED'S LARGEST ENTRY IS KEPT.
//
// A record at its log's ceiling is a frame of the transport's ceiling less
// what a stored record carries beside its payload (statelog's appendOverhead),
// sealed under the longest key id a keyring may name; its entry is that frame
// beside a header at its worst — every number at its largest, a long subject,
// and a detail of control characters, each escaped as six bytes. Kept through
// this backend over the wire, it is accepted: a feed's entry that the
// transport refused would be a record that circles at the head of its consumer
// for ever.
//
// Mutation: put the frame back in the header, or let the detail grow past its
// quarter kibibyte, and the keep is refused as too large.
func TestAWakeFeedsLargestEntryIsKept(t *testing.T) {
	t.Parallel()
	signer, err := recordseal.NewSigner("work", recordseal.OneKey(strings.Repeat("k", 64), "material"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	frame := signer.Seal(bytes.Repeat([]byte{0xff}, statelog.MaxTransportRecordBytes))
	header, err := changefeed.EncodeDeadLetter(changefeed.DeadLetter{
		Source: "work", Group: "crewlet-tracker-feed", Stream: "CREWLET_TRACKER_LOG",
		Seq: math.MaxUint64, Gen: math.MaxUint64,
		Key: "crewlet.tracker.log.task." + strings.Repeat("k", 512), OpID: strings.Repeat("o", 64),
		Reason: changefeed.DeferUntranslatable, Detail: strings.Repeat("\x01", 1<<10),
		StoredAt: time.Unix(1_700_000_000, 999_999_999).UTC(), Delivered: math.MaxUint64,
		DeadLetteredAt: time.Unix(1_700_003_600, 999_999_999).UTC(),
	})
	if err != nil {
		t.Fatalf("EncodeDeadLetter: %v", err)
	}
	q := openForTest(t, Config{})
	subject := changefeed.DeadLetterSubject(changefeed.Source{Name: "work", Group: "crewlet-tracker-feed"})
	// THE LONGEST IDENTITY a feed derives too, which the message carries at
	// a fixed size whatever its length.
	id := strings.Repeat("CREWLET_TRACKER_LOG", 32)
	if _, err := q.KeepDeadLetter(t.Context(), subject, id, header, frame); err != nil {
		t.Fatalf("an entry of a %d-byte frame and a %d-byte header was refused: %v",
			len(frame), len(header), err)
	}
	kept, err := q.DeadLettersOn(t.Context(), subject)
	if err != nil || len(kept) != 1 || !bytes.Equal(kept[0].Header, header) {
		t.Fatalf("the largest entry lists as %d entries (%v), want it with its header", len(kept), err)
	}
	back, held, err := q.DeadLetterFrame(t.Context(), subject, kept[0].Seq)
	if err != nil || !held || !bytes.Equal(back, frame) {
		t.Errorf("the largest entry's frame reads back as %d bytes, held %v (%v), want its %d",
			len(back), held, err, len(frame))
	}
}
