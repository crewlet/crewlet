package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// openCodeEndpoint is the endpoint a provider's requests reach, by how the run
// signs in to it: the key the run's environment carries, or an OAuth login the
// box's document holds.
type openCodeEndpoint struct{ key, oauth string }

// of is the endpoint for a sign-in of kind ("oauth" or anything else, a key).
func (e openCodeEndpoint) of(kind string) string {
	if kind == "oauth" {
		return e.oauth
	}
	return e.key
}

// openCodeEndpoints is the endpoint a run pins for each provider it knows, by
// sign-in kind: OpenCode 2.3.2's own resolution for each, measured. OpenAI's
// differs by sign-in: a key reaches the platform API, a ChatGPT login Codex's
// backend, and OpenCode sends either to whatever baseURL it is given. Pinned
// in the run's configuration, which ranks above a repository's; the entry
// names no endpoint OpenCode reads (it ignores ANTHROPIC_BASE_URL and
// OPENAI_BASE_URL, measured). A provider not here is checked instead
// ([openCodeEndpointCheck]).
var openCodeEndpoints = map[string]openCodeEndpoint{
	"anthropic": {key: "https://api.anthropic.com/v1", oauth: "https://api.anthropic.com/v1"},
	"openai":    {key: "https://api.openai.com/v1", oauth: "https://chatgpt.com/backend-api/codex"},
}

// openCodeModelListWait bounds each of the two `model.list` calls of
// [openCodeEndpointCheck], whose answer is the whole catalogue (about 400 KiB).
//
// THIRTY SECONDS, about eight times the slowest measured: 3.9 s for a private
// process's (`--standalone`) and 2.8 s for the run's server's first, on a
// busy 2-core box, OpenCode 2.3.2. Past it the run does not start, as where
// the endpoints differ: an endpoint not checked is never assumed.
const openCodeModelListWait = 30 * time.Second

// openCodeModelRef is model's provider and the model within it, split at the
// first slash as OpenCode splits it; false where model names none.
func openCodeModelRef(model string) (provider, id string, ok bool) {
	provider, id, ok = strings.Cut(model, "/")
	return provider, id, ok && provider != "" && id != ""
}

// openCodeSignInKind is how the run signs in to provider: "oauth" where the
// login document the box was given ([Paths.CredentialDocument]) holds an
// active OAuth credential for it, which OpenCode uses over a key in the
// environment (measured on 2.3.2), else "key".
func openCodeSignInKind(ctx context.Context, box sandbox.Sandbox, paths Paths, provider string) (string, error) {
	doc := shellword.Quote(paths.CredentialDocument())
	res, err := box.Exec(ctx, "if [ -f "+doc+" ]; then cat "+doc+"; fi", sandbox.ExecOptions{})
	if err != nil {
		return "", fmt.Errorf("executor: reading the box's OpenCode login to pin its endpoint: %w", err)
	}
	var creds []struct {
		IntegrationID string `json:"integrationID"`
		Active        bool   `json:"active"`
		Value         struct {
			Type string `json:"type"`
		} `json:"value"`
	}
	if json.Unmarshal([]byte(res.Stdout), &creds) != nil {
		return "key", nil
	}
	for _, c := range creds {
		if c.IntegrationID == provider && c.Active && c.Value.Type == "oauth" {
			return "oauth", nil
		}
	}
	return "key", nil
}

// openCodeEndpointCheck is the part of a run's script that, for a model whose
// provider the run pins no endpoint for, refuses the run where the endpoint
// OpenCode resolves for the model with the repository's configuration loaded
// (the run's own server) differs from the one it resolves without it (a
// private process with the project's configuration off): its package and
// settings, baseURL among them, as `model.list` gives them. Through ans; each
// call bounded ([openCodeModelListWait]) and ended by an end request, after
// which nothing more is asked and no run starts. "" for a model naming no
// provider, or one the run pins ([openCodeEndpoints]).
func openCodeEndpointCheck(model, ans string, wait int) string {
	provider, id, ok := openCodeModelRef(model)
	if _, pinned := openCodeEndpoints[provider]; !ok || pinned {
		return ""
	}
	pattern := shellword.Quote(`"modelID":"` + basicRegexp(id) + `","providerID":"` + basicRegexp(provider) +
		`",[^{]*"settings":{[^}]*}`)
	clean := ans + ".clean"
	fail := `|| [ -n "$endasked" ] || { echo ` + shellword.Quote("crewlet: OpenCode could not list its models to "+
		"check the endpoint of "+model+", so the run does not start") + ` >&2; exit 1; }`
	return fmt.Sprintf(`if [ -z "$endasked" ]; then`+"\n"+
		`  bounded endpoint %[1]d e opencode api --server "$url" model.list > %[2]s 2>&1 %[3]s`+"\n"+
		`  bounded endpoint %[1]d e env OPENCODE_CONFIG_PROJECT_DISABLE=1 opencode api --standalone model.list > %[4]s 2>&1 %[3]s`+"\n"+
		`  if [ -z "$endasked" ] && [ "$(grep -o %[5]s %[2]s)" != "$(grep -o %[5]s %[4]s)" ]; then echo %[6]s >&2; exit 1; fi`+"\n"+
		`  rm -f %[2]s %[4]s`+"\n"+
		`fi`+"\n",
		wait, ans, fail, clean, pattern,
		shellword.Quote("crewlet: the repository's OpenCode configuration resolves "+model+" to an endpoint "+
			"other than OpenCode's own, so the run does not start; a run reaches its provider at the "+
			"endpoint OpenCode resolves without the repository"))
}

// basicRegexp is s with every character a POSIX basic regular expression
// reads as other than itself escaped.
func basicRegexp(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.[]*^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
