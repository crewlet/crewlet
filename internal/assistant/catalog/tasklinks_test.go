package catalog_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A TASK IS READ WITH WHAT IT BELONGS TO AND WHAT IT IS LINKED TO. GET
// /worklet/{id} answers the parent and both directions of every link beside
// the task, and the model reads them from the card: the parent by its key,
// title and status, and each link by its other end's key, title and status,
// the relation, and which end holds it, a page's by its id. Carded from the
// task alone, the model could not say what a task waits on or belongs to.
// Mutation: card the task's record alone, or drop a link's facts, and this
// fails.
func TestATaskIsReadWithWhatItBelongsToAndIsLinkedTo(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(map[string]any{
		"task": map[string]any{"id": "7d0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f", "key": "ENG-41",
			"title": "Serve versioned docs pages", "status": "in_progress"},
		"parent": map[string]any{"id": "p-1", "key": "ENG-30", "title": "Docs site 2.0",
			"status": "in_progress"},
		"links": []any{
			map[string]any{"kind": "waiting_on", "other": "t-38", "key": "ENG-38",
				"title": "Backend release", "status": "in_review"},
			map[string]any{"kind": "waiting_on", "other": "t-44", "key": "ENG-44",
				"title": "Edge rule clean-up", "status": "todo", "derived": true},
			map[string]any{"kind": "page", "other": "f3a1", "note": "The routing runbook"},
		},
	})
	res, err := catalog.Shape(mustTool(t, "get_task"), map[string]any{"item": "ENG-41"},
		exec.Response{Status: http.StatusOK, Body: body}, provenance.NewBoundary(),
		evals.Epoch, cards.Options{})
	if err != nil {
		t.Fatal(err)
	}
	read := modelRead(t, res.Model)
	for _, want := range []string{
		`"Parent":"ENG-30"`, `"Parent's title":"Docs site 2.0"`, `"Parent's status":"in_progress"`,
		`"id":"t-38","label":"Backend release","fact":"ENG-38"`, `"Relation":"waiting_on"`,
		`"Status":"in_review"`,
		`"Held by":"the other end, so it reads from there to this task"`,
		`"id":"f3a1","label":"page"`, `"Note":"The routing runbook"`,
	} {
		if !strings.Contains(read, want) {
			t.Errorf("get_task hands the model no %s: %s", want, read)
		}
	}
	if strings.Count(read, `"Held by"`) != 1 {
		t.Errorf("only the link the other task holds says so: %s", read)
	}
	if c := res.Cards[0].Entity; c.Key != "ENG-41" || c.Title != "Serve versioned docs pages" ||
		c.Open == nil {
		t.Errorf("the task's card is headed %q %q opening %+v, want the task itself", c.Key,
			c.Title, c.Open)
	}
}
