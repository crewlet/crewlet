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
//
// # Why every "no" is an ACK and not a nak
//
// A record naked for having nothing to say comes back for ever: the consumer
// redelivers it, the same node decides the same thing, and the feed's position
// never advances past it — so every later record in the company waits behind a
// rank move nobody wanted to hear about. A decision is handling.
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
			// The absence is on the RECORD rather than a parameter to
			// the feed, so a redelivery months later still knows not to
			// wake anybody — which a runtime flag could not.
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
			// MACHINERY, like the barrier: the trim's record removes
			// anchors and is about no object anybody watches, so it is
			// named as one — a notification on it wakes nobody.
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

// THE WHOLE RECORD TRAVELS IN THE BODY, INCLUDING WHAT THIS BUILD CANNOT READ.
//
// The node that WINS a feed message is rarely the one running the seat it
// wakes, and is often behind on its own rows — so routing from a local read
// would use a stale head or block the feed until it caught up. And a parser on
// a newer node reading a record an older node relayed must see everything the
// writer wrote, which is why the body is the record's own bytes rather than a
// struct this build knows how to fill in.
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
	// A FIELD FROM A NEWER BUILD, carried through the record's own
	// unknown-key retention.
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
// answered "no wake".
//
// Its envelope reads, so this node knows it is a task's change, and its version
// is past what this build decodes, so the notification rules are a newer
// build's. Answered "no wake", the feed would acknowledge it — and the feed's
// consumer is the fleet's, so no peer on the newer build would ever see it and
// its wake would be gone (#838): what `ActorApps` records lost on an older node
// during a roll. It answers the feed's deferral instead.
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
