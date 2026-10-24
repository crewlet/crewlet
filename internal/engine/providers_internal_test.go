package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// storeSource stands in for the secret store: it answers by NAME, and nothing
// it holds is exported into the process environment. That is what makes these
// tests prove the resolver was consulted rather than os.Getenv — a backend
// falling back to the process environment answers "unset" for every one.
type storeSource map[string]string

func (s storeSource) Lookup(name string) (string, bool) {
	v, ok := s[name]
	return v, ok
}

// tierB is the Tier B chain: the store first, the environment behind it.
func tierB(values map[string]string) *config.Resolver {
	return config.NewResolver(storeSource(values), config.EnvSource{})
}

// TestBuildCLIAgentResolvesModel pins the scalar a Tier B document most often
// writes a reference into. The model becomes the CLI's --model argv, so an
// unresolved one is not an error an operator can read: it is the CLI refusing
// a model literally named "${CLI_MODEL}".
func TestBuildCLIAgentResolvesModel(t *testing.T) {
	t.Parallel()
	r := tierB(map[string]string{"CLI_MODEL": "sonnet"})

	p, err := BuildCLIAgent("subscription", config.LLMProvider{
		Model:    "${CLI_MODEL}",
		StateDir: t.TempDir(),
	}, r)
	if err != nil {
		t.Fatalf("BuildCLIAgent: %v", err)
	}
	if got := p.Model(); got != "sonnet" {
		t.Errorf("Model() = %q, want %q", got, "sonnet")
	}
}

// A CREDENTIAL THE ENTRY NAMES NO VARIABLE FOR IS READ THROUGH THE STORE.
//
// Both of an entry's credentials fall back to the CLI's own conventional
// variable — the headless token's under auth.mode subscription, the API key's
// under api-key — and the fallback has to go through the resolver, which reads
// the secret store before the environment. `crewlet llm login -capture-token`
// and `crewlet secrets set ANTHROPIC_API_KEY` both write into the store, so a
// fallback that read os.Getenv could not see the very value Crewlet itself
// just wrote, and every operator would wire up a ${VAR} for it by hand.
//
// Observed where it is used: the sign-in a coding box is handed, which is
// what the entry's own calls run under.
func TestAnEntrysConventionalCredentialIsReadThroughTheStore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		auth    config.CLIAgentAuth
		varName string
		want    string
		absent  string
	}{{
		name:    "a subscription token",
		auth:    config.CLIAgentAuth{},
		varName: "CLAUDE_CODE_OAUTH_TOKEN",
		want:    "tok-from-the-store",
		absent:  "ANTHROPIC_API_KEY",
	}, {
		name:    "an api key",
		auth:    config.CLIAgentAuth{Mode: config.AuthAPIKey},
		varName: "ANTHROPIC_API_KEY",
		want:    "sk-ant-from-the-store",
		absent:  "CLAUDE_CODE_OAUTH_TOKEN",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := tierB(map[string]string{
				"CLAUDE_CODE_OAUTH_TOKEN": "tok-from-the-store",
				"ANTHROPIC_API_KEY":       "sk-ant-from-the-store",
			})
			p, err := BuildCLIAgent("claude", config.LLMProvider{
				Model:    "sonnet",
				StateDir: t.TempDir(),
				Auth:     tc.auth,
			}, r)
			if err != nil {
				t.Fatalf("BuildCLIAgent: %v", err)
			}
			env := p.SandboxEnv()
			if got := env[tc.varName]; got != tc.want {
				t.Errorf("%s = %q, want %q — the conventional fallback did not "+
					"consult the secret store", tc.varName, got, tc.want)
			}
			// The mode decides which of the two signs in, and the other
			// stays out: a subscription entry carrying a metered key would
			// bill it while the operator believed they were on the plan.
			if got, ok := env[tc.absent]; ok {
				t.Errorf("%s = %q reached a %s entry", tc.absent, got, tc.name)
			}
		})
	}
}

// AN UNRESOLVED ${VAR} IS NOT A MISSING FIELD, and the two need different
// sentences.
//
// A provider entry that plainly reads `model: "${LLM_MODEL}"` and is refused
// with "Model is required" sends an operator to look at a field they can see
// is filled in — and one that silently ran on the CLI's own default model
// would bill and answer as a model nobody chose. Naming the variable sends
// them to the one thing they have to change, which is the rule every error in
// this tree is held to.
func TestAnUnresolvedModelNamesTheVariable(t *testing.T) {
	t.Parallel()
	_, err := BuildCLIAgent("default", config.LLMProvider{
		Model:    "${LLM_MODEL}",
		StateDir: t.TempDir(),
	}, tierB(nil))
	if err == nil {
		t.Fatal("a provider with no model built")
	}
	for _, want := range []string{"default", "${LLM_MODEL}", "LLM_MODEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// AND A MISSING CREDENTIAL STILL BUILDS. The asymmetry is deliberate: every
// call then fails with the CLI's own sign-in error, which names the CLI and
// is exactly what `crewlet llm doctor` explains — far easier to diagnose than
// a boot that died over one key, where a missing model has no such tell.
func TestAMissingCredentialStillBuilds(t *testing.T) {
	t.Parallel()
	if _, err := BuildCLIAgent("default", config.LLMProvider{
		Model:    "sonnet",
		StateDir: t.TempDir(),
		Auth: config.CLIAgentAuth{
			Mode:   config.AuthAPIKey,
			APIKey: "${NOBODY_SET_THIS}",
		},
	}, tierB(nil)); err != nil {
		t.Fatalf("a provider with no credential was refused: %v", err)
	}
}

// THE TIMEOUT THAT RUNS IS THE CONFIG'S. The CLI backend keeps no default of
// its own — the engine hands it the entry's resolved cap — so the config's
// default is the one every call runs under, and it is the one the docs and the
// schema state.
func TestTheCLITimeoutDefaultIsTheDocumentedOne(t *testing.T) {
	t.Parallel()
	if got := (&config.LLMProvider{}).Timeout(); got != 600 {
		t.Fatalf("an entry naming no timeout_seconds gets %v s, want the documented 600", got)
	}
	if got := (&config.LLMProvider{TimeoutSeconds: 45}).Timeout(); got != 45 {
		t.Errorf("an entry naming 45 s gets %v", got)
	}
}
