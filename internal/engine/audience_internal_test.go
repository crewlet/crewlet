package engine

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// noAudience resolves every question to nobody: the coordinators built with
// it are about the answer route, not whom a question is put to.
type noAudience struct{}

func (noAudience) ResolveAudience(sandbox.PendingRun, string) sandbox.Audience {
	return sandbox.Audience{}
}

// testAnswers is the seal a coordinator a case builds by hand seals and opens
// answers under ([sandbox.CoordinatorOptions.Answers]): one fleet keyring for
// every such coordinator, as a fleet shares one.
var testAnswers = sync.OnceValue(func() *sandbox.AnswerSeal {
	seal, err := sandbox.NewAnswerSeal(statelog.OneKey("answers", "the engine cases' fleet key material"))
	if err != nil {
		panic(err)
	}
	return seal
})

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

// EVERY AUDIENCE `ask_and_wait` TAKES (`to`) RESOLVES TO THE SEATS IT MEANS, and one
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

// THE HARNESS IS TOLD HOW TO ASK, WITH NAMES THE PARK RESOLVES, AND TO
// GIVE ITS OPTIONS AS OPTIONS. The roster and the manager it is given are
// handles, so a `to` copied from either resolves exactly rather than falling
// back; and a run told only to ask wrote its choices into the question as
// prose, which a person could not answer with a press.
func TestTheAskBriefNamesWhomTheParkCanResolve(t *testing.T) {
	t.Parallel()
	o := audienceChart(t)
	seat := o.SeatByHandle("swe")
	brief := askBrief(o, seat)
	if !strings.Contains(brief, "`"+builtin.AskAndWaitTool+"`") {
		t.Fatalf("the brief does not tell the harness how to ask:\n%s", brief)
	}
	for _, field := range []string{"`options`", "`recommended`", "`rationale`", "`branch`"} {
		if !strings.Contains(brief, field) {
			t.Errorf("the brief does not tell the harness to fill %s:\n%s", field, brief)
		}
	}
	// AND THE WAY THAT DOES NOT WAIT: a question the run can go on without
	// is asked with the seat's own tools and answered in its next turn, so a
	// run told only of the park parks, holding its box, on every question.
	if !strings.Contains(brief, "`"+builtin.CommentOnWorkTool+"` with `ask`") ||
		!strings.Contains(brief, runner.SubmitWorkTool) {
		t.Fatalf("the brief does not tell the harness to ask with its tools and "+
			"finish when it can go on without the answer:\n%s", brief)
	}
	if !strings.Contains(brief, "Teammates you can name: cto, ada, qa.") ||
		!strings.Contains(brief, "Your manager: cto.") {
		t.Fatalf("the brief does not name the seat's teammates and manager by handle:\n%s", brief)
	}
	for _, name := range []string{"cto", "ada", "qa"} {
		if got := resolveAudience(o, nil, nil, sandbox.PendingRun{AgentHandle: "swe"}, name); got.Fallback {
			t.Errorf("a `to` %q copied from the brief fell back", name)
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
	ref := sandboxTurnRef(t.Context(), &turnctx.Turn{RunID: "run-1", Requester: "ada"}, "SWE", nil)
	if ref.Requester != "ada" {
		t.Fatalf("the run's turn reference names requester %q, want ada", ref.Requester)
	}
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
