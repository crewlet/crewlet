package jetstream

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// readSlack is what a read across the link may carry towards the leaf beside
// the message it reads: the leader's answers about the stream, the consumers'
// creations, infos and deletions, a probe's header block and the protocol
// lines around them — a few kibibytes, held here at sixty-four so a case
// fails only on a message that crossed twice, or crossed where only its
// headers should have.
const readSlack = 64 << 10

// A DEAD LETTER AT THE KEEP'S CEILING IS READ WHOLE AND DROPPED THROUGH A
// LEAF, AND THE LEAF'S LINK STAYS UP.
//
// A node without the `data` role reaches the dead letters across its leaf
// link, and the server on the leaf's side closes that WHOLE link on any
// message larger than its max_payload (nats-server v2.15.2, leafnode.go,
// processLeafMsgArgs → maxPayloadViolation) — the transport's eight
// mebibytes. The stream leader's message read answers in JSON, the entry
// base64-encoded inside it, a third larger than the entry: an entry at the
// keep's ceiling came back as about eleven megabytes, the leaf closed its
// link, and the read was asked again over a link redialled only to be closed
// again, every lease and mailbox the node holds cut off with it
// (crewlet/crewlet#1031). So across a link the frame crosses RAW, delivered by
// a consumer, and the drop's check reads the entry's headers alone.
//
// Held here: the frame reads back as it was kept, having crossed the link
// once; the drop removes it, moving none of its body; and the member's
// connection to the leaf is the same one throughout.
//
// Mutation: read the frame, or check the drop, with the leader's message
// read across a link, and the leaf closes its link on the reply — the read
// fails, and the member's link to the leaf is another connection; or check
// the drop with a consumer that delivers the payload, and the drop carries
// the entry's body across the link.
func TestADeadLetterAtTheKeepCeilingIsReadAndDroppedThroughALeafItsLinkUp(t *testing.T) {
	t.Parallel()
	member, q := leafReading(t)
	ctx := t.Context()
	subject := topics.DeadLetter("ceiling.letters", "grp")
	header := []byte(`{"v":1}`)
	body := incompressible(t, queue.MaxPayloadBytes-queue.LetterFraming-len(header))
	if _, err := q.KeepDeadLetter(ctx, subject, "at the ceiling", header, body); err != nil {
		t.Fatalf("keep a dead letter at the ceiling through the leaf: %v", err)
	}
	// ONE MEMBER, so the copy a listing reads is the stream itself, and the
	// listing is current.
	kept, err := q.DeadLettersOn(ctx, subject)
	if err != nil || len(kept) != 1 {
		t.Fatalf("the subject lists %d entries (%v), want the one kept", len(kept), err)
	}
	seq := kept[0].Seq

	before := leafLink(t, member)
	frame, held, err := q.DeadLetterFrame(ctx, subject, seq)
	if err != nil || !held {
		t.Fatalf("DeadLetterFrame of the entry at the ceiling through the leaf = held %t, %v: "+
			"want it read whole", held, err)
	}
	if !bytes.Equal(frame, body) {
		t.Fatalf("the frame reads back as %d bytes, not the %d-byte body kept", len(frame), len(body))
	}
	read := leafLink(t, member)
	if crossed := read.OutBytes - before.OutBytes; crossed < int64(len(body)) ||
		crossed > int64(len(body))+readSlack {
		t.Errorf("reading the frame carried %d bytes towards the leaf, want the %d-byte body "+
			"once and at most %d beside it", crossed, len(body), readSlack)
	}

	if err := q.DropDeadLetter(ctx, subject, seq); err != nil {
		t.Fatalf("DropDeadLetter of the entry at the ceiling through the leaf: %v", err)
	}
	dropped := leafLink(t, member)
	if crossed := dropped.OutBytes - read.OutBytes; crossed > readSlack {
		t.Errorf("dropping the entry carried %d bytes towards the leaf, want its headers "+
			"alone, under %d", crossed, readSlack)
	}
	marks, err := q.DeadLetterMarks(ctx, topics.DeadLettersUnder("ceiling"))
	if err != nil {
		t.Fatalf("DeadLetterMarks: %v", err)
	}
	if got := marks.Counts[subject]; got != 0 {
		t.Errorf("after the drop the subject counts %d entries, want none", got)
	}
	if frame, held, err := q.DeadLetterFrame(ctx, subject, seq); err != nil || held || frame != nil {
		t.Errorf("a frame read of the dropped entry answered %d bytes, %v, %v: want it not "+
			"held, with no error", len(frame), held, err)
	}
	if after := leafLink(t, member); after.ID != before.ID {
		t.Errorf("the member's link to the leaf is connection %d, was %d: the link was closed "+
			"under the reads", after.ID, before.ID)
	}
}

// A RECORD AT THE TRANSPORT'S CEILING IS READ BACK THROUGH A LEAF, AND THE
// LEAF'S LINK STAYS UP.
//
// A state log's record may be as large as statelog.MaxTransportRecordBytes,
// and a log allows no direct get, so the stream leader's message read is how
// a record is read by its position (DomainLog.At) and how a subject's newest
// is found (DomainLog.LastSeq) — a JSON reply a third larger than the record,
// past what a leaf's link carries, and the leaf closes the whole link on it
// (crewlet/crewlet#1031). So across a link the record crosses RAW, delivered by
// a consumer, and the newest on a subject is found from its headers alone.
//
// Held here: the subject's newest is the record appended, having moved none
// of it; the record reads back whole by its position, having crossed once; a
// subject never written and a position past the log's end are absent; and
// the member's connection to the leaf is the same one throughout.
//
// Mutation: read either with the leader's message read across a link, and the
// leaf closes its link on the reply.
func TestARecordAtTheTransportsCeilingIsReadThroughALeafItsLinkUp(t *testing.T) {
	t.Parallel()
	member, q := leafReading(t)
	ctx := t.Context()
	spec := DomainStream{Name: "CREWLET_CEILING_LOG", Subjects: []string{"crewlet.ceiling.log.>"},
		MaxBytes: 4 * queue.MaxPayloadBytes, MaxMessageBytes: queue.MaxPayloadBytes,
		Duplicates: 2 * time.Minute}
	if err := q.EnsureDomainStream(ctx, spec); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	log, err := q.DomainLog(ctx, spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	const subject = "crewlet.ceiling.log.object.big"
	record := incompressible(t, statelog.MaxTransportRecordBytes)
	none := uint64(0)
	seq, _, err := log.Append(ctx, subject, "ceiling-create", &none, record)
	if err != nil {
		t.Fatalf("append a record at the transport's ceiling through the leaf: %v", err)
	}

	before := leafLink(t, member)
	last, found, err := log.LastSeq(ctx, subject)
	if err != nil || !found || last != seq {
		t.Fatalf("LastSeq of the subject through the leaf = (%d, %t, %v), want the record "+
			"appended at %d", last, found, err, seq)
	}
	newest := leafLink(t, member)
	if crossed := newest.OutBytes - before.OutBytes; crossed > readSlack {
		t.Errorf("finding the subject's newest carried %d bytes towards the leaf, want its "+
			"headers alone, under %d", crossed, readSlack)
	}

	got, payload, _, ok, err := log.At(ctx, seq)
	if err != nil || !ok {
		t.Fatalf("At(%d) through the leaf = ok %t, %v: want the record read whole", seq, ok, err)
	}
	if got != subject || !bytes.Equal(payload, record) {
		t.Fatalf("At(%d) read %d bytes on %q, want the %d-byte record on %q", seq,
			len(payload), got, len(record), subject)
	}
	read := leafLink(t, member)
	if crossed := read.OutBytes - newest.OutBytes; crossed < int64(len(record)) ||
		crossed > int64(len(record))+readSlack {
		t.Errorf("reading the record carried %d bytes towards the leaf, want the %d-byte "+
			"record once and at most %d beside it", crossed, len(record), readSlack)
	}

	if seq, found, err := log.LastSeq(ctx, "crewlet.ceiling.log.object.none"); err != nil || found {
		t.Errorf("LastSeq of a subject never written = (%d, %t, %v), want it absent", seq, found, err)
	}
	if _, _, _, ok, err := log.At(ctx, seq+1); err != nil || ok {
		t.Errorf("At(%d), past the log's end = ok %t, %v, want it absent", seq+1, ok, err)
	}
	if after := leafLink(t, member); after.ID != before.ID {
		t.Errorf("the member's link to the leaf is connection %d, was %d: the link was closed "+
			"under the reads", after.ID, before.ID)
	}
}

// leafReading is a member with a leaf listener, and a client of a leaf joined
// to it: the member's server, whose connection to the leaf a case watches
// ([leafLink]), and the leaf's queue.
func leafReading(t *testing.T) (*server.Server, *Queue) {
	t.Helper()
	cfg := testTimings(Config{FleetAuth: testFleetAuth()})
	memberCfg := cfg
	memberCfg.ServerName = "member"
	memberCfg.LeafHost, memberCfg.LeafPort = "127.0.0.1", AnyPort
	// A MEMBER THAT SERVES LEAVES PERSISTS, and the leaf creates what it
	// is first to use in the file store to match.
	memberCfg.StoreDir, memberCfg.StoreMaxBytes = brokertest.TempDir(t), brokertest.StoreMaxBytes
	member := startTestServer(t, memberCfg)

	leafCfg := cfg
	leafCfg.ServerName = "leaf"
	leafCfg.LeafURLs = []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", member.LeafPort())}
	leaf := startTestServer(t, leafCfg)
	q, err := leaf.Client(t.Context())
	if err != nil {
		t.Fatalf("a client of the leaf did not reach the fleet: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return member.embedded.ns, q
}

// leafLink is the member's one connection to its leaf, as the member reports
// it: its id, which a closed and redialled link changes, and the bytes it has
// carried towards the leaf.
func leafLink(t *testing.T, member *server.Server) server.LeafInfo {
	t.Helper()
	leafz, err := member.Leafz(nil)
	if err != nil {
		t.Fatalf("read the member's leaf connections: %v", err)
	}
	if len(leafz.Leafs) != 1 {
		t.Fatalf("the member holds %d leaf connections, want the one to its leaf: the link "+
			"was closed and not yet dialled again", len(leafz.Leafs))
	}
	return *leafz.Leafs[0]
}
