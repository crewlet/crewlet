package kv

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// statusOf reads a bucket's status through the client's handle, which is a
// metadata read the stream leader answers. A test asks it; nothing in the
// package does, which is why the interface the package holds has no such
// method.
func statusOf(ctx context.Context, b *leaderBucket) (jetstream.KeyValueStatus, error) {
	kv, ok := b.client.(jetstream.KeyValue)
	if !ok {
		return nil, fmt.Errorf("the bucket's client is a %T, not the client's own handle", b.client)
	}
	return kv.Status(ctx)
}

// A LEADER READ SEES A REMOVAL EXACTLY AS THE CLIENT WROTE IT.
//
// The leader's answer is a stored message, and whether that message is a value
// or a removal is spelled in headers the CLIENT writes and does not export —
// so this package restates the spelling, and this case holds the restatement
// to the client by writing every kind of removal through the client and
// reading it back through the leader. A spelling that drifted would read a
// delete marker as a live value with an empty body: a released claim still
// held, a purged counter at zero rather than absent, a removed key listed.
//
// Mutation: spell [kvOperationDelete] or [kvOperationPurge] differently, or
// make [operationOf] answer a put for a marker, and the removed keys read back
// as present.
func TestLeaderReadsSeeTheClientsOwnRemovals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openFleetForTest(t, embeddedNATS(t), fmt.Sprintf("r%d", bucketSeq.Add(1)))
	b := store.runs

	for _, key := range []string{"kept", "deleted", "purged"} {
		if _, err := b.Put(ctx, key, []byte(`{"v":1}`)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	if err := b.Delete(ctx, "deleted"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := b.Purge(ctx, "purged"); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if e, err := b.Get(ctx, "kept"); err != nil || string(e.Value()) != `{"v":1}` {
		t.Fatalf("Get(kept) = (%v, %v), want the value written", e, err)
	}
	for key, want := range map[string]jetstream.KeyValueOp{
		"deleted": jetstream.KeyValueDelete,
		"purged":  jetstream.KeyValuePurge,
	} {
		if e, err := b.Get(ctx, key); !errors.Is(err, jetstream.ErrKeyNotFound) {
			t.Errorf("Get(%s) = (%v, %v), want ErrKeyNotFound for a removed key", key, e, err)
		}
		e, found, err := b.latest(ctx, key)
		if err != nil || !found || e.Operation() != want {
			t.Errorf("the leader's newest message on %s reads as (%v, found=%t, %v), want "+
				"the client's %v marker", key, e, found, err, want)
		}
	}

	var listed []string
	if err := eachEntry(ctx, b, func(e jetstream.KeyValueEntry) error {
		listed = append(listed, e.Key())
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !slices.Equal(listed, []string{"kept"}) {
		t.Errorf("the walk listed %v, want only the key nobody removed", listed)
	}

	// AND A CREATE STEPS OVER EITHER MARKER, which is the read the client's
	// own Create made through a replica.
	for _, key := range []string{"deleted", "purged"} {
		if _, err := b.Create(ctx, key, []byte(`{"v":2}`)); err != nil {
			t.Errorf("Create over the %s marker: %v, want it written", key, err)
		}
	}
	if _, err := b.Create(ctx, "kept", []byte(`{"v":2}`)); !errors.Is(err, jetstream.ErrKeyExists) {
		t.Errorf("Create over a live value = %v, want ErrKeyExists", err)
	}
}

// lagging is a bucket's client handle whose ordered pass is served by a copy
// that STOPPED at an earlier moment: it delivers the snapshot it was given and
// the end-of-initial-values marker, and nothing the bucket learned since.
//
// That is exactly what a pass hosted on a follower hands over when the
// follower has not applied the newest writes — the server places the pass's
// consumer on a random member of the stream — and it is staged rather than
// raced because a race reproduces it a few times in a hundred: a case that
// rests on that can pass with the fix removed.
type lagging struct {
	clientBucket
	snapshot []jetstream.KeyValueEntry
}

func (l lagging) Watch(context.Context, string, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	ch := make(chan jetstream.KeyValueEntry, len(l.snapshot)+1)
	for _, e := range l.snapshot {
		ch <- e
	}
	ch <- nil
	return stubWatcher{ch: ch}, nil
}

// passOf records what the client's own ordered pass delivers right now — the
// bucket as one copy holds it at this moment, removals included.
func passOf(ctx context.Context, t *testing.T, b *leaderBucket) []jetstream.KeyValueEntry {
	t.Helper()
	var out []jetstream.KeyValueEntry
	if err := orderedPass(ctx, b.client, jetstream.AllKeys, "the snapshot",
		func(e jetstream.KeyValueEntry) error {
			out = append(out, e)
			return nil
		}); err != nil {
		t.Fatalf("take the snapshot: %v", err)
	}
	return out
}

// A WALK WHOSE PASS IS SERVED BY A COPY THAT IS BEHIND COMES BACK CURRENT.
//
// The pass is an ordered consumer, and on a replicated bucket the server
// places it on any member of the stream, so it can be served by a follower
// that has not applied the newest acknowledged writes. Read alone, that pass
// was the listing: the listing a seat-pause watch starts from misses the pause
// just taken, so every node reads that seat as free to work, and the trim
// floor — a minimum over the position rows — rises over a row it cannot see.
//
// Every way the copy can be behind is staged against one snapshot: a key that
// was rewritten since (the old value must not be listed), one removed by a
// delete and one by a purge (neither may be listed), and one created since (it
// must be). And the copy that had nothing at all, which is a pass that is
// behind by the whole bucket.
//
// Mutation: make [leaderBucket.closeOnLeader] return without asking, and every
// one of those comes back as the copy had it.
func TestAWalkServedByACopyThatIsBehindIsClosedByTheLeader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openFleetForTest(t, embeddedNATS(t), fmt.Sprintf("l%d", bucketSeq.Add(1)))
	b := store.budgets

	put := func(key, value string) {
		t.Helper()
		if _, err := b.Put(ctx, key, []byte(value)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	for _, key := range []string{"rewritten", "deleted", "purged", "unchanged"} {
		put(key, "before")
	}
	behind := passOf(ctx, t, b)

	put("rewritten", "after")
	if err := b.Delete(ctx, "deleted"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := b.Purge(ctx, "purged"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	put("created", "after")

	want := map[string]string{"rewritten": "after", "unchanged": "before", "created": "after"}
	for name, snapshot := range map[string][]jetstream.KeyValueEntry{
		"a copy that stopped part way": behind,
		"a copy that had nothing":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			stale := *b
			stale.client = lagging{clientBucket: b.client, snapshot: snapshot}
			got := map[string]string{}
			if err := eachEntry(ctx, &stale, func(e jetstream.KeyValueEntry) error {
				got[e.Key()] = string(e.Value())
				return nil
			}); err != nil {
				t.Fatalf("walk: %v", err)
			}
			if !maps.Equal(got, want) {
				t.Errorf("the walk listed %v, want %v: a pass served by a copy that "+
					"is behind was handed over as the bucket", got, want)
			}
		})
	}
}

// scripted is a bucket's client handle whose conditional writes answer from a
// script, one answer per attempt, and which counts the attempts.
type scripted struct {
	clientBucket
	answers  []error
	attempts int
}

func (s *scripted) Update(context.Context, string, []byte, uint64) (uint64, error) {
	s.attempts++
	if len(s.answers) == 0 {
		return 7, nil
	}
	answer := s.answers[0]
	s.answers = s.answers[1:]
	if answer == nil {
		return 7, nil
	}
	return 0, answer
}

func (s *scripted) Bucket() string { return "scripted" }

// refusedAs is a conditional write's refusal as the client hands it back:
// the leader's API error, wrapped in the revision mismatch the client maps
// BOTH "wrong last sequence" codes to — which is exactly why the code, and
// not the sentinel, is what tells them apart.
func refusedAs(code jetstream.ErrorCode) error {
	return fmt.Errorf("%w: %w", &jetstream.APIError{
		Code: 400, ErrorCode: code, Description: "wrong last sequence",
	}, jetstream.ErrKeyRevisionMismatch)
}

// A CONDITIONAL WRITE IS ANSWERED BY WHAT THE LEADER DECIDED, NEVER BY ITS
// "STILL IN PROCESS".
//
// A replicated stream's leader refuses a conditional write with 10164 while
// another write to the subject is still in process — including one that has
// already landed and been acknowledged, this caller's own, whose mark the
// leader clears only after it acks. The client reports that as a revision
// mismatch, the same as the decided refusal, and under load it was: a removal
// at the revision its caller had just read was refused, and a charge spent its
// sixteen compare-and-set rounds on refusals no other writer had caused.
//
// Staged rather than raced, because the window is a few hundred microseconds
// on an idle machine and a race reproduces it only under load.
//
// Mutation: make [leaderBucket.settle] hand back the first answer, and the
// write that landed reads as lost, the decided refusal is unchanged, and the
// write still undecided at its deadline reads as a race somebody won.
func TestAConditionalWriteIsAnsweredByWhatTheLeaderDecided(t *testing.T) {
	t.Parallel()
	inProcess := refusedAs(jetstream.JSErrCodeStreamWrongLastSequenceConstant)
	decided := refusedAs(jetstream.JSErrCodeStreamWrongLastSequence)
	bucket := func(answers ...error) (*leaderBucket, *scripted) {
		s := &scripted{answers: answers}
		return &leaderBucket{client: s, timeout: time.Minute}, s
	}

	t.Run("in process, then landed", func(t *testing.T) {
		t.Parallel()
		b, s := bucket(inProcess, inProcess, nil)
		if _, err := b.Update(context.Background(), "k", []byte("v"), 3); err != nil {
			t.Fatalf("Update = %v after the write ahead of it settled and this one "+
				"landed, want it written", err)
		}
		if s.attempts != 3 {
			t.Errorf("%d attempts, want 3: two waited out and the one that landed", s.attempts)
		}
	})

	t.Run("in process, then decided", func(t *testing.T) {
		t.Parallel()
		b, _ := bucket(inProcess, decided)
		_, err := b.Update(context.Background(), "k", []byte("v"), 3)
		if !lostUpdateRace(err) || !isWrongLastSequence(err) {
			t.Fatalf("Update = %v, want the leader's decided refusal: somebody else's "+
				"write was the one in process, and it landed first", err)
		}
	})

	t.Run("still in process at the deadline", func(t *testing.T) {
		t.Parallel()
		answers := make([]error, 1000)
		for i := range answers {
			answers[i] = inProcess
		}
		b, _ := bucket(answers...)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := b.Update(ctx, "k", []byte("v"), 3)
		switch {
		case err == nil:
			t.Fatal("Update succeeded while every answer said another write was in process")
		case lostUpdateRace(err), lostCreateRace(err):
			t.Fatalf("Update = %v, which every caller reads as a race another writer "+
				"won; nothing about it says one did, so it is unknown", err)
		case !errors.Is(err, context.DeadlineExceeded):
			t.Errorf("Update = %v, want it to carry the deadline that ended the wait", err)
		}
	})

	t.Run("a create waits it out too", func(t *testing.T) {
		t.Parallel()
		b, _ := bucket(inProcess, nil)
		if _, err := b.Create(context.Background(), "k", []byte("v")); err != nil {
			t.Fatalf("Create = %v, want it written once the write ahead settled: read as "+
				"a key that exists, a create race over a removed record has no winner", err)
		}
	})
}
