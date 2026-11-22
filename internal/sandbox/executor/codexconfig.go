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

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// codexTelemetryEnv is the OTLP exporter's variables, the endpoint and its
// headers, which carry the run's telemetry credential: excluded from the
// model's shell commands, kept for Codex's own export.
const codexTelemetryEnv = "OTEL_EXPORTER_OTLP_*"

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
// shell inherits the run's environment less [codexShellExcluded], keeping
// PATH, HOME, GIT_CONFIG_* and the seat's tool credentials.
func codexPosture(req sandbox.RunRequest) []string {
	// WriteConfig refused a run whose names do not read, before any script.
	excluded, _ := codexShellExcluded(req)
	set := []string{
		`sandbox_mode="danger-full-access"`,
		`approval_policy="never"`,
		`analytics.enabled=false`,
		`features.plugins=false`,
		`features.remote_plugin=false`,
		`skills.bundled.enabled=false`,
		`tools.experimental_request_user_input.enabled=false`,
		`shell_environment_policy.inherit="all"`,
		`shell_environment_policy.exclude=` + tomlArray(excluded),
	}
	// THE ENTRY'S WEB TOOLS OFF where it denies them, as its runs on the engine
	// host have none: web search is Codex's one web tool, and with `disabled` the
	// request offers none (measured through the app-server on 0.171.0, under
	// this posture; without it the request offers `web_search`). A hosted tool:
	// Codex runs nothing for it, so none is left to refuse. The shell's reach is
	// the box cell's egress policy's.
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
// without (its shell_environment_policy.exclude): the names the run signs in
// by ([sandbox.RunRequest.SignInEnv]) and the note helper's routes, through
// the one rule every runner keeps ([toolEnvOmitted]), and the telemetry
// exporter's endpoint and headers ([codexTelemetryEnv], a pattern, which
// Codex keeps for its own export). A name no runner could keep out is refused.
func codexShellExcluded(req sandbox.RunRequest) ([]string, error) {
	names, err := toolEnvOmitted(req.SignInEnv, codexNotesEnv, codexSkillsEnv)
	if err != nil {
		return nil, err
	}
	return append(names, codexTelemetryEnv), nil
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
func (c Codex) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, _ Session,
	paths Paths) (string, error) {
	if _, err := codexShellExcluded(req); err != nil {
		return "", err
	}
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
		if f.Steer.Set() != req.Steer.Set() || (len(req.Skills) > 0 && !f.SkillLoads.Set()) {
			return "", errors.New("executor: the run's two wirings, for the session it continues and for " +
				"one of its own, do not reach the same routes, so the run cannot be wired to whichever " +
				"session its box starts")
		}
		if err := box.WriteFile(ctx, paths.CodexConfigFresh(), []byte(codexConfigTOML(f.MCPServers, system, req.Model))); err != nil {
			return "", fmt.Errorf("executor: writing Codex's configuration for a session of the run's own: %w", err)
		}
		if err := writeCodexRoutes(ctx, box, req, *f, paths.CodexNotesURLFresh(), paths.CodexSkillsURLFresh()); err != nil {
			return "", err
		}
	}
	if codexHelped(req) {
		if err := writeCodexNoteHelper(ctx, box, paths); err != nil {
			return "", err
		}
		wired := sandbox.Wiring{Steer: req.Steer, SkillLoads: req.SkillLoads}
		if err := writeCodexRoutes(ctx, box, req, wired, paths.CodexNotesURL(), paths.CodexSkillsURL()); err != nil {
			return "", err
		}
	}
	if err := writeCodexSkills(ctx, box, req.Skills, paths); err != nil {
		return "", err
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
// run's. The note helper reports each load ([codexStartHelper]): Codex reads
// a skill's file through its shell, which the helper sees off the app-server
// and holds to the digest of the file as written ([codexSkillDigests]).
func writeCodexSkills(ctx context.Context, box sandbox.Sandbox, skills []sandbox.Skill, paths Paths) error {
	if _, err := box.Exec(ctx, "rm -rf "+shellword.Quote(paths.CodexSkills())+"/"+
		shellword.Quote(sandbox.SkillNamespace+":")+"*", sandbox.ExecOptions{}); err != nil {
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
