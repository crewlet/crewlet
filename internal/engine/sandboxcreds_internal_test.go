package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// creditedCompany is a one-seat company whose sandbox model is the named
// subscription CLI.
//
// A REAL cliagent provider rather than a stand-in, because the whole question
// is which credentials that provider's profile can actually put inside a box —
// a fake would answer whatever the test wanted and prove nothing about codex.
func creditedCompany(t *testing.T, agent string) (*Company, *org.Role) {
	t.Helper()
	provider, err := cliagent.New(cliagent.Config{
		Key: "sub", Agent: agent, StateDir: t.TempDir(), Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("cliagent.New(%s): %v", agent, err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "sub", Provider: provider}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	seat := &org.Role{Name: "SWE", LLM: org.ProviderKeys{"sub"}}
	return &Company{Models: models, Config: &config.Company{}}, seat
}

// A SUBSCRIPTION LOGIN CANNOT FOLLOW A RUN INTO A REMOTE BOX.
//
// The credential files stay on the engine host — they carry a refresh token
// whose rotation is fleet state — so a CLI that mints no headless token has
// nothing inside a remote box to authenticate with. Left unchecked the run
// provisions a box, installs the agent, applies every setup step and starts
// the job, and the agent fails at its first model call with the vendor's own
// "not authenticated", minutes in, naming nothing an operator could act on.
func TestARemoteRunWithNoTravellingCredentialIsRefused(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompany(t, "codex")
	err := sandboxCredentials(c, seat, sandbox.E2B, nil)
	if err == nil {
		t.Fatal("a remote run with no credential that travels was launched")
	}
	var credErr *SandboxCredentialError
	if !errors.As(err, &credErr) {
		t.Fatalf("error is %T, want *SandboxCredentialError — the remedy is a "+
			"config edit, and retrying it as a provider outage would loop", err)
	}
	// The remedy has to be THIS operator's: codex mints no token, so
	// telling them to capture one would send them at a flag that does not
	// exist.
	if strings.Contains(err.Error(), "capture-token") {
		t.Errorf("the error offers a token codex cannot mint: %v", err)
	}
	if !strings.Contains(err.Error(), string(sandbox.Direct)) {
		t.Errorf("the error does not name the local cell that would work: %v", err)
	}
}

// THE OTHER CASE GETS THE OTHER REMEDY. Claude Code mints a headless token
// that travels to any box, so the operator's fix is one command rather than a
// placement change — and offering only the placement change would move a seat
// onto the engine host for no reason.
func TestTheRemedyNamesTheTokenWhereTheCLIMintsOne(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompany(t, "claude-code")
	err := sandboxCredentials(c, seat, sandbox.E2B, nil)
	if err == nil {
		t.Fatal("a remote run with no resolved token was launched")
	}
	if !strings.Contains(err.Error(), "-capture-token") {
		t.Errorf("the error does not name the token claude-code can mint: %v", err)
	}
}

// A CREDENTIAL THE OPERATOR DECLARED THEMSELVES COUNTS, which is what lets
// this be a refusal rather than a warning: the engine names no tool-specific
// variable of its own, so it asks the profile which names authenticate and
// looks for those in the merged run environment — where role.sandbox.env
// already sits.
func TestASeatThatBroughtItsOwnKeyIsNotRefused(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompany(t, "codex")
	env := map[string]string{"OPENAI_API_KEY": "sk-not-a-real-key"}
	if err := sandboxCredentials(c, seat, sandbox.E2B, env); err != nil {
		t.Fatalf("a seat carrying its own key was refused: %v", err)
	}
	// Blank is not a value: an unresolved ${VAR} lands here as empty, and
	// reading that as "authenticated" is the whole failure again.
	if err := sandboxCredentials(c, seat, sandbox.E2B, map[string]string{
		"OPENAI_API_KEY": "  ",
	}); err == nil {
		t.Error("an empty credential variable was read as authentication")
	}
}

// A LOCAL CELL SEEDS THE FILES AND WRITES A REFRESHED ONE BACK, so files alone
// are a complete answer there — which is the entire reason the check turns on
// the placement rather than on the provider.
func TestALocalRunNeedsNoTravellingCredential(t *testing.T) {
	t.Parallel()
	c, seat := creditedCompany(t, "codex")
	for _, placement := range []sandbox.Placement{sandbox.Direct, sandbox.Container} {
		if err := sandboxCredentials(c, seat, placement, nil); err != nil {
			t.Errorf("run_in %q was refused: %v", placement, err)
		}
	}
}
