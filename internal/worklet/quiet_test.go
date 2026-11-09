package worklet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// AN AGENT'S REPLY IN A THREAD WAKES ONLY WHO STILL OWES AN ANSWER THERE
// (ADR-0113). Two agents thanking each other in one thread six times in five
// minutes were each woken under `thread` by the other's thanks. Every
// participant is still told, in its inbox; the wake is the open ask's.
func TestAnAgentsReplyInAThreadWakesOnlyWhoStillOwesAnAnswer(t *testing.T) {
	t.Parallel()
	thanks := worklet.Notify{Kind: worklet.ChangeComment, Snapshot: worklet.Snapshot{
		ThreadParticipants: []string{"backend-engineer", "app-engineer"},
		CommentAuthorKind:  worklet.AuthorAgent,
	}}
	cands := worklet.Candidates(&thanks)
	if got := handles(cands); !slices.Equal(got, []string{"app-engineer", "backend-engineer"}) {
		t.Fatalf("the thanks concerned %v, want both participants told", got)
	}
	if woken := worklet.Route(cands, everyone, "app-engineer"); len(woken) != 0 {
		t.Fatalf("an agent's thanks woke %v, want nobody", handles(woken))
	}

	owed := thanks
	owed.Snapshot.ThreadOwing = []string{"backend-engineer"}
	woken := worklet.Route(worklet.Candidates(&owed), everyone, "app-engineer")
	if got := handles(woken); !slices.Equal(got, []string{"backend-engineer"}) {
		t.Fatalf("a follow-up to an open ask woke %v, want the seat it is open to", got)
	}
	if reasonOf(woken, "backend-engineer") != worklet.ReasonThread {
		t.Errorf("the seat that owes an answer heard it as %q, want thread",
			reasonOf(woken, "backend-engineer"))
	}
}

// A PERSON'S COMMENT NAMING SOMEBODY WAKES ONLY WHOM IT NAMES, asks or
// answers: the owner, the watchers and the thread hear it in their inbox.
func TestAPersonsCommentNamingSomebodyWakesOnlyWhomItNames(t *testing.T) {
	t.Parallel()
	snapshot := worklet.Snapshot{
		Assignee: "backend-engineer", Watchers: []string{"qa"},
		ThreadParticipants: []string{"designer"},
		CommentAuthorKind:  worklet.AuthorHuman,
	}
	for name, tc := range map[string]struct {
		mutate func(*worklet.Notify)
		want   []string
	}{
		"naming nobody":   {func(*worklet.Notify) {}, []string{"backend-engineer", "designer", "qa"}},
		"naming a person": {func(n *worklet.Notify) { n.Mentions = []string{"sara"} }, []string{"sara"}},
		"asking a person": {func(n *worklet.Notify) { n.Snapshot.CommentAsk = "sara" }, []string{"sara"}},
		"answering a seat": {func(n *worklet.Notify) { n.Snapshot.AnsweredAuthor = "designer" },
			[]string{"designer"}},
	} {
		n := worklet.Notify{Kind: worklet.ChangeComment, Snapshot: snapshot}
		tc.mutate(&n)
		woken := worklet.Route(worklet.Candidates(&n), everyone, "miles")
		if got := handles(woken); !slices.Equal(got, tc.want) {
			t.Errorf("a person's comment %s woke %v, want %v", name, got, tc.want)
		}
	}
}
