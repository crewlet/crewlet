package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A HOME OR A MACHINE'S POLICY CANNOT SWITCH CREWLET'S OWN SERVER OR NOTES
// EXTENSION OFF UNSEEN. Copilot CLI 1.0.114 ranks nothing of the run's above
// the home's settings.json switching the extension off or `--yolo` off, a value
// there it rejects (which drops the whole file, the run's `experimental` with
// it), a home mcp-config.json it cannot read (no MCP server starts at all), or
// the machine's managed policy ([copilotPolicyVerdict]); all measured. So
// before anything is written, the launch asks the box's Copilot which user
// settings it loads and whether it rejects the home's MCP file, reads the
// policy file, and refuses the run naming the file, the key and the entry
// ([copilotGuard]), a `disabledMcpServers` entry naming the bridge, the home's
// or a repository's, among them; a server of the bridge's name in any MCP file
// the run's `--additional-mcp-config` replaces (measured).

// copilotPolicyFile is where Copilot 1.0.114 reads a Linux machine's managed
// policy; no variable names another, its test override included (measured).
const copilotPolicyFile = "/etc/github-copilot/managed-settings.json"

// copilotPolicy is the managed policy file a guard reads and the uid Copilot
// requires to own it. The zero value is the machine's: [copilotPolicyFile],
// owned by root.
type copilotPolicy struct {
	file  string
	owner int
}

// path is the policy file the guard reads.
func (p copilotPolicy) path() string {
	if p.file == "" {
		return copilotPolicyFile
	}
	return p.file
}

// copilotGuardWait bounds the guard's two starts of the box's copilot, each
// 0.6 to 1 s on the build VM (1.0.114, no network): a loaded box has room,
// and a box that cannot answer in that time cannot start the run either.
const copilotGuardWait = 30 * time.Second

// copilotGuardMark opens each section of the guard's answer: what Copilot says
// of the user settings, of the home's MCP file, and the policy file.
const copilotGuardMark = "crewlet-guard: "

// copilotRuleFamilies is every rule family a managed `permissions.deny` entry
// may name; 1.0.114 rejects the whole policy for any other (its log says so).
var copilotRuleFamilies = []string{"Bash", "Shell", "PowerShell", "Read", "Edit", "Write", "Domain", "Mcp",
	"CustomTool"}

// copilotGuard refuses req's run where something the run cannot undo switches
// off its own servers ([crewletServers]) or, for a run written the notes
// extension, that extension: a run with notes or a cap, and every run handing
// its checkouts (handing), whose extension hands a checkout cloned during it
// and removes a launch attempt Copilot refused. settings is the home's
// settings.json as the run will write it, which the box's Copilot is asked
// about in a home of its own, so a value the run's own key replaces is not
// held against it. A box with no copilot is the script's to refuse; its
// policy file is still read.
func copilotGuard(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths,
	policy copilotPolicy, settings []byte, handing bool) error {
	servers := crewletServers(req.MCPServers)
	notes := req.Steer.Set() || req.Limits.MaxTurns > 0 || handing
	if len(servers) == 0 && !notes {
		return nil
	}
	if err := box.WriteFile(ctx, paths.CopilotGuardSettings(), settings); err != nil {
		return fmt.Errorf("executor: writing the settings the launch asks Copilot about: %w", err)
	}
	// A LOGIN SHELL WITH THE BOX'S BIN DIRECTORY FIRST, as the run's wrapper
	// finds its copilot.
	res, err := box.Exec(ctx, "sh -lc "+shellword.Quote(copilotGuardCommand(paths, policy.path(), len(servers) > 0)),
		sandbox.ExecOptions{TimeoutSec: copilotGuardWait.Seconds()})
	if err != nil {
		return fmt.Errorf("executor: asking the box's Copilot what it loads for the run: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("executor: asking the box's Copilot what it loads for the run: exit %d: %s",
			res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	said := copilotGuardVerdict(res.Stdout, req, paths, policy, servers, notes)
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: Copilot would start without what the run's tools, notes or checkouts rest on, and no "+
		"setting of the run's undoes it: %s. Change those entries, or run on a box without them",
		strings.Join(said, "; "))
}

// copilotGuardCommand is the shell printing, each after its mark, what the
// box's Copilot says of the user settings ([Paths.CopilotGuardSettings], moved
// into a home of its own) with `config --list`; where mcp, the errors of `mcp
// list` in the run's home and the repository's settings (`--repo`, `--local`);
// and the policy file's kind, owner and content.
func copilotGuardCommand(paths Paths, policy string, mcp bool) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`PATH=%s:"$PATH"; export PATH`, q(paths.BinDir()))
	line(`unset %s`, strings.Join(copilotSwitchesOff, " "))
	line(`COPILOT_AUTO_UPDATE=false COPILOT_TELEMETRY_DISABLED=true; export COPILOT_AUTO_UPDATE COPILOT_TELEMETRY_DISABLED`)
	line(`gs=%s; gt=; trap 'rm -rf "$gt" "$gs"' EXIT`, q(paths.CopilotGuardSettings()))
	line(`if command -v copilot > /dev/null 2>&1; then`)
	line(`  gt=$(mktemp -d) && mv -f "$gs" "$gt/settings.json" || exit 1`)
	line(`  echo %s`, q(copilotGuardMark+"settings"))
	line(`  COPILOT_HOME="$gt" copilot config --list < /dev/null 2>&1`)
	if mcp {
		line(`  echo %s`, q(copilotGuardMark+"mcp"))
		line(`  COPILOT_HOME=%s copilot mcp list < /dev/null 2>&1 > /dev/null`, q(paths.CopilotHome()))
		line(`  echo %s`, q(copilotGuardMark+"repo"))
		line(`  COPILOT_HOME="$gt" copilot config --list --repo < /dev/null 2>&1`)
		line(`  echo %s`, q(copilotGuardMark+"local"))
		line(`  COPILOT_HOME="$gt" copilot config --list --local < /dev/null 2>&1`)
	}
	line(`fi`)
	line(`echo %s`, q(copilotGuardMark+"policy"))
	line(`gp=%s`, q(policy))
	line(`if [ -L "$gp" ]; then echo symlink`)
	line(`elif [ -e "$gp" ]; then`)
	line(`  if [ -f "$gp" ]; then set -- $(ls -lnd -- "$gp"); echo "owner $3"; cat -- "$gp"; else echo other; fi`)
	line(`fi`)
	line(`exit 0`)
	return b.String()
}

// copilotGuardSections is the guard's answer by section: "settings", "mcp",
// "repo", "local" and "policy", each present only where the answer holds its
// mark.
func copilotGuardSections(out string) map[string]string {
	sections := map[string]string{}
	name := ""
	var body strings.Builder
	flush := func() {
		if name != "" {
			sections[name] = body.String()
		}
		body.Reset()
	}
	for l := range strings.Lines(out) {
		if mark, ok := strings.CutPrefix(strings.TrimSuffix(l, "\n"), copilotGuardMark); ok &&
			slices.Contains([]string{"settings", "mcp", "repo", "local", "policy"}, mark) && name != "policy" {
			flush()
			name = mark
			continue
		}
		body.WriteString(l)
	}
	flush()
	return sections
}

// copilotGuardVerdict is each reason out, the guard's answer, gives to refuse
// the run, as FILE: KEY ENTRY: what Copilot's own `config --list` lists of the
// user settings, on a run with the extension the warning that it ignores them
// whole; what its `mcp list` rejects of the home's mcp-config.json; and the
// policy's ([copilotPolicyVerdict]).
func copilotGuardVerdict(out string, req sandbox.RunRequest, paths Paths, policy copilotPolicy,
	servers []string, notes bool) []string {
	sections := copilotGuardSections(out)
	settings := paths.CopilotSettings()
	extension := "user:" + path.Base(paths.CopilotExtension())
	var said []string
	for l := range strings.Lines(sections["settings"]) {
		l = strings.TrimSuffix(l, "\n")
		switch {
		case notes && strings.HasPrefix(l, "Warning: Ignoring values from settings.json"):
			said = append(said, fmt.Sprintf("%s holds a value Copilot rejects, so it ignores every setting there, "+
				"the run's experimental (without which it loads no extension) among them; it says %q", settings, l))
		case notes && l == "extensions.mode=disabled":
			said = append(said, fmt.Sprintf(`%s sets extensions.mode to "disabled", which loads no extension, `+
				"the run's notes extension among them", settings))
		case notes && l == "extensions.disabledExtensions="+extension:
			said = append(said, fmt.Sprintf("%s lists %s, the run's notes extension, in extensions.disabledExtensions",
				settings, extension))
		case strings.HasPrefix(l, "permissions.disableBypassPermissionsMode="):
			said = append(said, fmt.Sprintf("%s sets permissions.disableBypassPermissionsMode to %q, which turns "+
				"--yolo off, so every call of the run's tools and its notes extension are refused", settings,
				strings.TrimPrefix(l, "permissions.disableBypassPermissionsMode=")))
		}
	}
	// A DISABLED SERVER OF THE RUN'S OWN, in the home's settings or the
	// repository's (`config --list`, `--repo`, `--local`, one line an item),
	// which the run never enables back (measured on 1.0.114: the bridge off).
	for section, file := range map[string]string{"settings": settings,
		"repo": ".github/copilot/settings.json", "local": ".github/copilot/settings.local.json"} {
		for l := range strings.Lines(sections[section]) {
			name, ok := strings.CutPrefix(strings.TrimSuffix(l, "\n"), "disabledMcpServers=")
			if ok && slices.Contains(servers, name) {
				said = append(said, fmt.Sprintf("%s lists %s, Crewlet's own MCP server, in disabledMcpServers, which "+
					"the run never switches back on", file, name))
			}
		}
	}
	mcp := "Error: Invalid MCP config at " + paths.CopilotHome() + "/mcp-config.json: "
	for l := range strings.Lines(sections["mcp"]) {
		if why, ok := strings.CutPrefix(strings.TrimSuffix(l, "\n"), mcp); ok {
			said = append(said, fmt.Sprintf("%s/mcp-config.json is a file Copilot rejects (%s), and then it starts "+
				"no MCP server, the run's own among them", paths.CopilotHome(), why))
		}
	}
	if answer, ok := sections["policy"]; ok {
		said = append(said, copilotPolicyVerdict(answer, req, policy, servers, notes)...)
	}
	return said
}

// copilotMatcher is one entry of a managed `allowedMcpServers` or
// `deniedMcpServers` list: one of a server's literal name or a URL pattern
// (a stdio server's command is matched by Copilot alone).
type copilotMatcher struct {
	ServerName    *string         `json:"serverName"`
	ServerURL     *string         `json:"serverUrl"`
	ServerCommand json.RawMessage `json:"serverCommand"`
}

// copilotManaged is the part of a managed policy the guard reads.
type copilotManaged struct {
	Allowed     *[]json.RawMessage `json:"allowedMcpServers"`
	Denied      []json.RawMessage  `json:"deniedMcpServers"`
	ManagedOnly json.RawMessage    `json:"allowManagedMcpServersOnly"`
	Strict      json.RawMessage    `json:"strictPluginOnlyCustomization"`
	Permissions struct {
		Deny   []json.RawMessage `json:"deny"`
		Bypass json.RawMessage   `json:"disableBypassPermissionsMode"`
	} `json:"permissions"`
}

// copilotPolicyVerdict is each reason the policy file's answer (its kind,
// owner and content) gives to refuse the run. Copilot 1.0.114 loads no MCP
// server and no extension where the file is a symbolic link, not root's, not a
// JSON object, comments and trailing commas included, or names an unknown rule
// family; otherwise a deny entry matching a server, an allow list leaving it
// out, `allowManagedMcpServersOnly`, any `Mcp(...)` deny rule (every call
// then needs an approval a headless run cannot give),
// `strictPluginOnlyCustomization` and `disableBypassPermissionsMode` (any
// string) switch the server or the extension off. All measured.
func copilotPolicyVerdict(answer string, req sandbox.RunRequest, policy copilotPolicy, servers []string,
	notes bool) []string {
	file := policy.path()
	closed := "and then it loads no MCP server and no extension, the run's own among them"
	head, content, _ := strings.Cut(answer, "\n")
	switch {
	case head == "":
		return nil
	case head == "symlink":
		return []string{fmt.Sprintf("%s is a symbolic link, which Copilot rejects, %s", file, closed)}
	case head == "other":
		return []string{fmt.Sprintf("%s is not a regular file, which Copilot rejects, %s", file, closed)}
	}
	if owner, _ := strings.CutPrefix(head, "owner "); owner != strconv.Itoa(policy.owner) {
		return []string{fmt.Sprintf("%s is not owned by root (uid %s), which Copilot rejects, %s", file, owner, closed)}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &object); err != nil || object == nil {
		return []string{fmt.Sprintf("%s does not hold a JSON object (%v), which Copilot rejects, %s", file, err, closed)}
	}
	var m copilotManaged
	_ = json.Unmarshal([]byte(content), &m)
	for _, raw := range m.Permissions.Deny {
		var rule string
		if json.Unmarshal(raw, &rule) != nil {
			continue
		}
		family, _, _ := strings.Cut(rule, "(")
		if !slices.ContainsFunc(copilotRuleFamilies, func(f string) bool { return strings.EqualFold(f, family) }) {
			return []string{fmt.Sprintf("%s lists %q in permissions.deny, a rule family Copilot does not know, so it "+
				"rejects the whole policy, %s", file, rule, closed)}
		}
	}
	var said []string
	var bypass string
	if json.Unmarshal(m.Permissions.Bypass, &bypass) == nil {
		said = append(said, fmt.Sprintf("%s sets permissions.disableBypassPermissionsMode to %q, which turns --yolo "+
			"off, so every call of the run's tools and its notes extension are refused", file, bypass))
	}
	locks := copilotLocks(m.Strict)
	if notes && locks["extensions"] {
		said = append(said, fmt.Sprintf("%s sets strictPluginOnlyCustomization to %s, which locks extensions to "+
			"plugins and the policy, the run's notes extension among them", file, codexShown(m.Strict)))
	}
	if len(servers) == 0 {
		return said
	}
	for _, raw := range m.Permissions.Deny {
		var rule string
		if json.Unmarshal(raw, &rule) == nil && strings.HasPrefix(rule, "Mcp(") {
			said = append(said, fmt.Sprintf("%s lists %q in permissions.deny, under which Copilot refuses every call "+
				"of an MCP server's tools, the run's own among them", file, rule))
		}
	}
	if locks["mcp"] {
		said = append(said, fmt.Sprintf("%s sets strictPluginOnlyCustomization to %s, which locks MCP servers to "+
			"plugins and the policy, the run's own among them", file, codexShown(m.Strict)))
	}
	var only bool
	if json.Unmarshal(m.ManagedOnly, &only) == nil && only {
		said = append(said, fmt.Sprintf("%s sets allowManagedMcpServersOnly to true, which admits no server "+
			"the run adds, its own among them", file))
	}
	for _, name := range servers {
		said = append(said, copilotServerLists(file, name, req.MCPServers[name], m)...)
	}
	return said
}

// copilotLocks is the surfaces a `strictPluginOnlyCustomization` value locks:
// every one for true, those it names for a list.
func copilotLocks(raw json.RawMessage) map[string]bool {
	var all bool
	if json.Unmarshal(raw, &all) == nil {
		return map[string]bool{"mcp": all, "extensions": all}
	}
	var named []string
	_ = json.Unmarshal(raw, &named)
	locks := map[string]bool{}
	for _, s := range named {
		locks[s] = true
	}
	return locks
}

// copilotServerLists is each reason the policy's server lists give to block
// the server name, s, as measured on 1.0.114: any deny entry matching it, by
// its literal name (case kept) or a URL pattern; an allow list, empty
// included, with no entry admitting it, where a remote server's name counts
// only in a list holding no URL entry. A stdio server under an allow list
// holding a command entry is Copilot's alone to judge.
func copilotServerLists(file, name string, s sandbox.MCPServer, m copilotManaged) []string {
	matches := func(e copilotMatcher, byName bool) bool {
		if byName && e.ServerName != nil && *e.ServerName == name {
			return true
		}
		return e.ServerURL != nil && s.Transport == sandbox.TransportHTTP && copilotURLMatches(*e.ServerURL, s.URL)
	}
	var said []string
	for _, raw := range m.Denied {
		var e copilotMatcher
		if json.Unmarshal(raw, &e) == nil && matches(e, true) {
			said = append(said, fmt.Sprintf("%s lists %s in deniedMcpServers, which blocks the server %s",
				file, codexShown(raw), name))
		}
	}
	if m.Allowed == nil {
		return said
	}
	entries := make([]copilotMatcher, 0, len(*m.Allowed))
	urls, commands := false, false
	for _, raw := range *m.Allowed {
		var e copilotMatcher
		if json.Unmarshal(raw, &e) != nil {
			return said
		}
		urls = urls || e.ServerURL != nil
		commands = commands || len(e.ServerCommand) > 0
		entries = append(entries, e)
	}
	if s.Transport != sandbox.TransportHTTP && commands {
		return said
	}
	byName := s.Transport != sandbox.TransportHTTP || !urls
	if !slices.ContainsFunc(entries, func(e copilotMatcher) bool { return matches(e, byName) }) {
		shown, _ := json.Marshal(m.Allowed)
		said = append(said, fmt.Sprintf("%s sets allowedMcpServers to %s, which leaves the server %s out",
			file, codexShown(shown), name))
	}
	return said
}

// copilotURLMatches reports whether pattern, a managed list's serverUrl,
// matches url as Copilot 1.0.114 does (measured): `*` stands for any run of
// characters, anywhere, and the scheme compares without case, the path with
// it.
func copilotURLMatches(pattern, url string) bool {
	pattern, url = copilotFoldOrigin(pattern), copilotFoldOrigin(url)
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == url
	}
	rest, ok := strings.CutPrefix(url, parts[0])
	if !ok {
		return false
	}
	for _, part := range parts[1 : len(parts)-1] {
		at := strings.Index(rest, part)
		if at < 0 {
			return false
		}
		rest = rest[at+len(part):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

// copilotFoldOrigin is u with its scheme and host lowercased, up to the path:
// a URL's scheme and host name no case.
func copilotFoldOrigin(u string) string {
	at := strings.Index(u, "://")
	if at < 0 {
		return u
	}
	end := len(u)
	if slash := strings.IndexByte(u[at+3:], '/'); slash >= 0 {
		end = at + 3 + slash
	}
	return strings.ToLower(u[:end]) + u[end:]
}
