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
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
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
    manages: [founder]
  - name: Founder
    handle: founder
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

// leadPrompt is everything the CEO's executor is launched with on the first
// iteration of a turn c builds: the standing instructions and the brief its
// run is handed.
func leadPrompt(t *testing.T, c *Company) string {
	t.Helper()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: refusingProvider{}}})
	if err != nil {
		t.Fatalf("phase registry: %v", err)
	}
	c.Models = models
	sent := &promptRecorder{}
	r, err := c.RunnerFor(t.Context(), "ceo", tools.NewRegistry(), RunnerInput{Task: "plan the week",
		Reply: turn.NoReply(), Launcher: sent, Sessions: testSessions(t)})
	if err != nil {
		t.Fatalf("RunnerFor: %v", err)
	}
	// Only what the run was handed is under test.
	_, _, _ = r.Execute(t.Context(), 1, "", nil)
	return sent.first()
}

// promptRecorder is an executor launcher that records the prompt of the first
// run it is handed and launches nothing.
type promptRecorder struct {
	mu   sync.Mutex
	text string
	seen bool
}

func (s *promptRecorder) LaunchExecutor(_ context.Context, req runner.AgentRunRequest) (runner.Launched, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seen {
		s.seen, s.text = true, req.System+"\n"+req.Brief+"\n"
	}
	return runner.Launched{}, errors.New("the recorder launches nothing")
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
// person's contact ids may each be a `${VAR}`, and the one here is set in no
// process — only in the environment the engine was handed — so a seam that
// read the process environment instead finds nobody, while notification
// routing, which resolves through the chain, goes on mentioning the person it
// could not find. One engine, because what each subtest holds is a different
// seam over the same chart.
func TestTheEnginesChartSeamsResolveThroughItsOwnChain(t *testing.T) {
	t.Parallel()
	const slack = "CREWLET_ENGINE_TEST_FOUNDER_SLACK"
	if _, set := os.LookupEnv(slack); set {
		t.Fatalf("the premise: %s is set in no process", slack)
	}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: Founder
    handle: founder
    kind: human
    contact:
      slack_user_id: ${` + slack + `}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	SeedStore(t, &b)
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg,
		Environment: config.MapSource{slack: "U0HANDED"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	deps := e.workDeps(e.Company())

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

// A RUNNER'S ROSTER KNOWS WHICH CHAT THE COMPANY RUNS. On the engine's own a
// person answers to their seat's handle, so a lead's roster gives it and is
// told to mention them by it; on a company whose chat is nobody's they have
// no account to be mentioned on. Read off the prompt a runner the epoch builds sends, because
// the backend is decided by the epoch's configuration and handed to the roster
// by [Company.RunnerFor], and the roster alone cannot see it.
func TestARunnersRosterKnowsWhichChatTheCompanyRuns(t *testing.T) {
	t.Parallel()
	for backend, want := range map[string]string{
		"chatlet": "  - Chat: @founder",
		"none":    "nobody to @-mention",
	} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.ParseCompany([]byte(`
name: Acme
chat:
  backend: ` + backend + `
roles:
  - name: CEO
    handle: ceo
    manages: [founder]
  - name: Founder
    handle: founder
    kind: human
`))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			c, err := NewCompany(cfg)
			if err != nil {
				t.Fatalf("NewCompany: %v", err)
			}
			if prompt := leadPrompt(t, c); !strings.Contains(prompt, want) {
				t.Errorf("on %s chat the CEO's prompt never says %q about its "+
					"human report:\n%s", backend, want, prompt)
			}
		})
	}
}
