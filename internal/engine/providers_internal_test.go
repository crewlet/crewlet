package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// storeSource stands in for the secret store: it answers by NAME and exports
// nothing to the process environment, so these tests prove the resolver was
// consulted rather than os.Getenv.
type storeSource map[string]string

func (s storeSource) Lookup(name string) (string, bool) {
	v, ok := s[name]
	return v, ok
}

// tierB is the Tier B chain: the store first, the environment behind it.
func tierB(values map[string]string) *config.Resolver {
	return config.NewResolver(storeSource(values), config.EnvSource{})
}

// TestBuildHarnessAgentResolvesModel pins the model's ${VAR} resolution:
// unresolved, the harness refuses a model literally named "${HARNESS_MODEL}".
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

// A CREDENTIAL THE ENTRY NAMES NO VARIABLE FOR IS READ THROUGH THE STORE: the
// conventional fallback (the token under subscription, the API key under
// api-key) must see what `crewlet llm login -capture-token` and
// `crewlet secrets set` wrote there, which os.Getenv cannot. Observed on the
// sign-in a coding box is handed.
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
			// The mode picks one; a subscription entry carrying a metered
			// key would bill it while the operator believes they are on
			// the plan.
			if got, ok := env[tc.absent]; ok {
				t.Errorf("%s = %q reached a %s entry", tc.absent, got, tc.name)
			}
		})
	}
}

// AN UNRESOLVED ${VAR} IS NOT A MISSING FIELD: the refusal names the variable,
// not "Model is required", and never runs the harness's default model.
// "Resolved to nothing" covers unset, set to "" or white space, and a
// composite with one part unset, which would hand the harness "/sonnet".
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

// A MODEL IS HANDED TO THE HARNESS TRIMMED: a stored secret pasted with its
// newline is otherwise a --model the harness refuses.
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

// AND A MISSING CREDENTIAL STILL BUILDS, deliberately unlike a model: every
// call then fails with the harness's own sign-in error, which
// `crewlet llm doctor` explains, rather than the boot dying over one key.
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

// THE TIMEOUT THAT RUNS IS THE CONFIG'S: the harness backend keeps no default
// of its own, so the config's is the one the docs and the schema state.
func TestTheHarnessTimeoutDefaultIsTheDocumentedOne(t *testing.T) {
	t.Parallel()
	if got := (&config.LLMProvider{}).Timeout(); got != 600 {
		t.Fatalf("an entry naming no timeout_seconds gets %v s, want the documented 600", got)
	}
	if got := (&config.LLMProvider{TimeoutSeconds: 45}).Timeout(); got != 45 {
		t.Errorf("an entry naming 45 s gets %v", got)
	}
}
