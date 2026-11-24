package kv

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// termed is a client whose every request waits term where its caller set no
// deadline: what a settled bucket asks of the client it was opened on. Its
// publishes, a settled bucket's conditional writes ([settledBucket.update]),
// go to publish where it is set, else to the client it embeds.
type termed struct {
	jetstream.JetStream
	term    time.Duration
	publish func(ctx context.Context, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error)
}

func (c termed) Options() jetstream.JetStreamOptions {
	return jetstream.JetStreamOptions{DefaultTimeout: c.term}
}

func (c termed) PublishMsg(ctx context.Context, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	if c.publish != nil {
		return c.publish(ctx, m, opts...)
	}
	return c.JetStream.PublishMsg(ctx, m, opts...)
}

// scripted is a client whose conditional writes answer from a script, one
// answer per attempt, and which counts the attempts. An answer of nil hands
// the write to the client underneath, when there is one.
type scripted struct {
	under    jetstream.JetStream
	answers  []error
	attempts int
}

func (s *scripted) publish(ctx context.Context, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	s.attempts++
	var answer error
	if len(s.answers) > 0 {
		answer, s.answers = s.answers[0], s.answers[1:]
	}
	switch {
	case answer != nil:
		return nil, answer
	case s.under != nil:
		return s.under.PublishMsg(ctx, m, opts...)
	}
	return &jetstream.PubAck{Sequence: 7}, nil
}

// leaderRefusal is a conditional publish's refusal as the leader answers it:
// its API error, the code alone telling a decided refusal from one in flight.
func leaderRefusal(code jetstream.ErrorCode) error {
	return &jetstream.APIError{Code: 400, ErrorCode: code, Description: "wrong last sequence"}
}

// refusedAs is a conditional write's refusal as a bucket's Update hands it
// back: [leaderRefusal], wrapped in the revision mismatch BOTH "wrong last
// sequence" codes map to, which is why the code, and not the sentinel, is
// what tells them apart.
func refusedAs(code jetstream.ErrorCode) error {
	return fmt.Errorf("%w: %w", leaderRefusal(code), jetstream.ErrKeyRevisionMismatch)
}

// A CONDITIONAL WRITE IS ANSWERED BY WHAT THE LEADER DECIDED, NEVER BY ITS
// "STILL IN FLIGHT". A replicated stream's leader refuses a conditional write
// with 10164 while another write to the subject is in flight, this caller's
// own acknowledged one included, and the client reports that as the same
// revision mismatch as a decided refusal. Staged rather than raced: the window
// is a few hundred microseconds on an idle machine.
//
// Mutation: make [settledBucket.settle] hand back the first answer, and the
// write that landed reads as lost, the decided refusal is unchanged, and the
// write still undecided at its deadline reads as a race somebody won.
func TestAConditionalWriteIsAnsweredByWhatTheLeaderDecided(t *testing.T) {
	t.Parallel()
	inFlight := leaderRefusal(jetstream.JSErrCodeStreamWrongLastSequenceConstant)
	decided := leaderRefusal(jetstream.JSErrCodeStreamWrongLastSequence)
	bucket := func(answers ...error) (settledBucket, *scripted) {
		s := &scripted{answers: answers}
		return settledBucket{KeyValue: named{bucket: "scripted"},
			js: termed{term: time.Minute, publish: s.publish}, holding: wholeValues}, s
	}

	t.Run("in flight, then landed", func(t *testing.T) {
		t.Parallel()
		b, s := bucket(inFlight, inFlight, nil)
		if _, err := b.Update(context.Background(), "k", []byte("v"), 3); err != nil {
			t.Fatalf("Update = %v after the write ahead of it settled and this one "+
				"landed, want it written", err)
		}
		if s.attempts != 3 {
			t.Errorf("%d attempts, want 3: two waited out and the one that landed", s.attempts)
		}
	})

	t.Run("in flight, then decided", func(t *testing.T) {
		t.Parallel()
		b, _ := bucket(inFlight, decided)
		_, err := b.Update(context.Background(), "k", []byte("v"), 3)
		if !lostUpdateRace(err) || !isWrongLastSequence(err) {
			t.Fatalf("Update = %v, want the leader's decided refusal: somebody else's "+
				"write was the one in flight, and it landed first", err)
		}
	})

	t.Run("still in flight at the deadline", func(t *testing.T) {
		t.Parallel()
		answers := make([]error, 1000)
		for i := range answers {
			answers[i] = inFlight
		}
		b, _ := bucket(answers...)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := b.Update(ctx, "k", []byte("v"), 3)
		switch {
		case err == nil:
			t.Fatal("Update succeeded while every answer said another write was in flight")
		case lostUpdateRace(err), errors.Is(err, jetstream.ErrKeyExists):
			t.Fatalf("Update = %v, which every caller reads as a race another writer "+
				"won; nothing about it says one did, so it is unknown", err)
		case !errors.Is(err, context.DeadlineExceeded):
			t.Errorf("Update = %v, want it to carry the deadline that ended the wait", err)
		}
	})

	// The in-flight answer is a REPLICATED stream's, so no single server
	// shows a call site that forgot to settle. This walks EVERY bucket field
	// the fleet store has, not a list, so a bucket added later is held too.
	t.Run("every bucket a store opens is settled", func(t *testing.T) {
		t.Parallel()
		nc := embeddedNATS(t)
		store := openFleetForTest(t, nc, fmt.Sprintf("s%d", bucketSeq.Add(1)))
		fields := reflect.ValueOf(store).Elem()
		bucketType := reflect.TypeFor[jetstream.KeyValue]()
		buckets := 0
		for i := range fields.NumField() {
			field := fields.Type().Field(i)
			if field.Type != bucketType {
				continue
			}
			buckets++
			kv := fields.Field(i)
			if kv.IsNil() {
				t.Errorf("the fleet store's %s bucket was never opened", field.Name)
				continue
			}
			// The dynamic type, read without Interface: the fields are
			// unexported, and reflection will not hand their values out.
			if got := kv.Elem().Type(); got != reflect.TypeFor[settledBucket]() {
				t.Errorf("the fleet store's %s bucket is a %v, not settled", field.Name, got)
			}
		}
		if buckets == 0 {
			t.Fatal("premise: the walk found no bucket field on the fleet store, " +
				"so it held nothing")
		}
		leases := openStore(t, nc, time.Minute)
		for name, kv := range map[string]jetstream.KeyValue{
			"leases": leases.leases.kv, "duties": leases.duties.kv, "epochs": leases.counter.kv,
		} {
			if _, ok := kv.(settledBucket); !ok {
				t.Errorf("the lease store's %s bucket is a %T, not settled", name, kv)
			}
		}
	})
}

// A CREATE WAITS THE IN-FLIGHT ANSWER OUT TOO: [createKey] is a conditional
// publish at revision zero, and the in-flight answer read as a refusal is a
// key that exists, leaving a create race over a just-removed record no winner.
func TestACreateWaitsOutTheLeadersInFlightAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openFleetForTest(t, embeddedNATS(t), fmt.Sprintf("c%d", bucketSeq.Add(1)))
	underneath, ok := f.runs.(settledBucket)
	if !ok {
		t.Fatalf("the runs bucket is a %T, not settled", f.runs)
	}
	s := &scripted{
		under:   f.js,
		answers: []error{leaderRefusal(jetstream.JSErrCodeStreamWrongLastSequenceConstant)},
	}
	if _, err := createKey(ctx, f.js, settledBucket{KeyValue: underneath.KeyValue,
		js: termed{term: time.Minute, publish: s.publish}, holding: runsHolding},
		"turn-1", []byte(`{"n":1}`), nil); err != nil {
		t.Fatalf("createKey = %v, want it written once the write ahead settled", err)
	}
	if s.attempts != 2 {
		t.Errorf("%d attempts, want 2: one waited out and the one that landed", s.attempts)
	}
}

// A BUCKET IS SETTLED WITH ONE HOLDING. Settled again with its own it is
// returned as it is; with another, the second would be silently ignored and
// its reads timed by the first, so it is a wiring mistake that panics.
//
// Mutation: return an already settled handle whatever holding is passed.
func TestABucketSettledAgainKeepsOneHolding(t *testing.T) {
	t.Parallel()
	once := settled(nil, named{bucket: "runs"}, InProcess, runsHolding)
	if again := settled(nil, once, InProcess, runsHolding); !again.(settledBucket).holding.same(runsHolding) {
		t.Errorf("settled again with its own holding, the handle holds %+v", again.(settledBucket).holding)
	}
	defer func() {
		if recover() == nil {
			t.Error("a bucket settled again with another holding kept its first without a word")
		}
	}()
	settled(nil, once, InProcess, claimHolding)
}
