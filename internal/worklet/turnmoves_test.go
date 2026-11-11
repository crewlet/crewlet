package worklet_test

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A TURN MOVED WORK ONLY WHERE IT WROTE MORE THAN WORDS: a turn that only
// commented moved nothing, one that changed a status moved it, and a turn that
// wrote nothing, or one this tracker never saw, is not in the answer.
func TestATurnMovedWorkOnlyWhereItWroteMoreThanWords(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("the health check")
	words := aTurnNobodyWoke()
	words.TurnID = "turn-words"
	talker := r.writer.As("sre", worklet.AuthorAgent, words)
	if _, err := talker.UpdateTask(t.Context(), "op-1", task.ID, "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Comment: &worklet.Comment{Body: "All green."}}, worklet.ChangeComment, nil); err != nil {
		t.Fatal(err)
	}
	r.drain()
	moved := aTurnNobodyWoke()
	moved.TurnID = "turn-moved"
	mover := r.writer.As("sre", worklet.AuthorAgent, moved)
	done := worklet.StatusDone
	if _, err := mover.UpdateTask(t.Context(), "op-2", task.ID, "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
		t.Fatal(err)
	}
	r.drain()
	got, err := r.reader.TurnsMoved(t.Context(), worklet.TurnsMovedQuery{
		Turns: []string{"turn-words", "turn-moved", "turn-idle"}, Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"turn-moved": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("moved = %v, want only the turn that changed a status", got)
	}
}
