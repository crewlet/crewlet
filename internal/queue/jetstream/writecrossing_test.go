package jetstream

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// slowLinkRate is the pace the cases below hold a leaf's link to, towards its
// member: a mebibyte a second, thirty times the slowest a link is allowed to
// be ([objstore.MiBPace]). A write at the transport's ceiling takes about
// eight seconds to cross it — past the five seconds a write whose caller set
// no deadline was given whatever it carried, and far inside the time its bytes
// are given to cross at the slowest pace allowed.
const slowLinkRate = 1 << 20

// A WRITE ACROSS A LINK IS WAITED FOR AS LONG AS ITS BYTES TAKE TO CROSS IT.
//
// A node without the `data` role reaches the fleet's broker as a leaf, across
// its link to a member, and every node of a fleet on an external broker dials
// one: either way a write it makes is acknowledged only once its last byte has
// crossed that link, which the engine allows to carry as little as a
// mebibyte in [objstore.MiBPace]. A write whose caller set no deadline was
// held to the client's five seconds whatever it carried, so one at the
// transport's ceiling could not be acknowledged at any pace below about a
// mebibyte and a half a second, and failed as the broker being unavailable: a
// keeper retries such an entry for ever, as an outage, while each copy of it
// crosses the link and lands (crewlet/crewlet#1005, the conformance suite's
// keep at the ceiling through a leaf, on a loaded runner).
//
// Each case writes at its write's ceiling with no deadline of its own, across
// a leaf's link carrying a mebibyte a second — side by side, so their
// transfers overlap — and then asks the broker what landed. The payloads are
// random, so a link that compresses still carries every byte.
//
// Mutation: drop the crossing from the budget KeepDeadLetter, Publish, Append
// or the budget's dead letter waits within, and that case fails at five
// seconds: the broker unavailable, or nothing kept.
func TestAWriteAcrossALinkIsWaitedForAsLongAsItsBytesTakeToCross(t *testing.T) {
	t.Parallel()

	t.Run("a_dead_letter_at_the_contracts_ceiling", func(t *testing.T) {
		t.Parallel()
		q := leafAcrossSlowLink(t, slowLinkRate)
		subject := topics.DeadLetter("slowlink.letters", "grp")
		header := []byte(`{"v":1}`)
		body := incompressible(t, queue.MaxPayloadBytes-queue.LetterFraming-len(header))
		if _, err := q.KeepDeadLetter(t.Context(), subject, "at the ceiling", header, body); err != nil {
			t.Fatalf("a dead letter at the ceiling, across a link carrying a mebibyte a "+
				"second, was not kept: %v", err)
		}
		marks, err := q.DeadLetterMarks(t.Context(), topics.DeadLettersUnder("slowlink"))
		if err != nil {
			t.Fatalf("DeadLetterMarks: %v", err)
		}
		if got := marks.Counts[subject]; got != 1 {
			t.Errorf("the subject holds %d entries after the keep answered, want the one kept", got)
		}
	})

	// THE DELIVERY BUDGET'S DEAD LETTER, which the consume loop writes on
	// a context with no deadline: a message at the ceiling the budget gives
	// up on across a slow link is kept, and said to be. Its bytes land
	// whatever the publish answers, so the answer is what is held here —
	// at five seconds it was `dead_letter_failed` for a message the stream
	// went on to keep.
	t.Run("a_message_the_budget_dead_letters_at_the_ceiling", func(t *testing.T) {
		t.Parallel()
		q := leafAcrossSlowLink(t, slowLinkRate)
		const topic, group = "slowlink.retired", "grp"
		if err := q.deadLetter(t.Context(), topics.DeadLetter(topic, group), "a message at the ceiling",
			incompressible(t, queue.MaxPayloadBytes)); err != nil {
			t.Fatalf("a message at the ceiling the budget dead-lettered, across a link "+
				"carrying a mebibyte a second, was answered as not kept: %v", err)
		}
		marks, err := q.DeadLetterMarks(t.Context(), topics.DeadLettersUnder("slowlink"))
		if err != nil {
			t.Fatalf("DeadLetterMarks: %v", err)
		}
		if got := marks.Counts[topics.DeadLetter(topic, group)]; got != 1 {
			t.Errorf("the retired message's subject holds %d entries after the budget "+
				"dead-lettered it, want the one message", got)
		}
	})

	t.Run("an_event_at_the_largest_its_stream_takes", func(t *testing.T) {
		t.Parallel()
		q := leafAcrossSlowLink(t, slowLinkRate)
		topic := topics.Event("slowlink_probe")
		if err := q.Publish(t.Context(), topic, eventAtTheCeiling(t, q)); err != nil {
			t.Fatalf("an event at the transport's ceiling, across a link carrying a "+
				"mebibyte a second, was not published: %v", err)
		}
		stream, err := q.streamFor(t.Context(), topic)
		if err != nil {
			t.Fatalf("resolve the event's stream: %v", err)
		}
		info, err := q.js.Stream(t.Context(), stream)
		if err != nil {
			t.Fatalf("read the event stream's state: %v", err)
		}
		if state := info.CachedInfo().State; state.Msgs != 1 || state.Bytes < queue.MaxPayloadBytes {
			t.Errorf("the event stream holds %d messages in %d bytes after the publish "+
				"answered, want the one event of at least %d", state.Msgs, state.Bytes,
				queue.MaxPayloadBytes)
		}
	})

	t.Run("a_record_at_the_largest_its_log_takes", func(t *testing.T) {
		t.Parallel()
		q := leafAcrossSlowLink(t, slowLinkRate)
		spec := DomainStream{Name: "CREWLET_SLOWLINK_LOG", Subjects: []string{"crewlet.slowlink.log.>"},
			MaxBytes: 4 * queue.MaxPayloadBytes, MaxMessageBytes: queue.MaxPayloadBytes,
			Duplicates: 2 * time.Minute}
		if err := q.EnsureDomainStream(t.Context(), spec); err != nil {
			t.Fatalf("EnsureDomainStream: %v", err)
		}
		log, err := q.DomainLog(t.Context(), spec.Name)
		if err != nil {
			t.Fatalf("open the log: %v", err)
		}
		// A CREATE, as the state log's publisher makes one: its operation's
		// id and the expectation that the subject holds nothing ride as
		// headers beside the largest record the transport carries.
		none := uint64(0)
		seq, duplicate, err := log.Append(t.Context(), "crewlet.slowlink.log.object.big",
			"slowlink-create", &none, incompressible(t, statelog.MaxTransportRecordBytes))
		if err != nil || duplicate {
			t.Fatalf("a record at the transport's ceiling, across a link carrying a mebibyte "+
				"a second = (%d, duplicate %t, %v), want it appended", seq, duplicate, err)
		}
		// THE LOG'S OWN STATE, never the record read back: a leader's reply
		// carrying the record base64-encoded is past what a leaf's link
		// carries, and the leaf closes the link it arrives on.
		stats, err := log.Stats(t.Context())
		if err != nil {
			t.Fatalf("read the log's state: %v", err)
		}
		if stats.LastSeq != seq || stats.Messages != 1 || stats.Bytes < statelog.MaxTransportRecordBytes {
			t.Errorf("the log ends at %d holding %d records in %d bytes after the append "+
				"answered %d, want the one record of at least %d", stats.LastSeq,
				stats.Messages, stats.Bytes, seq, statelog.MaxTransportRecordBytes)
		}
	})
}

// leafAcrossSlowLink is a client of a leaf whose link to its member crosses a
// slow link carrying rate bytes a second towards the member ([linktest.Slow]).
func leafAcrossSlowLink(t *testing.T, rate int64) *Queue {
	t.Helper()
	cfg := testTimings(Config{FleetAuth: testFleetAuth()})
	memberCfg := cfg
	memberCfg.ServerName = "member"
	memberCfg.LeafHost, memberCfg.LeafPort = "127.0.0.1", AnyPort
	// A MEMBER THAT SERVES LEAVES PERSISTS, and the leaf creates what it
	// is first to use in the file store to match.
	memberCfg.StoreDir, memberCfg.StoreMaxBytes = t.TempDir(), brokertest.StoreMaxBytes
	member := startTestServer(t, memberCfg)

	link := linktest.Slow(t, fmt.Sprintf("127.0.0.1:%d", member.LeafPort()), rate)
	leafCfg := cfg
	leafCfg.ServerName = "leaf"
	leafCfg.LeafURLs = []string{"nats-leaf://" + link.Addr()}
	leaf := startTestServer(t, leafCfg)
	q, err := leaf.Client(t.Context())
	if err != nil {
		t.Fatalf("a leaf across a slow link did not reach the fleet: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return q
}

// eventAtTheCeiling is an event whose encoding, as q publishes it, is exactly
// the transport's ceiling — the largest event a stream of the engine's takes,
// none of which states a largest message of its own — its bulk random text.
func eventAtTheCeiling(t *testing.T, q *Queue) *events.Event {
	t.Helper()
	e := ev(1)
	encoded := func(blob string) int {
		e.Extra = map[string]json.RawMessage{"blob": json.RawMessage(`"` + blob + `"`)}
		data, err := json.Marshal(q.contract.Stamp(e))
		if err != nil {
			t.Fatalf("encode the event: %v", err)
		}
		return len(data)
	}
	room := queue.MaxPayloadBytes - encoded("")
	blob := base64.StdEncoding.EncodeToString(incompressible(t, room))[:room]
	if got := encoded(blob); got != queue.MaxPayloadBytes {
		t.Fatalf("the event encodes in %d bytes, want the ceiling's %d", got, queue.MaxPayloadBytes)
	}
	return e
}

// incompressible is n random bytes: what a compressing link carries whole.
func incompressible(t *testing.T, n int) []byte {
	t.Helper()
	out := make([]byte, n)
	if _, err := rand.Read(out); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	return out
}
