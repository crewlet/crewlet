package queries_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// A READ IS OFFERED EXACTLY WHEN ITS QUESTION ADMITS THE PERSON.
//
// The catalogue offers a read tool on the apps it declares (catalog.Offered),
// and the route answers on its question's posture; the two are one fact
// written twice, so this derives what every question-backed read must declare
// from the posture the registry itself holds: none for an Everybody question,
// the one grant a Requires question asks, and the app the verb's row asks for
// a DecidedBy question, whose answer is decided per record through it.
// Narrower is a person told the assistant cannot read what the dashboard
// shows them (search_knowledge was offered on the knowledge app while the
// knowledge question is everybody's); wider is a tool offered to be refused.
// Mutation: declare search_knowledge on the knowledge app again, or a Requires
// question's tool on no app, and its row fails.
func TestAReadIsOfferedExactlyWhenItsQuestionAdmits(t *testing.T) {
	t.Parallel()
	registry := queries.NewRegistry()
	queries.Register(registry, everySeam(t))
	checked := 0
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindRead {
			continue
		}
		req, err := tool.Request(assistanttest.ExampleArgs(t, tool.Parameters))
		if err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		what, ok := api.QuestionOf(req.Method, req.Path)
		if !ok {
			continue
		}
		posture := registry.PostureOf(what)
		if !posture.Valid() {
			t.Errorf("%s reads %q, which is no question", tool.Name, what)
			continue
		}
		var want []iam.Grant
		switch {
		case posture.IsEverybody():
		case posture.Grant() != "":
			want = []iam.Grant{posture.Grant()}
		default:
			if app, _ := authz.AppOf(posture.Verb()); app != "" {
				want = []iam.Grant{app}
			}
		}
		if !slices.Equal(tool.Apps, want) {
			t.Errorf("%s is offered on %v and its question %q (%s) admits on %v",
				tool.Name, tool.Apps, what, posture, want)
		}
		checked++
	}
	if checked < 10 {
		t.Fatalf("only %d read tools were matched to a question, so this is not "+
			"reading the routes the catalogue sends", checked)
	}
}
