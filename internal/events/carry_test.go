package events_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/jsoncarry"
	"github.com/crewlet/crewlet/internal/jsoncarry/jsoncarrytest"
)

// wireShape is a payload with a member of every shape a payload has.
type wireShape struct {
	Note    string            `json:"note"`
	Count   int               `json:"count,omitempty"`
	Tags    []string          `json:"tags,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Nested  *wireNested       `json:"nested,omitempty"`
	Skipped string            `json:"-"`
}

type wireNested struct {
	Depth int    `json:"depth"`
	Why   string `json:"why,omitempty"`
}

func (*wireShape) EventType() string { return "test_wire_shape" }

func init() { events.Register[wireShape]() }

// filledEvent is an envelope with every member set, carrying a registered
// body with every member set.
func filledEvent() *events.Event {
	return &events.Event{
		ID:              uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		Type:            "test_wire_shape",
		Timestamp:       time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Source:          "engine",
		Payload:         map[string]any{"bag": "free-form"},
		TraceID:         "abcd1234abcd1234abcd1234abcd1234",
		SpanID:          "1234abcd1234abcd",
		ParentSpanID:    "feedfacefeedface",
		DelegationDepth: 2,
		ParentTurnID:    "turn-1",
		DelegationChain: []string{"cto", "eng"},
		Data: &wireShape{
			Note: "<b>", Count: 3, Tags: []string{"a"}, Labels: map[string]string{"k": "v"},
			Nested: &wireNested{Depth: 1, Why: "because"},
		},
	}
}

// The envelope as every node already running writes it: ONE flat object in
// key order, the body's members beside the envelope's own.
const (
	knownEventGolden = `{"count":3,"delegation_chain":["cto","eng"],"delegation_depth":2,"id":"11111111-2222-3333-4444-555555555555","labels":{"k":"v"},"nested":{"depth":1,"why":"because"},"note":"\u003cb\u003e","parent_span_id":"feedfacefeedface","parent_turn_id":"turn-1","payload":{"bag":"free-form"},"source":"engine","span_id":"1234abcd1234abcd","tags":["a"],"timestamp":"2026-01-02T03:04:05Z","trace_id":"abcd1234abcd1234abcd1234abcd1234","type":"test_wire_shape"}`

	// unknownEventWire is a type this build has never heard of, as a newer
	// build publishes it; unknownEventGolden is what this build re-publishes.
	unknownEventWire   = `{"id":"11111111-2222-3333-4444-555555555555","type":"test_from_the_future","timestamp":"2026-01-02T03:04:05Z","source":"engine","trace_id":"t","span_id":"s","parent_span_id":"","delegation_depth":0,"parent_turn_id":"","issue_id":1234567890123456789,"detail":{"b":[1,2],"a":"x"}}`
	unknownEventGolden = `{"delegation_depth":0,"detail":{"b":[1,2],"a":"x"},"id":"11111111-2222-3333-4444-555555555555","issue_id":1234567890123456789,"parent_span_id":"","parent_turn_id":"","source":"engine","span_id":"s","timestamp":"2026-01-02T03:04:05Z","trace_id":"t","type":"test_from_the_future"}`
)

// AN EVENT THIS BUILD PUBLISHES IS THE BYTES IT ALWAYS WAS.
//
// Two builds share one stream through a rolling upgrade, so the envelope's
// bytes are a contract between peers: an event carrying nothing this build
// does not know encodes as it always has, and one this build cannot read is
// re-published as it always has been.
func TestAnEventEncodesAsItAlwaysHas(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(filledEvent())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != knownEventGolden {
		t.Fatalf("the event encodes as\n  %s\nand every node publishes\n  %s", got, knownEventGolden)
	}
	var back events.Event
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	again, err := json.Marshal(&back)
	if err != nil {
		t.Fatalf("marshal again: %v", err)
	}
	if string(again) != knownEventGolden {
		t.Fatalf("decoded and encoded again it is\n  %s\nnot\n  %s", again, knownEventGolden)
	}

	var unknown events.Event
	if err := json.Unmarshal([]byte(unknownEventWire), &unknown); err != nil {
		t.Fatalf("unmarshal an unknown type: %v", err)
	}
	relayed, err := json.Marshal(&unknown)
	if err != nil {
		t.Fatalf("marshal an unknown type: %v", err)
	}
	if string(relayed) != unknownEventGolden {
		t.Fatalf("an unknown type re-publishes as\n  %s\nnot\n  %s", relayed, unknownEventGolden)
	}
}

// A MEMBER THE ENVELOPE OR THE BODY DECODES IS NEVER CARRIED, however it is
// spelled: encoding/json matches a member to a field ignoring case, and one
// filed in Extra as well would be published twice — once as the field writes
// it and once as it arrived.
func TestAMemberTheEventDecodesIsNeverCarried(t *testing.T) {
	t.Parallel()
	const raw = `{"ID":"11111111-2222-3333-4444-555555555555","TYPE":"test_wire_shape",` +
		`"timestamp":"2026-01-02T03:04:05Z","ſource":"engine","NOTE":"n","Count":2,"later":1}`
	var event events.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	body, ok := events.DataAs[*wireShape](&event)
	if !ok || body.Note != "n" || body.Count != 2 || event.Source != "engine" {
		t.Fatalf("the members did not decode into their fields: %+v, %+v", event, body)
	}
	if len(event.Extra) != 1 || string(event.Extra["later"]) != "1" {
		t.Errorf("Extra is %v, want the one member nothing decodes", event.Extra)
	}
	out, err := json.Marshal(&event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, once := range []string{`"n"`, `"engine"`, `"later":1`} {
		if n := strings.Count(string(out), once); n != 1 {
			t.Errorf("%s is written %d times in %s", once, n, out)
		}
	}
}

// A MEMBER THE BODY KNOWS IS NEVER WRITTEN FROM EXTRA, even one the body's
// own value leaves out — a cleared field goes out absent, not as a stale copy.
func TestAMemberTheBodyKnowsIsNeverWrittenFromExtra(t *testing.T) {
	t.Parallel()
	event := filledEvent()
	event.Data.(*wireShape).Count = 0
	event.Extra = map[string]json.RawMessage{"COUNT": json.RawMessage(`9`), "id": json.RawMessage(`"x"`)}
	out, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(out)), `"count"`) || strings.Contains(string(out), `"x"`) {
		t.Errorf("a stale copy of a known member was written from Extra: %s", out)
	}
}

// carriedShape is a payload whose nested object carries, the shape a body a
// newer build may extend inside takes.
type carriedShape struct {
	Inner carriedInner `json:"inner"`
}

type carriedInner struct {
	Depth int `json:"depth"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (c carriedInner) MarshalJSON() ([]byte, error) {
	type fields carriedInner
	return jsoncarry.Marshal(fields(c), c.Extra)
}

func (c *carriedInner) UnmarshalJSON(b []byte) error {
	type fields carriedInner
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

func (*carriedShape) EventType() string { return "test_carried_shape" }

func init() { events.Register[carriedShape]() }

// A MEMBER A NEWER BUILD ADDED INSIDE A BODY'S OBJECT SURVIVES A RE-PUBLISH
// when that object carries — at the top of the event through Extra, inside
// the body through the object's own carry.
func TestAMemberInsideABodysCarryingObjectSurvivesARepublish(t *testing.T) {
	t.Parallel()
	const raw = `{"id":"11111111-2222-3333-4444-555555555555","type":"test_carried_shape",` +
		`"timestamp":"2026-01-02T03:04:05Z","source":"engine","trace_id":"","span_id":"",` +
		`"parent_span_id":"","delegation_depth":0,"parent_turn_id":"",` +
		`"inner":{"depth":1,"later":{"z":9007199254740993,"a":1}},"top":true}`
	var event events.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(&event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, kept := range []string{`"later":{"z":9007199254740993,"a":1}`, `"top":true`} {
		if !strings.Contains(string(out), kept) {
			t.Errorf("the re-published event lost %s: %s", kept, out)
		}
	}
}

// AN EVENT DECODED INTO ONE THAT HELD ANOTHER IS THE NEW EVENT ALONE: its
// body follows from its own type, so a body — or a member — left from the
// event that was there before would be published under the new type.
func TestDecodingOverAnEventReplacesIt(t *testing.T) {
	t.Parallel()
	event := *filledEvent()
	event.Extra = map[string]json.RawMessage{"left_over": json.RawMessage(`1`)}
	if err := json.Unmarshal([]byte(unknownEventWire), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Data != nil {
		t.Errorf("the event of a type this build does not know kept the previous body %+v", event.Data)
	}
	if _, kept := event.Extra["left_over"]; kept || event.Payload != nil || len(event.DelegationChain) != 0 {
		t.Errorf("the decoded event kept members of the one before it: %+v", event)
	}
	relayed, err := json.Marshal(&event)
	if err != nil || string(relayed) != unknownEventGolden {
		t.Errorf("re-published as %s (%v), want %s", relayed, err, unknownEventGolden)
	}
}

// AN EVENT HELD BY VALUE IS WRITTEN WITH ITS BODY, wherever it sits — in a
// map, or in a struct marshalled by value — where encoding/json cannot take
// its address.
func TestAnEventHeldByValueIsWrittenWithItsBody(t *testing.T) {
	t.Parallel()
	byValue, err := json.Marshal(map[string]events.Event{"e": *filledEvent()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"e":` + knownEventGolden + `}`; string(byValue) != want {
		t.Errorf("an event held by value is written as\n  %s\nnot\n  %s", byValue, want)
	}
}

// A MEMBER THE ENVELOPE DECODES IS NEVER WRITTEN FROM EXTRA, however a caller
// spelled it there: encoding/json matches a member to a field ignoring case,
// so an "ID" or a "Type" in Extra is the envelope's id and type, and writing
// it beside them would publish an event with two of each.
//
// Mutation: compare Extra's names exactly in MarshalJSON and both are written.
func TestAnEnvelopeMemberSpelledAnotherWayIsNeverWrittenFromExtra(t *testing.T) {
	t.Parallel()
	event := filledEvent()
	event.Extra = map[string]json.RawMessage{
		"ID": json.RawMessage(`"not-the-id"`), "Type": json.RawMessage(`"not_the_type"`),
		"later": json.RawMessage(`1`),
	}
	out, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, stale := range []string{"not-the-id", "not_the_type", `"ID"`, `"Type"`} {
		if strings.Contains(string(out), stale) {
			t.Errorf("%s was written from Extra: %s", stale, out)
		}
	}
	if !strings.Contains(string(out), `"later":1`) {
		t.Errorf("the member nothing decodes was not written: %s", out)
	}
}

// AN EVENT CARRIES WHAT IT DOES NOT KNOW, and so does every object it holds
// that the walk reaches — its body is an interface the walk cannot see into,
// and every registered payload's objects are walked in internal/events/types.
func TestEveryObjectOnAnEventCarries(t *testing.T) {
	t.Parallel()
	for _, missing := range jsoncarrytest.Uncarried(nil, reflect.TypeFor[events.Event]()) {
		t.Error(missing)
	}
}

// A MEMBER A NEWER BUILD ADDED SURVIVES A RE-PUBLISH, byte for byte — at the
// top of the event through its Extra, and inside a body's object through that
// object's carry.
func TestAMemberANewerBuildAddedSurvivesARepublish(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(events.NewFrom(&carriedShape{Inner: carriedInner{Depth: 1}},
		events.TraceContext{TraceID: "t"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	jsoncarrytest.Survives(t, reflect.TypeFor[carriedShape](), raw, func(in []byte) ([]byte, error) {
		var event events.Event
		if err := json.Unmarshal(in, &event); err != nil {
			return nil, err
		}
		if event.Data == nil {
			return nil, fmt.Errorf("the body did not decode, so it was carried whole")
		}
		return json.Marshal(&event)
	})
}

// A NUMBER IN THE FREE-FORM BAG SURVIVES A ROUND TRIP AS ITS DIGITS, at every
// depth — an integer past 2^53 included, which as a float64 would come back a
// different integer — and decodes as a json.Number, the one type its reader
// asserts.
//
// Mutation: decode the bag without UseNumber and both integers are rounded.
func TestANumberInThePayloadBagSurvivesARoundTrip(t *testing.T) {
	t.Parallel()
	event := filledEvent()
	event.Payload = map[string]any{
		"big":    int64(9007199254740993),
		"nested": map[string]any{"big": uint64(1<<60 + 1)},
		"ratio":  0.5,
	}
	out, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back events.Event
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, ok := back.Payload["big"].(json.Number); !ok || got.String() != "9007199254740993" {
		t.Errorf("the bag's integer read back as %#v", back.Payload["big"])
	}
	again, err := json.Marshal(&back)
	if err != nil {
		t.Fatalf("marshal again: %v", err)
	}
	for _, exact := range []string{`"big":9007199254740993`, `"big":1152921504606846977`, `"ratio":0.5`} {
		if !strings.Contains(string(again), exact) {
			t.Errorf("the bag lost %s: %s", exact, again)
		}
	}
}
