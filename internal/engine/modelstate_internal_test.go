package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// epochEngine is a bare engine whose active revision is c.
func epochEngine(c *Company) *Engine {
	e := &Engine{}
	e.epoch.current.Store(c)
	return e
}

// rowOf is key's model row in rows.
func rowOf(t *testing.T, rows []coord.ModelStatus, key string) coord.ModelStatus {
	t.Helper()
	for _, row := range rows {
		if row.Key == key {
			return row
		}
	}
	t.Fatalf("no row for %q in %+v", key, rows)
	return coord.ModelStatus{}
}

// EACH ENTRY'S ROW IS ITS SIGN-IN'S STATE ON THIS NODE: the plan prefix that
// tells entries on one sign-in apart, the route it signs in by, the window it
// sits out with the vendor's own words, and the seats this node holds parked
// on a chain naming it, since when. An entry that sits nothing out says so by
// having no window, never a zero one.
//
// Mutation: publish another entry's window, drop the parked seats from a chain
// member that is not its head, or publish the whole sign-in id, and this fails.
func TestEachEntrysRowIsItsSignInsStateOnThisNode(t *testing.T) {
	t.Parallel()
	c, _ := chainCompany(t,
		chainMember{key: "plan", agent: "claude-code", model: "opus"},
		chainMember{key: "spare", agent: "claude-code", model: "haiku", env: map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.example.com"}},
	)
	if !planOf(t, c).RunEnded(t.Context(), harness.RunEnd{Answer: "Usage limit reached · resets 5pm", At: time.Now()}) {
		t.Fatal("the premise: the plan entry sits out")
	}
	e := epochEngine(c)
	seat := uuid.MustParse("6f1c2a3b-4d5e-5f60-8172-839405a6b7c8")
	e.joinLimitProbe(t.Context(), providerBasis{models: c.Models, keys: []string{"plan", "spare"}},
		seat, "swe", time.Now().Add(time.Hour))
	t.Cleanup(func() { e.leaveLimitProbe(seat, providerBasis{models: c.Models, keys: []string{"plan", "spare"}}) })

	rows := beatOf(t, e).Models
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want one per entry", rows)
	}
	plan, spare := rowOf(t, rows, "plan"), rowOf(t, rows, "spare")
	if plan.SittingOut == nil || !strings.Contains(plan.SittingOut.Said, "Usage limit reached") ||
		plan.SittingOut.Until.IsZero() {
		t.Errorf("the spent entry's window = %+v, want its sit-out in the vendor's words", plan.SittingOut)
	}
	if spare.SittingOut != nil {
		t.Errorf("the spare entry sits out %+v, want nothing", spare.SittingOut)
	}
	for _, row := range []coord.ModelStatus{plan, spare} {
		if len(row.Plan) != coord.ModelPlanChars || row.SignIn != "api-key" || !row.Resolves {
			t.Errorf("%s's sign-in = plan %q route %q resolves %v, want a %d-character prefix "+
				"of an api-key that resolves", row.Key, row.Plan, row.SignIn, row.Resolves, coord.ModelPlanChars)
		}
		if len(row.Parked) != 1 || row.Parked[0].Seat != seat || row.Parked[0].Since.IsZero() {
			t.Errorf("%s's parked seats = %+v, want swe, since it parked", row.Key, row.Parked)
		}
	}
	if full := string(planOf(t, c).SignInID()); strings.Contains(plan.Plan, full) && len(full) > coord.ModelPlanChars {
		t.Errorf("the row publishes the whole sign-in id %q", full)
	}
}

// A NODE THAT RUNS NO MODEL SAYS NONE, which reads as "this node runs none",
// never as a list of empty rows.
func TestANodeWithNoModelsPublishesNoRows(t *testing.T) {
	t.Parallel()
	if rows := beatOf(t, &Engine{}).Models; rows != nil {
		t.Fatalf("a node with no revision published %+v", rows)
	}
}

// statusHarness is a subscription claude-code entry whose harness's status command
// exits with code, as the login watcher runs it.
func statusHarness(t *testing.T, key string, code int) *harness.Provider {
	t.Helper()
	script := filepath.Join(t.TempDir(), "status-cli")
	body := fmt.Sprintf("#!/bin/sh\necho '{\"loggedIn\": true, \"authMethod\": \"claude.ai\"}'\nexit %d\n", code)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	provider, err := harness.New(harness.Config{
		Key: key, Harness: "claude-code", Model: "opus", StateDir: t.TempDir(),
		Overrides: map[string]any{"binary": script}, Timeout: time.Minute,
		Auth: harness.Auth{Mode: harness.AuthSubscription},
	})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	return provider
}

// THE LOGIN WATCHER SAYS WHAT THE HARNESS SAYS OF ITS LOGIN, for an entry that
// signs in with one, and only for such an entry: a person's check answers the
// status command's verdict now and the heartbeat carries it from then on; an
// entry on a key has no login to check, and an entry the revision does not
// have is not found.
//
// Mutation: publish a login row for a key entry, or answer a check from the
// last beat's reading rather than running the command, and this fails.
func TestTheLoginWatcherSaysWhatTheHarnessSaysOfItsLogin(t *testing.T) {
	t.Parallel()
	c, _ := chainCompany(t, chainMember{key: "keyed", agent: "claude-code", model: "opus"})
	signedIn, signedOut := statusHarness(t, "host", 0), statusHarness(t, "gone", 1)
	keyed, _ := c.Models.Provider("keyed")
	models, err := phase.NewRegistry([]phase.Entry{
		{Key: "keyed", Provider: keyed}, {Key: "host", Provider: signedIn}, {Key: "gone", Provider: signedOut},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	c.Models = models
	c.Config.Providers.LLM["host"] = config.LLMProvider{Harness: "claude-code", Model: "opus"}
	c.Config.Providers.LLM["gone"] = config.LLMProvider{Harness: "claude-code", Model: "opus"}
	e := epochEngine(c)

	if _, err := e.CheckHostLogin(t.Context(), "keyed"); !errors.Is(err, ErrNoHostLogin) {
		t.Errorf("checking a key entry's login = %v, want ErrNoHostLogin", err)
	}
	if _, err := e.CheckHostLogin(t.Context(), "nobody"); !errors.Is(err, ErrNoModel) {
		t.Errorf("checking an unknown entry = %v, want ErrNoModel", err)
	}
	got, err := e.CheckHostLogin(t.Context(), "host")
	if err != nil || got.State != coord.HostSignedIn || got.CheckedAt.IsZero() {
		t.Fatalf("CheckHostLogin(host) = %+v, %v; want signed in, checked now", got, err)
	}

	e.checkHostLogins(t.Context(), true)
	rows := beatOf(t, e).Models
	if row := rowOf(t, rows, "gone"); row.HostLogin == nil || row.HostLogin.State != coord.HostSignedOut {
		t.Errorf("the signed-out entry's row = %+v, want its harness's signed_out", row.HostLogin)
	}
	if row := rowOf(t, rows, "keyed"); row.HostLogin != nil {
		t.Errorf("the key entry's row carries a login %+v", row.HostLogin)
	}
}

// beatOf is what e's next heartbeat carries, a case failing where the beat
// would publish nothing (the status reads [Engine.nodeStatus] makes did not
// answer).
func beatOf(t *testing.T, e *Engine) coord.NodeStatus {
	t.Helper()
	status, err := e.nodeStatus(t.Context())
	if err != nil {
		t.Fatalf("the beat answered nothing: %v", err)
	}
	return status
}
