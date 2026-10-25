package learning

import (
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// promptKey is a JSON key a prompt asks the model to answer with.
var promptKey = regexp.MustCompile(`"([a-z_]+)"\s*:`)

// jsonKeys is the keys a decoder struct reads, off its own tags, so the
// expectation moves with the decoder rather than with a list beside it.
func jsonKeys(v any) map[string]bool {
	keys := map[string]bool{}
	ty := reflect.TypeOf(v)
	for i := range ty.NumField() {
		if tag, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ","); tag != "" {
			keys[tag] = true
		}
	}
	return keys
}

// EVERY PASS ASKS FOR EXACTLY THE KEYS ITS DECODER READS.
//
// A key a prompt asks for and nothing reads is output paid for on every
// answer — the persist classifier asked for a `rationale` on every DOC that no
// line of the engine ever looked at — and a key the decoder reads that the
// prompt never names is a field that is always empty. The three drafting
// prompts share one answer shape because they share one decoder, and this is
// what holds each of them to it.
func TestEveryPassAsksForExactlyTheKeysItsDecoderReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, prompt string
		read         map[string]bool
		// decline is how the prompt says "nothing", where the pass has
		// one: an answer the parser reads as no draft at all.
		decline string
	}{
		{"persist", PersistSystemPrompt,
			map[string]bool{"kind": true, "content": true, "target_hint": true, "ttl_days": true}, ""},
		{"synthesis", SynthesisSystemPrompt, jsonKeys(skillDraft{}), "{}"},
		{"cluster", ClusterSystemPrompt, jsonKeys(skillDraft{}), "{}"},
		{"promotion", PromotionSystemPrompt, jsonKeys(skillDraft{}), "{}"},
		{"refinement", RefinementSystemPrompt, jsonKeys(refinementChoice{}), "{}"},
		{"compactor", CompactorSystemPrompt, map[string]bool{
			"common_task_pattern": true, "common_outcome": true,
			"subjects_involved": true, "notable_patterns": true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			asked := map[string]bool{}
			for _, m := range promptKey.FindAllStringSubmatch(tc.prompt, -1) {
				asked[m[1]] = true
			}
			if !maps.Equal(asked, tc.read) {
				t.Errorf("the prompt asks for %v; its decoder reads %v",
					slices.Sorted(maps.Keys(asked)), slices.Sorted(maps.Keys(tc.read)))
			}
			flat := strings.ToLower(strings.Join(strings.Fields(tc.prompt), " "))
			if tc.decline != "" && !strings.Contains(flat, "answer exactly "+tc.decline) {
				t.Errorf("the prompt never says that %s is the answer for nothing", tc.decline)
			}
		})
	}
}
