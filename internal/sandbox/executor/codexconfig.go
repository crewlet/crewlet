package executor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// codexMCPStartupWait is how long Codex waits for a required MCP server to
// finish its handshake before the run fails ([codexConfigTOML]): SIXTY
// SECONDS, the wait OpenCode's script gives the same bridge
// ([openCodeMCPWait]), since the box and the bridge are the same. Codex's own
// default is thirty (measured on 0.171.0: a 20-second handshake passed, a
// 35-second one failed at 30 s, and a 50-second one passed under sixty),
// which a box dialling a busy node over its network can miss, and a required
// server that misses it fails the run before its first step.
const codexMCPStartupWait = 60 * time.Second

// codexConfigTOML is a Codex run's [Paths.CodexConfig]: its compaction point,
// its standing
// instructions as `developer_instructions` (the developer message after
// Codex's own prompt, which it keeps), and its MCP servers, every value a TOML
// basic string, array or inline table and every key quoted ([tomlString]).
// THE ENGINE'S OWN TOOLS (sandbox.MCPServer.AlwaysLoaded) ARE REQUIRED: Codex
// drops a server it cannot reach without a word and runs on with no tools,
// where `required` fails the run naming it; they are offered on every request,
// never behind Codex's tool search, where the model saw none (measured).
// Every server's calls are approved unasked and made at once, as the codex
// profile measured.
func codexConfigTOML(servers map[string]sandbox.MCPServer, system, model string) string {
	var b strings.Builder
	// THE COMPACTION POINT PINNED where the engine judges a continuation
	// ([codexCompactLimit]).
	fmt.Fprintf(&b, "model_auto_compact_token_limit = %d\n", codexCompactLimit(model))
	if system != "" {
		fmt.Fprintf(&b, "developer_instructions = %s\n", tomlString(system))
	}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		s := servers[name]
		fmt.Fprintf(&b, "\n[mcp_servers.%s]\n", tomlString(name))
		if s.Transport == sandbox.TransportHTTP {
			fmt.Fprintf(&b, "url = %s\n", tomlString(s.URL))
			if len(s.Headers) > 0 {
				fmt.Fprintf(&b, "http_headers = %s\n", tomlTable(s.Headers))
			}
		} else {
			fmt.Fprintf(&b, "command = %s\n", tomlString(s.Command))
			if len(s.Args) > 0 {
				fmt.Fprintf(&b, "args = %s\n", tomlArray(s.Args))
			}
			if len(s.Env) > 0 {
				fmt.Fprintf(&b, "env = %s\n", tomlTable(s.Env))
			}
		}
		if secs := waitSeconds(s.CallTimeout); secs > 0 {
			fmt.Fprintf(&b, "tool_timeout_sec = %d\n", secs)
		}
		if s.AlwaysLoaded {
			fmt.Fprintf(&b, "required = true\nstartup_timeout_sec = %d\n", waitSeconds(codexMCPStartupWait))
			b.WriteString("omit_tools_from = [\"deferred\"]\n")
		}
		b.WriteString("supports_parallel_tool_calls = true\ndefault_tools_approval_mode = \"approve\"\n")
	}
	return b.String()
}

// waitSeconds is d in whole seconds, rounded up so a sub-second wait is never
// none; 0 for none.
func waitSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// codexPosture is the run's posture as app-server `-c` overrides, TOML and
// shell-quoted: on argv, the top configuration layer, so neither
// [Paths.CodexConfig] nor a repository's configuration moves it. The sandbox
// off and approval never (a box cannot make the user namespaces Codex's
// sandbox needs, and nobody answers); no analytics; no request_user_input (a
// question parks through ask_and_wait); Crewlet's own servers enabled with no
// tool disabled, over a trusted repository's `.codex/config.toml` (measured
// on 0.171.0: it otherwise switches them off); and the model's
// shell inherits the run's environment less [codexShellExcluded], keeping
// PATH, HOME, GIT_CONFIG_*, the sign-in and the seat's tool credentials.
func codexPosture(req sandbox.RunRequest) []string {
	set := []string{
		`sandbox_mode="danger-full-access"`,
		`approval_policy="never"`,
		`analytics.enabled=false`,
		`tools.experimental_request_user_input.enabled=false`,
		`shell_environment_policy.inherit="all"`,
		`shell_environment_policy.exclude=` + tomlArray(codexShellExcluded()),
	}
	for _, name := range crewletServers(req.MCPServers) {
		set = append(set, "mcp_servers."+name+".enabled=true", "mcp_servers."+name+".disabled_tools=[]")
	}
	// THE ENTRY'S WEB TOOLS OFF where it denies them, as its runs on the engine
	// host have none: web search is Codex's one web tool, and with `disabled` the
	// request offers none (measured through the app-server on 0.171.0, under
	// this posture; without it the request offers `web_search`). A hosted tool:
	// Codex runs nothing for it, so none is left to refuse. The shell still
	// reaches whatever the box's network does.
	if req.WebDenied {
		set = append(set, `web_search="disabled"`)
	}
	// THE ENTRY'S MODEL AND EFFORT AS WRITTEN, as its runs on the engine host
	// hand them (the codex profile's model_args and effort).
	if req.Model != "" {
		set = append(set, "model="+tomlString(req.Model))
	}
	if req.Effort != "" {
		set = append(set, "model_reasoning_effort="+tomlString(req.Effort))
	}
	out := make([]string, 0, 2*len(set))
	for _, kv := range set {
		out = append(out, "-c", shellword.Quote(kv))
	}
	return out
}

// codexShellExcluded is what the commands a Codex run's model runs start
// without (its shell_environment_policy.exclude): the note helper's routes,
// which lead to the run's bridge token, so that a tool the model runs does
// not pick one up by accident. No protection: the run's `config.toml` holds
// the token (code-sandbox.md, "Credentials and the run environment").
func codexShellExcluded() []string { return []string{codexNotesEnv, codexSkillsEnv} }

// tomlString is s as a TOML basic string: `"` and `\` escaped, every control
// character as an escape, and bytes that are not UTF-8 as U+FFFD, which TOML
// would refuse.
func tomlString(s string) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteString(`\u` + fmt.Sprintf("%04X", r))
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlArray is values as a TOML array of basic strings.
func tomlArray(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = tomlString(v)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// tomlTable is m as a TOML inline table of basic strings, in key order.
func tomlTable(m map[string]string) string {
	pairs := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		pairs = append(pairs, tomlString(k)+" = "+tomlString(m[k]))
	}
	return "{ " + strings.Join(pairs, ", ") + " }"
}

// WriteConfig holds the home's own configuration ([codexHomeConfig]), refuses
// a run whose server a repository changes ([codexGuard]), and writes what a Codex run starts from: that configuration with the run's over
// it ([codexConfigOver]) for the session it was asked to work in and, where it
// was handed two wirings, for one of its own; for a run wired
// to its notes, the note helper's build for the box and its notes route
// ([Paths.CodexNotesURL]); and its own skills ([writeCodexSkills]). It
// writes no path the harness is pointed at: the script moves the
// configuration into place once the session is settled ([codexSessionGate]).
//
// THE STANDING INSTRUCTIONS ARE TOLD OF NOTES on a run wired to them
// ([withNoteInstruction]), and a run that continues a session has them in its
// renew config too ([Paths.CodexRenew]).
func (c Codex) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, session Session,
	paths Paths) (string, error) {
	// EVERY REFUSAL BEFORE THE RUN WRITES ITS OWN, a home configuration that
	// does not read and the guard's included: a refused launch leaves the box
	// as it found it, less an earlier run's leftovers ([codexHomeConfig]).
	if n := len(paths.CodexSocket()); n > maxCodexSocketPath {
		return "", fmt.Errorf("executor: a Codex run's socket, %s, is %d bytes, past the %d a unix socket's "+
			"path holds; a local box's home is under providers.sandbox.local.state_dir, which needs a "+
			"shorter path", paths.CodexSocket(), n, maxCodexSocketPath)
	}
	if f := req.FreshWiring; f != nil && (f.Steer.Set() != req.Steer.Set() || (len(req.Skills) > 0 && !f.SkillLoads.Set())) {
		return "", errors.New("executor: the run's two wirings, for the session it continues and for " +
			"one of its own, do not reach the same routes, so the run cannot be wired to whichever " +
			"session its box starts")
	}
	skills, err := codexSkillFiles(req.Skills)
	if err != nil {
		return "", err
	}
	home, err := codexHomeConfig(ctx, box, paths)
	if err != nil {
		return "", err
	}
	// THE RUN'S SKILLS FIRST, so the home's switches are held to their files
	// as Codex resolves a path ([codexSkillPathsSame]).
	if err = writeCodexSkills(ctx, box, skills, paths); err != nil {
		return "", err
	}
	if entries := codexSkillEntries(home); len(skills) > 0 && len(entries) > 0 {
		same, sameErr := codexSkillPathsSame(ctx, box, entries, skills, paths)
		if sameErr != nil {
			return "", sameErr
		}
		if err = codexSkillsOff(entries, skills, same); err != nil {
			return "", err
		}
	}
	if err = codexServersOff(home, req.MCPServers); err != nil {
		return "", err
	}
	system := req.System
	if req.Steer.Set() {
		system = withNoteInstruction(system, c.NoteInstruction())
	}
	config, err := codexConfigOver(home, codexConfigTOML(req.MCPServers, system, req.Model))
	if err != nil {
		return "", err
	}
	// THE HELPER IN EVERY BOX ITS MACHINE HAS A BUILD FOR: the nested hook
	// runs it ([codexNestedHookFlags]), so the guard holds the hook's switches
	// only where it is written. A run that needs it for its notes, cap or
	// skills is refused without one; any other starts without the hook.
	helperErr := writeNoteHelper(ctx, box, paths.CodexNoteHelper())
	if helperErr != nil && (codexHelped(req) || !errors.Is(helperErr, errNoHelperBuild)) {
		return "", helperErr
	}
	docLimit, said, err := codexGuard(ctx, box, req, paths, home, helperErr == nil)
	if err != nil {
		return "", err
	}
	var fresh string
	if f := req.FreshWiring; f != nil {
		if fresh, err = codexConfigOver(home, codexConfigTOML(f.MCPServers, system, req.Model)); err != nil {
			return "", err
		}
	}
	if err = holdHome(ctx, box, paths.CodexLiveConfig(), paths.CodexHomeConfig()); err != nil {
		return "", err
	}
	if err = box.WriteFile(ctx, paths.CodexConfig(), []byte(config)); err != nil {
		return "", fmt.Errorf("executor: writing Codex's configuration: %w", err)
	}
	if session.Continues {
		if err = writeJSON(ctx, box, paths.CodexRenew(), map[string]string{"instructions": system}); err != nil {
			return "", fmt.Errorf("executor: writing the renew config of the session the run continues: %w", err)
		}
	}
	if f := req.FreshWiring; f != nil {
		if err = box.WriteFile(ctx, paths.CodexConfigFresh(), []byte(fresh)); err != nil {
			return "", fmt.Errorf("executor: writing Codex's configuration for a session of the run's own: %w", err)
		}
		if err = writeCodexRoutes(ctx, box, req, *f, paths.CodexNotesURLFresh(), paths.CodexSkillsURLFresh()); err != nil {
			return "", err
		}
	}
	if codexHelped(req) {
		wired := sandbox.Wiring{Steer: req.Steer, SkillLoads: req.SkillLoads}
		if err = writeCodexRoutes(ctx, box, req, wired, paths.CodexNotesURL(), paths.CodexSkillsURL()); err != nil {
			return "", err
		}
	}
	if err = box.WriteFile(ctx, paths.CodexGuardSaid(), []byte(said)); err != nil {
		return "", fmt.Errorf("executor: writing what the launch's guard has the run say: %w", err)
	}
	launch := codexCheckoutHome(paths).launchConfig(codexCheckouts(docLimit), req.ReportedRepos)
	if err = box.WriteFile(ctx, paths.CodexCheckoutLaunch(), launch); err != nil {
		return "", fmt.Errorf("executor: writing what hands the run's repositories their instructions: %w", err)
	}
	return "", nil
}

// writeCodexRoutes writes the routes w gives the note helper of req's run to
// notes and skills, each a file of its own read into the helper's environment
// alone ([codexStartHelper]).
func writeCodexRoutes(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, w sandbox.Wiring,
	notes, skills string) error {
	if req.Steer.Set() {
		if err := box.WriteFile(ctx, notes, []byte(w.Steer.URL)); err != nil {
			return fmt.Errorf("executor: writing the run's notes route: %w", err)
		}
	}
	if len(req.Skills) > 0 {
		if err := box.WriteFile(ctx, skills, []byte(w.SkillLoads.URL)); err != nil {
			return fmt.Errorf("executor: writing the run's skill-load route: %w", err)
		}
	}
	return nil
}

// codexSkill is one of the run's skills as [writeCodexSkills] writes it: its
// listed name and its rendered SKILL.md.
type codexSkill struct {
	name string
	file []byte
}

// codexSkillFiles renders the run's skills, refusing one that does not render
// before anything is written ([skillFile]).
func codexSkillFiles(skills []sandbox.Skill) ([]codexSkill, error) {
	out := make([]codexSkill, 0, len(skills))
	for _, s := range skills {
		file, err := skillFile(s.Listed(), s.Description, s.Body)
		if err != nil {
			return nil, err
		}
		out = append(out, codexSkill{name: s.Listed(), file: file})
	}
	return out, nil
}

// codexSkillsTable is a layer's `skills` table as Codex 0.171.0 types it
// (codex-rs/config/src/skills_config.rs SkillsConfig). One that does not fit,
// an entry without `enabled` or `max_context_tokens = 0`, switches nothing
// off unseen: Codex then fails to start the run's thread (measured).
type codexSkillsTable struct {
	Bundled *struct {
		Enabled *bool `toml:"enabled"`
	} `toml:"bundled"`
	IncludeInstructions *bool             `toml:"include_instructions"`
	MaxContextTokens    *uint64           `toml:"max_context_tokens"`
	Config              []codexSkillEntry `toml:"config"`
}

// codexSkillEntry is one `[[skills.config]]` entry as Codex 0.171.0 reads it.
type codexSkillEntry struct {
	Name    *string `toml:"name"`
	Path    *string `toml:"path"`
	Enabled *bool   `toml:"enabled"`
}

// codexSkillsSettled is, for each of skills, the entry of entries, read in
// order, that last names it by its listed name or its SKILL.md (same, by the
// entry's index), where that entry switches it off (measured on 0.171.0: a
// later entry's enabled true listed a skill an earlier one switched off, a
// later false hid one an earlier true listed; the directory's path, another
// case and a project's entry do nothing). As Codex's
// skill_config_rules_from_stack reads them, a name is trimmed and an entry
// naming both a name and a path, or neither, is ignored.
func codexSkillsSettled(entries []codexSkillEntry, skills []string, same func(entry int, skill string) bool) map[string]codexSkillEntry {
	off := map[string]codexSkillEntry{}
	for i, c := range entries {
		if c.Enabled == nil || (c.Name == nil) == (c.Path == nil) {
			continue
		}
		for _, name := range skills {
			if c.Name != nil && strings.TrimSpace(*c.Name) != name || c.Path != nil && !same(i, name) {
				continue
			}
			if *c.Enabled {
				delete(off, name)
			} else {
				off[name] = c
			}
		}
	}
	return off
}

// codexSkillEntries is the home's own configuration's (home,
// [codexHomeConfig]) `[[skills.config]]` entries as Codex reads them: from
// that file alone, the user layer (rust-v0.171.0
// skill_config_rules_from_stack: never /etc/codex, a managed layer or a
// project's), none where it is no TOML (which the merge refuses) or its
// table does not fit ([codexSkillsTable]).
func codexSkillEntries(home string) []codexSkillEntry {
	var config struct {
		Skills *codexSkillsTable `toml:"skills"`
	}
	if decoded := toml.Unmarshal([]byte(home), &config) == nil; !decoded || config.Skills == nil {
		return nil
	}
	if t := config.Skills; t.MaxContextTokens != nil && *t.MaxContextTokens == 0 ||
		slices.ContainsFunc(t.Config, func(c codexSkillEntry) bool { return c.Enabled == nil }) {
		return nil
	}
	return config.Skills.Config
}

// codexSkillPathsSame is, for entries' paths, which of the run's skills'
// written SKILL.md each names as Codex 0.171.0 resolves a path (measured: a
// `~/` path from the home, a relative one from the Codex home, a `..` past a
// directory that is not there, a link to the skills directory and a linked
// Codex home each hid the skill): made absolute and cleaned here, then held
// to the file by `test -ef` in the box, which follows links as Codex's
// canonicalization does. A path naming nothing there names no skill.
func codexSkillPathsSame(ctx context.Context, box sandbox.Sandbox, entries []codexSkillEntry, skills []codexSkill,
	paths Paths) (func(entry int, skill string) bool, error) {
	var b strings.Builder
	for i, c := range entries {
		if c.Path == nil {
			continue
		}
		p := *c.Path
		switch {
		case p == "~" || strings.HasPrefix(p, "~/"):
			p = paths.Home + p[1:]
		case !path.IsAbs(p):
			p = paths.CodexHome() + "/" + p
		}
		for j, s := range skills {
			fmt.Fprintf(&b, "[ %s -ef %s ] && echo %s%d.%d\n", shellword.Quote(path.Clean(p)),
				shellword.Quote(paths.CodexSkills()+"/"+s.name+"/SKILL.md"), codexSameMark, i, j)
		}
	}
	same := map[[2]int]bool{}
	if b.Len() > 0 {
		b.WriteString("exit 0\n")
		res, err := box.Exec(ctx, b.String(), sandbox.ExecOptions{})
		if err == nil && res.ExitCode != 0 {
			err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
		}
		if err != nil {
			return nil, fmt.Errorf("executor: resolving the paths of the box home's Codex skill switches: %w", err)
		}
		for line := range strings.Lines(res.Stdout) {
			var i, j int
			if _, err := fmt.Sscanf(strings.TrimSpace(line), codexSameMark+"%d.%d", &i, &j); err == nil {
				same[[2]int{i, j}] = true
			}
		}
	}
	return func(entry int, skill string) bool {
		j := slices.IndexFunc(skills, func(s codexSkill) bool { return s.name == skill })
		return same[[2]int{entry, j}]
	}, nil
}

// codexSameMark opens each line of [codexSkillPathsSame]'s answer, so a
// login profile's output reads as no answer.
const codexSameMark = "crewlet-same "

// codexSkillsOff refuses a run where the home's entries ([codexSkillEntries])
// switch one of skills off ([codexSkillsSettled], its paths resolved by
// same), which Codex leaves unlisted with no word; the run never switches it
// back on.
func codexSkillsOff(entries []codexSkillEntry, skills []codexSkill, same func(entry int, skill string) bool) error {
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.name)
	}
	off := codexSkillsSettled(entries, names, same)
	var said []string
	for _, name := range names {
		if c, ok := off[name]; ok {
			said = append(said, fmt.Sprintf("a [[skills.config]] entry (%s) sets enabled = false for the seat skill %s",
				codexSkillSelector(c), name))
		}
	}
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: the box home's ~/.codex/config.toml switches off a skill of the run's, which "+
		"Codex would then go without, with no word: %s; the run never switches back on what a person switched "+
		"off, so remove that entry", strings.Join(said, "; "))
}

// codexSkillSelector names an entry by the selector it holds.
func codexSkillSelector(c codexSkillEntry) string {
	if c.Name != nil {
		return fmt.Sprintf("name %q", *c.Name)
	}
	return fmt.Sprintf("path %q", *c.Path)
}

// codexServersOff refuses a run where the home's own configuration (home,
// [codexHomeConfig]) switches one of Crewlet's own servers off: its
// `[mcp_servers.<name>]` entry with `enabled = false`, a `disabled_tools`
// naming one of the tools it lists ([sandbox.MCPServer.Tools]), or an
// `enabled_tools` leaving one out. The run's own entry replaces the home's of
// that name ([codexConfigOver]) and states it enabled with no tool disabled,
// so it would otherwise switch back on what the person switched off. Another
// layer holding the entry at all is [codexGuard]'s to refuse.
func codexServersOff(home string, servers map[string]sandbox.MCPServer) error {
	var config struct {
		Servers map[string]struct {
			Enabled  *bool     `toml:"enabled"`
			Disabled []string  `toml:"disabled_tools"`
			Only     *[]string `toml:"enabled_tools"`
		} `toml:"mcp_servers"`
	}
	if decoded := toml.Unmarshal([]byte(home), &config) == nil; !decoded {
		return nil
	}
	var said []string
	for _, name := range crewletServers(servers) {
		entry, ok := config.Servers[name]
		if !ok {
			continue
		}
		if entry.Enabled != nil && !*entry.Enabled {
			said = append(said, fmt.Sprintf("mcp_servers.%s.enabled = false", name))
		}
		for _, tool := range servers[name].Tools {
			if slices.Contains(entry.Disabled, tool) {
				said = append(said, fmt.Sprintf("mcp_servers.%s.disabled_tools names %s", name, tool))
			}
			if entry.Only != nil && !slices.Contains(*entry.Only, tool) {
				said = append(said, fmt.Sprintf("mcp_servers.%s.enabled_tools leaves %s out", name, tool))
			}
		}
	}
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: the box home's ~/.codex/config.toml switches off Crewlet's own MCP server, "+
		"which the run's tools go through: %s; the run never switches back on what a person switched off, so "+
		"remove that key", strings.Join(said, "; "))
}

// writeCodexSkills writes each of a run's rendered skills as
// `<skills>/<listed name>/SKILL.md` ([Paths.CodexSkills]), after removing an
// earlier run's, the `crewlet:` ones, so a run lists its own beside the
// home's. The note helper reports each load ([codexStartHelper]): Codex reads
// a skill's file through its shell, which the helper sees off the app-server
// and holds to the digest of the file as written ([skillDigests]).
func writeCodexSkills(ctx context.Context, box sandbox.Sandbox, skills []codexSkill, paths Paths) error {
	skillsDir := shellword.Quote(paths.CodexSkills())
	clear := "[ ! -d " + skillsDir + " ] || find " + skillsDir + " -mindepth 1 -maxdepth 1 -name " +
		shellword.Quote(sandbox.SkillNamespace+":*") + " -exec rm -rf {} +"
	res, err := box.Exec(ctx, clear, sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		return fmt.Errorf("executor: clearing the skills an earlier run left in the Codex home: %w", err)
	}
	for _, s := range skills {
		if err = box.WriteFile(ctx, paths.CodexSkills()+"/"+s.name+"/SKILL.md", s.file); err != nil {
			return fmt.Errorf("executor: writing the run's skill %q: %w", s.name, err)
		}
	}
	return nil
}
