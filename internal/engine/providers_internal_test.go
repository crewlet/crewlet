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

// TestBuildHarnessAgentResolvesModel pins the scalar a Tier B document most often
// writes a reference into. The model becomes the harness's --model argv, so an
// unresolved one is not an error an operator can read: it is the harness refusing
// a model literally named "${HARNESS_MODEL}".
func TestBuildHarnessAgentResolvesModel(t *testing.T) {
	t.Parallel()
	r := tierB(map[string]string{"HARNESS_MODEL": "sonnet"})

	p, err := BuildHarness("subscription", config.LLMProvider{
		Model:    "${HARNESS_MODEL}",
		StateDir: t.TempDir(),
	}, r, nil)
	if err != nil {
		t.Fatalf("BuildHarness: %v", err)
	}
	if got := p.Model(); got != "sonnet" {
		t.Errorf("Model() = %q, want %q", got, "sonnet")
	}
}

// A CREDENTIAL THE ENTRY NAMES NO VARIABLE FOR IS READ THROUGH THE STORE.
//
// Both of an entry's credentials fall back to the harness's own conventional
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
		auth    config.HarnessAuth
		varName string
		want    string
		absent  string
	}{{
		name:    "a subscription token",
		auth:    config.HarnessAuth{},
		varName: "CLAUDE_CODE_OAUTH_TOKEN",
		want:    "tok-from-the-store",
		absent:  "ANTHROPIC_API_KEY",
	}, {
		name:    "an api key",
		auth:    config.HarnessAuth{Mode: config.AuthAPIKey},
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
			p, err := BuildHarness("claude", config.LLMProvider{
				Model:    "sonnet",
				StateDir: t.TempDir(),
				Auth:     tc.auth,
			}, r, nil)
			if err != nil {
				t.Fatalf("BuildHarness: %v", err)
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
// is filled in — and one that silently ran on the harness's own default model
// would bill and answer as a model nobody chose. Naming the variable sends
// them to the one thing they have to change, which is the rule every error in
// this tree is held to.
//
// "Resolved to nothing" is every way a reference gives the harness no model, not
// only a variable nobody set: one set to "" (compose passing an unset host
// variable through, an empty stored secret, which is authoritative) built an
// entry with no model at all, and a composite reference with one part unset
// handed the harness the rest of it ("/sonnet") as a model.
func TestAnUnresolvedModelNamesTheVariable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		model  string
		values map[string]string
		names  []string
	}{
		{name: "unset", model: "${LLM_MODEL}", names: []string{"LLM_MODEL"}},
		{
			name: "set to an empty value", model: "${LLM_MODEL}",
			values: map[string]string{"LLM_MODEL": ""}, names: []string{"LLM_MODEL"},
		},
		{
			name: "set to white space", model: "${LLM_MODEL}",
			values: map[string]string{"LLM_MODEL": "  \t"}, names: []string{"LLM_MODEL"},
		},
		{
			name: "a composite with one part unset", model: "${PROVIDER}/sonnet",
			names: []string{"PROVIDER"},
		},
		{
			name: "a composite with one part set to an empty value", model: "${PROVIDER}/${MODEL}",
			values: map[string]string{"PROVIDER": "", "MODEL": "sonnet"}, names: []string{"PROVIDER"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := BuildHarness("default", config.LLMProvider{
				Harness:  config.HarnessOpenCode,
				Model:    tc.model,
				StateDir: t.TempDir(),
			}, tierB(tc.values), nil)
			if err == nil {
				t.Fatalf("an entry whose model %q resolved to nothing built", tc.model)
			}
			for _, want := range append([]string{`"default"`, tc.model}, tc.names...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}

// A MODEL IS HANDED TO THE HARNESS TRIMMED. White space around a resolved value —
// a stored secret pasted with its newline — is no part of any model's name,
// and a padded --model argument is one the harness refuses as a model it has
// never heard of.
func TestAResolvedModelIsTrimmed(t *testing.T) {
	t.Parallel()
	p, err := BuildHarness("default", config.LLMProvider{
		Model:    "${LLM_MODEL}",
		StateDir: t.TempDir(),
	}, tierB(map[string]string{"LLM_MODEL": " sonnet\n"}), nil)
	if err != nil {
		t.Fatalf("BuildHarness: %v", err)
	}
	if got := p.Model(); got != "sonnet" {
		t.Errorf("Model() = %q, want %q", got, "sonnet")
	}
}

// AND A MISSING CREDENTIAL STILL BUILDS. The asymmetry is deliberate: every
// call then fails with the harness's own sign-in error, which names the harness and
// is exactly what `crewlet llm doctor` explains — far easier to diagnose than
// a boot that died over one key, where a missing model has no such tell.
func TestAMissingCredentialStillBuilds(t *testing.T) {
	t.Parallel()
	if _, err := BuildHarness("default", config.LLMProvider{
		Model:    "sonnet",
		StateDir: t.TempDir(),
		Auth: config.HarnessAuth{
			Mode:   config.AuthAPIKey,
			APIKey: "${NOBODY_SET_THIS}",
		},
	}, tierB(nil), nil); err != nil {
		t.Fatalf("a provider with no credential was refused: %v", err)
	}
}

// THE TIMEOUT THAT RUNS IS THE CONFIG'S. The harness backend keeps no default of
// its own — the engine hands it the entry's resolved cap — so the config's
// default is the one every call runs under, and it is the one the docs and the
// schema state.
func TestTheHarnessTimeoutDefaultIsTheDocumentedOne(t *testing.T) {
	t.Parallel()
	if got := (&config.LLMProvider{}).Timeout(); got != 600 {
		t.Fatalf("an entry naming no timeout_seconds gets %v s, want the documented 600", got)
	}
	if got := (&config.LLMProvider{TimeoutSeconds: 45}).Timeout(); got != 45 {
		t.Errorf("an entry naming 45 s gets %v", got)
	}
}
