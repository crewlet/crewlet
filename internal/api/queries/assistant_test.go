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

// EVERY QUESTION A CURATED WRITE'S PREVIEW READS IS ONE THIS BUILD ANSWERS.
//
// Walked over every question this build registers (every seam wired): a row of
// the catalogue's exposure table naming a question nobody answers is a write
// whose preview fails on every call while looking complete. Mutation: point a
// row at a question that is not registered, and this fails.
func TestEveryCuratedQuestionIsAnswered(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, everySeam(t))
	if names := r.Names(); len(names) < 60 {
		t.Fatalf("the registry answers %d questions, so every seam is not wired", len(names))
	}
	rows := 0
	for _, e := range catalog.Exposures() {
		if e.Question == "" {
			continue
		}
		rows++
		if !r.PostureOf(e.Question).Valid() {
			t.Errorf("%s reads %s, which this build does not answer", e.Name, e.Question)
		}
	}
	if rows < 10 {
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
// ADR-0052's parity: a question is a read, and none answers a credential or a
// secret in clear, so the registry keeps nothing from a principal acting
// through the assistant that it keeps from the same person at their keyboard.
// The one exception is code_change's viewed_key, withheld from an assistant
// grant (codechange.go): its conversation would keep it past the repository's
// purge. Over every registered question, a person holding one app and an
// administrator are answered or refused through the assistant exactly as at
// the keyboard, with the same reason and permissions named. Mutation: put a
// gate on assisted principals back in AnswerWith and the assistant is refused
// what its person reads.
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
