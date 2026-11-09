package worklet_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
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

// A STAKE IN A THREAD IS WOKEN BY ANY REPLY BY SOMEBODY ELSE, whatever it
// names (ADR-0113): the seat that asked, and the seat asked that still owes,
// even when the reply is addressed to somebody else, from a person or an agent.
func TestTheAskerOfAnOpenAskIsWokenByAnyReply(t *testing.T) {
	t.Parallel()
	snapshot := worklet.Snapshot{
		Assignee: "backend-engineer", ThreadParticipants: []string{"backend-engineer"},
		ThreadOwing: []string{"designer"}, ThreadAwaiting: []string{"backend-engineer"},
	}
	for name, n := range map[string]worklet.Notify{
		"the lead naming the asked seat": {Kind: worklet.ChangeComment,
			Mentions: []string{"designer"}, Snapshot: withAuthor(snapshot, worklet.AuthorHuman)},
		"another agent with the information": {Kind: worklet.ChangeComment,
			Snapshot: withAuthor(snapshot, worklet.AuthorAgent)},
	} {
		woken := handles(worklet.Route(worklet.Candidates(&n), everyone, "lead"))
		if !slices.Contains(woken, "backend-engineer") || !slices.Contains(woken, "designer") {
			t.Errorf("%s woke %v, want the asker and the seat it asked", name, woken)
		}
	}
}

// A CO-ASKER'S REPLY DOES NOT WAKE THE OTHER ASKER (ADR-0113): two seats
// awaiting one addressee in a thread are not each other's stake, while the
// addressee's reply, or anybody else's, still wakes both.
func TestACoAskersReplyLeavesTheOtherAskerAsleep(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("ship or hold")
	post := func(op string, c worklet.Comment) {
		t.Helper()
		c.Task, c.CreatedAt, c.AuthorKind = created.ID, wednesday, worklet.AuthorAgent
		if _, err := r.writer.UpdateTask(t.Context(), op, created.ID, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &c}, worklet.ChangeComment, nil); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		r.drain()
	}
	root := "c-dev"
	post("op-dev", worklet.Comment{ID: root, Author: "dev", Body: "Friday?", Ask: "ana"})
	post("op-qa", worklet.Comment{ID: "c-qa", Author: "qa", Body: "and the notes?",
		Ask: "ana", ReplyTo: &root})
	for author, want := range map[string][]string{
		"qa":   {"qa"},
		"dev":  {"dev"},
		"ana":  {"dev", "qa"},
		"lead": {"dev", "qa"},
	} {
		q := worklet.ThreadQuery{Task: created.ID, ReplyTo: root, Author: author}
		if author == "ana" {
			q.Answers = "c-qa"
		}
		got, err := r.reader.Thread(t.Context(), q, statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("read the thread as %s: %v", author, err)
		}
		if !slices.Equal(got.Awaiting, want) {
			t.Errorf("a reply by %s has %v awaiting, want %v", author, got.Awaiting, want)
		}
	}
}

// A PERSON'S OWN ASK IS A STAKE (ADR-0113): a person awaiting the seat an
// agent awaits is a co-asker, so the person's reply does not wake the agent,
// as in a chat thread; somebody with no ask there still does.
func TestAPersonsOpenAskMakesACoAsker(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("ship or hold")
	root := "c-dev"
	for _, c := range []worklet.Comment{
		{ID: root, Author: "dev", AuthorKind: worklet.AuthorAgent, Body: "Friday?", Ask: "ana"},
		{ID: "c-jane", Author: "jane", AuthorKind: worklet.AuthorHuman, Body: "and the notes?",
			Ask: "ana", ReplyTo: &root},
	} {
		c.Task, c.CreatedAt = created.ID, wednesday
		if _, err := r.writer.UpdateTask(t.Context(), "op-"+c.ID, created.ID, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &c}, worklet.ChangeComment, nil); err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
		r.drain()
	}
	for author, want := range map[string][]string{"jane": {"jane"}, "lead": {"dev", "jane"}} {
		got, err := r.reader.Thread(t.Context(), worklet.ThreadQuery{Task: created.ID,
			ReplyTo: root, Author: author}, statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("read the thread as %s: %v", author, err)
		}
		if !slices.Equal(got.Awaiting, want) {
			t.Errorf("a reply by %s has %v awaiting, want %v", author, got.Awaiting, want)
		}
	}
}

// AN AGENT'S MENTION THAT ASKS NOTHING IS A REFERENCE, through the record it
// writes (ADR-0113): the measured loop, an agent's "nothing for @qa" stating
// it asks nothing, wakes no agent, at the version that says so, while asking,
// a person's mention, and an older build's comment with no statement wake qa,
// and the person it names is addressed either way.
func TestAnAgentsMentionThatAsksNothingWakesNoAgent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("release checklist")
	for i, c := range []struct {
		author  string
		kind    worklet.AuthorKind
		asks    *bool
		agents  []string
		woken   bool
		version int
	}{
		// STATED, so its notice rows keep the statement: the version that says so.
		{"pm", worklet.AuthorAgent, ptr(false), []string{"qa"}, false, worklet.NoticeAsksVersion},
		{"pm", worklet.AuthorAgent, ptr(true), []string{"qa"}, true, worklet.NoticeAsksVersion},
		// AN OLDER BUILD'S, with no statement, and one naming its agents
		// without a statement: the statement alone makes a reference.
		{"pm", worklet.AuthorAgent, nil, nil, true, 1},
		{"pm", worklet.AuthorAgent, nil, []string{"qa"}, true, 5},
		{"jane", worklet.AuthorHuman, nil, nil, true, 1},
	} {
		comment := worklet.Comment{ID: fmt.Sprintf("c-%d", i), Task: created.ID, Author: c.author,
			AuthorKind: c.kind, Body: "My approval still stands, nothing for @qa or @sara.",
			Mentions: []string{"qa", "sara"}, CreatedAt: wednesday}
		agents := c.agents
		wake := worklet.Wake{Kind: worklet.ChangeComment, Before: created, After: created,
			Comment: &comment, Mentions: comment.Mentions, Asks: c.asks,
			MentionedAgents: agents}.Notify(nil)
		if _, err := r.writer.As(c.author, c.kind, worklet.Provenance{}).UpdateTask(t.Context(),
			"op-"+comment.ID, created.ID, "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Comment: &comment}, worklet.ChangeComment, wake); err != nil {
			t.Fatalf("%s's comment: %v", c.author, err)
		}
		r.drain()
		record, err := worklet.Decode(r.lastRecord(t))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if record.V != c.version {
			t.Errorf("case %d is stamped version %d, want %d", i, record.V, c.version)
		}
		woken := handles(worklet.Route(worklet.Candidates(record.Notify), everyone, c.author))
		if got := slices.Contains(woken, "qa"); got != c.woken {
			t.Errorf("case %d: a %s comment woke %v, want qa woken %v", i, c.kind, woken, c.woken)
		}
		if heard, told := worklet.CandidateFor(record.Notify, "qa"); !told || heard.Reason != worklet.ReasonMention {
			t.Errorf("case %d did not tell qa of the mention", i)
		}
		if heard, _ := worklet.CandidateFor(record.Notify, "sara"); !heard.Addressed {
			t.Errorf("case %d: the person it names is not addressed", i)
		}
	}
}

func withAuthor(s worklet.Snapshot, kind worklet.AuthorKind) worklet.Snapshot {
	s.CommentAuthorKind = kind
	return s
}
