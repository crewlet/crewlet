package assistant_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// EVERY ASSISTANT READ IS FENCED, end to end (ADR-0040).
//
// Every read is read_api's, so a turn reads each kind of route through it
// (a task, the executors, the atlas, a chat room, the audit log) against an
// answer of somebody's words, and what reaches the model is inside a fence
// labelled as an API answer. Asserted at the boundary the model reads.
//
// Mutation: hand the model the answer unfenced (drop the fence in
// catalog.Shape's modelText) and every case fails.
func TestEveryAssistantToolResultIsFenced(t *testing.T) {
	t.Parallel()
	for _, read := range []struct{ route, path string }{
		{"GET /worklet/{id}", "ENG-7"},
		{"GET /executors", ""},
		{"GET /atlas", ""},
		{"GET /chatlet/channels/{channel_id}/messages", "launch"},
		{"GET /iam/audit", ""},
	} {
		t.Run(read.route, func(t *testing.T) {
			t.Parallel()
			args := map[string]any{"route": read.route}
			if read.path != "" {
				_, name, _ := strings.Cut(read.route, "{")
				name, _, _ = strings.Cut(name, "}")
				args["path"] = map[string]any{name: read.path}
			}
			f := assistanttest.NewRig(t, evals.RigOptions{
				Tools: []catalog.Tool{assistanttest.ReadAPI(t, read.route)},
				Handler: func(exec.Request) exec.Response {
					return evals.JSON(map[string]any{"words": "Somebody's words."})
				},
				Rounds: []evals.Round{evals.Call("c1", catalog.ReadAPITool, args), evals.Say("done")}})
			f.Person = evals.Person("grace", "grace", iam.GrantAdmin)
			accepted, err := f.Ask("fence", "Read it", nil)
			if err != nil {
				t.Fatal(err)
			}
			f.Follow(accepted.TurnID, 0)
			var result llm.Message
			for _, m := range f.Model.Seen() {
				if m.Role == llm.RoleTool {
					result = m
				}
			}
			if result.Failed || !strings.Contains(result.Content, "[untrusted:") ||
				!strings.Contains(result.Content, "source=api_answer") {
				t.Errorf("%s reached the model unfenced (failed %v):\n%s", read.route,
					result.Failed, result.Content)
			}
		})
	}
}
