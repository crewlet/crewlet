package livestate_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// round is one progress frame of the call (t1, execute, 0), carrying what a
// frame of that round does.
func round(at string, roundNum int, fields map[string]any) *livestate.Envelope {
	payload := map[string]any{
		"role": "Lead", "turn_id": "t1", "phase": "execute", "iteration": float64(0),
		"round_num": float64(roundNum),
	}
	maps.Copy(payload, fields)
	return &livestate.Envelope{ID: "p-" + at, Type: "agent_turn_progress", Timestamp: at, Category: "task", Payload: payload}
}

func narrated(n int) []any {
	out := make([]any, 0, n)
	for i := range n {
		out = append(out, map[string]any{"round": float64(i + 1), "content": strings.Repeat("c", i+1)})
	}
	return out
}

// moved reports, field by field, which of a call's versions grew from was to
// now — and fails the test on one that went back, since a version is never
// handed out twice and only ever grows.
func moved(t *testing.T, was, now livestate.CallVersions) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for name, pair := range map[string][2]int{
		"prompt": {was.Prompt, now.Prompt}, "response": {was.Response, now.Response},
		"narration": {was.Narration, now.Narration}, "executions": {was.Executions, now.Executions},
		"rounds": {was.Rounds, now.Rounds},
	} {
		if pair[1] < pair[0] {
			t.Errorf("the %s version went back from %d to %d", name, pair[0], pair[1])
		}
		out[name] = pair[1] > pair[0]
	}
	return out
}

// only is the moved map in which exactly the named fields grew.
func only(names ...string) map[string]bool {
	out := map[string]bool{"prompt": false, "response": false, "narration": false, "executions": false, "rounds": false}
	for _, n := range names {
		out[n] = true
	}
	return out
}

// A HEAVY FIELD'S VERSION MOVES WITH THAT FIELD AND NOTHING ELSE. A frame that
// streams the round being written leaves every committed field where it was,
// and its version with it — which is what lets a push leave the field out; a
// frame that commits a round moves the narration's, and the opening frame that
// lands late moves the prompt's.
//
// Mutation: bump every version on every frame, and the streaming frame moves
// them all.
func TestAVersionMovesOnlyWithItsField(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	versions := func() livestate.CallVersions {
		t.Helper()
		o := s.AgentOverlay("Lead")
		if o == nil || o.LiveCall == nil {
			t.Fatal("no live call")
		}
		return o.LiveCall.Versions
	}
	s.Apply(round("2026-10-06T09:00:01Z", 0, map[string]any{"round_narration": narrated(1), "response": "one"}))
	first := versions()

	s.Apply(round("2026-10-06T09:00:02Z", 1, map[string]any{
		"round_narration": narrated(1), "response": "one",
		"partial_round": map[string]any{"round": float64(2), "content": "writing"},
	}))
	if got := moved(t, first, versions()); !reflect.DeepEqual(got, only()) {
		t.Errorf("a streaming frame moved %v; want nothing moved", got)
	}

	s.Apply(round("2026-10-06T09:00:03Z", 1, map[string]any{"round_narration": narrated(2), "response": "one two"}))
	committed := versions()
	if got := moved(t, first, committed); !reflect.DeepEqual(got, only("narration", "response")) {
		t.Errorf("a committed round moved %v; want the narration and the response", got)
	}

	// The opening frame travels on its own subject and can land last.
	s.Apply(round("2026-10-06T09:00:00Z", -1, map[string]any{
		"prompt": "fix it", "prompt_messages": []any{map[string]any{"role": "system", "content": "lead"}},
	}))
	if got := moved(t, committed, versions()); !reflect.DeepEqual(got, only("prompt")) {
		t.Errorf("the late opening frame moved %v; want the prompt alone", got)
	}
}

// A CALL OF ITS OWN WRITES EVERY FIELD, at a version past every one the
// projection has handed out — so a tab holding any copy of any call, under
// any key, reads the new call's fields as newer than it.
func TestANewCallsVersionsArePastEveryOneHandedOut(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(round("2026-10-06T09:00:01Z", 0, map[string]any{"round_narration": narrated(3)}))
	s.Apply(round("2026-10-06T09:00:02Z", 1, map[string]any{"round_narration": narrated(4)}))
	was := s.AgentOverlay("Lead").LiveCall.Versions
	review := round("2026-10-06T09:00:03Z", 0, map[string]any{"round_narration": narrated(1)})
	review.Payload["phase"] = "review"
	s.Apply(review)
	now := s.AgentOverlay("Lead").LiveCall.Versions
	newest := max(was.Prompt, was.Response, was.Narration, was.Executions, was.Rounds)
	for name, v := range map[string]int{"prompt": now.Prompt, "response": now.Response,
		"narration": now.Narration, "executions": now.Executions, "rounds": now.Rounds} {
		if v <= newest {
			t.Errorf("the next phase's %s is at %d, not past the last call's newest, %d", name, v, newest)
		}
	}
}

// A CALL BUILT AGAIN UNDER THE SAME KEY IS NEWER THAN THE ONE BEFORE IT. A
// suspended Execute phase publishes a completion checkpoint, which clears its
// call, and streams its resumed rounds under the same turn, phase and
// iteration. Counted per call, the rebuilt call's versions began again at one,
// and a tab that missed the push clearing the call and the first push after
// it — the hub drops a slow tab's oldest envelopes — took every later push for
// one overtaken by the copy it held: it kept the call from before the
// suspension and asked for nothing.
//
// Mutation: number a call of its own from one again, and the resumed call's
// versions are below the suspended call's.
func TestACallBuiltAgainUnderItsKeyIsNewerThanTheOneBefore(t *testing.T) {
	t.Parallel()
	for name, restarted := range map[string]bool{
		"its next round first":                  false,
		"the phase opened again, a placeholder": true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := livestate.New()
			for i := range 5 {
				s.Apply(round(fmt.Sprintf("2026-10-06T09:00:0%dZ", i+1), i, map[string]any{
					"round_narration": narrated(i + 1), "response": strings.Repeat("old ", i+1),
					"prompt":          "fix it",
					"tool_executions": []any{map[string]any{"name": "read_file", "round": float64(i + 1)}},
				}))
			}
			suspended := s.AgentOverlay("Lead").LiveCall.Versions
			key := map[string]any{"role": "Lead", "turn_id": "t1", "phase": "execute", "iteration": float64(0)}
			s.Apply(&livestate.Envelope{
				ID: "checkpoint", Type: "agent_phase_completed", Timestamp: "2026-10-06T09:00:06Z",
				Category: "task", Payload: maps.Clone(key),
			})
			if o := s.AgentOverlay("Lead"); o != nil && o.LiveCall != nil {
				t.Fatal("the checkpoint did not clear the call; this case needs it cleared")
			}
			if restarted {
				// The placeholder a phase's start seeds is a call of its
				// own too: it writes every field, empty as they are.
				s.Apply(&livestate.Envelope{
					ID: "started", Type: "agent_phase_started", Timestamp: "2026-10-06T09:04:59Z",
					Category: "task", Payload: maps.Clone(key),
				})
			}
			// The resumed loop's first round, under the same key and newer
			// than the checkpoint.
			s.Apply(round("2026-10-06T09:05:00Z", 5, map[string]any{"round_narration": narrated(1), "response": "new"}))
			resumed := s.AgentOverlay("Lead").LiveCall
			if resumed == nil || resumed.Key() != (livestate.CallKey{TurnID: "t1", Phase: "execute", Iteration: 0}) {
				t.Fatalf("the resumed call is %+v; want it under the suspended call's key", resumed)
			}
			for field, pair := range map[string][2]int{
				"prompt": {suspended.Prompt, resumed.Versions.Prompt}, "response": {suspended.Response, resumed.Versions.Response},
				"narration":  {suspended.Narration, resumed.Versions.Narration},
				"executions": {suspended.Executions, resumed.Versions.Executions},
				"rounds":     {suspended.Rounds, resumed.Versions.Rounds},
			} {
				if pair[1] <= pair[0] {
					t.Errorf("the resumed call's %s is at %d, the suspended call's was %d; want it newer", field, pair[1], pair[0])
				}
			}
		})
	}
}

// A FROZEN CALL'S VERSIONS MOVE WITH WHAT THE FAILURE WROTE INTO IT. A failed
// phase writes its last tool calls and narration into the call it freezes, in
// place — and a tab holding the frame before would otherwise be told nothing
// moved.
func TestAFailureMovesTheVersionsOfWhatItWrote(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(round("2026-10-06T09:00:01Z", 0, map[string]any{"round_narration": narrated(1)}))
	before := s.AgentOverlay("Lead").LiveCall.Versions
	s.Apply(&livestate.Envelope{
		ID: "c1", Type: "agent_phase_completed", Timestamp: "2026-10-06T09:00:05Z", Category: "task",
		Payload: map[string]any{
			"role": "Lead", "turn_id": "t1", "phase": "execute", "iteration": float64(0), "failed": true,
			"error": "the provider died", "round_narration": narrated(2),
			"tool_executions": []any{map[string]any{"name": "read_file", "round": float64(2)}},
		},
	})
	call := s.AgentOverlay("Lead").LiveCall
	if call == nil || !call.Failed {
		t.Fatalf("the failed call was not frozen: %+v", call)
	}
	if got := moved(t, before, call.Versions); !reflect.DeepEqual(got, only("narration", "executions")) {
		t.Errorf("the failure moved %v; want the narration and the tool calls it wrote", got)
	}
}

// A PUSH LEAVES OUT EXACTLY THE FIELDS WHOSE VERSION IS HELD, and carries the
// rest as they are: with nothing held it is the whole call, field for field,
// and with everything held it is the whole call less exactly the fields
// [livestate.CallDetail] names — absent, never empty.
func TestACallLeavesOutExactlyTheDetailItsTabsHold(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(round("2026-10-06T09:00:00Z", -1, map[string]any{
		"prompt": "fix it", "prompt_messages": []any{map[string]any{"role": "system", "content": "lead"}},
	}))
	s.Apply(round("2026-10-06T09:00:01Z", 0, map[string]any{
		"round_narration": narrated(1), "response": "one",
		"tool_executions": []any{map[string]any{"name": "read_file"}},
		"rounds":          []any{map[string]any{"round": float64(1)}},
	}))
	call := s.AgentOverlay("Lead").LiveCall
	decode := func(v any) map[string]any {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	whole := decode(call)
	if nothingHeld := decode(call.Without(livestate.CallVersions{})); !reflect.DeepEqual(nothingHeld, whole) {
		t.Errorf("with nothing held the push is\n%v\nwant the whole call\n%v", nothingHeld, whole)
	}
	lean := decode(call.Without(call.Versions))
	var detail []string
	for _, fields := range livestate.CallDetail {
		detail = append(detail, fields...)
	}
	for key := range whole {
		_, kept := lean[key]
		if left := slices.Contains(detail, key); kept == left {
			t.Errorf("%q: kept %v with everything held; want it left out exactly when CallDetail names it",
				key, kept)
		}
	}
	if _, ok := lean["versions"]; !ok {
		t.Error("a lean push names no versions, so a tab cannot tell what it holds is current")
	}
}

// EVERY VERSION NAMES THE FIELDS THE DASHBOARD MERGES BY IT, both ways. The
// dashboard keeps the copy it holds of a field a push leaves out, keyed by the
// version its `LIVE_CALL_DETAIL` files the field under: a field this engine
// versions that the table does not name is one a push leaves out and the merge
// drops from the screen, and one it names that the engine does not version is
// one the merge waits on for a version that never moves.
func TestTheDashboardMergesEveryVersionedField(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "LIVE_CALL_DETAIL")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	client := map[string][]string{}
	for _, key := range keys {
		fields, err := clientsource.Property(body, key)
		if err != nil {
			t.Fatal(err)
		}
		client[key] = clientsource.Strings(fields)
	}
	if !maps.EqualFunc(client, livestate.CallDetail, slices.Equal[[]string]) {
		t.Errorf("the dashboard merges %v and the engine versions %v — change LIVE_CALL_DETAIL in "+
			"contract/wire.ts to the engine's table", client, livestate.CallDetail)
	}
	// And every version the engine names on the wire is one of them.
	var tags []string
	versions := reflect.TypeFor[livestate.CallVersions]()
	for i := range versions.NumField() {
		tag, _, _ := strings.Cut(versions.Field(i).Tag.Get("json"), ",")
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	if want := slices.Sorted(maps.Keys(livestate.CallDetail)); !slices.Equal(tags, want) {
		t.Errorf("the wire's versions are %v and CallDetail names %v", tags, want)
	}
}
