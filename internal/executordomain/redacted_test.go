package executordomain_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog"
)

// plantedToken is a GitHub token's shape, which the redaction knows: what a
// row's words hold where a run met one.
var plantedToken = "ghp_" + strings.Repeat("Qx7Rb2", 6)

// A SESSION'S ROW IS SHOWN REDACTED: ITS WORDS SOMEBODY ELSE WROTE, AND NOTHING
// ELSE. Every field of the row is set, every text spelling a token the
// redaction knows: Redacted hands out what the turn was asked, the ending's
// four prose fields and its unkept capture's reason redacted, and every other
// field — the conversation's key and the work key among them, which are a
// surface's and the engine's identifiers — exactly as the row holds it; and the
// row it was called on keeps its words, its ending shared with no copy. The
// fixture sets every field, which the test checks first, so a field added to
// the row is set here too and this says whether it is shown as it is held.
//
// Mutation: leave the unkept capture's reason as stored, or redact the
// conversation key too, and a check goes red.
func TestASessionsRowIsShownRedacted(t *testing.T) {
	t.Parallel()
	said := "the deploy used " + plantedToken
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	stream := executordomain.Domain{}.Stream().Name
	row := executordomain.Session{
		Handle: "ana", Turn: "0193f0c3-0d5e-7a14-8b2f-61c9e2d0a7f3",
		ConversationKey: "chat:" + plantedToken, WorkKey: plantedToken,
		Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: plantedToken,
		Object: objstore.Object{Key: objstore.KeyAt(at), Hash: objstore.Hash(strings.Repeat("a", 64)),
			Size: 4096},
		ContentKey: "k1", ArchiveFormat: 1, LaunchSeq: 2, CaptureSeq: 3,
		Home: "/home/" + plantedToken, Workspace: "/work/" + plantedToken,
		ContextTokens: 1200, Interrupted: true, CapturedAt: at,
		ReadThrough: statelog.Position{Stream: stream, Generation: 1, Seq: 7},
		Absorbs:     []string{"0193f0c3-0d5e-7a14-8b2f-61c9e2d0a7f4"},
		BaseTurn:    "0193f0c3-0d5e-7a14-8b2f-61c9e2d0a7f5",
		RetiredBy:   "0193f0c3-0d5e-7a14-8b2f-61c9e2d0a7f6",
		EndedAt:     at.Add(time.Minute), EndedAtPosition: statelog.Position{Stream: stream, Generation: 1, Seq: 9},
		Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, Decision: "done",
			Delivered: true, DeliveredTo: "chatlet", Answer: "answered", DeliveryUnknown: true,
			CompletedWork: said, BlockedOn: said, StoppedBy: "ana.diaz", StopReason: said, Failure: said,
			Unkept: &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 4, Reason: said}, WentOn: true,
			Exhausted: []string{plantedToken}},
		AskExcerpt: "fix the deploy with " + plantedToken,
	}
	everyFieldSet(t, "the row", reflect.ValueOf(row))

	got := row.Redacted()
	if got.Ending == nil || got.Ending.Unkept == nil {
		t.Fatalf("the redacted row carries no ending or no unkept capture: %+v", got)
	}
	for field, text := range map[string]string{
		"ask_excerpt": got.AskExcerpt, "completed_work": got.Ending.CompletedWork,
		"blocked_on": got.Ending.BlockedOn, "stop_reason": got.Ending.StopReason,
		"failure": got.Ending.Failure, "unkept.reason": got.Ending.Unkept.Reason,
	} {
		if strings.Contains(text, plantedToken) || !strings.Contains(text, redact.Marker) {
			t.Errorf("the redacted row's %s reads %q, want the token redacted", field, text)
		}
	}
	if got.Ending == row.Ending || got.Ending.Unkept == row.Ending.Unkept {
		t.Error("the redacted row shares its ending with the row it was made from")
	}
	if !strings.Contains(row.Ending.CompletedWork, plantedToken) ||
		!strings.Contains(row.Ending.Unkept.Reason, plantedToken) ||
		!strings.Contains(row.AskExcerpt, plantedToken) {
		t.Error("redacting the row changed the row it was called on")
	}

	// NOTHING ELSE CHANGED: with the six fields put back as the row holds
	// them, the copy is the row.
	ending, unkept := *got.Ending, *got.Ending.Unkept
	ending.CompletedWork, ending.BlockedOn = row.Ending.CompletedWork, row.Ending.BlockedOn
	ending.StopReason, ending.Failure = row.Ending.StopReason, row.Ending.Failure
	unkept.Reason = row.Ending.Unkept.Reason
	ending.Unkept, got.Ending, got.AskExcerpt = &unkept, &ending, row.AskExcerpt
	if !reflect.DeepEqual(got, row) {
		t.Errorf("the redacted row changed a field beyond the six:\n got %+v\nwant %+v", got, row)
	}
}

// A ROW IS SHOWN WITH NO UNKEPT CAPTURE IT KEEPS. An end decided off the run's
// record can name as not kept a capture whose record the log took before the
// end — one a taker handed on before its lease's window closed, which the node
// that waited on it settled refused — and the row's place is then that
// capture's own. The one form every reader is shown a row in names the end's
// unkept capture only where it is newer than the row's place
// ([executordomain.Session.Unkept]), its reason redacted, so no surface that
// shows the row tells the session it keeps as lost; the row it was called on
// keeps the ending as the log holds it.
//
// Mutation: copy the ending's unkept capture as the row holds it, and the row
// at that capture's own place, or past it, is shown with it.
func TestARowIsShownWithNoUnkeptCaptureItKeeps(t *testing.T) {
	t.Parallel()
	said := "its taker node-a stopped before it was kept, while it read " + plantedToken
	for _, c := range []struct {
		name            string
		launch, capture int64
		shown           bool
	}{
		{name: "at the row's own place", launch: 2, capture: 3},
		{name: "older than the row's place", launch: 2, capture: 2},
		{name: "of an earlier launch", launch: 1, capture: 9},
		{name: "newer than the row's place", launch: 2, capture: 4, shown: true},
		{name: "of a later launch", launch: 3, capture: 1, shown: true},
	} {
		row := executordomain.Session{Handle: "ana", Turn: "0193f0c3-0d5e-7a14-8b2f-61c9e2d0a7f3",
			LaunchSeq: 2, CaptureSeq: 3,
			Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
				Unkept: &executorkeep.Unkept{LaunchSeq: c.launch, CaptureSeq: c.capture, Reason: said}}}
		got := row.Redacted().Ending
		switch {
		case got == nil:
			t.Errorf("%s: the shown row carries no ending", c.name)
		case !c.shown && got.Unkept != nil:
			t.Errorf("%s: the row is shown with %+v unkept, the capture it keeps or an older one",
				c.name, *got.Unkept)
		case c.shown && (got.Unkept == nil || got.Unkept.LaunchSeq != c.launch ||
			got.Unkept.CaptureSeq != c.capture):
			t.Errorf("%s: the row is shown with %+v unkept, want (launch %d, capture %d)", c.name,
				got.Unkept, c.launch, c.capture)
		case c.shown && (strings.Contains(got.Unkept.Reason, plantedToken) ||
			!strings.Contains(got.Unkept.Reason, redact.Marker)):
			t.Errorf("%s: the shown unkept capture's reason reads %q, want the token redacted", c.name,
				got.Unkept.Reason)
		}
		if row.Ending.Unkept == nil || row.Ending.Unkept.Reason != said {
			t.Errorf("%s: showing the row changed the ending it was called on: %+v", c.name, row.Ending.Unkept)
		}
	}
}

// everyFieldSet fails t for a field of the struct v holds that is its zero
// value, and for one of a struct a field points to: a fixture that leaves one
// unset proves nothing about it.
func everyFieldSet(t *testing.T, name string, v reflect.Value) {
	t.Helper()
	for i := range v.NumField() {
		field, value := v.Type().Field(i), v.Field(i)
		if value.IsZero() {
			t.Errorf("%s leaves %s unset: set it, so this says how it is shown", name, field.Name)
			continue
		}
		if value.Kind() == reflect.Pointer && value.Elem().Kind() == reflect.Struct {
			everyFieldSet(t, name+"'s "+field.Name, value.Elem())
		}
	}
}
