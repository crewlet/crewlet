package queries_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/iam"
)

// EVERY QUESTION A CURATED TOOL READS IS ONE THIS BUILD ANSWERS, AT ITS POSTURE.
//
// Walked over every question this build registers (every seam wired): a row of
// the catalogue's exposure table naming a question nobody answers is a curated
// tool that fails on every call while looking complete, and a curated read of a
// question only one app opens is offered on exactly that app, so the tool is
// not offered to whom the question refuses. Mutation: point a row at a question
// that is not registered, or offer get_task on the knowledge app, and this
// fails.
func TestEveryCuratedQuestionIsAnswered(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, everySeam(t))
	if names := r.Names(); len(names) < 60 {
		t.Fatalf("the registry answers %d questions, so every seam is not wired", len(names))
	}
	tools := map[string]catalog.Tool{}
	for _, tool := range catalog.Tools() {
		tools[tool.Name] = tool
	}
	rows := 0
	for _, e := range catalog.Exposures() {
		if e.Question == "" {
			continue
		}
		rows++
		posture := r.PostureOf(e.Question)
		if !posture.Valid() {
			t.Errorf("%s reads %s, which this build does not answer", e.Name, e.Question)
			continue
		}
		if g := posture.Grant(); g != "" && e.As == catalog.AsReadTool {
			if apps := tools[e.Name].Apps; len(apps) != 1 || apps[0] != g {
				t.Errorf("%s requires %s and its tool %s is offered on %v", e.Question, g,
					e.Name, apps)
			}
		}
	}
	if rows < 20 {
		t.Fatalf("walked %d curated questions; the exposure table is not being read", rows)
	}
}

// through is a person acting through an assistant read grant.
func through(p iam.Principal) context.Context {
	p.Via, p.AssistScope = iam.AssistName(uuid.New()), iam.AssistScopeRead
	return iam.WithPrincipal(context.Background(), p)
}

// AN ASSISTED QUESTION IS ANSWERED AS ITS PERSON'S OWN, AT EVERY POSTURE.
//
// ADR-0052's parity: a question is a read, and no question answers a
// credential or a secret in clear, so the registry keeps nothing from a
// principal acting through the assistant that it does not keep from the same
// person at their keyboard. Over every question this build registers, each at
// its own posture and answered by a stub, a person holding one app and an
// administrator are each answered or refused through the assistant exactly as
// at the keyboard, with the same reason and the same permissions named. The
// split is real: the one-app person is refused something and answered
// something. Mutation: put a gate on assisted principals back in AnswerWith
// and the assistant is refused what its person reads.
func TestAnAssistedQuestionIsAnsweredAsItsPersonsOwn(t *testing.T) {
	t.Parallel()
	real := queries.NewRegistry()
	queries.Register(real, everySeam(t))
	stubbed := queries.NewRegistry()
	for _, what := range real.Names() {
		stubbed.Register(what, real.PostureOf(what), answersWith("answered"))
	}
	for _, grants := range [][]iam.Grant{{iam.GrantWorklet}, {iam.GrantAdmin}} {
		person := iam.Principal{ID: uuid.New(), Login: "jo.park", Kind: iam.KindPerson,
			Seat: "analyst", Stage: iam.StageActive, Grants: grants}
		keyboard := iam.WithPrincipal(context.Background(), person)
		answered, refused := 0, 0
		for _, what := range stubbed.Names() {
			_, mine := stubbed.AnswerWith(keyboard, what, queries.Params{})
			_, theirs := stubbed.AnswerWith(through(person), what, queries.Params{})
			if !sameRefusal(mine, theirs) {
				t.Errorf("%s for %v answered %v at the keyboard and %v through the assistant",
					what, grants, mine, theirs)
			}
			if mine == nil {
				answered++
			} else {
				refused++
			}
		}
		if answered == 0 || (grants[0] != iam.GrantAdmin && refused == 0) {
			t.Errorf("%v was answered %d and refused %d questions: the walk does not split",
				grants, answered, refused)
		}
	}
}

// sameRefusal reports two answers that say the same thing to a caller.
func sameRefusal(a, b error) bool {
	var x, y *queries.Refusal
	switch {
	case a == nil || b == nil:
		return a == nil && b == nil
	case errors.As(a, &x) && errors.As(b, &y):
		return x.Reason == y.Reason && len(x.Grants) == len(y.Grants)
	}
	return a.Error() == b.Error()
}
