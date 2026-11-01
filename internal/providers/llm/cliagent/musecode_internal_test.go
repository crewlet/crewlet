package cliagent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// museLine is one envelope of Muse Code's `exec --json` stream.
func museLine(t *testing.T, payloadType string, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"schema_version": 1, "record_type": "event",
		"payload_type": payloadType, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A MUSE CODE RUN IS CHARGED THE COUNTS ITS STREAM REPORTS. Every run prints a
// `run.usage` event before its terminal one, its counts per model: the prompt
// with its cached part in it, and the output with its reasoning in it, beside
// each part's own count (measured on 1.4.13). Read as the whole prompt, the
// cached part is not charged twice; read with the reasoning added, the
// reasoning is.
//
// Mutation: drop the usage paths, and the run is estimated; read the prompt
// as less its cached part, and it is charged 120 tokens more than it spent;
// read the reasoning beside the output, and 21 more.
func TestAMuseCodeRunIsChargedTheCountsItsStreamReports(t *testing.T) {
	t.Parallel()
	p, ok := cliprofile.Builtin("muse-code")
	if !ok {
		t.Fatal("no built-in muse-code profile")
	}
	stdout := strings.Join([]string{
		museLine(t, "run.output.delta", map[string]any{"text": "The build "}),
		museLine(t, "run.output.delta", map[string]any{"text": "is green."}),
		museLine(t, "run.usage", map[string]any{"kind": "run_usage", "calls": 3, "models": []any{
			map[string]any{"model": "a-model", "calls": 2, "input_tokens": 205, "cached_tokens": 80,
				"output_tokens": 24, "reasoning_tokens": 14},
			map[string]any{"model": "another", "calls": 1, "input_tokens": 104, "cached_tokens": 40,
				"output_tokens": 12, "reasoning_tokens": 7},
		}}),
		museLine(t, "run.terminal.completed", map[string]any{"terminal": "completed", "text": "The build is green."}),
	}, "\n")
	got := extract(p, stdout)
	if got.text != "The build is green." {
		t.Errorf("answer = %q, want the terminal event's", got.text)
	}
	if !got.reported {
		t.Fatal("the run's counts were not read, so it would be charged an estimate")
	}
	input := p.Usage.PromptTokens(got.input, got.cacheRead, got.cacheWrite)
	if input != 309 || got.cacheRead != 120 || got.output+got.reasoning != 36 {
		t.Errorf("charged %d in (%d cached) and %d out, want the run's 309 in (120 cached) and 36 out",
			input, got.cacheRead, got.output+got.reasoning)
	}
}
