package events_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
)

// LOSSLESSNESS IS THE PACKAGE'S LOAD-BEARING PROPERTY, and it had no test
// that could fail.
//
// A rolling upgrade puts two builds on one stream. An event type the older
// build does not know must decode, round-trip and re-publish byte-for-byte,
// or the upgrade silently rewrites the fleet's traffic. Carried through
// map[string]any, every JSON number became a float64 — so a 19-digit id came
// back off by a hundred and a large token count came back wrong, on a path
// whose whole job is to change nothing.
func TestAnUnknownTypesLargeIntegersSurviveARoundTrip(t *testing.T) {
	t.Parallel()
	const raw = `{
		"id":"11111111-2222-3333-4444-555555555555",
		"type":"quantum_turn_entangled",
		"timestamp":"2026-01-02T03:04:05Z",
		"source":"engine",
		"issue_id":1234567890123456789,
		"ratio":0.1,
		"tokens":9007199254740993
	}`

	var event events.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("an unknown type must never fail to decode: %v", err)
	}
	if event.Data != nil {
		t.Fatalf("unknown type decoded a typed body: %#v", event.Data)
	}

	out, err := json.Marshal(&event)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		`"issue_id":1234567890123456789`,
		`"ratio":0.1`,
		`"tokens":9007199254740993`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("re-published event lost %s\ngot: %s", want, got)
		}
	}
}

// AND SO DO A KNOWN TYPE'S, which is the half that fires on every publish.
//
// MarshalJSON remapped the envelope and the typed body through
// map[string]any before merging, so this was not an unknown-type problem at
// all: every event this build publishes went through the same float64.
func TestAKnownTypesLargeIntegersSurviveARoundTrip(t *testing.T) {
	t.Parallel()
	events.Register[bigNumbers]()

	event := events.New(bigNumbers{
		IssueID: 1234567890123456789,
		Tokens:  9007199254740993,
	}, events.TraceContext{})

	out, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		`"issue_id":1234567890123456789`,
		`"tokens":9007199254740993`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("published event lost %s\ngot: %s", want, got)
		}
	}

	var back events.Event
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	body, ok := events.DataAs[*bigNumbers](&back)
	if !ok {
		t.Fatal("the typed body did not decode")
	}
	if body.IssueID != 1234567890123456789 || body.Tokens != 9007199254740993 {
		t.Errorf("body = %+v, want the ids unchanged", *body)
	}
}

type bigNumbers struct {
	IssueID int64 `json:"issue_id"`
	Tokens  int64 `json:"tokens"`
}

func (*bigNumbers) EventType() string { return "test_big_numbers" }

// A KNOWN TYPE'S OWN KEYS NEVER REACH Extra, even when the value that
// arrived is the zero one.
//
// Extra's contract is "fields belonging to neither the envelope nor a known
// Data type". The field set used to be learned by re-marshalling the decoded
// body, and an `omitempty` field holding its zero value encodes to no key at
// all — so the type's own field was classified as foreign and carried a
// second, redundant copy of itself into Extra. Deriving the set from the
// struct tags is what makes the classification a property of the TYPE rather
// than of the particular value that happened to arrive.
func TestAZeroOmitemptyFieldIsNotCarriedIntoExtra(t *testing.T) {
	t.Parallel()
	events.Register[sparse]()

	const raw = `{
		"id":"11111111-2222-3333-4444-555555555555",
		"type":"test_sparse",
		"timestamp":"2026-01-02T03:04:05Z",
		"source":"engine",
		"note":"",
		"count":0
	}`
	var event events.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Data == nil {
		t.Fatal("a registered type did not decode")
	}
	for _, own := range []string{"note", "count"} {
		if _, leaked := event.Extra[own]; leaked {
			t.Errorf("the type's own field %q was carried into Extra as well", own)
		}
	}
}

// AND AN UNREGISTERED-BODY EVENT STILL CARRIES EVERY KEY.
//
// A registered type whose body fails to decode leaves Data nil on purpose,
// and the field set must then be ignored — otherwise the keys the type owns
// are dropped from Extra too and the event is silently gutted rather than
// carried. This is why the lookup is gated on Data rather than on the type
// being registered.
func TestARegisteredTypeWithAnUndecodableBodyKeepsEveryField(t *testing.T) {
	t.Parallel()
	events.Register[strict]()

	// count is declared int but arrives as a string, so the body fails and
	// Data falls through to nil.
	const raw = `{
		"id":"11111111-2222-3333-4444-555555555555",
		"type":"test_strict",
		"timestamp":"2026-01-02T03:04:05Z",
		"source":"engine",
		"note":"kept",
		"count":"not-a-number"
	}`
	var event events.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Data != nil {
		t.Fatal("a body that cannot decode must leave Data nil")
	}
	for _, key := range []string{"note", "count"} {
		if _, ok := event.Extra[key]; !ok {
			t.Errorf("field %q was dropped from an event whose body did not decode; "+
				"the event is gutted rather than carried", key)
		}
	}
}

type sparse struct {
	Note  string `json:"note,omitempty"`
	Count int    `json:"count,omitempty"`
}

func (*sparse) EventType() string { return "test_sparse" }

type strict struct {
	Note  string `json:"note"`
	Count int    `json:"count"`
}

func (*strict) EventType() string { return "test_strict" }
