package tracker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
)

// routedWake is the wake a record becomes, as the node that routes it decodes
// it: translated, built by the change feed, encoded for the broker and decoded
// again — with the size of the wake's encoding.
func routedWake(t *testing.T, record MutationRecord) (types.RawWebhook, int) {
	t.Helper()
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	delivery, wakes, err := NewTranslator().Translate(t.Context(),
		changefeed.Record{ID: record.OpID, Key: "k", Payload: payload})
	if err != nil || !wakes {
		t.Fatalf("Translate = %v, %v", wakes, err)
	}
	ev, err := changefeed.Wake(Source, delivery)
	if err != nil {
		t.Fatalf("build the wake: %v", err)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("encode the wake: %v", err)
	}
	var routed events.Event
	if err := json.Unmarshal(raw, &routed); err != nil {
		t.Fatalf("decode the wake: %v", err)
	}
	w, ok := events.DataAs[*types.RawWebhook](&routed)
	if !ok || w == nil {
		t.Fatalf("the wake is not a raw webhook: %+v", routed)
	}
	return *w, len(raw)
}

// exactRecord is a record carrying an integer past 2^53 in a field this build
// decodes and in a member only a newer build knows.
func exactRecord() MutationRecord {
	return MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: RecordVersion, OpID: "op-t-1", Subject: TaskSubject("t-1"), Op: OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
			Scope: ScopeSet{Subject: true, Container: "ENG"},
		},
		Expect:   1<<60 + 1,
		Mutation: json.RawMessage(`{}`), Actor: "ana", ActorKind: AuthorHuman,
		Notify: &Notify{
			Kind:     ChangeStatus,
			Snapshot: Snapshot{Key: "ENG-1", Project: "ENG", Assignee: "bo"},
			Extra:    map[string]json.RawMessage{"later": json.RawMessage(`9007199254740993`)},
		},
	}
}

// THE RECORD A PARSER READS OUT OF A WAKE IS THE ONE ITS WRITER WROTE, integers
// past 2^53 included — the version it was decided against and a number a newer
// build carried inside the notification. The routing node decodes the wake's
// map with every number a float64, so the record is read from the wake's exact
// bytes.
//
// Mutation: read the record out of Body alone and the version comes back as
// 1152921504606847000.
func TestTheRecordAParserReadsOutOfAWakeIsExact(t *testing.T) {
	t.Parallel()
	w, _ := routedWake(t, exactRecord())
	record, err := recordFromBody(w)
	if err != nil {
		t.Fatalf("read the record out of the wake: %v", err)
	}
	if record.Expect != 1<<60+1 {
		t.Errorf("the record's version came back as %d, want %d", record.Expect, uint64(1<<60+1))
	}
	if record.Notify == nil || string(record.Notify.Extra["later"]) != "9007199254740993" {
		t.Errorf("the newer build's number came back as %v", record.Notify)
	}
}

// A WAKE WITH NO EXACT BYTES — one a feed on a build that predates them relayed
// — is read from its map, which still yields the record with every member.
func TestAWakeWithoutItsBytesIsReadFromItsMap(t *testing.T) {
	t.Parallel()
	w, _ := routedWake(t, exactRecord())
	w.BodyRaw = nil
	record, err := recordFromBody(w)
	if err != nil {
		t.Fatalf("read the record out of the map: %v", err)
	}
	if record.OpID != "op-t-1" || record.Notify == nil || record.Notify.Snapshot.Key != "ENG-1" {
		t.Errorf("the record read out of the map lost its members: %+v", record)
	}
	out, err := record.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(out), `"later":`) {
		t.Errorf("the member a newer build wrote did not survive the map: %s", out)
	}
}

// A COMMIT AT ITS DESIGN MAXIMUM STILL WAKES. A wake carries its record twice —
// as the map, and as the map's bytes in base64 — and a wake past the
// transport's ceiling is refused at publish, so the feed returns its record for
// ever and every wake behind it waits. The record here is [MaxCommitBytes]
// exactly, its padding a string every byte of which JSON writes six bytes for.
func TestACommitAtItsDesignMaximumStillWakes(t *testing.T) {
	t.Parallel()
	record := exactRecord()
	bare, err := record.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// The member's name, colon, quotes and comma, then one six-byte escape
	// per padded byte, with the remainder in plain letters.
	room := MaxCommitBytes - len(bare) - len(`,"pad":""`)
	pad := strings.Repeat(`\u0001`, room/6) + strings.Repeat("x", room%6)
	record.Extra = map[string]json.RawMessage{"pad": json.RawMessage(`"` + pad + `"`)}
	if full, err := record.Encode(); err != nil || len(full) != MaxCommitBytes {
		t.Fatalf("the padded record is %d bytes (%v), want %d", len(full), err, MaxCommitBytes)
	}
	_, size := routedWake(t, record)
	t.Logf("the wake of a commit at its design maximum is %d bytes of the transport's %d",
		size, queue.MaxPayloadBytes)
	if size > queue.MaxPayloadBytes {
		t.Errorf("the wake is %d bytes, past the transport's %d", size, queue.MaxPayloadBytes)
	}
}
