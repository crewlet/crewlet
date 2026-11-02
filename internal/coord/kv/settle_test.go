package kv

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// scripted is a bucket's client handle whose conditional writes answer from a
// script, one answer per attempt, and which counts the attempts. An answer of
// nil hands the write to the bucket underneath, when there is one.
type scripted struct {
	jetstream.KeyValue
	answers  []error
	attempts int
}

func (s *scripted) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	s.attempts++
	var answer error
	if len(s.answers) > 0 {
		answer, s.answers = s.answers[0], s.answers[1:]
	}
	switch {
	case answer != nil:
		return 0, answer
	case s.KeyValue != nil:
		return s.KeyValue.Update(ctx, key, value, revision)
	}
	return 7, nil
}

func (s *scripted) Bucket() string {
	if s.KeyValue != nil {
		return s.KeyValue.Bucket()
	}
	return "scripted"
}

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
// "STILL IN FLIGHT".
//
// A replicated stream's leader refuses a conditional write with 10164 while
// another write to the subject is still in flight — including one that has
// already landed and been acknowledged, this caller's own, whose mark the
// leader clears only after it acks. The client reports that as a revision
// mismatch, the same as the decided refusal, and under load it was: a removal
// at the revision its caller had just read was refused, and a charge spent its
// sixteen compare-and-set rounds on refusals no other writer had caused.
//
// Staged rather than raced, because the window is a few hundred microseconds
// on an idle machine and a race reproduces it only under load.
//
// Mutation: make [settledBucket.settle] hand back the first answer, and the
// write that landed reads as lost, the decided refusal is unchanged, and the
// write still undecided at its deadline reads as a race somebody won.
func TestAConditionalWriteIsAnsweredByWhatTheLeaderDecided(t *testing.T) {
	t.Parallel()
	inFlight := refusedAs(jetstream.JSErrCodeStreamWrongLastSequenceConstant)
	decided := refusedAs(jetstream.JSErrCodeStreamWrongLastSequence)
	bucket := func(answers ...error) (settledBucket, *scripted) {
		s := &scripted{answers: answers}
		return settledBucket{KeyValue: s, timeout: time.Minute}, s
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

	// The leader's in-flight answer is a REPLICATED stream's, so nothing a
	// single server runs ever meets it — which is what made a call site
	// that forgot to settle invisible. Every bucket the stores open is
	// wrapped where it is opened; this holds that, over EVERY bucket field
	// the fleet store has rather than a list of them, so a bucket added
	// later is held without anybody remembering to name it here.
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

// A CREATE WAITS THE IN-FLIGHT ANSWER OUT TOO.
//
// [createKey] is a conditional publish at revision zero, and read as a refusal
// the in-flight answer is a key that exists: a create race over a just-removed
// record then had no winner. Through a settled bucket the create is retried
// until the leader decides it, and lands.
func TestACreateWaitsOutTheLeadersInFlightAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := openFleetForTest(t, embeddedNATS(t), fmt.Sprintf("c%d", bucketSeq.Add(1)))
	underneath, ok := f.runs.(settledBucket)
	if !ok {
		t.Fatalf("the runs bucket is a %T, not settled", f.runs)
	}
	s := &scripted{
		KeyValue: underneath.KeyValue,
		answers:  []error{refusedAs(jetstream.JSErrCodeStreamWrongLastSequenceConstant)},
	}
	if _, err := createKey(ctx, f.js, settledBucket{KeyValue: s, timeout: time.Minute},
		"turn-1", []byte(`{"n":1}`)); err != nil {
		t.Fatalf("createKey = %v, want it written once the write ahead settled", err)
	}
	if s.attempts != 2 {
		t.Errorf("%d attempts, want 2: one waited out and the one that landed", s.attempts)
	}
}
