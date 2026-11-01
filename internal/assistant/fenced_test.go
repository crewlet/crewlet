package assistant_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// EVERY ASSISTANT TOOL RESULT IS FENCED, end to end (ADR-0040).
//
// For every read tool in the catalogue whose answer carries text somebody
// else wrote, a turn calls it through the Surface against an answer shaped
// like the route's own (built from the tool's card spec, every field a
// string), and what reaches the model's messages is inside a fence labelled
// with the tool's source. Asserted at the boundary the model reads, so it
// holds whichever package does the fencing.
//
// Mutation: hand the model the summary unfenced (drop the fence in
// catalog.Shape's modelText) and every case fails.
func TestEveryAssistantToolResultIsFenced(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindRead || !tool.Fence {
			continue
		}
		checked++
		t.Run(tool.Name, func(t *testing.T) {
			t.Parallel()
			spec, ok := tool.CardFor(catalog.Args(requiredArgs(tool)))
			if !ok {
				t.Fatalf("%s cards no answer for its required arguments", tool.Name)
			}
			answer := answerFor(spec)
			f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{tool},
				Handler: func(exec.Request) exec.Response { return evals.JSON(answer) },
				Rounds: []evals.Round{
					evals.Call("c1", tool.Name, requiredArgs(tool)),
					evals.Say("done")}})
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
				!strings.Contains(result.Content, "source="+string(tool.Source)) {
				t.Errorf("%s reached the model unfenced (failed %v):\n%s", tool.Name,
					result.Failed, result.Content)
			}
		})
	}
	if checked < 5 {
		t.Fatalf("checked %d fenced read tools, so the walk reads nothing it guards", checked)
	}
}

// requiredArgs is a call naming every argument the tool requires, each with a
// value its schema admits.
func requiredArgs(tool catalog.Tool) map[string]any {
	args := map[string]any{}
	props, _ := tool.Parameters["properties"].(map[string]any)
	required, _ := tool.Parameters["required"].([]string)
	for _, name := range required {
		prop, _ := props[name].(map[string]any)
		switch {
		case prop["enum"] != nil:
			args[name] = firstOf(prop["enum"])
		case prop["type"] == "integer" || prop["type"] == "number":
			args[name] = 1
		default:
			args[name] = "ENG-7"
		}
	}
	return args
}

// firstOf is an enum's first value.
func firstOf(enum any) any {
	raw, _ := json.Marshal(enum)
	var values []any
	_ = json.Unmarshal(raw, &values)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// answerFor is an answer the spec cards: every path it reads holds text.
func answerFor(spec cards.Spec) any {
	root := map[string]any{}
	switch {
	case spec.Table != nil:
		row := map[string]any{}
		for _, c := range spec.Table.Columns {
			set(row, c.Path, "written by somebody")
		}
		if spec.Table.Rows == "" {
			return []any{row}
		}
		set(root, spec.Table.Rows, []any{row})
	case spec.Entity != nil:
		record := map[string]any{}
		for _, path := range []string{spec.Entity.ID, spec.Entity.Key, spec.Entity.Title,
			spec.Entity.Subtitle, spec.Entity.Text} {
			set(record, path, "written by somebody")
		}
		for _, fact := range spec.Entity.Facts {
			set(record, fact.Path, "written by somebody")
		}
		if spec.Entity.Root == "" {
			root = record
		} else {
			set(root, spec.Entity.Root, record)
		}
		if items := spec.Entity.Items; items != nil {
			item := map[string]any{}
			for _, path := range []string{items.ID, items.Label, items.Fact} {
				set(item, path, "written by somebody")
			}
			set(root, items.Path, []any{item})
		}
	}
	return root
}

// set puts v at a dotted path in m, making each map on the way.
func set(m map[string]any, path string, v any) {
	if path == "" {
		return
	}
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}
