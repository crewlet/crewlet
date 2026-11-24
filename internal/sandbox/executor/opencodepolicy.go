package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A CONFIGURATION'S POLICY MAY NOT STOP THE RUN'S PLUGINS UNSEEN. OpenCode
// 2.3.2 loads no plugin a `policies` entry ({action, resource, effect}) of a
// configuration document denies with `plugin.use` (or `integration.use` on
// `plugin:<target>`), with no word: the home's, a repository's `.opencode/` or
// root opencode.json, an OPENCODE_CONFIG_DIR's, and (read in its code, root's
// to write) /etc/opencode's (measured). Its rule is read from its code:
// documents in the order `config.get` lists them, reversed, their policies
// flattened, the last rule whose action and resource glob match deciding; so
// within a document the last match wins and an earlier document's beats a
// later one's (measured both ways). The launch asks the box's OpenCode for
// those documents and refuses a run whose plugin they deny ([openCodePolicy]).

// openCodePolicyDoc is one entry of `opencode api config.get`'s answer.
type openCodePolicyDoc struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Info struct {
		Policies []openCodePolicyRule `json:"policies"`
	} `json:"info"`
}

// openCodePolicyRule is one `policies` entry, with the document it is in.
type openCodePolicyRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
	doc      string
}

// openCodeUseActions is what an `integration.use` rule on `<kind>:<target>`
// is read as (measured for plugin, read for the rest).
var openCodeUseActions = map[string]string{"mcp": "mcp.use", "skill": "skill.use", "plugin": "plugin.use"}

// openCodePolicyDenies is the rule of docs, `config.get`'s answer, that
// decides action on target where it denies it; false where none does.
func openCodePolicyDenies(docs []openCodePolicyDoc, action, target string) (openCodePolicyRule, bool) {
	var rules []openCodePolicyRule
	for _, d := range slices.Backward(docs) {
		if d.Type != "document" {
			continue
		}
		for _, r := range d.Info.Policies {
			if r.Action == "integration.use" {
				if kind, rest, ok := strings.Cut(r.Resource, ":"); ok && openCodeUseActions[kind] != "" {
					r.Action, r.Resource = openCodeUseActions[kind], rest
				}
			}
			r.doc = d.Path
			rules = append(rules, r)
		}
	}
	for _, r := range slices.Backward(rules) {
		if r.Action == action && openCodeGlob(r.Resource).MatchString(strings.ReplaceAll(target, `\`, "/")) {
			return r, r.Effect == "deny"
		}
	}
	return openCodePolicyRule{}, false
}

// openCodeGlob is pattern as OpenCode 2.3.2 matches a resource with it (read
// in its code): `\` as `/`, `*` any run of characters, `?` one, a trailing
// ` *` optional, the whole value, case kept.
func openCodeGlob(pattern string) *regexp.Regexp {
	var b strings.Builder
	for _, r := range strings.ReplaceAll(pattern, `\`, "/") {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	s := b.String()
	if rest, ok := strings.CutSuffix(s, " .*"); ok {
		s = rest + "( .*)?"
	}
	return regexp.MustCompile("(?s)^" + s + "$")
}

// openCodePolicy refuses req's run where the box's OpenCode, asked in the
// run's environment for its configuration documents, would deny one of the
// run's plugins, naming the document and the rule. An answer it cannot give
// (no opencode, a release without the call) is no verdict.
func openCodePolicy(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths,
	plugins []string) error {
	res, err := box.Exec(ctx, "sh -lc "+shellword.Quote(`PATH=`+shellword.Quote(paths.BinDir())+`:"$PATH"; `+
		`unset OPENCODE_CONFIG_CONTENT; opencode api --standalone config.get < /dev/null 2> /dev/null`),
		sandbox.ExecOptions{Env: req.Env, TimeoutSec: openCodePointWait.Seconds()})
	if err != nil {
		return fmt.Errorf("executor: asking the box's OpenCode for its configuration's policies: %w", err)
	}
	var docs []openCodePolicyDoc
	if answered := res.ExitCode == 0 && json.Unmarshal([]byte(res.Stdout), &docs) == nil; !answered {
		return nil
	}
	var said []string
	for _, p := range plugins {
		if r, denied := openCodePolicyDenies(docs, "plugin.use", p); denied {
			said = append(said, fmt.Sprintf("%s's policies deny %s %q (effect deny), which stops the run's plugin %s",
				r.doc, r.Action, r.Resource, p))
		}
	}
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: OpenCode would start without the run's own plugins, which carry its notes, "+
		"skill loads and command environment, and with no word: %s; remove that policy or allow the plugin "+
		"after it", strings.Join(said, "; "))
}
