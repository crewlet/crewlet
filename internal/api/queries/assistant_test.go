package queries_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// EVERY QUESTION HAS ONE ASSISTANT EXPOSURE, and the decision can stand.
//
// Walked over every question this build registers (every seam wired), so a
// question registered without a row is a red build that says "classify this
// question for the assistant", and a row naming no question is a decision about
// nothing. A question decided by a verb the authority table keeps from the
// assistant can only be Never, a question the assistant reads as a tool is
// offered only to who its posture admits (its app is the tool's), and an
// administrator's question is read only by an administrator's tool. Mutation:
// register a question with no row, or read `config` as a tool, and this fails.
func TestEveryQuestionHasOneAssistantExposure(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, everySeam(t))
	names := r.Names()
	if len(names) < 60 {
		t.Fatalf("the registry answers %d questions, so every seam is not wired", len(names))
	}
	rows := map[string][]catalog.Exposure{}
	for _, e := range catalog.Exposures() {
		if e.Question != "" {
			rows[e.Question] = append(rows[e.Question], e)
		}
	}
	tools := map[string]catalog.Tool{}
	for _, tool := range catalog.Tools() {
		tools[tool.Name] = tool
	}
	for _, what := range names {
		got := rows[what]
		if len(got) != 1 {
			t.Errorf("%s has %d assistant exposure rows; give it exactly one in "+
				"internal/assistant/catalog", what, len(got))
			continue
		}
		e, posture := got[0], r.PostureOf(what)
		if verb := posture.Verb(); verb != "" {
			if class, _ := authz.AssistOf(verb); class == authz.AssistNever && e.As != catalog.AsNever {
				t.Errorf("%s is decided by %s, which the authority table keeps from the "+
					"assistant, and the catalogue exposes it as %s", what, verb, e.As)
			}
		}
		if e.As != catalog.AsReadTool {
			continue
		}
		if g := posture.Grant(); g != "" {
			if apps := tools[e.Name].Apps; len(apps) != 1 || apps[0] != g {
				t.Errorf("%s requires %s and its tool %s is offered on %v", what, g, e.Name, apps)
			}
		}
	}
	for what := range rows {
		if !r.PostureOf(what).Valid() {
			t.Errorf("the catalogue decides about %s, which this build does not answer", what)
		}
	}
}

// assisted is a person acting through an assistant read grant.
func assisted(grants ...iam.Grant) context.Context {
	grant := uuid.New()
	p := iam.Principal{ID: uuid.New(), Login: "jo.park", Kind: iam.KindPerson, Seat: "analyst",
		Stage: iam.StageActive, Grants: grants, Via: iam.AssistName(grant),
		AssistScope: iam.AssistScopeRead}
	return iam.WithPrincipal(context.Background(), p)
}

// AN ASSISTED QUESTION THE CATALOGUE DOES NOT EXPOSE IS REFUSED, whatever its
// posture.
//
// An Everybody or a Requires question never reaches the authority table, so
// its assistant column cannot keep the assistant from it: the registry's own
// gate does, admitting an assisted principal to exactly the questions the
// catalogue reads and refusing every other `assist_refused`, an administrator
// included. The person at their keyboard is not touched, and a registry
// wired with no gate admits no assisted principal at all. Mutation: drop the
// gate, or let a nil gate admit, and this fails.
func TestAnAssistedQuestionTheCatalogueDoesNotExposeIsRefused(t *testing.T) {
	t.Parallel()
	// EVERY QUESTION AT ITS OWN POSTURE, answered by a stub, behind the gate
	// the API wires: the answers are not what this is about.
	real := queries.NewRegistry()
	queries.Register(real, everySeam(t))
	gated := queries.NewRegistry()
	queries.Register(gated, queries.Sources{AssistReads: catalog.ReadsQuestion})
	for _, what := range real.Names() {
		if !gated.PostureOf(what).Valid() {
			gated.Register(what, real.PostureOf(what), answersWith("answered"))
		}
	}
	ctx := assisted(iam.GrantAdmin)
	refused := 0
	for _, what := range gated.Names() {
		_, err := gated.AnswerWith(ctx, what, queries.Params{})
		var refusal *queries.Refusal
		isRefused := errors.As(err, &refusal) && refusal.Reason == authz.ReasonAssistRefused
		if catalog.ReadsQuestion(what) == isRefused {
			t.Errorf("%s through the assistant: %v (the catalogue reads it: %v)", what, err,
				catalog.ReadsQuestion(what))
		}
		if isRefused {
			refused++
		}
	}
	if refused < 20 {
		t.Fatalf("%d questions were refused the assistant; the gate is not asked", refused)
	}
	keyboard := iam.WithPrincipal(context.Background(), iam.Principal{ID: uuid.New(),
		Login: "jo.park", Kind: iam.KindPerson, Seat: "analyst", Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAdmin}})
	if _, err := gated.AnswerWith(keyboard, "limits", queries.Params{}); isAssistRefusal(err) {
		t.Error("the person at their keyboard was refused as the assistant")
	}
	open := queries.NewRegistry()
	open.Register("budgets", queries.Everybody(), answersWith("answered"))
	if _, err := open.AnswerWith(ctx, "budgets", queries.Params{}); !isAssistRefusal(err) {
		t.Errorf("a registry wired with no gate answered the assistant: %v", err)
	}
}

func isAssistRefusal(err error) bool {
	var refusal *queries.Refusal
	return errors.As(err, &refusal) && refusal.Reason == authz.ReasonAssistRefused
}
