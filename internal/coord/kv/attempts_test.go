package kv

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// apiRequests records every JetStream API request a connection publishes while
// it is armed, by subject.
type apiRequests struct {
	mu       sync.Mutex
	subjects []string
}

// watchAPI subscribes to the JetStream API on nc itself, which sees every
// request the connection sends: the broker echoes a connection's own publishes
// to its own subscriptions, so the request that opens a consumer and the one
// that reads a record both arrive here as well as at the server.
func watchAPI(t *testing.T, nc *nats.Conn) *apiRequests {
	t.Helper()
	seen := &apiRequests{}
	sub, err := nc.Subscribe("$JS.API.>", func(m *nats.Msg) {
		seen.mu.Lock()
		defer seen.mu.Unlock()
		seen.subjects = append(seen.subjects, m.Subject)
	})
	if err != nil {
		t.Fatalf("subscribe to the JetStream API: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return seen
}

// since answers every request recorded after a mark, flushing first so a
// request already sent has arrived.
func (a *apiRequests) since(t *testing.T, nc *nats.Conn, mark int) []string {
	t.Helper()
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.subjects[mark:]...)
}

func (a *apiRequests) mark() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.subjects)
}

// READING A PAIR'S WINDOW IS ONE GET AND OPENS NO CONSUMER.
//
// The throttle reads a climbing pair before every attempt its curve admits and
// every fresh name once, so this read is paid at whatever rate a guessing run
// reaches it. It was a History: an ephemeral ordered consumer over the
// subject's own revisions — on a clustered bucket, two proposals through the
// metadata group every seat lease and every stream in the fleet shares — so
// one unauthenticated source typing fresh names drove the fleet's metadata
// layer at line rate. The record is one value now, holding the instants
// itself, and reading it is a single direct get.
//
// Mutation: read the window through History and a consumer is created on
// every call.
func TestReadingTheAttemptsWindowOpensNoConsumer(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	store := openFleetForTest(t, nc, fmt.Sprintf("a%d", bucketSeq.Add(1)))
	ctx := context.Background()
	now := time.Now().UTC()
	for range 3 {
		if err := store.Fail(ctx, "pair-digest", now); err != nil {
			t.Fatalf("Fail: %v", err)
		}
	}

	api := watchAPI(t, nc)
	mark := api.mark()
	got, err := store.Failures(ctx, "pair-digest", now)
	if err != nil {
		t.Fatalf("Failures: %v", err)
	}
	if got.Count != 3 || !got.Last.Equal(now) {
		t.Fatalf("Failures = %+v, want 3 attempts, the newest at %v", got, now)
	}
	requests := api.since(t, nc, mark)
	for _, subject := range requests {
		if strings.HasPrefix(subject, "$JS.API.CONSUMER.") {
			t.Errorf("reading the window sent %s: it opened a consumer, which "+
				"on a clustered bucket is a proposal through the metadata group",
				subject)
		}
	}
	if len(requests) != 1 {
		t.Errorf("reading the window sent %d JetStream requests (%v), want the "+
			"one get", len(requests), requests)
	}
}

// A RECORD AN OLDER BUILD WROTE STILL COUNTS, AND A WRITE CARRIES IT FORWARD.
//
// Two builds share this bucket through a rolling upgrade, and the one before
// this wrote each attempt as its own revision holding a bare RFC 3339 instant.
// Read as this build's record, such a value is the one attempt it records; the
// next failure written on top of it keeps it. And a value neither build could
// have written is still an attempt, dated by the broker's own receipt, because
// the direction that cannot be defended is a value nobody can parse
// un-throttling the caller it was written against.
//
// Mutations: decode only this build's shape and the older value counts
// nothing; drop the unreadable arm and the garbage counts nothing.
func TestAnOlderBuildsAttemptStillCounts(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	store := openFleetForTest(t, nc, fmt.Sprintf("a%d", bucketSeq.Add(1)))
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	earlier := now.Add(-time.Minute)

	if _, err := store.attempts.Put(ctx, encodeKey("older-build"),
		[]byte(earlier.Format(time.RFC3339Nano))); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got, err := store.Failures(ctx, "older-build", now); err != nil ||
		got.Count != 1 || !got.Last.Equal(earlier) {
		t.Fatalf("an older build's attempt reads %+v (%v), want one attempt at %v",
			got, err, earlier)
	}
	if err := store.Fail(ctx, "older-build", now); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if got, err := store.Failures(ctx, "older-build", now); err != nil ||
		got.Count != 2 || !got.Last.Equal(now) {
		t.Fatalf("after a failure on top of it the window reads %+v (%v), want "+
			"both attempts, the newest at %v", got, err, now)
	}

	if _, err := store.attempts.Put(ctx, encodeKey("unreadable"),
		[]byte("not an instant at all")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got, err := store.Failures(ctx, "unreadable", now.Add(time.Minute)); err != nil ||
		got.Count != 1 || got.Last.IsZero() {
		t.Fatalf("an unreadable record reads %+v (%v), want one attempt dated "+
			"by the broker", got, err)
	}
}
