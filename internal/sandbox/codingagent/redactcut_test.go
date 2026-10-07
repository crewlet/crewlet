package codingagent_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// The two credential shapes a coding run's environment most often carries,
// each with a body of distinct characters so a surviving fragment is
// unambiguous.
const (
	cutGitHubBody = "Q7wErTy9UiOp3AsDfGh5JkLzXcVbNm1PqRsT" // 36, the rule's floor
	cutGitHub     = "ghp_" + cutGitHubBody
	cutAnthropic  = "sk-ant-api03-" + "Zx8Cv7Bn6Mq5Wr4Et3Yu2Io1PaSdFgHjKlLm"
)

// A CREDENTIAL IS REDACTED BEFORE A PREVIEW IS CUT, at every place a
// transcript line previews a value: a tool call's subject, a failed call's
// error and a run's ending, for both CLIs. The transcript is redacted again
// downstream, but by then the cut has been made — a token whose start fell
// just before the limit survived as `ghp_` and most of its body, shorter than
// the rule's length floor, so no later pass could see it, and it was stored
// on the run's record and shown on the live view.
//
// Every offset that puts the token across the cut is tried, and NO run of the
// token's body long enough to identify it may survive.
func TestACredentialIsRedactedBeforeATranscriptLineIsCut(t *testing.T) {
	t.Parallel()
	sites := []struct {
		name   string
		events func() codingagent.Decoder
		line   func(value string) string
	}{
		{"claude tool subject", claude().Events, func(v string) string {
			return claudeLine(map[string]any{"type": "assistant", "message": map[string]any{
				"content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "Bash",
					"input": map[string]any{"command": v}}}}})
		}},
		{"claude tool error", claude().Events, func(v string) string {
			return claudeLine(map[string]any{"type": "user", "message": map[string]any{
				"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1",
					"is_error": true, "content": v}}}})
		}},
		{"claude run ending", claude().Events, func(v string) string {
			return claudeLine(map[string]any{"type": "result", "subtype": "error_during_execution",
				"is_error": true, "errors": []string{v}})
		}},
		{"opencode tool subject", opencode().Events, func(v string) string {
			return claudeLine(map[string]any{"type": "tool_use", "part": map[string]any{"tool": "bash",
				"state": map[string]any{"status": "completed", "input": map[string]any{"command": v}}}})
		}},
		{"opencode tool error", opencode().Events, func(v string) string {
			return claudeLine(map[string]any{"type": "tool_use", "part": map[string]any{"tool": "bash",
				"state": map[string]any{"status": "error", "error": v,
					"input": map[string]any{"command": "git push"}}}})
		}},
	}
	for _, site := range sites {
		for _, token := range []string{cutGitHub, cutAnthropic} {
			body := token[strings.LastIndexAny(token, "_-")+1:]
			for pad := 60; pad <= 200; pad++ {
				value := strings.Repeat("x", pad) + " https://x-access-token:" + token + "@github.com/acme/api"
				dec := site.events()
				dec.Line([]byte(site.line(value)))
				// As every reader of a transcript sees it: after the
				// downstream pass, which a fragment slips through.
				got := redact.Secrets(strings.Join(dec.Entries(), "\n"))
				if got == "" {
					t.Fatalf("%s: the decoder showed nothing for the line", site.name)
				}
				for i := 0; i+8 <= len(body); i++ {
					if strings.Contains(got, body[i:i+8]) {
						t.Fatalf("%s, %d bytes before the token: a fragment of it survived the cut: %q",
							site.name, pad, got)
					}
				}
			}
		}
	}
}

// claudeLine is one stream line: a JSON object, encoded as a CLI writes it.
func claudeLine(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}
