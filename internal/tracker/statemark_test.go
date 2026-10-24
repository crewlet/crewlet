package tracker_test

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// WHAT THE OBJECT A WRITE IS ABOUT REFUSES BY ITS STATE IS MARKED [tracker.ErrState],
// AND WHAT THE REQUEST GOT WRONG [tracker.ErrInvalid] — never both.
//
// The one rule every domain's refusals are classed by (internal/mcp's
// Refusal): a task frozen in the trash, a project archived before work is
// filed into it and a person's pins at their cap each refuse the same request
// until somebody restores, reopens or unpins, so a caller told `invalid` sends
// another argument to an object that refuses every one. A parent in the trash
// is the other kind: the call is about the child, and the fix is another
// parent. Each case is driven through the writer, so a site that marks its
// refusal with the other sentinel goes red here rather than at a surface.
//
// Mutation: mark any one of the state sites back `invalid` (write.go's frozen
// task, sequence.go's archived project, person.go's pins on the set they
// leave) and its row goes red; mark the removed parent `refusedByState` and
// the argument row goes red; drop person.go's request-level pin cap and the
// over-long set reads as the record's state.
func TestAWriteTheObjectsStateRefusesIsMarkedAsItsState(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "frozen")
	filedTask(t, r, "kid")
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "frozen", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	title := "rewritten"
	frozen := "frozen"
	pinned := make([]string, tracker.MaxPinnedViews)
	for i := range pinned {
		pinned[i] = fmt.Sprintf("v-%d", i)
	}
	tooMany := append(append([]string{}, pinned...), "v-over")
	ana := r.writer.As("ana", tracker.AuthorHuman, tracker.Provenance{})

	for _, c := range []struct {
		name      string
		mark, not error
		write     func() error
	}{
		{"an edit of a task frozen in the trash", tracker.ErrState, tracker.ErrInvalid,
			func() error {
				_, err := r.writer.UpdateTask(t.Context(), "op-edit", "frozen", "ENG",
					tracker.NoIfMatch, tracker.TaskPatch{Title: &title},
					tracker.ChangeFields, nil)
				return err
			}},
		{"a child filed under a parent in the trash", tracker.ErrInvalid, tracker.ErrState,
			func() error {
				_, err := r.writer.UpdateTask(t.Context(), "op-reparent", "kid", "ENG",
					tracker.NoIfMatch, tracker.TaskPatch{Parent: &frozen},
					tracker.ChangeReparented, nil)
				return err
			}},
		{"a whole set of pins longer than the list may be", tracker.ErrInvalid,
			tracker.ErrState, func() error {
				_, err := ana.WritePins(t.Context(), "op-pins-set", "ana",
					tracker.PinGesture{Views: tracker.SetChange[string]{Set: &tooMany}},
					tracker.PersonAuthority{})
				return err
			}},
		{"a pin past what the record already holds", tracker.ErrState, tracker.ErrInvalid,
			func() error {
				if _, err := ana.WritePins(t.Context(), "op-pins-fill", "ana",
					tracker.PinGesture{Views: tracker.SetChange[string]{Set: &pinned}},
					tracker.PersonAuthority{}); err != nil {
					t.Fatalf("pin %d views, the most a person pins: %v", len(pinned), err)
				}
				r.drain()
				_, err := ana.WritePins(t.Context(), "op-pins-add", "ana",
					tracker.PinGesture{Views: tracker.SetChange[string]{Add: []string{"v-over"}}},
					tracker.PersonAuthority{})
				return err
			}},
	} {
		err := c.write()
		switch {
		case err == nil:
			t.Errorf("%s was written", c.name)
		case !errors.Is(err, c.mark) || errors.Is(err, c.not):
			t.Errorf("%s was refused %v, want %v and not %v", c.name, err, c.mark, c.not)
		case tracker.Sentence(err) == err.Error():
			t.Errorf("%s reads %q to a person, with the package's prefix", c.name, err)
		case errors.Is(c.mark, tracker.ErrState) && !forAPerson(tracker.Sentence(err)):
			// A STATE IS SHOWN TO THE PERSON WHOSE GESTURE MET IT, in the
			// domain's words: no id, no instant, no aside.
			t.Errorf("%s reads %q, which is not written for the person shown it",
				c.name, tracker.Sentence(err))
		}
	}

	// AND A PROJECT ARCHIVED BEFORE WORK IS FILED INTO IT, last, because
	// it closes the project every row above wrote in.
	// Each NAMES WHO CHANGES IT — the archive's own relation, the project's
	// lead or an administrator — because a state's sentence sends a person to
	// whoever can lift it, and "unarchive it first" sent everybody to do what
	// most of them may not.
	archiveENG(t, r)
	_, err := r.writer.CreateTask(t.Context(), "op-create", newTask("late"), nil)
	if !errors.Is(err, tracker.ErrState) || errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("a create into an archived project was refused %v, want the "+
			"project's state alone", err)
	}
	files := tracker.AcceptFiles("ENG", true)
	if !errors.Is(files, tracker.ErrState) || errors.Is(files, tracker.ErrInvalid) {
		t.Errorf("a file into an archived project is refused %v, want the "+
			"project's state alone", files)
	}
	for _, refused := range []error{err, files} {
		if said := tracker.Sentence(refused); !forAPerson(said) ||
			!strings.Contains(said, "Its lead or an administrator can unarchive it") {
			t.Errorf("an archived project reads %q, which does not name who "+
				"unarchives it", said)
		}
	}
}

// forAPerson is whether a refusal's sentence reads for the person a screen
// shows it to: no em dash (an aside a short sentence does not need), no RFC
// 3339 instant and no task id — a uuid, or this test's own `frozen`, the id
// its trashed task is filed under — neither of which a person beside the task
// acts on.
func forAPerson(said string) bool {
	return !strings.Contains(said, "—") && !rfc3339.MatchString(said) &&
		!uuidLike.MatchString(said) && !strings.Contains(said, "frozen")
}

var (
	rfc3339  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)
	uuidLike = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-`)
)
