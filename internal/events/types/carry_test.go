package types_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/jsoncarry/jsoncarrytest"
)

// memberTypes is the type of every member a payload writes, an untagged
// embedded struct's members among them — the payload is flat, so such a
// struct is not an object on the wire, and its members are the payload's.
func memberTypes(typ reflect.Type) []reflect.Type {
	var out []reflect.Type
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		inner := field.Type
		if inner.Kind() == reflect.Pointer {
			inner = inner.Elem()
		}
		if field.Anonymous && name == "" && inner.Kind() == reflect.Struct {
			out = append(out, memberTypes(inner)...)
			continue
		}
		if field.IsExported() {
			out = append(out, field.Type)
		}
	}
	return out
}

// payloadTypes is every payload type this build registers.
func payloadTypes(t *testing.T) map[string]reflect.Type {
	t.Helper()
	out := map[string]reflect.Type{}
	for _, name := range events.RegisteredTypes() {
		payload, ok := events.PayloadFor(name)
		if !ok {
			t.Fatalf("%s is registered and has no payload", name)
		}
		out[name] = reflect.TypeOf(payload).Elem()
	}
	if len(out) == 0 {
		t.Fatal("no payload is registered, so nothing here was checked")
	}
	return out
}

// EVERY OBJECT INSIDE A PAYLOAD CARRIES WHAT IT DOES NOT KNOW, at every depth.
//
// The payload itself is flat and its unknown members ride the envelope's
// Extra; an object inside it is decoded through its own type, so one that
// does not carry is a member a newer build wrote inside it that a relay by
// this build erases (carry.go).
//
// Mutation: drop any one of carry.go's UnmarshalJSON methods and that type is
// named here.
func TestEveryObjectInsideAPayloadCarries(t *testing.T) {
	t.Parallel()
	var roots []reflect.Type
	for _, typ := range payloadTypes(t) {
		roots = append(roots, memberTypes(typ)...)
	}
	for _, missing := range jsoncarrytest.Uncarried(nil, roots...) {
		t.Error(missing)
	}
}

// A MEMBER A NEWER BUILD ADDED INSIDE ANY OBJECT OF ANY PAYLOAD SURVIVES THIS
// BUILD'S DECODE AND RE-PUBLISH of the event, byte for byte — beside the
// payload's own members through the envelope, and inside each of its objects
// through that object's own carry.
func TestAMemberANewerBuildAddedSurvivesInsideEveryPayload(t *testing.T) {
	t.Parallel()
	for name, typ := range payloadTypes(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body, ok := jsoncarrytest.FilledOf(typ).Interface().(events.Payload)
			if !ok {
				t.Fatalf("%v is not a payload", typ)
			}
			raw, err := json.Marshal(events.NewFrom(body, events.TraceContext{}))
			if err != nil {
				t.Fatalf("encode the event: %v", err)
			}
			jsoncarrytest.Survives(t, typ, raw, func(in []byte) ([]byte, error) {
				var back events.Event
				if err := json.Unmarshal(in, &back); err != nil {
					return nil, err
				}
				if back.Data == nil {
					// A BODY THAT FAILED TO DECODE is carried whole,
					// which would pass this check for every object
					// in it without one of them carrying.
					return nil, fmt.Errorf("the body of %s did not decode", name)
				}
				return json.Marshal(&back)
			})
		})
	}
}

// A TRIGGER CARRYING NOTHING IS THE BYTES IT ALWAYS WAS, and one carrying a
// newer build's member writes it after its own, in key order. A member the
// descriptor reads into a field is never carried as well, however it is
// spelled, and a descriptor that names nothing and carries nothing is `{}`.
func TestATriggerCarriesWhatANewerBuildWrote(t *testing.T) {
	t.Parallel()
	trigger := types.Trigger{
		ID: "ev-1", Type: "task_assigned", Summary: "s", Actor: "ana",
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	out, err := json.Marshal(trigger)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const golden = `{"actor":"ana","id":"ev-1","summary":"s","timestamp":"2026-01-02T03:04:05Z","type":"task_assigned"}`
	if string(out) != golden {
		t.Fatalf("the trigger encodes as\n  %s\nnot\n  %s", out, golden)
	}

	newer := `{"actor":"ana","ID":"ev-1","lane":{"z":9007199254740993},"summary":"s",` +
		`"timestamp":"2026-01-02T03:04:05Z","type":"task_assigned"}`
	var back types.Trigger
	if err := json.Unmarshal([]byte(newer), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ID != "ev-1" || len(back.Extra) != 1 {
		t.Fatalf("the trigger read back as %+v", back)
	}
	again, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("marshal again: %v", err)
	}
	const carried = `{"actor":"ana","id":"ev-1","lane":{"z":9007199254740993},"summary":"s",` +
		`"timestamp":"2026-01-02T03:04:05Z","type":"task_assigned"}`
	if string(again) != carried {
		t.Errorf("the trigger carrying a member encodes as\n  %s\nnot\n  %s", again, carried)
	}

	if out, err := json.Marshal(types.Trigger{}); err != nil || string(out) != `{}` {
		t.Errorf("the zero trigger encodes as %s (%v), want {}", out, err)
	}
	onlyNewer := types.Trigger{Extra: map[string]json.RawMessage{"lane": json.RawMessage(`1`)}}
	if onlyNewer.IsZero() {
		t.Error("a trigger carrying a newer build's member reads as naming no trigger")
	}
}
