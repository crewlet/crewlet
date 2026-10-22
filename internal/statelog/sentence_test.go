package statelog_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TWO REFUSALS THAT MEAN ONE THING ARE TOLD IN WORDS FOR A PERSON,
// however a domain wrapped them — and nothing else is.
//
// The framework's own text for each names the subject it met, a broker path;
// a domain's wrap adds its own sentinel in front. Both are the same refusal,
// and a surface asking [statelog.Sentence] about either is handed the same
// words, with no package prefix and no subject in them. A condition of the node
// is not one of the two, and a surface told it was would tell a person a lost
// race where the node could not serve.
//
// Mutation: drop any arm of [statelog.Sentence] and its rows go red; answer
// the error's own text and every row carries the subject.
func TestTheFrameworksRefusalsAreToldInWordsForAPerson(t *testing.T) {
	t.Parallel()
	wrapped := errors.New("doclet: the page kept changing under this write")
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"a lost race", fmt.Errorf("%w: crewlet.iam.directory kept changing under "+
			"this write", statelog.ErrConflict), statelog.LostRace},
		{"a lost race a domain wrapped", fmt.Errorf("%w: crewlet.pages.page.p-1: %w",
			wrapped, statelog.ErrConflict), statelog.LostRace},
		{"a create of what is there", fmt.Errorf("%w: crewlet.tracker.task.t-1",
			statelog.ErrExists), statelog.AlreadyThere},
	} {
		said, ok := statelog.Sentence(c.err)
		if !ok || said != c.want {
			t.Errorf("%s is told as %q (%v), want %q", c.name, said, ok, c.want)
		}
		for _, leak := range []string{"statelog", "crewlet.", "doclet:", "—"} {
			if strings.Contains(said, leak) {
				t.Errorf("%s is told with %q in it: %s", c.name, leak, said)
			}
		}
	}
	for _, err := range []error{
		nil,
		errors.New("disk I/O error"),
		&statelog.Unavailable{Reason: statelog.ReasonBehind, Detail: "behind"},
		&statelog.Unavailable{Reason: statelog.ReasonDeleted, Detail: "deleted"},
		&statelog.Unavailable{Reason: statelog.ReasonOpReused, Detail: "reused"},
		&statelog.Refused{Code: statelog.RefuseBehind},
	} {
		if said, ok := statelog.Sentence(err); ok {
			t.Errorf("%v is told as one of the two: %q", err, said)
		}
	}
}
