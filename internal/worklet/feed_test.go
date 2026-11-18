package worklet_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A RECORD WAKES SOMEBODY, OR IS HANDLED FOR A REASON THIS BUILD CAN NAME.
// Every "no" is an ack, not a nak: a naked record is redelivered and decided
// the same way for ever, and every later record waits behind it.
func TestTheFeedWakesOnlyWhatCarriesANotification(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_700_000_000, 0).UTC()
	for name, tc := range map[string]struct {
		record worklet.MutationRecord
		wakes  bool
	}{
		"a task change with a notification": {
			record: feedRecord(worklet.TaskSubject("t-1"), worklet.OpPatch,
				&worklet.Notify{Kind: worklet.ChangeStatus}, at),
			wakes: true,
		},
		"a task change nobody announced": {
			// The absence is on the record, so a redelivery months later
			// still wakes nobody.
			record: feedRecord(worklet.TaskSubject("t-1"), worklet.OpPatch, nil, at),
		},
		"a rank move": {
			record: feedRecord(worklet.RankOrderSubject("ENG"), worklet.OpPatch,
				&worklet.Notify{Kind: worklet.ChangeFields}, at),
		},
		"a turn's spend": {
			record: feedRecord(worklet.TurnSubject("t-1"), worklet.OpTurn, nil, at),
		},
		"a barrier": {
			record: feedRecord(worklet.BarrierSubject(), worklet.OpBarrier, nil, at),
		},
		"a trimmed record, whatever it carries": {
			// Machinery, like the barrier: a notification on it wakes nobody.
			record: feedRecord(worklet.TrimmedSubject(statelog.Position{
				Stream: topics.TrackerLogStream, Generation: 1, Seq: 40}),
				worklet.OpTrimmed, &worklet.Notify{Kind: worklet.ChangeFields}, at),
		},
		"an eviction": {
			record: feedRecord(worklet.EvictionSubject("node-b"),
				worklet.OpEviction, nil, at),
		},
		"a generation": {
			record: feedRecord(worklet.GenerationSubject(2),
				worklet.OpGeneration, nil, at),
		},
		"a project edit with a notification": {
			record: feedRecord(worklet.ProjectSubject("ENG"), worklet.OpPatch,
				&worklet.Notify{Kind: worklet.ChangeFields}, at),
			wakes: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			payload, err := tc.record.Encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			delivery, wakes, err := worklet.NewTranslator().Translate(t.Context(),
				changefeed.Record{ID: tc.record.OpID, Key: "k", Payload: payload})
			if err != nil {
				t.Fatalf("Translate: %v — a record the feed cannot handle stalls "+
					"every later one behind it", err)
			}
			if wakes != tc.wakes {
				t.Fatalf("Translate wakes=%v, want %v", wakes, tc.wakes)
			}
			if !wakes {
				return
			}
			if delivery.ID != tc.record.OpID {
				t.Errorf("the delivery's id is %q and the record's is %q — the "+
					"wake is deduplicated on the id INSIDE the record, and only "+
					"this package can read it out", delivery.ID, tc.record.OpID)
			}
			if delivery.Actor != tc.record.Actor {
				t.Errorf("the delivery names actor %q, not %q — a parser that "+
					"cannot tell who wrote a change cannot decline to wake "+
					"them about it", delivery.Actor, tc.record.Actor)
			}
		})
	}
}

// THE WHOLE RECORD TRAVELS IN THE BODY, INCLUDING WHAT THIS BUILD CANNOT READ:
// the winning node routes from the record's own snapshot, not a local read
// (see [worklet.Translator]), and a newer parser sees everything the writer
// wrote.
func TestTheFeedBodyIsTheWholeRecord(t *testing.T) {
	t.Parallel()
	record := feedRecord(worklet.TaskSubject("t-1"), worklet.OpPatch,
		&worklet.Notify{
			Kind:    worklet.ChangeStatus,
			Excerpt: "shipped it",
			Snapshot: worklet.Snapshot{
				Key: "ENG-1", Project: "ENG", Assignee: "ana",
				Watchers: []string{"bo", "cy"},
			},
		}, time.Unix(1_700_000_000, 0).UTC())
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// A field from a newer build, kept by the record's unknown-key retention.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw["a_field_from_the_future"] = json.RawMessage(`"kept"`)
	payload, err = json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}

	delivery, wakes, err := worklet.NewTranslator().Translate(t.Context(),
		changefeed.Record{ID: record.OpID, Key: "k", Payload: payload})
	if err != nil || !wakes {
		t.Fatalf("Translate = %v, %v", wakes, err)
	}
	notify, ok := delivery.Body["notify"].(map[string]any)
	if !ok {
		t.Fatalf("the body carries no notification: %v — the recipient's own "+
			"routing snapshot is what lets the winning node route without "+
			"reading anything", delivery.Body)
	}
	snapshot, ok := notify["snapshot"].(map[string]any)
	if !ok || snapshot["assignee"] != "ana" {
		t.Fatalf("the body's snapshot is %v, and routing from a local read "+
			"instead would use whatever this node had applied", snapshot)
	}
	if delivery.Body["a_field_from_the_future"] != "kept" {
		t.Fatalf("a field this build does not understand was dropped from the " +
			"body — a parser on a newer node must see everything the writer " +
			"wrote, and a rolling upgrade puts exactly that on the wire")
	}
}

// A RECORD A NEWER BUILD WROTE IS DEFERRED TO A PEER ON THAT BUILD, never
// answered "no wake": the feed's consumer is the fleet's, so an ack here would
// lose its wake for every peer.
//
// Mutation: answer the future-version arm with no wake, and the deferral is
// gone.
func TestARecordANewerBuildWroteIsDeferredToAPeer(t *testing.T) {
	t.Parallel()
	record := feedRecord(worklet.TaskSubject("t-1"), worklet.OpPatch,
		&worklet.Notify{Kind: worklet.ChangeStatus, Snapshot: worklet.Snapshot{Key: "ENG-1",
			Project: "ENG", Assignee: "ana"}}, time.Unix(1_700_000_000, 0).UTC())
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw["v"] = json.RawMessage(strconv.Itoa(worklet.RecordVersion + 1))
	if payload, err = json.Marshal(raw); err != nil {
		t.Fatalf("re-encode: %v", err)
	}

	_, wakes, err := worklet.NewTranslator().Translate(t.Context(),
		changefeed.Record{ID: record.OpID, Key: "k", Payload: payload})
	var deferral *changefeed.Deferral
	if wakes || !errors.As(err, &deferral) || deferral.Reason != changefeed.DeferNewerRecord {
		t.Fatalf("a newer build's record answered wake=%v, %v; want it deferred to a peer "+
			"on that build (%s)", wakes, err, changefeed.DeferNewerRecord)
	}
}

func feedRecord(subject worklet.Subject, op worklet.OpKind,
	notify *worklet.Notify, at time.Time) worklet.MutationRecord {

	return worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "op-" + subject.ID,
			Subject: subject, Op: op, CreatedAt: at, Writer: "node-a",
			Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: json.RawMessage(`{}`), Actor: "ana",
		ActorKind: worklet.AuthorHuman, Notify: notify,
	}
}
