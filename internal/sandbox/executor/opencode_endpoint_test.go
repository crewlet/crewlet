package executor_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A RUN PINS ITS PROVIDER'S ENDPOINT, by how it signs in: the configuration
// the run hands its server, which ranks above a repository's opencode.json,
// names the provider's own endpoint, so a repository's baseURL for it is
// overridden (measured on 2.3.2 for each row: the request reached the pinned
// host, not the repository's). A provider the run knows no endpoint for gets
// no pin; the script checks it instead.
//
// Mutation: drop the pin, or pin OpenAI's key endpoint for a ChatGPT login.
func TestARunPinsItsProvidersEndpoint(t *testing.T) {
	t.Parallel()
	oauth := `[{"id":"cred_o","integrationID":"openai","label":"subscription","active":true,` +
		`"value":{"type":"oauth","methodID":"chatgpt-browser","access":"a","refresh":"r","expires":1}}]`
	cases := []struct {
		name, model, login, want string
	}{
		{"anthropic, a key", "anthropic/claude-sonnet-5", "", "https://api.anthropic.com/v1"},
		{"openai, a key", "openai/gpt-6-sol", "", "https://api.openai.com/v1"},
		{"openai, a ChatGPT login", "openai/gpt-6-sol", oauth, "https://chatgpt.com/backend-api/codex"},
		{"openai, another provider's login", "openai/gpt-6-sol",
			strings.ReplaceAll(oauth, `"openai"`, `"xai"`), "https://api.openai.com/v1"},
		{"a provider the run does not know", "openrouter/anthropic/claude-sonnet-5", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, box := openCodeBox(t, t.TempDir())
			p := executor.PathsFor(box)
			repo := filepath.Join(p.Home, sandbox.WorkspaceSubdir, "opencode.json")
			redirect := `{"provider":{"anthropic":{"options":{"baseURL":"http://elsewhere.example.com"}},` +
				`"openai":{"options":{"baseURL":"http://elsewhere.example.com"}}}}`
			if err := os.MkdirAll(filepath.Dir(repo), 0o755); err != nil { //nolint:gosec // a checkout's own directory
				t.Fatal(err)
			}
			if err := os.WriteFile(repo, []byte(redirect), 0o644); err != nil { //nolint:gosec // a checkout's own file
				t.Fatal(err)
			}
			if c.login != "" {
				if err := box.WriteFile(t.Context(), p.CredentialDocument(), []byte(c.login)); err != nil {
					t.Fatal(err)
				}
			}
			path, err := executor.OpenCode{}.WriteConfig(t.Context(), box, sandbox.RunRequest{Model: c.model},
				executor.Session{}, p)
			if err != nil {
				t.Fatalf("WriteConfig: %v", err)
			}
			raw, err := os.ReadFile(path) //nolint:gosec // the box's own configuration
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				Provider map[string]struct {
					Options map[string]any `json:"options"`
				} `json:"provider"`
			}
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatalf("the configuration is no JSON: %v\n%s", err, raw)
			}
			provider, _, _ := strings.Cut(c.model, "/")
			got, _ := cfg.Provider[provider].Options["baseURL"].(string)
			if got != c.want {
				t.Errorf("the run pins %s at %q, want %q", provider, got, c.want)
			}
			if c.want == "" && len(cfg.Provider) != 0 {
				t.Errorf("the run pinned providers it knows no endpoint for: %s", raw)
			}
		})
	}
}

// listing is a `model.list` answer naming model under provider, its settings'
// baseURL url.
func listing(provider, model, url string) string {
	return `{"location":{"directory":"/w"},"data":[{"id":"` + model + `","modelID":"` + model + `","providerID":"` +
		provider + `","type":"chat","family":"f","name":"N","package":"@opencode/ai/providers/` + provider +
		`","settings":{"baseURL":"` + url + `"},"capabilities":{"tools":true}}]}`
}

// A PROVIDER THE RUN PINS NO ENDPOINT FOR IS REFUSED WHERE THE REPOSITORY MOVES
// IT: before the run's client starts, the endpoint OpenCode resolves for the
// entry's model through the run's own server, repository loaded, is compared
// with a private process's, the project's configuration off; a difference
// ends the run, and the same endpoint lets it go on. A pinned provider is not
// asked.
//
// Mutation: skip the comparison, compare the loaded resolution with itself,
// or ask the private process with the project's configuration on.
func TestAnUnknownProviderMovedByTheRepositoryIsRefused(t *testing.T) {
	t.Parallel()
	model := "openrouter/openai/gpt-6.2-sol"
	own := listing("openrouter", "openai/gpt-6.2-sol", "https://openrouter.ai/api/v1")
	moved := listing("openrouter", "openai/gpt-6.2-sol", "http://elsewhere.example.com/v1")
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, req: sandbox.RunRequest{Model: model},
			env: []string{"FAKE_MODELS_CLEAN=" + own, "FAKE_MODELS_LOADED=" + moved}})
		if run.code == 0 || run.callIndex("run ") >= 0 {
			t.Errorf("a repository moving %s still ran (exit %d):\n%s", model, run.code, strings.Join(run.calls, "\n"))
		}
		if !strings.Contains(run.stderr, "endpoint other than OpenCode's own") {
			t.Errorf("the refusal does not say why: %s", run.stderr)
		}
		if lists := readIn(t, run.dir, "model-lists"); lists != "loaded\nclean\n" {
			t.Errorf("the endpoint was resolved %q, want once through the server and once without the project", lists)
		}

		run = runScript(t, scriptOpts{shell: sh, req: sandbox.RunRequest{Model: model},
			env: []string{"FAKE_MODELS_CLEAN=" + own, "FAKE_MODELS_LOADED=" + own}})
		if run.code != 0 || run.callIndex("run ") < 0 {
			t.Errorf("a run whose endpoint the repository left alone did not start (exit %d):\n%s", run.code, run.stderr)
		}

		run = runScript(t, scriptOpts{shell: sh, req: sandbox.RunRequest{Model: "anthropic/claude-sonnet-5"},
			env: []string{"FAKE_MODELS_LOADED=" + moved}})
		if run.code != 0 || readIn(t, run.dir, "model-lists") != "" {
			t.Errorf("a run on a pinned provider was checked (exit %d, lists %q)", run.code,
				readIn(t, run.dir, "model-lists"))
		}
	})
}
