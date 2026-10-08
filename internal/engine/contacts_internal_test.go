package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// AN EPOCH RESOLVES ITS ROSTER'S CONTACTS THROUGH THE RESOLVER IT WAS BUILT
// WITH — on a running node its own chain, the secret store and then the
// environment it was handed — and never the process environment behind it, so
// a teammate's id reads in a seat's prompt as notification routing reads it.
// Read off the prompt a runner the epoch builds sends, because the resolver is
// held by the epoch and handed to the prompt's roster by [Company.RunnerFor],
// and either half alone could drop it.
func TestAnEpochResolvesItsRosterThroughTheResolverItWasBuiltWith(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_ENGINE_TEST_EPOCH_CONTACT"
	if _, set := os.LookupEnv(variable); set {
		t.Fatalf("the premise: %s is set in no process", variable)
	}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
roles:
  - name: CEO
    handle: ceo
    manages: [Founder]
  - name: Founder
    kind: human
    contact:
      slack_user_id: ${` + variable + `}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	built, err := NewCompanyWith(cfg, config.NewResolver(config.MapSource{variable: "U0HANDED"}))
	if err != nil {
		t.Fatalf("NewCompanyWith: %v", err)
	}
	if prompt := leadPrompt(t, built); !strings.Contains(prompt, "U0HANDED") {
		t.Errorf("the CEO's prompt does not name its report by the id the epoch's "+
			"resolver gives them:\n%s", prompt)
	}
	// THE CONTROL: an epoch built from the environment alone — `crewlet
	// validate`'s — reads the process's, which has no such variable.
	plain, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	if prompt := leadPrompt(t, plain); !strings.Contains(prompt, "Founder") ||
		strings.Contains(prompt, "U0HANDED") {
		t.Errorf("an environment-only epoch's prompt, want the founder named and "+
			"the id no process holds absent:\n%s", prompt)
	}
}

// leadPrompt is everything the CEO's executor sends its model on the first
// round of a turn c builds, the model answering nothing back.
func leadPrompt(t *testing.T, c *Company) string {
	t.Helper()
	sent := &promptRecorder{}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: sent}})
	if err != nil {
		t.Fatalf("phase registry: %v", err)
	}
	c.Models = models
	r, err := c.RunnerFor("ceo", tools.NewRegistry(), RunnerInput{Task: "plan the week",
		Reply: turn.NoReply()})
	if err != nil {
		t.Fatalf("RunnerFor: %v", err)
	}
	// The turn fails, its model refusing every call: only what it was sent
	// is under test.
	_, _, _ = r.Execute(t.Context(), 1, "", nil)
	return sent.first()
}

// promptRecorder is a model that records the messages of the first request it is
// sent and refuses every request.
type promptRecorder struct {
	mu   sync.Mutex
	text string
	seen bool
}

func (*promptRecorder) Model() string { return "recorder" }

func (s *promptRecorder) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seen {
		s.seen = true
		var b strings.Builder
		for _, m := range req.Messages {
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
		s.text = b.String()
	}
	return nil, errors.New("the recorder answers nothing")
}

func (s *promptRecorder) first() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seen {
		return ""
	}
	return s.text
}

// THE ENGINE'S OWN CHART SEAMS FIND A PERSON THROUGH THIS NODE'S CHAIN. A
// person's contact ids and their binding may each be a `${VAR}`, and the two
// here are set in no process — only in the environment the engine was handed
// — so a seam that read the process environment instead finds nobody, while
// notification routing, which resolves through the chain, goes on mentioning
// the person it could not find. One engine, because what each subtest holds is
// a different seam over the same chart.
func TestTheEnginesChartSeamsResolveThroughItsOwnChain(t *testing.T) {
	t.Parallel()
	const bound, slack = "CREWLET_ENGINE_TEST_BOUND_FOUNDER", "CREWLET_ENGINE_TEST_FOUNDER_SLACK"
	for _, variable := range []string{bound, slack} {
		if _, set := os.LookupEnv(variable); set {
			t.Fatalf("the premise: %s is set in no process", variable)
		}
	}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: Founder
    kind: human
    contact:
      crewlet_operator_id: ${` + bound + `}
      slack_user_id: ${` + slack + `}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	SeedStore(t, &b)
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg,
		Environment: config.MapSource{bound: "founder-token", slack: "U0HANDED"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	deps := e.workDeps(e.Company())

	// A SEAT READING A PERSON READS THEM UNDER BOTH OF THEIR NAMES. A change
	// filed by somebody through the dashboard concerns them under the
	// credential their seat binds, and the applier writes that person's notice
	// under it — so a seat's get_person or work_inbox about them, the handle
	// alone, never saw it. The seat surface answers the party as the operator
	// surface does.
	t.Run("a_seat_reads_a_person_under_both_of_their_names", func(t *testing.T) {
		t.Parallel()
		if deps.Reader == nil {
			t.Fatal("the node has no native tracker, so this case shows nothing")
		}
		if deps.Party == nil {
			t.Fatal("a seat's work tools read a person under their handle alone")
		}
		if got := deps.Party("founder"); got.Handle != "founder" || got.OperatorID != "founder-token" {
			t.Errorf("the founder's party is %+v, want the seat and the credential the "+
				"handed environment binds to it", got)
		}
	})

	// THE WORK TOOLS' ROSTER, which a handle a seat typed is checked against,
	// holds the person by the id the handed environment gives them.
	t.Run("the_work_tools_roster_knows_them_by_that_id", func(t *testing.T) {
		t.Parallel()
		found := colleague.Resolve("U0HANDED", deps.Seats())
		if len(found) != 1 || found[0].Seat.Handle != "founder" {
			t.Errorf("the roster resolves U0HANDED to %+v, want the founder", found)
		}
	})

	// A MENTION BY THAT ID REACHES THEIR SEAT: what a comment's @-mention is
	// turned into a wake through.
	t.Run("a_mention_by_that_id_reaches_their_seat", func(t *testing.T) {
		t.Parallel()
		if handle, ok := (liveSeats{engine: e}).ResolveSeat("U0HANDED"); !ok || handle != "founder" {
			t.Errorf("a mention of U0HANDED resolves to %q (%v), want the founder", handle, ok)
		}
	})

	// A CODING RUN'S QUESTION PUT TO THAT ID IS PUT TO THEM, rather than
	// falling back to the asking seat's lead chain as a name nobody has.
	t.Run("a_coding_runs_question_by_that_id_is_put_to_them", func(t *testing.T) {
		t.Parallel()
		got := audienceResolver{engine: e}.ResolveAudience(
			sandbox.PendingRun{TurnID: "t1", AgentHandle: "ceo"}, "U0HANDED")
		if got.Fallback || !slices.Equal(got.Handles, []string{"founder"}) {
			t.Errorf("a question for U0HANDED is put to %+v, want the founder by name", got)
		}
	})
}
