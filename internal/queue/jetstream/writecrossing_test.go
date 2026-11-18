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

// slowLinkRate is the pace the cases hold a leaf's link to, towards its member:
// a mebibyte a second, thirty times the slowest allowed ([objstore.MiBPace]).
// A write at the transport's ceiling takes about eight seconds to cross it,
// past the request term's five seconds and far inside its budget at the floor.
const slowLinkRate = 1 << 20

// A WRITE ACROSS A LINK IS WAITED FOR AS LONG AS ITS BYTES TAKE TO CROSS IT
// (package doc, "A write across a link is given the time its bytes take to
// cross"): held to five seconds, a write at the ceiling fails as the broker
// unavailable, and a keeper retries it for ever while each copy lands.
//
// Each case writes at its write's ceiling with no deadline of its own, side by
// side across one slow leaf link so their transfers overlap, then asks the
// broker what landed. Random payloads, so a compressing link carries every byte.
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

	// The delivery budget's dead letter, written on a context with no
	// deadline: its bytes land whatever the publish answers, so the answer
	// is what is held (a kept message must not read dead_letter_failed).
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
		// A create as the state log's publisher makes one: an operation id
		// and an expect-empty header beside the largest record.
		none := uint64(0)
		seq, duplicate, err := log.Append(t.Context(), "crewlet.slowlink.log.object.big",
			"slowlink-create", &none, incompressible(t, statelog.MaxTransportRecordBytes))
		if err != nil || duplicate {
			t.Fatalf("a record at the transport's ceiling, across a link carrying a mebibyte "+
				"a second = (%d, duplicate %t, %v), want it appended", seq, duplicate, err)
		}
		// The log's own state, never the record read back: a leader's reply
		// carrying it base64-encoded is past what a leaf's link carries, and
		// the leaf closes the link it arrives on.
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
	// A member that serves leaves persists, and the leaf creates what it
	// first uses in the file store to match.
	memberCfg.StoreDir, memberCfg.StoreMaxBytes = brokertest.TempDir(t), brokertest.StoreMaxBytes
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

// eventAtTheCeiling is an event whose encoding as q publishes it is exactly the
// transport's ceiling (no engine stream states a smaller largest message), its
// bulk random text.
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
