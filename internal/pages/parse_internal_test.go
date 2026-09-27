package pages

import (
	"encoding/json"
	"fmt"
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
	payload, err := Encode(record)
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	delivery, wakes, err := NewTranslator(nil).Translate(t.Context(),
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

// savedRecord is a save of one page, announced to one watcher.
func savedRecord() MutationRecord {
	return MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: RecordVersion, OpID: "op-save", Subject: PageSubject("page-1"), Op: OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Gen: 1, Writer: "node-a",
			Scope: ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: json.RawMessage(`{}`), Actor: "ada", ActorKind: AuthorHuman,
		Notify: &Notify{Kind: ChangeSaved, PageID: "page-1", Recipients: []string{"bo"}},
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
	rec := savedRecord()
	rec.Expect = 1<<60 + 1
	rec.Notify.Extra = map[string]json.RawMessage{"later": json.RawMessage(`9007199254740993`)}
	w, _ := routedWake(t, rec)
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
// — is read from its map, and one carrying neither is refused.
func TestAWakeWithoutItsBytesIsReadFromItsMap(t *testing.T) {
	t.Parallel()
	w, _ := routedWake(t, savedRecord())
	w.BodyRaw = nil
	record, err := recordFromBody(w)
	if err != nil {
		t.Fatalf("read the record out of the map: %v", err)
	}
	if record.OpID != "op-save" || record.Notify == nil || record.Notify.PageID != "page-1" {
		t.Errorf("the record read out of the map lost its members: %+v", record)
	}
	if _, err := recordFromBody(types.RawWebhook{}); err == nil {
		t.Error("a wake carrying no record decoded as one")
	}
}

// A CREATE AT EVERY CAP STILL WAKES. A wake carries its record twice — as the
// map, and as the map's bytes in base64 — and a wake past the transport's
// ceiling is refused at publish, so the feed returns its record for ever and
// every wake behind it waits. The fixture is what the caps permit: a body at
// [MaxBody] and a title, labels and an excerpt at theirs, every byte one JSON
// writes six bytes for, with [MaxWatchers] long handles watching and named.
func TestACreateAtEveryCapStillWakes(t *testing.T) {
	t.Parallel()
	escaping := func(n int) string { return strings.Repeat("\x01", n) }
	handles := make([]string, MaxWatchers)
	for i := range handles {
		handles[i] = fmt.Sprintf("handle-%04d-%s", i, strings.Repeat("x", 52))
	}
	labels := make([]string, MaxLabels)
	for i := range labels {
		labels[i] = escaping(MaxLabelLength)
	}
	create, err := json.Marshal(CreatePayload{
		V: 1, PageID: "page-1", Container: "ENG", Title: escaping(MaxTitle),
		Body: escaping(MaxBody), Status: "current", Labels: labels, Watchers: handles,
		Author: "ada",
	})
	if err != nil {
		t.Fatalf("encode the create: %v", err)
	}
	rec := savedRecord()
	rec.Subject = TitleSubject("ENG", escaping(MaxTitle))
	rec.Op, rec.Mutation = OpCreate, create
	rec.Notify = &Notify{Kind: ChangeCreated, PageID: "page-1", Recipients: handles,
		Mentions: handles, Excerpt: escaping(MaxExcerpt), Container: "ENG",
		Title: escaping(MaxTitle)}
	_, size := routedWake(t, rec)
	t.Logf("the wake of a create at every cap is %d bytes of the transport's %d",
		size, queue.MaxPayloadBytes)
	if size > queue.MaxPayloadBytes {
		t.Errorf("the wake is %d bytes, past the transport's %d", size, queue.MaxPayloadBytes)
	}
}
