package livestate_test

import (
	"encoding/json"
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
	if first.Narration != 1 || first.Response != 1 || first.Prompt != 0 || first.Executions != 0 {
		t.Fatalf("after the first round = %+v; want narration and response at 1, nothing else written", first)
	}

	s.Apply(round("2026-10-06T09:00:02Z", 1, map[string]any{
		"round_narration": narrated(1), "response": "one",
		"partial_round": map[string]any{"round": float64(2), "content": "writing"},
	}))
	if got := versions(); got != first {
		t.Errorf("a streaming frame moved the versions %+v -> %+v; want them where they were", first, got)
	}

	s.Apply(round("2026-10-06T09:00:03Z", 1, map[string]any{"round_narration": narrated(2), "response": "one two"}))
	if got := versions(); got.Narration != 2 || got.Response != 2 || got.Prompt != 0 {
		t.Errorf("a committed round = %+v; want narration and response at 2", got)
	}

	// The opening frame travels on its own subject and can land last.
	s.Apply(round("2026-10-06T09:00:00Z", -1, map[string]any{
		"prompt": "fix it", "prompt_messages": []any{map[string]any{"role": "system", "content": "lead"}},
	}))
	if got := versions(); got.Prompt != 1 || got.Narration != 2 {
		t.Errorf("the late opening frame = %+v; want the prompt at 1 and the rest unmoved", got)
	}
}

// A CALL OF ITS OWN STARTS FROM NOTHING: a version is a fact about one call,
// so the next phase's first frame numbers its fields afresh rather than going
// on from the last phase's.
func TestANewCallsVersionsStartFromNothing(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(round("2026-10-06T09:00:01Z", 0, map[string]any{"round_narration": narrated(3)}))
	s.Apply(round("2026-10-06T09:00:02Z", 1, map[string]any{"round_narration": narrated(4)}))
	review := round("2026-10-06T09:00:03Z", 0, map[string]any{"round_narration": narrated(1)})
	review.Payload["phase"] = "review"
	s.Apply(review)
	if got := s.AgentOverlay("Lead").LiveCall.Versions; got.Narration != 1 {
		t.Errorf("the next phase's first frame = %+v; want its narration at 1", got)
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
	if call.Versions.Narration != 2 || call.Versions.Executions != 1 {
		t.Errorf("the frozen call's versions = %+v; want narration 2 and executions 1", call.Versions)
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
