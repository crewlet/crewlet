package engine

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/google/uuid"
)

// noAudience resolves every question to nobody: the coordinators built with
// it are about the answer route, not whom a question is put to.
type noAudience struct{}

func (noAudience) ResolveAudience(sandbox.PendingRun, string) sandbox.Audience {
	return sandbox.Audience{}
}

// audienceChart is a founder over one unit: its lead, the seat whose runs ask
// the questions, a person beside it, and another agent.
func audienceChart(t *testing.T) *org.Organization {
	t.Helper()
	o := &org.Organization{
		Name: "Nimbus",
		Roles: []*org.Role{
			{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman, Manages: []string{"cto"}},
		},
		Units: []*org.Unit{{
			Name: "Platform", Lead: "cto",
			Roles: []*org.Role{
				{Name: "CTO", DeclaredHandle: "cto"},
				{Name: "SWE", DeclaredHandle: "swe"},
				{Name: "Ada Okonkwo", DeclaredHandle: "ada", Kind: org.KindHuman},
				{Name: "QA", DeclaredHandle: "qa"},
			},
		}},
	}
	o.Normalize()
	if manager := o.Manager(o.SeatByHandle("swe")); manager == nil || manager.Handle() != "cto" {
		t.Fatalf("the fixture's swe is managed by %v, want the unit lead cto", manager)
	}
	return o
}

// EVERY LABEL THE ASK SHIM DOCUMENTS RESOLVES TO THE SEATS IT MEANS, and one
// that names nobody the chart has falls back to the seat's lead chain — SAYING
// it fell back, because "put to me" and "reached me because nobody else could
// be named" are different things to the person reading it.
func TestAQuestionIsPutToTheSeatsItsLabelNames(t *testing.T) {
	t.Parallel()
	o := audienceChart(t)
	run := sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe", Requester: "ada"}
	chain := []string{"cto", "founder"}
	for _, tc := range []struct {
		name, label string
		requester   string
		want        []string
		fallback    bool
	}{
		{"the requester is who woke the turn", "requester", "ada", []string{"ada"}, false},
		{"a requester nobody recorded falls back", "requester", "", chain, true},
		{"a requester who has left falls back", "requester", "gone", chain, true},
		{"the manager", "manager", "ada", []string{"cto"}, false},
		{"the labels are not case-sensitive", "Manager", "ada", []string{"cto"}, false},
		{"the team is its lead and its people, never its agents or the seat",
			"team", "ada", []string{"cto", "ada"}, false},
		{"a handle", "founder", "ada", []string{"founder"}, false},
		{"a mention", "@ada", "", []string{"ada"}, false},
		{"a role name", "Ada Okonkwo", "", []string{"ada"}, false},
		{"a partial name is not a person", "Okon", "", chain, true},
		{"a name nobody has", "the product owner", "", chain, true},
		{"no label at all", "", "", chain, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := run
			r.Requester = tc.requester
			got := resolveAudience(o, nil, nil, r, tc.label)
			if !slices.Equal(got.Handles, tc.want) || got.Fallback != tc.fallback {
				t.Errorf("%q resolved to %v (fallback %v), want %v (fallback %v)",
					tc.label, got.Handles, got.Fallback, tc.want, tc.fallback)
			}
		})
	}
}

// A RUN OF A SEAT THE CHART NO LONGER HAS is put to nobody, as a fallback —
// never to a guess.
func TestAQuestionFromASeatTheChartLostIsPutToNobody(t *testing.T) {
	t.Parallel()
	got := resolveAudience(audienceChart(t), nil, nil,
		sandbox.PendingRun{TurnID: "t1", AgentHandle: "left-the-company"}, "manager")
	if len(got.Handles) != 0 || !got.Fallback {
		t.Fatalf("resolved to %v (fallback %v), want nobody as a fallback", got.Handles, got.Fallback)
	}
}

// THE CODING AGENT IS TOLD HOW TO ASK, WITH NAMES THE PARK RESOLVES. The
// roster and the manager it is given are handles, so a `--to` copied from
// either resolves exactly rather than falling back.
func TestTheAskBriefNamesWhomTheParkCanResolve(t *testing.T) {
	t.Parallel()
	o := audienceChart(t)
	seat := o.SeatByHandle("swe")
	brief := askBrief(o, seat)
	if !strings.Contains(brief, "crewlet-ask") {
		t.Fatalf("the brief does not tell the coding agent how to ask:\n%s", brief)
	}
	if !strings.Contains(brief, "Teammates you can name: cto, ada, qa.") ||
		!strings.Contains(brief, "Your manager: cto.") {
		t.Fatalf("the brief does not name the seat's teammates and manager by handle:\n%s", brief)
	}
	for _, name := range []string{"cto", "ada", "qa"} {
		if got := resolveAudience(o, nil, nil, sandbox.PendingRun{AgentHandle: "swe"}, name); got.Fallback {
			t.Errorf("a --to %q copied from the brief fell back", name)
		}
	}
}

// WHO WOKE THE TURN is the first event's asker or first speaker — never a later
// voice, and nobody for a wake no seat made.
func TestTheRequesterIsWhoseWakeStartedTheTurn(t *testing.T) {
	t.Parallel()
	ask := events.New(types.A2ARequest{ChannelID: "c1", Requester: "cto", Content: "why?"},
		events.TraceContext{})
	if got := requesterOf([]*events.Event{ask}, nil); got != "cto" {
		t.Errorf("an ask's requester = %q, want the asking seat", got)
	}
	spoke := []types.InboundInteraction{
		{Sender: types.CanonicalIdentity{Handle: ""}},
		{Sender: types.CanonicalIdentity{Handle: "ada"}},
	}
	chat := events.New(types.ExternalNotification{NotificationSource: "slack"}, events.TraceContext{})
	if got := requesterOf([]*events.Event{chat}, spoke); got != "" {
		t.Errorf("a thread started by a stranger names %q, want nobody — a later voice "+
			"in it is not who asked", got)
	}
	if got := requesterOf([]*events.Event{chat}, spoke[1:]); got != "ada" {
		t.Errorf("a colleague's message names %q, want ada", got)
	}
	tick := events.New(types.TaskAssigned{}, events.TraceContext{})
	if got := requesterOf([]*events.Event{tick}, nil); got != "" {
		t.Errorf("a schedule's wake names %q, want nobody", got)
	}
}

// WHO WOKE THE TURN REACHES THE RUN'S ROW, through the one builder both launch
// paths share — the park that resolves "requester" reads nothing else.
func TestALaunchedRunRecordsWhoWokeItsTurn(t *testing.T) {
	t.Parallel()
	ref := sandboxTurnRef(t.Context(), &turnctx.Turn{RunID: "run-1", Requester: "ada"}, "SWE")
	if ref.Requester != "ada" {
		t.Fatalf("the run's turn reference names requester %q, want ada", ref.Requester)
	}
}

// AN ANSWER BY TURN ON A SEAT'S INBOX GOES TO THE SEAT'S CONTROL TOPIC, the
// same event under the same id, addressed by the seat id it was stamped with.
// It was settled on the inbox, where it waited behind every coding job that
// held the seat and every older message, so a busy seat never reached it; one
// a node on that build put there is moved to where answers by turn are settled
// now, and one naming no seat is refused rather than published nowhere.
func TestTheDispatcherForwardsAnAnswerByTurnToTheSeatsControlTopic(t *testing.T) {
	t.Parallel()
	e, _ := starting(t, refusingModels(t))
	sent := &topicLog{}
	e.backends.Queue = sent
	d := e.buildDispatcher(Options{Dispatch: &Dispatcher{NoteDeferred: func(string) {}}}, e.backends)
	if d.ForwardAnswer == nil {
		t.Fatal("the dispatcher has no route for an answer by turn that reached the inbox")
	}
	seat := uuid.MustParse("0f1d4c07-6d2a-4e2b-9a3c-5b8e17d04c21")
	given := types.SandboxAnswerGiven{TurnID: "t1", Agent: seat.String(), AgentHandle: "swe",
		Answer: "main", LaunchID: "launch-1"}
	ev := events.New(given, events.TraceContext{})
	if err := d.ForwardAnswer(t.Context(), ev); err != nil {
		t.Fatalf("ForwardAnswer: %v", err)
	}
	if got := sent.sent(); len(got) != 1 || got[0].topic != topics.AgentControl(seat) || got[0].ev.ID != ev.ID {
		t.Fatalf("forwarded %+v, want the same event once on %s", got, topics.AgentControl(seat))
	}

	given.Agent = ""
	if err := d.ForwardAnswer(t.Context(), events.New(given, events.TraceContext{})); err == nil {
		t.Fatal("an answer naming no seat id was forwarded, want it refused")
	}
}

// topicLog is a queue that records where each event was published.
type topicLog struct {
	queue.EventQueue
	mu   sync.Mutex
	pubs []topicPub
}

type topicPub struct {
	topic string
	ev    *events.Event
}

func (q *topicLog) Publish(_ context.Context, topic string, ev *events.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pubs = append(q.pubs, topicPub{topic: topic, ev: ev})
	return nil
}

func (q *topicLog) sent() []topicPub {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.pubs)
}

// THE REQUESTER IS KEPT BY ITS SEAT'S HANDLE, and a sender the org does not
// hold is nobody: a seat the company does not have is nobody to put a question
// to, and the run's "requester" then resolves to nobody rather than to a name.
func TestTheRequesterIsKeptByItsSeatsHandle(t *testing.T) {
	t.Parallel()
	o := audienceChart(t)
	company := &Company{Org: o}

	if got := requesterIdentity(company, "ada"); got != "ada" {
		t.Fatalf("the requester was recorded as %q, want ada", got)
	}
	if got := requesterIdentity(company, "nobody-we-know"); got != "" {
		t.Errorf("a sender the org does not hold was recorded as %q, want nobody", got)
	}
	got := resolveAudience(o, nil, nil, sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe",
		Requester: "ada"}, "requester")
	if !slices.Equal(got.Handles, []string{"ada"}) || got.Fallback {
		t.Errorf("the requester resolved to %v (fallback %v), want ada",
			got.Handles, got.Fallback)
	}
}
