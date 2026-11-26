package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
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
// `permissions` and `agents.<name>.permissions` rules (measured on 2.3.2).
type openCodePolicyDoc struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Info struct {
		Policies    []openCodePolicyRule `json:"policies"`
		Plugins     []json.RawMessage    `json:"plugins"`
		Permissions []openCodePolicyRule `json:"permissions"`
		Agents      map[string]struct {
			Disabled    *bool                `json:"disabled"`
			Permissions []openCodePolicyRule `json:"permissions"`
		} `json:"agents"`
		MCP struct {
			Servers map[string]struct {
				Disabled *bool `json:"disabled"`
			} `json:"servers"`
		} `json:"mcp"`
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
// rules (measured on 2.3.2): every document's `permission`, then the run's
// (top), then every document's `agent.build.permission`, then the run's
// agent's, the last rule whose action and resource glob match deciding.
func openCodePermissionDenies(docs []openCodePolicyDoc, top, agent []openCodePolicyRule, action,
	target string) (openCodePolicyRule, bool) {
	var rules []openCodePolicyRule
	for _, d := range docs {
		for _, r := range d.Info.Permissions {
			r.doc, r.key = d.Path, "permission"
			rules = append(rules, r)
		}
	}
	rules = append(rules, top...)
	for _, d := range docs {
		for _, r := range d.Info.Agents[openCodeAgent].Permissions {
			r.doc, r.key = d.Path, "agent."+openCodeAgent+".permission"
			rules = append(rules, r)
		}
	}
	rules = append(rules, agent...)
	for _, r := range slices.Backward(rules) {
		if openCodeGlob(r.Action).MatchString(action) && openCodeGlob(r.Resource).MatchString(target) {
			return r, r.Effect == "deny"
		}
	}
	return openCodePolicyRule{}, false
}

// openCodePersonsRules is every rule of docs' own top-level `permission` and
// `permissions` (config.get reports both as one list, ranked alike), in their
// order, an `ask` as `allow`: the run lays them after its own `*` allow, so a
// person's deny still decides and what would ask a person a headless run
// cannot answer is allowed (measured on 2.3.2: a home's, a repository's and an
// OPENCODE_CONFIG_DIR's deny, either form, a pattern's, and a narrower ask
// after a deny, which a copy of the deny alone would deny).
func openCodePersonsRules(docs []openCodePolicyDoc) []openCodePolicyRule {
	var rules []openCodePolicyRule
	for _, d := range docs {
		if d.Type != "document" {
			continue
		}
		for _, r := range d.Info.Permissions {
			if r.Effect == "ask" {
				r.Effect = "allow"
			}
			r.doc, r.key = d.Path, "permission"
			rules = append(rules, r)
		}
	}
	return rules
}

// openCodeRunRules is rules as the run's own, under key, and
// openCodeRuleList them as its configuration writes them.
func openCodeRunRules(key string, rules ...map[string]string) []openCodePolicyRule {
	out := make([]openCodePolicyRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, openCodePolicyRule{Action: r["action"], Resource: r["resource"], Effect: r["effect"],
			doc: "the run's configuration", key: key})
	}
	return out
}

func openCodeRuleList(rules []openCodePolicyRule) []map[string]string {
	out := make([]map[string]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, map[string]string{"action": r.Action, "resource": r.Resource, "effect": r.Effect})
	}
	return out
}

// openCodePluginIDs is each of the run's plugins' id, by its directory's
// name, read off the plugin's own source (opencode_steer.js,
// opencode_skills.js, opencode_toolenv.js, opencode_checkouts.js) so the two
// cannot disagree.
var openCodePluginIDs = map[string]string{
	path.Base(Paths{}.OpenCodeSteerPlugin()):     openCodePluginID(openCodeSteerPlugin),
	path.Base(Paths{}.OpenCodeSkillsPlugin()):    openCodePluginID(openCodeSkillsPlugin),
	path.Base(Paths{}.OpenCodeToolEnvPlugin()):   openCodePluginID(openCodeToolEnvPlugin),
	path.Base(Paths{}.OpenCodeCheckoutsPlugin()): openCodePluginID(openCodeCheckoutsPlugin),
}

// openCodePluginIDLine is a plugin source's own `id: "<id>",` line.
var openCodePluginIDLine = regexp.MustCompile(`(?m)^\s*id:\s*"([^"]+)",?\s*$`)

// openCodePluginID is the id a plugin's source declares, "" unless it
// declares exactly one.
func openCodePluginID(src []byte) string {
	if m := openCodePluginIDLine.FindAllSubmatch(src, -1); len(m) == 1 {
		return string(m[0][1])
	}
	return ""
}

// openCodeIDMatches is whether a `plugins` entry's pattern names the plugin
// id as OpenCode matches one: the id, `*`, or `<prefix>.*` that the id starts
// with (measured on 2.3.2 and 2.4.1: `-crewlet.toolenv`, `-crewlet.*` and
// `-*` removed crewlet.toolenv; its path, `file://` or `/index.js` forms,
// `crewlet*`, `*.toolenv`, `crewlet.toolenv.*`, another case and an object's
// `package` removed nothing).
func openCodeIDMatches(pattern, id string) bool {
	prefix, glob := strings.CutSuffix(pattern, ".*")
	return pattern == id || pattern == "*" || (glob && strings.HasPrefix(id, prefix+"."))
}

// openCodeUseActions is what an `integration.use` rule on `<kind>:<target>`
// is read as (measured for each on 2.3.2).
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

// openCodeDocMark opens config.get's answer in [openCodeDocs]' output, past
// whatever a login profile prints, and openCodeDocEnd closes it with its exit.
const (
	openCodeDocMark = "crewlet-config-get"
	openCodeDocEnd  = "crewlet-config-get-exit "
)

// openCodeDocs is the configuration documents the box's OpenCode, asked in
// the run's environment without the run's own content, answers config.get
// with; none for a box with no opencode, whose script refuses it. An opencode
// that answers no JSON (an error, a timeout, a release without the call)
// refuses the run, naming its exit and its error stream: without the
// documents, the run would override every deny of a person's.
func openCodeDocs(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest,
	paths Paths) ([]openCodePolicyDoc, error) {
	res, err := box.Exec(ctx, "sh -lc "+shellword.Quote(`PATH=`+shellword.Quote(paths.BinDir())+`:"$PATH"; `+
		`unset OPENCODE_CONFIG_CONTENT; command -v opencode > /dev/null 2>&1 || exit 0; e=$(mktemp) || exit 1; `+
		`echo `+openCodeDocMark+`; opencode api --standalone config.get < /dev/null 2> "$e"; c=$?; `+
		`echo; echo "`+openCodeDocEnd+`$c"; head -c 400 "$e"; rm -f "$e"`),
		sandbox.ExecOptions{Env: req.Env, TimeoutSec: openCodePointWait.Seconds()})
	if err != nil {
		return nil, fmt.Errorf("executor: asking the box's OpenCode for its configuration's policies: %w", err)
	}
	_, out, marked := strings.Cut(res.Stdout, openCodeDocMark+"\n")
	if !marked {
		if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("executor: the box's OpenCode was not asked for its configuration (exit %d: %s), so "+
			"the run cannot keep a person's own permission rules", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	body, tail, ended := strings.Cut(out, "\n"+openCodeDocEnd)
	code, stderr, _ := strings.Cut(tail, "\n")
	var docs []openCodePolicyDoc
	if answered := ended && code == "0" && json.Unmarshal([]byte(body), &docs) == nil; !answered {
		if !ended {
			code = fmt.Sprintf("none, the shell's %d", res.ExitCode)
		}
		return nil, fmt.Errorf("executor: the box's OpenCode gave no configuration documents for `opencode api "+
			"--standalone config.get` (exit %s: %q), so the run cannot keep a person's own permission rules and "+
			"policies; fix that opencode or its configuration", code, firstLine(stderr, transcriptDetailLimit))
	}
	return docs, nil
}

// openCodeAgentRules is the run's agent's rules as the box's OpenCode itself
// resolves them (`opencode debug agents --standalone`, in the run's
// environment with only the run's own top rules, top, as its content), so
// the rules of an agent file config.get does not list (`.opencode/agent/` or
// `agents/build.md`, measured on 2.3.2: its deny applied, config.get named
// none of it) are judged too; none for a box with no opencode. One that
// answers no list refuses the run, as config.get's absence does.
func openCodeAgentRules(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths,
	top []openCodePolicyRule) ([]openCodePolicyRule, error) {
	content, err := json.Marshal(map[string]any{"permissions": openCodeRuleList(top)})
	if err != nil {
		return nil, fmt.Errorf("executor: encoding the run's rules for OpenCode: %w", err)
	}
	env := maps.Clone(req.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["OPENCODE_CONFIG_CONTENT"] = string(content)
	res, err := box.Exec(ctx, "sh -lc "+shellword.Quote(`PATH=`+shellword.Quote(paths.BinDir())+`:"$PATH"; `+
		`command -v opencode > /dev/null 2>&1 || exit 0; e=$(mktemp) || exit 1; `+
		`echo `+openCodeDocMark+`; opencode debug agents --standalone < /dev/null 2> "$e"; c=$?; `+
		`echo; echo "`+openCodeDocEnd+`$c"; head -c 400 "$e"; rm -f "$e"`),
		sandbox.ExecOptions{Env: env, TimeoutSec: openCodePointWait.Seconds()})
	if err != nil {
		return nil, fmt.Errorf("executor: asking the box's OpenCode for its agent's rules: %w", err)
	}
	_, out, marked := strings.Cut(res.Stdout, openCodeDocMark+"\n")
	if !marked {
		if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("executor: the box's OpenCode was not asked for its agent's rules (exit %d: %s)",
			res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	body, tail, ended := strings.Cut(out, "\n"+openCodeDocEnd)
	code, stderr, _ := strings.Cut(tail, "\n")
	var agents []struct {
		ID          string               `json:"id"`
		Permissions []openCodePolicyRule `json:"permissions"`
	}
	if answered := ended && code == "0" && json.Unmarshal([]byte(body), &agents) == nil; !answered {
		if !ended {
			code = fmt.Sprintf("none, the shell's %d", res.ExitCode)
		}
		return nil, fmt.Errorf("executor: the box's OpenCode gave no agent rules for `opencode debug agents "+
			"--standalone` (exit %s: %q), so the run cannot keep a person's own permission rules; fix that "+
			"opencode or its configuration", code, firstLine(stderr, transcriptDetailLimit))
	}
	for _, a := range agents {
		if a.ID != openCodeAgent {
			continue
		}
		for i := range a.Permissions {
			a.Permissions[i].doc = "OpenCode's own rules for the agent " + openCodeAgent + " (`opencode debug agents`)"
		}
		return a.Permissions, nil
	}
	return nil, nil
}

// openCodeRulesDeny is the last of rules whose action and resource match,
// where it denies.
func openCodeRulesDeny(rules []openCodePolicyRule, action, target string) (openCodePolicyRule, bool) {
	for _, r := range slices.Backward(rules) {
		if openCodeGlob(r.Action).MatchString(action) && openCodeGlob(r.Resource).MatchString(target) {
			return r, r.Effect == "deny"
		}
	}
	return openCodePolicyRule{}, false
}

// openCodeDenial says that r, a policy denying something of the run's, stops
// what.
func openCodeDenial(r openCodePolicyRule, what string) string {
	return fmt.Sprintf("%s's policies deny %s %q (effect deny), which stops %s", r.doc, r.Action, r.Resource, what)
}

// openCodePluginStopped is what in docs keeps the run's plugin p from
// loading: a policy denying plugin.use on it, or a document's removal of it by
// a pattern naming its id ([openCodeIDMatches]), which the run's own entries
// after it would undo (2.3.2 keeps the removal as written in config.get).
func openCodePluginStopped(docs []openCodePolicyDoc, p string) []string {
	var said []string
	if r, denied := openCodePolicyDenies(docs, "plugin.use", p); denied {
		said = append(said, openCodeDenial(r, "the run's plugin "+p))
	}
	for _, d := range docs {
		for _, raw := range d.Info.Plugins {
			var entry string
			if json.Unmarshal(raw, &entry) != nil {
				continue
			}
			if removed, ok := strings.CutPrefix(entry, "-"); ok && openCodeIDMatches(removed, openCodePluginIDs[path.Base(p)]) {
				said = append(said, fmt.Sprintf("%s's plugins remove %q, the run's plugin %s, which the run never "+
					"loads back", d.Path, entry, p))
			}
		}
	}
	return said
}

// openCodePolicy refuses req's run where docs ([openCodeDocs]) would deny
// one of the run's plugins, its seat skills (skill.use) or its own MCP server
// (mcp.use), or, under the run's own rules (top and agent), its agent the
// `skill` permission for a seat skill, which hides it, or a tool of the
// bridge, ranked without the run's agent rules, which would otherwise
// override it (each measured on 2.3.2): naming the document and the rule.
func openCodePolicy(docs []openCodePolicyDoc, req sandbox.RunRequest, plugins []string, top,
	agent, resolved []openCodePolicyRule) error {
	var said []string
	deny := func(action, target, what string) {
		if r, denied := openCodePolicyDenies(docs, action, target); denied {
			said = append(said, openCodeDenial(r, what))
		}
	}
	for _, p := range plugins {
		said = append(said, openCodePluginStopped(docs, p)...)
	}
	for _, s := range req.Skills {
		deny("skill.use", s.Listed(), "the seat skill "+s.Listed())
		if r, denied := openCodePermissionDenies(docs, top, agent, "skill", s.Listed()); denied {
			said = append(said, fmt.Sprintf("%s's %s denies skill %q, which hides the seat skill %s from the "+
				"run's agent %s", r.doc, r.key, r.Resource, s.Listed(), openCodeAgent))
		} else if r, denied := openCodeRulesDeny(resolved, "skill", s.Listed()); denied {
			said = append(said, fmt.Sprintf("%s deny skill %q, which hides the seat skill %s: an agent file "+
				"config.get does not list (.opencode/agent/%s.md or agents/%s.md) holds that rule", r.doc,
				r.Resource, s.Listed(), openCodeAgent, openCodeAgent))
		}
	}
	// A DOCUMENT'S SWITCH OF THE RUN'S AGENT OR SERVER, which the run's own
	// entry of that name replaces whole (measured on 2.3.2: the bridge and the
	// agent ran under the home's `enabled: false` and `disable: true`), so the
	// run would switch back on what a person switched off. The last document
	// that sets it decides, as OpenCode merges them in config.get's order.
	agentOff := ""
	serverOff := map[string]string{}
	for _, d := range docs {
		if d.Type != "document" {
			continue
		}
		if v := d.Info.Agents[openCodeAgent].Disabled; v != nil {
			agentOff = map[bool]string{true: d.Path, false: ""}[*v]
		}
		for _, name := range crewletServers(req.MCPServers) {
			if v := d.Info.MCP.Servers[name].Disabled; v != nil {
				serverOff[name] = map[bool]string{true: d.Path, false: ""}[*v]
			}
		}
	}
	if agentOff != "" {
		said = append(said, fmt.Sprintf("%s switches the agent %s off (agent.%s.disable), the agent the run is",
			agentOff, openCodeAgent, openCodeAgent))
	}
	for _, name := range crewletServers(req.MCPServers) {
		if serverOff[name] != "" {
			said = append(said, fmt.Sprintf("%s switches Crewlet's MCP server %s off (mcp.%s.enabled false)",
				serverOff[name], name, name))
		}
	}
	for _, name := range crewletServers(req.MCPServers) {
		deny("mcp.use", name, "Crewlet's MCP server "+name+", which the run's tools go through")
		// A tool the bridge has not listed yet is held to by a name no rule
		// spells, which only a pattern covering every tool of the server matches.
		for _, tool := range append(slices.Clone(req.MCPServers[name].Tools), "\x00") {
			action := openCodeToolPrefix(name) + "_" + tool
			if r, denied := openCodePermissionDenies(docs, top, nil, action, "*"); denied {
				said = append(said, fmt.Sprintf("%s's %s denies %q, which takes Crewlet's MCP server %s's tools away",
					r.doc, r.key, r.Action, name))
				break
			}
			if r, denied := openCodeRulesDeny(resolved, action, "*"); denied {
				said = append(said, fmt.Sprintf("%s deny %q, which takes Crewlet's MCP server %s's tools away: an "+
					"agent file config.get does not list holds that rule", r.doc, r.Action, name))
				break
			}
		}
	}
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: OpenCode would start without what the run brings of its own (its plugins, "+
		"skills or MCP server), and with no word: %s; remove that policy or permission rule, or allow it "+
		"after the deny", strings.Join(said, "; "))
}
