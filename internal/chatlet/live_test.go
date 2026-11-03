package chatlet_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A COMMITTED EFFECT CROSSES THE WIRE WHOLE.
//
// A node whose own copy is out of service is relayed a sound peer's chat
// batches instead of its own applier's, so an [chatlet.Applied] one node gathered
// is decoded on another and rendered there. A field the encoding dropped — an
// unexported one, a type with no wire form, a tag that collides with another —
// would be a frame missing it, rendered on exactly the node nobody vouches for,
// with nothing anywhere to say so. The fixture sets EVERY field, the routing
// snapshot's included, and says so by walking them: a field added later with
// no value here fails before the round trip can pass by leaving it zero.
//
// Mutation: tag two fields alike, or give one a type that encodes nothing, and
// the round trip differs.
func TestACommittedEffectCrossesTheWireWhole(t *testing.T) {
	t.Parallel()
	applied := chatlet.Applied{
		Position: statelog.Position{Stream: topics.ChatLogStream, Generation: 2, Seq: 41},
		Subject:  chatlet.MessageSubject("c-1"), Op: chatlet.OpPost, OpID: "op-1",
		ChannelID: "c-1", MessageID: "m-1", ChannelSeq: 7,
		Actor: "ada", ActorKind: chatlet.AuthorHuman,
		At: time.Date(2026, 10, 17, 9, 30, 0, 123_000_000, time.UTC),
		Notify: &chatlet.Notify{
			MessageID: "m-1", ChannelID: "c-1", ChannelName: "launch",
			ChannelKind: chatlet.KindPublic, ThreadRoot: "m-0",
			Author: "ada", AuthorKind: chatlet.AuthorHuman,
			Excerpt: "ship it", Mentions: []string{"bo"}, Answerer: "cy",
			Recipients:     []chatlet.Recipient{{Handle: "bo", Reason: chatlet.ReasonMention}},
			WakesTruncated: true,
		},
		Removed: []string{"dee"},
	}
	everyFieldSet(t, "Applied", reflect.ValueOf(applied))
	everyFieldSet(t, "Notify", reflect.ValueOf(*applied.Notify))

	raw, err := json.Marshal(applied)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got chatlet.Applied
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, applied) {
		t.Fatalf("a committed effect did not survive the wire:\n got %+v\nwant %+v\n(%s)",
			got, applied, raw)
	}
}

// everyFieldSet fails for a field of v left at its zero value.
func everyFieldSet(t *testing.T, name string, v reflect.Value) {
	t.Helper()
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("the fixture leaves %s.%s unset, so the round trip cannot say "+
				"whether it crosses the wire", name, v.Type().Field(i).Name)
		}
	}
}
