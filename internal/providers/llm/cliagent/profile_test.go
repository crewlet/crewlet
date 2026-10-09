package cliagent

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// A CLI with no model flag cannot honour a model, and silently ignoring one
// would run every phase on the CLI's default while the config said otherwise.
func TestAProfileWithoutAModelFlagRefusesAModel(t *testing.T) {
	t.Parallel()
	_, err := New(Config{
		Key: "sub", Model: "sonnet", Agent: "custom", StateDir: t.TempDir(),
		Timeout: 1, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "x", "complete_args": []any{"-p"},
			"output": "text", "model_args": []any{},
		},
	})
	if err == nil {
		t.Fatal("a model was accepted by a profile with no model flag")
	}
	if !strings.Contains(err.Error(), "model_args") {
		t.Errorf("error %q does not name the field to declare", err)
	}
}

// `crewlet llm doctor` exists to settle one question — are these token counts
// the vendor's or this engine's guess — so the predicate behind it has to ask
// what the EXTRACTOR asks, not a narrower question that happens to agree on
// the built-in profiles.
func TestReadsUsageAgreesWithWhatTheExtractorActuallyReads(t *testing.T) {
	t.Parallel()
	usage := cliprofile.UsagePaths{
		Input:  []cliprofile.Path{{"usage", "input_tokens"}},
		Output: []cliprofile.Path{{"usage", "output_tokens"}},
	}
	for _, tc := range []struct {
		name string
		p    cliprofile.Profile
		want bool
	}{
		{"json with usage paths", cliprofile.Profile{Output: cliprofile.OutputJSON, TextPaths: []cliprofile.Path{{"result"}}, Usage: usage}, true},
		{"jsonl with usage paths", cliprofile.Profile{Output: cliprofile.OutputJSONL, TextPaths: []cliprofile.Path{{"result"}}, Usage: usage}, true},
		{"json with none", cliprofile.Profile{Output: cliprofile.OutputJSON, TextPaths: []cliprofile.Path{{"result"}}}, false},
		// The one that was wrong: `output: text` is a one-line override,
		// and the JSON paths it inherits are then walked by nothing —
		// extract never decodes a document in that mode.
		{"text, inheriting a json profile's usage paths", cliprofile.Profile{Output: cliprofile.OutputText, Usage: usage}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.p.ReadsUsage(); got != tc.want {
				t.Errorf("ReadsUsage() = %v, want %v", got, tc.want)
			}
			// The claim is only worth anything if it matches reality.
			out := extract(tc.p, `{"result":"hi","usage":{"input_tokens":3,"output_tokens":4}}`)
			if out.reported != tc.p.ReadsUsage() {
				t.Errorf("ReadsUsage() = %v but a real extraction reported = %v",
					tc.p.ReadsUsage(), out.reported)
			}
		})
	}
}
