package kv

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE MAILBOX REGISTRY IS LISTED IN ONE CERTIFIED WALK, NOT A GET PER RECORD.
//
// It was the one listing in this package that bypassed the walk: the client's
// ListKeys and then a Get per handle. So a quiet registry cost one read per
// record on top of the lister's consumer, and a registry being rewritten —
// every sweep that judges a record rewrites it, on every node — could come
// back without the record being rewritten, because the key lister ends where
// the same watcher guesses the initial values end.
//
// The listing runs on its OWN store over the same buckets, with its registry
// counted, so the case can tell a Get per record from a certifying read, and
// can prove the race happened: a churn in which no listing needed a single
// certifying read never raced a pass.
func TestTheMailboxRegistryIsListedInOneCertifiedWalk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	prefix := fmt.Sprintf("m%d", bucketSeq.Add(1))
	writer, reader := openFleetForTest(t, nc, prefix), openFleetForTest(t, nc, prefix)
	wire := countWireReads(t, nc, reader.mailboxes)

	handles := []string{"ceo", "cto", "eng", "ops", "pm"}
	type held struct{ rec coord.MailboxRecord }
	records := map[string]*held{}
	for _, handle := range handles {
		rec, created, err := writer.CreateMailbox(ctx, coord.MailboxRecord{Handle: handle})
		if err != nil || !created {
			t.Fatalf("CreateMailbox(%s): (%v, %v)", handle, created, err)
		}
		records[handle] = &held{rec}
	}
	list := func() []string {
		t.Helper()
		recs, err := reader.Mailboxes(ctx)
		if err != nil {
			t.Fatalf("Mailboxes: %v", err)
		}
		got := make([]string, 0, len(recs))
		for _, r := range recs {
			got = append(got, r.Handle)
		}
		return got
	}

	if got := list(); !slices.Equal(got, handles) {
		t.Fatalf("a quiet registry listed %v, want %v", got, handles)
	}
	if n := wire.leaderReads(t) + wire.directReads(t); n != 0 {
		t.Fatalf("listing a registry nobody was writing made %d reads record by "+
			"record, want 0: the walk carries every value, and a Get per handle "+
			"is a round trip per seat on every sweep", n)
	}

	stop := churnFor(t, []string{"ceo", "eng", "pm"}, func(handle string) error {
		cell := records[handle]
		next := cell.rec
		next.AbsentSince = time.Now().UTC()
		stored, ok, err := writer.UpdateMailbox(ctx, next)
		if err == nil && !ok {
			err = errors.New("lost a race nobody else was in")
		}
		if err != nil {
			return err
		}
		cell.rec = stored
		return nil
	})
	var wrong []string
	for range churnListings {
		recs, err := reader.Mailboxes(ctx)
		if err != nil {
			_, _ = stop()
			t.Fatalf("Mailboxes: %v", err)
		}
		got := make([]string, 0, len(recs))
		for _, r := range recs {
			got = append(got, r.Handle)
		}
		if !slices.Equal(got, handles) && len(wrong) < 5 {
			wrong = append(wrong, strings.Join(got, ","))
		}
	}
	rewrites, err := stop()
	if err != nil {
		t.Fatalf("a rewriter stopped, so its record was not rewritten throughout: %v", err)
	}
	if len(wrong) > 0 {
		t.Fatalf("Mailboxes did not answer every record exactly once while three "+
			"were rewritten; first answers: %v", wrong)
	}
	if wire.leaderReads(t) == 0 {
		t.Fatalf("%d rewrites across %d listings and not one listing needed a "+
			"certifying read, so the rewrites never raced a pass", rewrites, churnListings)
	}
}
