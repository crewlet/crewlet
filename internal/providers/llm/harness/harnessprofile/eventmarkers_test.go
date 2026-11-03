package harnessprofile

import (
	"slices"
	"strings"
	"testing"
)

// eventScope is a custom profile that reads a jsonl stream with a
// discriminator, plus the events marker scope's fields, with extra merged in.
func eventScope(extra map[string]any) map[string]any {
	base := map[string]any{
		"event_type_path": []any{"type"},
		"text_events":     []any{"text"},
		"marker_scope":    "events",
		"marker_events":   []any{"error"},
		"marker_path":     []any{"error", "type"},
		"limit_markers":   []any{map[string]any{"sentinel": "provider.quota"}},
	}
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// THE EVENTS SCOPE IS ONLY SAFE WHOLE. It compares a sentinel with the class in
// the harness's own failure event, and every part of that sentence is a field: a
// stream to have events in, the discriminator that tells the harness's failure
// event from one carrying the model's words, which events those are, and where
// the class is in one. A profile missing any of them would match nothing — a
// spent plan never recognised as one, so the entry never sits out, silently —
// or, on a text profile, match against the model's own reply.
func TestTheEventsMarkerScopeIsRefusedUnlessWhole(t *testing.T) {
	t.Parallel()
	if _, err := loadCustom(eventScope(nil)); err != nil {
		t.Fatalf("a whole events-scope profile was refused: %v", err)
	}
	for name, tc := range map[string]struct {
		extra map[string]any
		want  string
	}{
		"a text profile": {map[string]any{
			"output": "text", "event_type_path": []any{}, "text_events": []any{},
		}, `output is "text"`},
		"a single json document": {map[string]any{
			"output": "json", "event_type_path": []any{}, "text_events": []any{},
		}, `output is "json"`},
		"no discriminator": {map[string]any{
			"event_type_path": []any{}, "text_events": []any{},
		}, "event_type_path is empty"},
		"no failure events": {map[string]any{"marker_events": []any{}}, "marker_events is empty"},
		"no class path":     {map[string]any{"marker_path": []any{}}, "marker_path is empty"},
		"an answer event named a failure": {
			map[string]any{"marker_events": []any{"error", "text"}}, `both name "text"`,
		},
		"a reset read off a whole class": {map[string]any{"limit_markers": []any{map[string]any{
			"sentinel": "provider.quota", "reset_separator": "|", "reset_unit": "epoch",
		}}}, "reset_separator"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			refused(t, eventScope(tc.extra), tc.want)
		})
	}
}

// The scope's fields say where IT looks. Beside any other scope they would be
// read by nothing, and the markers matched as substrings of the answer and
// stderr while the profile appeared to say otherwise.
func TestMarkerEventsAreRefusedOutsideTheEventsScope(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"", "answer-and-stderr", "stderr"} {
		for _, extra := range []map[string]any{
			{"marker_scope": scope},
			{"marker_scope": scope, "marker_events": []any{}},
			{"marker_scope": scope, "marker_path": []any{}},
		} {
			refused(t, eventScope(extra), "marker_events or marker_path is set")
		}
	}
}

// The shipped profile turned into a text one is refused rather than left to
// match its classes against what is then the model's own reply.
func TestTheOpenCodeProfileTurnedTextIsRefused(t *testing.T) {
	t.Parallel()
	_, err := Load("opencode", map[string]any{"output": "text"})
	if err == nil || !strings.Contains(err.Error(), "marker_scope is events but output is \"text\"") {
		t.Fatalf("err = %v, want a refusal naming marker_scope and the text output", err)
	}
}

// THE SHIPPED OPENCODE PROFILE MATCHES THE CLASS, NOT THE BYTES. Its sentinels
// are the bare classes 2.0 names in `error.type`, compared by equality inside
// its `error` events — never the quoted spellings the first version of this
// scope searched stdout for, which a model's tool-call arguments reproduce
// verbatim.
func TestTheOpenCodeProfileComparesFailureClasses(t *testing.T) {
	t.Parallel()
	p, err := Load("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.EffectiveMarkerScope() != MarkerScopeEvents {
		t.Fatalf("marker_scope = %q, want %q", p.EffectiveMarkerScope(), MarkerScopeEvents)
	}
	if !slices.Equal(p.MarkerEvents, []string{"error"}) || p.MarkerPath.String() != "error.type" {
		t.Errorf("marker_events %v, marker_path %s — want [error] and error.type",
			p.MarkerEvents, p.MarkerPath)
	}
	var sentinels []string
	for _, m := range p.LimitMarkers {
		sentinels = append(sentinels, m.Sentinel)
	}
	for _, m := range p.AuthMarkers {
		sentinels = append(sentinels, m.Sentinel)
	}
	if want := []string{"provider.quota", "provider.rate-limit", "provider.auth"}; !slices.Equal(sentinels, want) {
		t.Errorf("sentinels = %q, want %q", sentinels, want)
	}
}
