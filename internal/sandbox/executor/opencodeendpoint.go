package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// openCodeEndpoint is the endpoint a provider's requests reach, by how the run
// signs in to it: the key the entry names under env, or an OAuth login the
// box's document holds.
type openCodeEndpoint struct{ key, oauth, env string }

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
	"anthropic": {key: "https://api.anthropic.com/v1", oauth: "https://api.anthropic.com/v1", env: "ANTHROPIC_API_KEY"},
	"openai":    {key: "https://api.openai.com/v1", oauth: "https://chatgpt.com/backend-api/codex", env: "OPENAI_API_KEY"},
}

// openCodeListWait bounds each of the four listings of [openCodeConfigCheck]
// through the run's server, a `model.list` answer the whole catalogue (about
// 400 KiB).
//
// THIRTY SECONDS, about ten times the slowest measured: 2.8 s for the
// server's first `model.list`, 0.4 s after, on a busy 2-core box, OpenCode
// 2.3.2. Past it the run does not start, as where the listings differ: a
// configuration not checked is never assumed.
const openCodeListWait = 30 * time.Second

// openCodeModelRef is model's provider and the model within it, split at the
// first slash as OpenCode splits it; false where model names none.
func openCodeModelRef(model string) (provider, id string, ok bool) {
	provider, id, ok = strings.Cut(model, "/")
	return provider, id, ok && provider != "" && id != ""
}

// openCodeBoxLogin is the login document the box was given
// ([Paths.CredentialDocument]) as it stands, nil where it holds none.
func openCodeBoxLogin(ctx context.Context, box sandbox.Sandbox, paths Paths) ([]byte, error) {
	doc := shellword.Quote(paths.CredentialDocument())
	res, err := box.Exec(ctx, "if [ -f "+doc+" ]; then cat "+doc+"; fi", sandbox.ExecOptions{})
	if err != nil {
		return nil, fmt.Errorf("executor: reading the box's OpenCode login: %w", err)
	}
	if res.Stdout == "" {
		return nil, nil
	}
	return []byte(res.Stdout), nil
}

// openCodeSignInKind is how the run signs in to provider: "oauth" where login,
// the box's document, holds an active OAuth credential for it, which OpenCode
// uses over a key (measured on 2.3.2), else "key".
func openCodeSignInKind(login []byte, provider string) string {
	var creds []struct {
		IntegrationID string `json:"integrationID"`
		Active        bool   `json:"active"`
		Value         struct {
			Type string `json:"type"`
		} `json:"value"`
	}
	if json.Unmarshal(login, &creds) != nil {
		return "key"
	}
	for _, c := range creds {
		if c.IntegrationID == provider && c.Active && c.Value.Type == "oauth" {
			return "oauth"
		}
	}
	return "key"
}

// openCodeWithoutKeys is login without the credentials an earlier run
// imported from its keys ([openCodeKeyPrefix]), which the run's export
// carried into the box's document; false where it held none, or is no store
// document. Imported with the login, such a key could be the provider's
// active credential ahead of the login, and an earlier key.
func openCodeWithoutKeys(login []byte) ([]byte, bool) {
	var creds []json.RawMessage
	if json.Unmarshal(login, &creds) != nil {
		return login, false
	}
	kept := make([]json.RawMessage, 0, len(creds))
	for _, raw := range creds {
		var c struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &c) == nil && strings.HasPrefix(c.ID, openCodeKeyPrefix) {
			continue
		}
		kept = append(kept, raw)
	}
	if len(kept) == len(creds) {
		return login, false
	}
	out, err := json.MarshalIndent(kept, "", "  ")
	if err != nil {
		return login, false
	}
	return out, true
}

// openCodeConfigCheck is the part of a run's script that refuses the run
// where the repository's OpenCode configuration changes the providers or models
// OpenCode resolves: each of `provider.list` and `model.list` asked of the
// run's own server for the run's directory, repository loaded, and for `/`,
// which holds none, then compared entry by entry (ids, packages, settings,
// headers). Measured on 2.3.2: with no repository configuration the two agree
// past the directory they name; a provider the repository declares, a header,
// an `{env:…}` or `{file:…}` in a provider's options, or a model's own
// options make them differ. Through ans; each call bounded ([openCodeListWait])
// and ended by an end request, after which nothing more is asked and no run
// starts. The refusal names only providers OpenCode itself defines, never a
// value; every answer is removed, since one may hold what a reference read.
func openCodeConfigCheck(ans string, wait int) string {
	q := shellword.Quote
	failed := `|| [ -n "$endasked" ] || { rm -f ` + ans + `.*; echo ` + q("crewlet: OpenCode could not list "+
		"the providers and models its configuration resolves, so the run does not start") + ` >&2; exit 1; }`
	call := func(list, at, file string) string {
		return fmt.Sprintf(`  bounded check %d e opencode api --server "$url" %s%s > %s 2>&1 %s`+"\n",
			wait, list, at, ans+file, failed)
	}
	root := ` --param 'location[directory]=/'`
	split := `sed 's/^{"location":{"directory":"[^"]*"},//; s/},{"id":"/}\` + "\n" + `{"id":"/g'`
	var b strings.Builder
	b.WriteString(`if [ -z "$endasked" ]; then` + "\n")
	b.WriteString(call("provider.list", "", ".pl") + call("provider.list", root, ".pc") +
		call("model.list", "", ".ml") + call("model.list", root, ".mc"))
	fmt.Fprintf(&b, `  if [ -z "$endasked" ]; then`+"\n"+
		`    for f in %[1]s.pl %[1]s.pc %[1]s.ml %[1]s.mc; do %[2]s "$f" > "$f.lines"; done`+"\n"+
		`    { grep -vxF -f %[1]s.pc.lines %[1]s.pl.lines; grep -vxF -f %[1]s.pl.lines %[1]s.pc.lines; `+
		`grep -vxF -f %[1]s.mc.lines %[1]s.ml.lines; grep -vxF -f %[1]s.ml.lines %[1]s.mc.lines; } > %[1]s.diff`+"\n"+
		`    if [ -s %[1]s.diff ]; then`+"\n"+
		`      named=; for id in $(grep -o '"id":"[A-Za-z0-9._-]*","integrationID"' %[1]s.pc.lines | sed 's/^"id":"\([^"]*\)".*/\1/'); do `+
		`if grep -qF "\"id\":\"$id\",\"integrationID\"" %[1]s.diff || grep -qF "\"providerID\":\"$id\"" %[1]s.diff; then named="$named $id"; fi; done`+"\n"+
		`      n=$(grep -c . %[1]s.diff); rm -f %[1]s.*`+"\n"+
		`      echo "crewlet: the repository's OpenCode configuration changes $n of the providers and models OpenCode resolves (providers:${named:- none OpenCode defines}), so the run does not start: a run in a box reaches its providers as OpenCode defines them" >&2; exit 1`+"\n"+
		`    fi`+"\n"+
		`  fi`+"\n"+
		`  rm -f %[1]s.*`+"\n", ans, split)
	b.WriteString("fi\n")
	return b.String()
}

// openCodeKeyPrefix begins the id of every credential the run imports from
// the entry's keys ([openCodeStoreKeys]), never an id a login of OpenCode's
// own takes.
const openCodeKeyPrefix = "crewlet-key-"

// openCodeStoreKeys is the run's keys for the providers [openCodeEndpoints]
// pins, as a store document the run imports ([Paths.OpenCodeKeys]), from the
// variables its environment does not carry ([OpenCode.storeSignIn]): a key
// the run signs in by stays out of OpenCode's environment, so no
// configuration's `{env:…}` reads it. Measured on 2.3.2: a key entry in the
// store, and no variable, signs in Anthropic and OpenAI. Nil where the run
// signs in by none of them.
func openCodeStoreKeys(req sandbox.RunRequest) ([]byte, error) {
	type value struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	type credential struct {
		ID            string `json:"id"`
		IntegrationID string `json:"integrationID"`
		Label         string `json:"label"`
		Active        bool   `json:"active"`
		Value         value  `json:"value"`
	}
	var doc []credential
	for _, provider := range slices.Sorted(maps.Keys(openCodeEndpoints)) {
		name := openCodeEndpoints[provider].env
		key := req.Env[name]
		if key == "" || !slices.Contains(req.SignInEnv, name) {
			continue
		}
		doc = append(doc, credential{ID: openCodeKeyPrefix + provider, IntegrationID: provider, Label: name,
			Active: true, Value: value{Type: "key", Key: key}})
	}
	if len(doc) == 0 {
		return nil, nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("executor: the run's provider keys as an OpenCode login: %w", err)
	}
	return out, nil
}

// storeSignIn is the variables of names OpenCode signs the run in by from its
// credential store rather than its environment ([openCodeStoreKeys]).
func (OpenCode) storeSignIn(names []string) []string {
	var out []string
	for _, provider := range slices.Sorted(maps.Keys(openCodeEndpoints)) {
		if name := openCodeEndpoints[provider].env; slices.Contains(names, name) {
			out = append(out, name)
		}
	}
	return out
}
