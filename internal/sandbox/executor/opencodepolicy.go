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

// A CONFIGURATION'S POLICY MAY NOT STOP THE RUN'S PLUGINS, SKILLS OR SERVER
// UNSEEN. OpenCode loads no plugin, lists no skill and starts no MCP server a
// `policies` entry ({action, resource, effect}) of a configuration document
// denies (`plugin.use`, `skill.use`, `mcp.use`, or `integration.use` on
// `<kind>:<target>`), with no word: the home's, a repository's `.opencode/` or
// root opencode.json, an OPENCODE_CONFIG_DIR's, and (read in its code, root's
// to write) /etc/opencode's (measured). Its rule, read from its code: the
// documents reversed, their policies flattened, the last matching rule
// deciding, so an earlier document's beats a later one's (measured both
// ways). The launch asks the box's OpenCode for those documents and refuses a
// run whose own they deny, or whose seat skill they deny its agent the
// `skill` permission for ([openCodePolicy]).

// openCodePolicyDoc is one entry of `opencode api config.get`'s answer. A
// document's `permission` and `agent.<name>.permission` come as
// `permissions` and `agents.<name>.permissions` rules (2.3.2 and 2.4.1).
type openCodePolicyDoc struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Info struct {
		Policies    []openCodePolicyRule `json:"policies"`
		Permissions []openCodePolicyRule `json:"permissions"`
		Agents      map[string]struct {
			Permissions []openCodePolicyRule `json:"permissions"`
		} `json:"agents"`
	} `json:"info"`
}

// openCodePolicyRule is one `policies` or permission entry, with the document
// and the key it is in.
type openCodePolicyRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
	doc, key string
}

// openCodePermissionDenies is the rule that decides the run's agent's
// permission for action on target where it denies it; false where none does.
// The order is OpenCode's own, as `opencode debug agents` lists the agent's
// rules (measured on 2.3.2 and 2.4.1): every document's `permission`, then
// the run's, then every document's `agent.build.permission`, then the run's
// agent's, the last rule whose action and resource glob match deciding.
func openCodePermissionDenies(docs []openCodePolicyDoc, top, agent []map[string]string, action,
	target string) (openCodePolicyRule, bool) {
	var rules []openCodePolicyRule
	run := func(rs []map[string]string, key string) {
		for _, r := range rs {
			rules = append(rules, openCodePolicyRule{Action: r["action"], Resource: r["resource"],
				Effect: r["effect"], doc: "the run's configuration", key: key})
		}
	}
	for _, d := range docs {
		for _, r := range d.Info.Permissions {
			r.doc, r.key = d.Path, "permission"
			rules = append(rules, r)
		}
	}
	run(top, "permissions")
	for _, d := range docs {
		for _, r := range d.Info.Agents[openCodeAgent].Permissions {
			r.doc, r.key = d.Path, "agent."+openCodeAgent+".permission"
			rules = append(rules, r)
		}
	}
	run(agent, "agents."+openCodeAgent+".permissions")
	for _, r := range slices.Backward(rules) {
		if openCodeGlob(r.Action).MatchString(action) && openCodeGlob(r.Resource).MatchString(target) {
			return r, r.Effect == "deny"
		}
	}
	return openCodePolicyRule{}, false
}

// openCodeUseActions is what an `integration.use` rule on `<kind>:<target>`
// is read as (measured for each on 2.3.2 and 2.4.1).
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
// run's plugins, its seat skills (skill.use) or its own MCP server (mcp.use),
// or its agent the `skill` permission for a seat skill under the run's own
// rules, top and agent, which hides the skill (each measured on 2.3.2 and
// 2.4.1), naming the document and the rule. An answer it cannot give (no
// opencode, a release without the call) is no verdict.
func openCodePolicy(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths,
	plugins []string, top, agent []map[string]string) error {
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
	deny := func(action, target, what string) {
		if r, denied := openCodePolicyDenies(docs, action, target); denied {
			said = append(said, fmt.Sprintf("%s's policies deny %s %q (effect deny), which stops %s", r.doc,
				r.Action, r.Resource, what))
		}
	}
	for _, p := range plugins {
		deny("plugin.use", p, "the run's plugin "+p)
	}
	for _, s := range req.Skills {
		deny("skill.use", s.Listed(), "the seat skill "+s.Listed())
		if r, denied := openCodePermissionDenies(docs, top, agent, "skill", s.Listed()); denied {
			said = append(said, fmt.Sprintf("%s's %s denies skill %q, which hides the seat skill %s from the "+
				"run's agent %s", r.doc, r.key, r.Resource, s.Listed(), openCodeAgent))
		}
	}
	for _, name := range crewletServers(req.MCPServers) {
		deny("mcp.use", name, "Crewlet's MCP server "+name+", which the run's tools go through")
	}
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: OpenCode would start without what the run brings of its own (its plugins, "+
		"skills or MCP server), and with no word: %s; remove that policy or permission rule, or allow it "+
		"after the deny", strings.Join(said, "; "))
}
