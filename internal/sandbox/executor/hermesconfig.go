package executor

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// hermesPluginName is the runner's plugin's name, the directory Hermes loads it
// from under its home's plugins ([Paths.HermesPlugin]) and the name its
// configuration enables: no bundled plugin's, and the script writes the run's
// own copy over whatever the home held under it. Hermes ranks a project's
// plugin above the home's only where HERMES_ENABLE_PROJECT_PLUGINS is set, and
// a project's is read from the gateway's own working directory, never a checkout.
const hermesPluginName = "crewlet-runner"

// hermesPluginSettingsEnv names the plugin's settings file for the gateway
// ([Paths.HermesPluginConfig]): paths and digests only, no secret.
const hermesPluginSettingsEnv = "CREWLET_HERMES_PLUGIN"

//go:embed hermes_plugin.py
var hermesPluginSource []byte

// hermesPluginManifest is the plugin's manifest, `plugin.yaml`: the hooks it
// registers ([hermesPluginSource]).
const hermesPluginManifest = `name: crewlet-runner
version: 1.0.0
description: "Crewlet's runner: the run's notes, skill loads, request record and later checkouts"
provides_hooks:
  - pre_llm_call
  - post_llm_call
  - post_api_request
  - post_tool_call
  - transform_tool_result
`

// hermesWebToolsets is what a web-denied Hermes run disables
// (agent.disabled_toolsets): `web` (web_search and web_extract), `browser`
// (browser_exec and its siblings, which lazy-install a browser) and
// `x_search`. Measured on 0.23.3: with `web` and `browser` disabled the request
// offers none of their tools, on a session created allowing them too.
var hermesWebToolsets = []string{"web", "browser", "x_search"}

// hermesBrowserToolset is Hermes's own browser (browser_exec and its
// siblings), which installs and runs a Chromium in whatever box the run lands
// in, so every run disables it but one handed a seat's browser, which runs
// only in a container box the launch checked (internal/browser). 0.23.3 merges
// a server named like a built-in toolset into it (toolsets.get_toolset), so
// disabling it beside the seat's `browser` server would hide that server too:
// measured, its tools and the server's alike were gone.
const hermesBrowserToolset = "browser"

// hermesSystemMaxBytes bounds a Hermes run's standing instructions, which ride
// one environment variable (HERMES_EPHEMERAL_SYSTEM_PROMPT): Linux holds one
// variable to 128 KiB with its name, its `=` and its NUL (MAX_ARG_STRLEN).
const hermesSystemMaxBytes = 128<<10 - len("HERMES_EPHEMERAL_SYSTEM_PROMPT=") - 1

// hermesOverlay is a Hermes run's overlay ([Paths.HermesManaged]): its MCP
// servers, every value as written, and on each of Crewlet's own
// ([crewletServers]) the leaves [hermesOwnLeaves] states; the toolsets it
// disables, which the box adds its own to ([hermesInstallOverlay]):
// [hermesWebToolsets] on a web-denied run, else [hermesBrowserToolset] on a
// run handed no seat's browser; and where a server is always loaded, Hermes's
// tool search off. JSON, which Hermes's YAML reader reads. Hermes merges it
// over the box's configuration per leaf (measured on 0.23.3), so the box's own
// servers stay. Tool search defers every MCP server's tools and exempts none by
// server (0.23.3's is_deferrable_tool_name), so it goes whole: every tool on
// every request.
func hermesOverlay(servers map[string]sandbox.MCPServer, webDenied bool) ([]byte, error) {
	overlay := map[string]any{}
	if len(servers) > 0 {
		out := map[string]map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(servers)) {
			s := servers[name]
			entry := map[string]any{}
			if secs := waitSeconds(s.CallTimeout); secs > 0 {
				entry["timeout"] = secs
			}
			put := func(key string, v any, set bool) {
				if set {
					entry[key] = v
				}
			}
			if s.Transport == sandbox.TransportHTTP {
				put("url", s.URL, s.URL != "")
				put("headers", s.Headers, len(s.Headers) > 0)
				if s.AlwaysLoaded {
					maps.Copy(entry, hermesOwnLeaves())
					if len(s.Headers) == 0 {
						entry["headers"] = []string{}
					}
				}
			} else {
				put("command", s.Command, s.Command != "")
				put("args", s.Args, len(s.Args) > 0)
				put("env", s.Env, len(s.Env) > 0)
			}
			out[name] = entry
		}
		overlay["mcp_servers"] = out
	}
	_, seatBrowser := servers[browser.ServerName]
	switch {
	case webDenied:
		overlay["agent"] = map[string]any{"disabled_toolsets": hermesWebToolsets}
	case !seatBrowser:
		overlay["agent"] = map[string]any{"disabled_toolsets": []string{hermesBrowserToolset}}
	}
	if slices.ContainsFunc(slices.Collect(maps.Values(servers)), func(s sandbox.MCPServer) bool { return s.AlwaysLoaded }) {
		overlay["tools"] = map[string]any{"tool_search": map[string]any{"enabled": "off"}}
	}
	// A `${` IN A VALUE would be expanded by Hermes from the process's
	// environment, so a value holding one is refused rather than changed.
	raw, err := json.Marshal(overlay)
	if err != nil {
		return nil, err
	}
	if strings.Contains(string(raw), "${") {
		return nil, errors.New("executor: a value of the run's MCP servers holds `${`, which Hermes would " +
			"expand from the box's environment, so the run cannot be wired as written")
	}
	return raw, nil
}

// hermesOwnLeaves are the leaves a Hermes run states on each of Crewlet's own
// servers, so the box's entry of that name cannot cut it off: Hermes 0.23.3
// merges the overlay per leaf, a null clearing a scalar or a list
// (_deep_merge). A box's `transport: sse`, `auth`, a `command` with `args`
// (skipped as suspicious), a malformed `tool_costs`, `spend_limit` or
// `cost_unit` scalar, an `ssl_verify` CA path or a `client_cert` or
// `client_key` (each left the bridge unreached) give way (measured). Its own
// `enabled: false`, `disabled` or `tools` lists the overlay never states back
// but the guard refuses, as it does an `env` or a `tool_costs` table, which a
// null cannot clear ([hermesGuard]); `headers` is replaced whole by an empty list where the run
// sends none, which every reader of 0.23.3 takes as no headers (`or {}`).
func hermesOwnLeaves() map[string]any {
	return map[string]any{"transport": nil, "auth": nil, "connect_timeout": hermesConnectSeconds, "command": nil, "args": nil,
		"tool_costs": nil, "spend_limit": nil, "cost_unit": nil, "ssl_verify": true, "client_cert": nil, "client_key": nil}
}

// hermesConnectSeconds is the run's servers' `connect_timeout`: Hermes
// 0.23.3's own default (tools/mcp_tool.py's _DEFAULT_CONNECT_TIMEOUT), under
// which every run whose box names none was measured to connect; stated so a
// box's smaller value cannot cut the bridge's first connection short.
const hermesConnectSeconds = 60

// hermesPluginSettings is the plugin's settings for req's run
// ([Paths.HermesPluginConfig]): its record, the route files it reads at each
// use, each skill's digest as installed, keyed by its listed name, and how it
// hands a checkout cloned during the run (nested, nil where the box has no
// note helper).
func hermesPluginSettings(req sandbox.RunRequest, paths Paths, nested *hermesNestedHand) ([]byte, error) {
	settings := map[string]any{"record": paths.HermesRecord()}
	if nested != nil {
		settings["checkouts"] = nested
	}
	if req.Steer.Set() {
		settings["notes_url_file"] = paths.HermesNotesURL()
	}
	if len(req.Skills) > 0 {
		digests := map[string]string{}
		for _, s := range req.Skills {
			file, err := skillFile(s.Listed(), s.Description, s.Body)
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(file)
			digests[s.Listed()] = hex.EncodeToString(sum[:])
		}
		settings["skills_url_file"] = paths.HermesSkillsURL()
		settings["skills"] = digests
	}
	return json.Marshal(settings)
}

// hermesSkillDir is the directory a skill is installed in under
// [Paths.HermesSkills]: its listed name.
func hermesSkillDir(s sandbox.Skill) string { return s.Listed() }

// WriteConfig writes what a Hermes run starts from, all of it under the work
// directory, for its script to install once the release is known to be one
// the runner drives ([hermesInstall]): the standing instructions
// ([Paths.System]), the overlay for the session it was asked to work in and,
// where it was handed two wirings, for one of its own; the plugin, its
// settings and the routes it reads; the run's skills; and the carry script.
// The box's own Hermes configuration is never written ([hermesOverlay]).
//
// THE STANDING INSTRUCTIONS ARE TOLD OF NOTES on a run wired to them
// ([withNoteInstruction]).
func (h Hermes) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, _ Session,
	paths Paths) (string, error) {
	// EVERY REFUSAL BEFORE THE BOX IS TOUCHED: a refused launch leaves the
	// box as it found it, as every runner's WriteConfig does.
	system := req.System
	if req.Steer.Set() {
		system = withNoteInstruction(system, h.NoteInstruction())
	}
	if len(system) > hermesSystemMaxBytes {
		return "", fmt.Errorf("executor: the run's standing instructions are %d bytes, and Hermes takes them in "+
			"one environment variable, which holds %d; shorten the seat's instructions or memory", len(system),
			hermesSystemMaxBytes)
	}
	overlay, err := hermesOverlay(req.MCPServers, req.WebDenied)
	if err != nil {
		return "", err
	}
	var fresh []byte
	if f := req.FreshWiring; f != nil {
		if f.Steer.Set() != req.Steer.Set() || (len(req.Skills) > 0 && !f.SkillLoads.Set()) {
			return "", errors.New("executor: the run's two wirings, for the session it continues and for " +
				"one of its own, do not reach the same routes, so the run cannot be wired to whichever " +
				"session its box starts")
		}
		if fresh, err = hermesOverlay(f.MCPServers, req.WebDenied); err != nil {
			return "", err
		}
	}
	// THE NOTE HELPER IN EVERY BOX ITS MACHINE HAS A BUILD FOR, which hands
	// the checkouts below the start directory theirs ([Hermes.checkoutHome]);
	// a box with none starts without them, its gate saying so.
	var nested *hermesNestedHand
	if err = writeNoteHelper(ctx, box, paths.HermesNoteHelper()); err == nil {
		nested = hermesNested(paths)
	} else if !errors.Is(err, errNoHelperBuild) {
		return "", err
	}
	launch := h.checkoutHome(paths).launchConfig(hermesCheckouts(paths), req.ReportedRepos)
	if err = box.WriteFile(ctx, paths.HermesCheckoutLaunch(), launch); err != nil {
		return "", fmt.Errorf("executor: writing what hands the run's repositories their instructions: %w", err)
	}
	settings, err := hermesPluginSettings(req, paths, nested)
	if err != nil {
		return "", err
	}
	stage := paths.HermesStage()
	files := []hermesFile{
		{paths.HermesOverlayStaged(), overlay, "Hermes's overlay"},
		{paths.HermesPluginConfig(), settings, "the Hermes plugin's settings"},
		{stage + "/plugin/plugin.yaml", []byte(hermesPluginManifest), "the Hermes plugin's manifest"},
		{stage + "/plugin/__init__.py", hermesPluginSource, "the Hermes plugin"},
		{paths.HermesCarry(), hermesCarryScript, "the Hermes session reader"},
	}
	if system != "" {
		files = append(files, hermesFile{paths.System(), []byte(system), "Hermes's standing instructions"})
	}
	for _, s := range req.Skills {
		file, err := skillFile(s.Listed(), s.Description, s.Body)
		if err != nil {
			return "", err
		}
		files = append(files, hermesFile{stage + "/skills/" + hermesSkillDir(s) + "/SKILL.md", file, fmt.Sprintf("the run's skill %q", s.Listed())})
	}
	write := func(path string, body []byte, what string) error {
		if err := box.WriteFile(ctx, path, body); err != nil {
			return fmt.Errorf("executor: writing %s: %w", what, err)
		}
		return nil
	}
	for _, f := range files {
		if err := write(f.path, f.body, f.what); err != nil {
			return "", err
		}
	}
	if f := req.FreshWiring; f != nil {
		if err := write(paths.HermesOverlayFresh(), fresh, "Hermes's overlay for a session of the run's own"); err != nil {
			return "", err
		}
		if err := hermesRoutes(write, req, *f, paths.HermesNotesURLFresh(), paths.HermesSkillsURLFresh()); err != nil {
			return "", err
		}
	}
	wired := sandbox.Wiring{Steer: req.Steer, SkillLoads: req.SkillLoads}
	if err := hermesRoutes(write, req, wired, paths.HermesNotesURL(), paths.HermesSkillsURL()); err != nil {
		return "", err
	}
	return "", nil
}

// hermesFile is one file WriteConfig writes: its path, its body and what it
// is, as an error names it.
type hermesFile struct {
	path string
	body []byte
	what string
}

// hermesRoutes writes the routes w gives the plugin of req's run to notes and
// skills, each a file of its own the plugin reads at each use.
func hermesRoutes(write func(string, []byte, string) error, req sandbox.RunRequest, w sandbox.Wiring,
	notes, skills string) error {
	if req.Steer.Set() {
		if err := write(notes, []byte(w.Steer.URL), "the run's notes route"); err != nil {
			return err
		}
	}
	if len(req.Skills) > 0 {
		if err := write(skills, []byte(w.SkillLoads.URL), "the run's skill-load route"); err != nil {
			return err
		}
	}
	return nil
}

// hermesInstall is the part of a run's script that installs what WriteConfig
// staged into the Hermes home, after the release gate: the plugin, enabled
// once per box with Hermes's own command (which adds it to the box's list and
// keeps the rest); the skills an earlier run installed removed and the run's
// own installed beside the box's ([Paths.HermesSkills]).
func hermesInstall(req sandbox.RunRequest, paths Paths) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	stage := paths.HermesStage()
	installed := q(paths.WorkDir() + "/hermes-installed-skills")
	line(`mkdir -p %s %s && rm -rf %s && cp -R %s %s || { echo 'crewlet: the runner'"'"'s Hermes plugin could not be installed, so the run does not start' >&2; exit 1; }`,
		q(paths.HermesHome()+"/plugins"), q(paths.HermesSkills()), q(paths.HermesPlugin()), q(stage+"/plugin"),
		q(paths.HermesPlugin()))
	// NOT OVER A PERSON'S DISABLE: `plugins enable` takes the plugin out of a
	// box file's `plugins.disabled` (measured on 0.23.3), so a box that
	// disables it before its first run is refused rather than switched back on.
	line(`[ -e %s ] || { po=$(hcarry plugoff %s); case "$po" in 'crewlet-carry refused '*) printf 'crewlet: Hermes Agent would not load the run'"'"'s own plugin: %%s, which the run never switches back on (remove it). The run was refused before Hermes Agent started.\n' "${po#crewlet-carry refused }" >&2; exit 1;; esac; }`,
		q(paths.HermesEnabled()), hermesPluginName)
	line(`[ -e %s ] || { hermes plugins enable %s < /dev/null > /dev/null 2>&1 && : > %s; } || { echo 'crewlet: hermes plugins enable %s failed, so the run does not start' >&2; exit 1; }`,
		q(paths.HermesEnabled()), hermesPluginName, q(paths.HermesEnabled()), hermesPluginName)
	line(`if [ -s %[1]s ]; then while IFS= read -r s; do case "$s" in ''|*/*|.*) ;; *) rm -rf %[2]s/"$s";; esac; done < %[1]s; fi`,
		installed, q(paths.HermesSkills()))
	line(`: > %s.next || exit 1`, installed)
	for _, s := range req.Skills {
		dir := hermesSkillDir(s)
		line(`rm -rf %[1]s && cp -R %[2]s %[1]s && printf '%%s\n' %[3]s >> %[4]s.next || { echo 'crewlet: the run'"'"'s skill %[3]s could not be installed, so the run does not start' >&2; exit 1; }`,
			q(paths.HermesSkills()+"/"+dir), q(stage+"/skills/"+dir), q(dir), installed)
	}
	line(`mv -f %[1]s.next %[1]s || exit 1`, installed)
	return b.String()
}
