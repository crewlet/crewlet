package executor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// codexMCPStartupWait is how long Codex waits for a required MCP server to
// finish its handshake before the run fails ([codexConfigTOML]): SIXTY
// SECONDS, the wait OpenCode's script gives the same bridge
// ([openCodeMCPWait]), since the box and the bridge are the same. Codex's own
// default is ten, which a box dialling a busy node over its network can miss,
// and a required server that misses it fails the run before its first step.
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
// sandbox needs, and nobody answers); no analytics, plugins or bundled skills
// (with all three off a run dials only its model and servers, measured); no
// request_user_input (a question parks through ask_and_wait); and the model's
// shell inherits the run's environment less the harness's own sign-in and the
// notes route ([codexNotesEnv]), which Codex otherwise hands every command
// (measured), keeping PATH, HOME, GIT_CONFIG_* and the seat's tool credentials.
func codexPosture(req sandbox.RunRequest) []string {
	profile, _ := harnessprofile.Builtin(CodexName)
	set := []string{
		`sandbox_mode="danger-full-access"`,
		`approval_policy="never"`,
		`analytics.enabled=false`,
		`features.plugins=false`,
		`features.remote_plugin=false`,
		`skills.bundled.enabled=false`,
		`tools.experimental_request_user_input.enabled=false`,
		`shell_environment_policy.inherit="all"`,
		`shell_environment_policy.exclude=` + tomlArray(append(profile.CredentialEnvNames(nil), codexNotesEnv)),
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
		out = append(out, "-c", sandbox.ShellQuote(kv))
	}
	return out
}

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

// codexBoxArch is the note helper build a box runs: its `uname -m` as Go
// names the architecture, refused where the engine embeds no build for it.
func codexBoxArch(machine string) (string, error) {
	switch strings.TrimSpace(machine) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("executor: this box's machine is %s, and the engine carries Codex's note "+
		"helper for x86_64 and aarch64 alone", strconv.Quote(strings.TrimSpace(machine)))
}

// WriteConfig writes what a Codex run starts from: its configuration
// ([codexConfigTOML]) for the session it was asked to work in and, where it
// was handed two wirings, for one of its own; for a run wired to its notes,
// the note helper's build for the box and its notes route
// ([Paths.CodexNotesURL]); and its own skills ([writeCodexSkills]). It
// writes no path the harness is pointed at: the script moves the
// configuration into place once the session is settled ([codexSessionGate]).
//
// THE STANDING INSTRUCTIONS ARE TOLD OF NOTES on a run wired to them
// ([withNoteInstruction]).
func (c Codex) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error) {
	if n := len(paths.CodexSocket()); n > maxCodexSocketPath {
		return "", fmt.Errorf("executor: a Codex run's socket, %s, is %d bytes, past the %d a unix socket's "+
			"path holds; a local box's home is under providers.sandbox.local.state_dir, which needs a "+
			"shorter path", paths.CodexSocket(), n, maxCodexSocketPath)
	}
	system := req.System
	if req.Steer.Set() {
		system = withNoteInstruction(system, c.NoteInstruction())
	}
	if err := box.WriteFile(ctx, paths.CodexConfig(), []byte(codexConfigTOML(req.MCPServers, system, req.Model))); err != nil {
		return "", fmt.Errorf("executor: writing Codex's configuration: %w", err)
	}
	if f := req.FreshWiring; f != nil {
		if f.Steer.Set() != req.Steer.Set() {
			return "", errors.New("executor: the run's two wirings, for the session it continues and for " +
				"one of its own, do not both take notes, so the run cannot be wired to whichever session " +
				"its box starts")
		}
		if err := box.WriteFile(ctx, paths.CodexConfigFresh(), []byte(codexConfigTOML(f.MCPServers, system, req.Model))); err != nil {
			return "", fmt.Errorf("executor: writing Codex's configuration for a session of the run's own: %w", err)
		}
		if f.Steer.Set() {
			if err := box.WriteFile(ctx, paths.CodexNotesURLFresh(), []byte(f.Steer.URL)); err != nil {
				return "", fmt.Errorf("executor: writing the notes route of a session of the run's own: %w", err)
			}
		}
	}
	if req.Steer.Set() {
		if err := writeCodexNoteHelper(ctx, box, paths); err != nil {
			return "", err
		}
		if err := box.WriteFile(ctx, paths.CodexNotesURL(), []byte(req.Steer.URL)); err != nil {
			return "", fmt.Errorf("executor: writing the run's notes route: %w", err)
		}
	}
	if err := writeCodexSkills(ctx, box, req.Skills, paths); err != nil {
		return "", err
	}
	return "", nil
}

// writeCodexNoteHelper writes the note helper's build for the box's machine
// ([noteHelperBinary]) at every launch, never cached: a box reused across
// builds would otherwise run another build's helper against this one's bridge.
func writeCodexNoteHelper(ctx context.Context, box sandbox.Sandbox, paths Paths) error {
	machine, err := box.Exec(ctx, "uname -m", sandbox.ExecOptions{})
	if err != nil {
		return fmt.Errorf("executor: reading the box's machine for Codex's note helper: %w", err)
	}
	arch, err := codexBoxArch(machine.Stdout)
	if err != nil {
		return err
	}
	helper, err := noteHelperBinary(arch)
	if err != nil {
		return fmt.Errorf("executor: Codex's note helper for %s: %w", arch, err)
	}
	if err = box.WriteFile(ctx, paths.CodexNoteHelper(), helper); err != nil {
		return fmt.Errorf("executor: writing Codex's note helper: %w", err)
	}
	return nil
}

// writeCodexSkills writes each of a run's skills as
// `<skills>/<listed name>/SKILL.md` ([Paths.CodexSkills]), after removing
// every skill an earlier run wrote there, so a reused box lists only this
// run's. A REQUIRED SKILL IS REFUSED: Codex reads a skill's file through its
// shell, which reports no load to the run's bridge, so the tools the skill
// covers would stay refused for the whole run.
func writeCodexSkills(ctx context.Context, box sandbox.Sandbox, skills []sandbox.Skill, paths Paths) error {
	for _, s := range skills {
		if s.Required {
			return fmt.Errorf("executor: the run's skill %q is required, and Codex reports no skill it "+
				"loads, so the tools it covers would stay refused; run the seat on claude-code or opencode, "+
				"or make the skill optional", s.Listed())
		}
	}
	if _, err := box.Exec(ctx, "rm -rf "+sandbox.ShellQuote(paths.CodexSkills())+"/"+
		sandbox.ShellQuote(sandbox.SkillNamespace+":")+"*", sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("executor: removing an earlier run's Codex skills: %w", err)
	}
	for _, s := range skills {
		file, err := skillFile(s.Listed(), s.Description, s.Body)
		if err != nil {
			return err
		}
		if err = box.WriteFile(ctx, paths.CodexSkills()+"/"+s.Listed()+"/SKILL.md", file); err != nil {
			return fmt.Errorf("executor: writing the run's skill %q: %w", s.Listed(), err)
		}
	}
	return nil
}
