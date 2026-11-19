package worklet

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/named"
)

// A TRACKER WAKE'S SUBJECT NAMES ITS TASK (package named): by its id, which a
// move keeps, spelled as the address the line already shows (its key, or its
// id where another task claimed the key). A subject about no task names none.
func TestATrackerWakesSubjectNamesItsTask(t *testing.T) {
	t.Parallel()
	task := Subject{Kind: KindTask, ID: "0198-task"}
	cases := []struct {
		name     string
		snapshot Snapshot
		subject  Subject
		text     string
		refs     []named.Ref
	}{{
		name:     "a task by its key",
		snapshot: Snapshot{Key: "ENG-287", Title: "Fix the login redirect"},
		subject:  task,
		text:     "ENG-287 comment: Fix the login redirect",
		refs:     []named.Ref{{Kind: named.WorkItem, ID: "0198-task", Text: "ENG-287"}},
	}, {
		name:     "a task whose key another claimed, by its id",
		snapshot: Snapshot{Key: "ENG-287", KeyCollision: true},
		subject:  task,
		text:     "0198-task: comment",
		refs:     []named.Ref{{Kind: named.WorkItem, ID: "0198-task", Text: "0198-task"}},
	}, {
		name:    "a person's priorities name nothing",
		subject: Subject{Kind: KindPerson, ID: "ana"},
		text:    "your priorities: comment",
	}}
	for _, c := range cases {
		got := subjectLine(c.snapshot, c.subject, ChangeComment)
		if got.Text != c.text || !reflect.DeepEqual(got.Refs, c.refs) {
			t.Errorf("%s: subject %q naming %+v, want %q naming %+v", c.name, got.Text, got.Refs, c.text, c.refs)
		}
	}
}
