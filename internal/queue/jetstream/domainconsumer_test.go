package jetstream

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY NODE READS THE WHOLE LOG, which is what makes N identical copies.
//
// This is not a work queue. A shared consumer would deliver each record to
// exactly one node — the opposite of replication, and a failure whose symptom
// is two nodes' tracker tables quietly disagreeing about different halves of
// the company.
func TestEveryNodeReadsEveryRecord(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_TEST_LOG", "crewlet.test.log")
	for i := range 5 {
		if _, _, err := log.Append(t.Context(), "crewlet.test.log.task."+id(i),
			"", nil, []byte(`{"n":`+itoa(i)+`}`)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	for _, node := range []string{"node-a", "node-b"} {
		cons, err := q.DomainConsumer(t.Context(), "CREWLET_TEST_LOG", node, 0)
		if err != nil {
			t.Fatalf("open %s's consumer: %v", node, err)
		}
		got := fetchAll(t, cons, 5)
		if len(got) != 5 {
			t.Fatalf("%s received %d of 5 records — a shared consumer would "+
				"deliver each record to exactly one node, which is the "+
				"opposite of replication", node, len(got))
		}
		for i, msg := range got {
			if msg.Seq != uint64(i+1) {
				t.Fatalf("%s received sequence %d at position %d",
					node, msg.Seq, i)
			}
			if msg.StoredAt.IsZero() {
				t.Fatal("a record arrived with no broker timestamp, which is " +
					"the instant every node must read identically")
			}
		}
	}
}

// A NEW CONSUMER RESUMES FROM THE ROWS, not from the head and not from zero.
//
// A consumer created at the head silently skips everything published before it
// existed, which on a state log is every record the node has not applied. One
// created at the beginning on a node that has applied a million records is a
// million redeliveries the applier drops one at a time. The SQL checkpoint is
// the only durable statement of where this node actually is.
func TestAConsumerResumesFromTheCheckpoint(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_RESUME_LOG", "crewlet.resume.log")
	for i := range 6 {
		if _, _, err := log.Append(t.Context(), "crewlet.resume.log.task."+id(i),
			"", nil, []byte(`{}`)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_RESUME_LOG", "node-a", 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got := fetchAll(t, cons, 2)
	if len(got) != 2 || got[0].Seq != 5 {
		t.Fatalf("a consumer resuming after 4 received %d record(s) starting "+
			"at %d, want 2 starting at 5", len(got), seqOf(got))
	}
	if pending, err := cons.Pending(t.Context()); err != nil || pending != 0 {
		t.Fatalf("Pending = (%d, %v) after draining", pending, err)
	}
}

// AN EXISTING CONSUMER IS TAKEN AS IT IS.
//
// The broker treats a consumer's start sequence as immutable, so a
// create-or-update would silently refuse to move it and report success. The
// applier resumes from its own checkpoint regardless and drops anything below
// it, so a consumer sitting lower costs redeliveries rather than correctness —
// but only because the checkpoint, not this, is what decides.
func TestReopeningAConsumerDoesNotMoveIt(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_REOPEN_LOG", "crewlet.reopen.log")
	for range 4 {
		if _, _, err := log.Append(t.Context(), "crewlet.reopen.log.task.a",
			"", nil, []byte(`{}`)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	first, err := q.DomainConsumer(t.Context(), "CREWLET_REOPEN_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	again, err := q.DomainConsumer(t.Context(), "CREWLET_REOPEN_LOG", "node-a", 3)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if first.Name() != again.Name() {
		t.Fatalf("two opens on one node gave %q and %q", first.Name(), again.Name())
	}
	got := fetchAll(t, again, 4)
	if len(got) != 4 || got[0].Seq != 1 {
		t.Fatalf("reopening at 3 moved an existing consumer: it delivered %d "+
			"record(s) from %d", len(got), seqOf(got))
	}
}

// ---- the fixtures ----------------------------------------------------- //

func openDomain(t *testing.T, stream, prefix string) (*Queue, *DomainLog) {
	t.Helper()
	q := domainQueue(t, DomainStream{
		Name: stream, Subjects: []string{prefix + ".>"},
		MaxBytes: 16 << 20, Duplicates: time.Minute,
	})
	log, err := q.DomainLog(t.Context(), stream)
	if err != nil {
		t.Fatalf("open %s: %v", stream, err)
	}
	return q, log
}

func fetchAll(t *testing.T, c *DomainConsumer, want int) []statelog.Message {
	t.Helper()
	var out []statelog.Message
	for range 10 {
		batch, err := c.Fetch(t.Context(), want-len(out), 0, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		for _, msg := range batch {
			if err := msg.Ack(); err != nil {
				t.Fatalf("ack: %v", err)
			}
		}
		out = append(out, batch...)
		if len(out) >= want {
			break
		}
	}
	return out
}

func seqOf(msgs []statelog.Message) uint64 {
	if len(msgs) == 0 {
		return 0
	}
	return msgs[0].Seq
}

func id(i int) string   { return string(rune('a' + i)) }
func itoa(i int) string { return string(rune('0' + i)) }

// A CANCELLED CONTEXT ENDS A GROUP'S BLOCKING READ.
//
// # Why this is a hang rather than a leak
//
// [jetstream.MessagesContext.Next] takes no context and blocks until a
// message arrives or the iterator is stopped. A quiet log means it blocks for
// ever — so a consumer that only cancelled a context and then joined its
// reader would wait on a goroutine that has no way to notice.
//
// It is not hypothetical. The tracker's wake feed sits in exactly this call
// for the life of a node, and shutdown joins it: without the watcher this
// asserts, every engine test that stopped before its own context expired
// wedged, and the only symptom was a suite that never finished.
//
// The assertion is the WALL CLOCK, because that is the symptom. A flag saying
// the watcher exists would go on being true while the block moved elsewhere.
func TestAGroupsBlockingReadEndsWithItsContext(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_GROUP_CANCEL", "crewlet.groupcancel")
	defer func() { _ = q.Stop(context.Background()) }()

	ctx, cancel := context.WithCancel(t.Context())
	group, err := log.Group(ctx, "waker")
	if err != nil {
		t.Fatalf("open the group: %v", err)
	}
	// NOTHING IS PUBLISHED, deliberately: an empty log is the state a
	// company spends almost all of its time in, and it is the one where
	// the blocking read never returns on its own.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			delivery, err := group.Next(ctx)
			if err != nil || delivery == nil {
				return
			}
		}
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a group's blocking read outlived its cancelled context, so " +
			"anything joining it waits for ever")
	}

	// AND AN EXPLICIT STOP IS THE SAME STOP, so a caller that does both —
	// which a shutdown ordering change makes ordinary — does not panic on
	// a second close.
	if err := group.Stop(); err != nil {
		t.Errorf("stopping an already-cancelled group: %v", err)
	}
	if err := group.Stop(); err != nil {
		t.Errorf("stopping twice: %v", err)
	}
}
