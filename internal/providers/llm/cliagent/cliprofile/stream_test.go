package cliprofile

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A STREAMED CALL RUNS THE SAME CLI, READ THE SAME WAY. Claude Code's variant
// appends its arguments to the argv the call already runs (the seats' and,
// with web denied, the assistant's) and changes nothing about where the
// answer, the failure flag and the token counts are read; it only names the
// event they are read from and where the fragments are.
func TestAStreamedCallKeepsEveryArgumentAndReadsTheSameAnswer(t *testing.T) {
	t.Parallel()
	for _, overrides := range []map[string]any{nil, {"web_tools": "denied"}} {
		p, err := Load("claude-code", overrides)
		if err != nil {
			t.Fatal(err)
		}
		s := p.Streamed()
		want := append(slices.Clone(p.CompleteArgs),
			"--output-format", "stream-json", "--verbose", "--include-partial-messages")
		if !slices.Equal(s.CompleteArgs, want) {
			t.Errorf("overrides %v: streamed argv %q, want %q", overrides, s.CompleteArgs, want)
		}
		if s.EffectiveOutput() != OutputJSONL || p.EffectiveOutput() != OutputJSON {
			t.Errorf("output %q streamed and %q as written, want jsonl and json", s.EffectiveOutput(), p.EffectiveOutput())
		}
		if !reflect.DeepEqual(s.TextPaths, p.TextPaths) || !reflect.DeepEqual(s.ErrorPaths, p.ErrorPaths) ||
			!reflect.DeepEqual(s.Usage, p.Usage) {
			t.Error("the streamed variant reads the answer, the failure flag or the counts from elsewhere")
		}
		if !slices.Equal(s.TextEvents, []string{"result"}) || len(s.DeltaPaths) != 1 {
			t.Errorf("text_events %q, delta_paths %v: the answer must come from the result event alone",
				s.TextEvents, s.DeltaPaths)
		}
		if err := s.Validate("claude-code"); err != nil {
			t.Errorf("the streamed profile does not validate: %v", err)
		}
		if again := s.Streamed(); !slices.Equal(again.CompleteArgs, s.CompleteArgs) {
			t.Errorf("streaming a streamed profile appended its arguments twice: %q", again.CompleteArgs)
		}
	}
}

// A PROFILE THAT ALREADY STREAMS, OR CANNOT, HAS NO VARIANT. Codex and the
// other jsonl CLIs stream every call as written, and every other built-in
// either has no streaming mode or has not been measured.
func TestOnlyClaudeCodeShipsAStreamedVariant(t *testing.T) {
	t.Parallel()
	for _, name := range BuiltinNames() {
		p, _ := Builtin(name)
		if got, want := p.CanStream(), name == "claude-code"; got != want {
			t.Errorf("%s: CanStream() = %v, want %v", name, got, want)
		}
		if !p.CanStream() {
			if s := p.Streamed(); !reflect.DeepEqual(s, p) {
				t.Errorf("%s: Streamed() changed a profile with no variant", name)
			}
		}
	}
}

// A VARIANT THAT CANNOT WORK IS REFUSED WHERE IT IS WRITTEN, naming the
// field, rather than on the first call that streams.
func TestAStreamVariantThatCannotWorkIsRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, agent string
		overrides   map[string]any
		want        string
	}{
		{"already a stream", "codex", map[string]any{"stream": map[string]any{
			"args": []any{"--x"}, "delta_paths": []any{[]any{"t"}}}}, "already jsonl"},
		{"no arguments", "claude-code", map[string]any{"stream": map[string]any{"args": []any{}}}, "stream.args is empty"},
		{"no fragment path", "claude-code", map[string]any{"stream": map[string]any{"delta_paths": []any{}}},
			"stream.delta_paths is empty"},
		{"an invalid streamed profile", "claude-code", map[string]any{"stream": map[string]any{"text_events": []any{}}},
			"with the stream applied, event_type_path is set but text_events is empty"},
		{"fragments on a json profile", "claude-code", map[string]any{"delta_paths": []any{[]any{"text"}}},
			"delta_paths or delta_events is set but output is \"json\""},
		{"fragment events with no path", "codex", map[string]any{"event_type_path": []any{"type"},
			"text_events": []any{"item.completed"}, "delta_events": []any{"item.delta"}}, "delta_paths is empty"},
		{"fragment events with no discriminator", "codex", map[string]any{
			"event_type_path": []any{}, "text_events": []any{},
			"delta_events": []any{"item.delta"}, "delta_paths": []any{[]any{"delta"}}}, "event_type_path is empty"},
	}
	for _, c := range cases {
		_, err := Load(c.agent, c.overrides)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %q", c.name, err, c.want)
		}
	}
}
