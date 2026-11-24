package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A HOME, A REPOSITORY OR A MACHINE'S POLICY CANNOT SWITCH A CLAUDE CODE RUN'S
// HOOKS OFF UNSEEN, AND THE RUN NEVER SWITCHES THEM BACK ON. Claude Code
// 2.1.330 runs no hook at all where the settings it merges leave
// `disableAllHooks` true, a later file's value winning, and only managed hooks
// where a managed file sets `allowManagedHooksOnly`, and no `--settings` hook,
// the notes' ride, where it sets `strictPluginOnlyCustomization` locking them;
// the run's notes and skill loads then go unread with no error (measured). A
// `false` in the run's own settings would run the home's and the repository's
// hooks too, where a person's session runs none. So a run that starts a hook
// of its own is refused before the harness starts, naming the file and the key
// ([claudeHooksOff], [claudeHooksOffGate]).

// claudeHooksOffWait bounds the one exec reading the settings files: a few
// small files, read in a box that must answer as fast to start the run.
const claudeHooksOffWait = 30 * time.Second

// claudeHooksOffFiles is every settings file Claude Code 2.1.330 merges for a
// run, lowest first: the home's (the run's pinned configuration directory),
// the start directory's shared and local files, then the managed ones. It
// reads no `settings.local.json` in the home, skips a file it cannot read (a
// directory, mode 000, a broken link: all measured), and the run's own
// `--settings`, between the local and the managed files, sets neither key.
func claudeHooksOffFiles(paths Paths, start string) []string {
	return append([]string{paths.ClaudeConfigDir() + "/settings.json", start + "/.claude/settings.json",
		start + "/.claude/settings.local.json"}, claudeManagedSettings...)
}

// claudeHooksOffVerdict is why Claude Code would run none of the run's hooks
// given files, read in merge order: the last file holding `disableAllHooks` as
// a boolean decides, and so does the last managed one holding
// `allowManagedHooksOnly`; and, for a run whose notes ride the `--settings`
// hooks, a managed `strictPluginOnlyCustomization` locking hooks
// ([claudeStrictLocksHooks]); "" where none says so. A file that is no JSON
// object, or holds either boolean key as anything but a boolean, Claude Code
// ignores whole (measured), so it decides nothing. One it drops for another
// key's invalid value is still read here, a wider refusal than the harness's.
func claudeHooksOffVerdict(files []boxFileRead, notes bool, skills []sandbox.Skill) string {
	var off, managedOnly string
	var strict []string
	for _, f := range files {
		var settings map[string]json.RawMessage
		if json.Unmarshal(f.raw, &settings) != nil {
			continue
		}
		all, allSet := claudeBool(settings, "disableAllHooks")
		only, onlySet := claudeBool(settings, "allowManagedHooksOnly")
		if !allSet || !onlySet {
			continue
		}
		if all != nil {
			off = ""
			if *all {
				off = f.path
			}
		}
		if !claudeManagedFile(f.path) {
			continue
		}
		if only != nil {
			managedOnly = ""
			if *only {
				managedOnly = f.path
			}
		}
		if raw, ok := settings["strictPluginOnlyCustomization"]; ok && notes && claudeStrictLocksHooks(raw) {
			strict = append(strict, f.path+" sets strictPluginOnlyCustomization to "+codexShown(raw)+
				", which locks the settings' hooks the run's notes ride")
		}
	}
	var said []string
	if off != "" {
		said = append(said, off+" sets disableAllHooks to true")
	}
	if managedOnly != "" {
		said = append(said, managedOnly+" sets allowManagedHooksOnly to true, which runs the machine's "+
			"managed hooks alone")
	}
	said = append(said, strict...)
	var parts []string
	if len(said) > 0 {
		parts = append(parts, fmt.Sprintf("Claude Code would run none of the run's hooks, which its notes and "+
			"skill loads go through: %s, and no setting of the run's may switch hooks back on, since that would "+
			"also run the home's and the repository's hooks where a person's session runs none", strings.Join(said, "; ")))
	}
	if off := claudeSkillsOff(files, skills); len(off) > 0 {
		parts = append(parts, fmt.Sprintf("Claude Code would go without the run's own skills: %s, and the run "+
			"never switches back on what a person's settings switch off", strings.Join(off, "; ")))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ". ") + ". Remove that key or entry from that file, or run without notes and skills"
}

// claudeSkillsPluginKey is how Claude Code 2.1.330 names the run's skills'
// plugin in `enabledPlugins`: the plugin's name and `inline`, the marketplace
// of a `--plugin-dir` plugin (measured: `crewlet` alone does nothing).
const claudeSkillsPluginKey = "crewlet@inline"

// claudeSkillsOff is what of files, read in merge order, switches the run's
// skills off, a sentence a file and entry: `enabledPlugins` holding the
// plugin false in the last file that holds it (the merge as for
// disableAllHooks: read, not measured for this key), which drops the plugin
// and every skill in it; and a `permissions.deny` entry `Skill`, which takes
// the skill tool away, or `Skill(<name>)` naming one of skills by its listed
// name, `*` ending a prefix (each measured on 2.1.330 but the prefix, which
// is read), which blocks its load. Deny entries add up across files.
func claudeSkillsOff(files []boxFileRead, skills []sandbox.Skill) []string {
	if len(skills) == 0 {
		return nil
	}
	var said []string
	plugin := ""
	for _, f := range files {
		var settings struct {
			Plugins     map[string]json.RawMessage `json:"enabledPlugins"`
			Permissions struct {
				Deny []string `json:"deny"`
			} `json:"permissions"`
		}
		if json.Unmarshal(f.raw, &settings) != nil {
			continue
		}
		switch string(settings.Plugins[claudeSkillsPluginKey]) {
		case "false":
			plugin = f.path
		case "true":
			plugin = ""
		}
		for _, d := range settings.Permissions.Deny {
			if d == "Skill" {
				said = append(said, fmt.Sprintf("%s's permissions.deny holds %q, which takes the skill tool away", f.path, d))
				continue
			}
			inner, ok := strings.CutPrefix(d, "Skill(")
			name, closed := strings.CutSuffix(inner, ")")
			if !ok || !closed {
				continue
			}
			for _, s := range skills {
				prefix, glob := strings.CutSuffix(name, "*")
				if name == s.Listed() || glob && strings.HasPrefix(s.Listed(), prefix) {
					said = append(said, fmt.Sprintf("%s's permissions.deny holds %q, the seat skill %s", f.path, d, s.Listed()))
				}
			}
		}
	}
	if plugin != "" {
		said = append([]string{fmt.Sprintf("%s sets enabledPlugins[%q] to false, the plugin the run's skills are in",
			plugin, claudeSkillsPluginKey)}, said...)
	}
	return said
}

// claudeStrictLocksHooks reports whether a managed strictPluginOnlyCustomization
// value locks the hooks surface: `true`, a list naming "hooks", or anything else
// but `false` and a list, which Claude Code 2.1.330 reads as true ("was present
// but invalid; treating it as true"; measured).
func claudeStrictLocksHooks(raw json.RawMessage) bool {
	var on bool
	if json.Unmarshal(raw, &on) == nil {
		return on
	}
	var surfaces []string
	if json.Unmarshal(raw, &surfaces) == nil {
		return slices.Contains(surfaces, "hooks")
	}
	return true
}

// claudeBool is settings' key as a boolean: nil where the key is missing or
// null, and false as its second answer where it holds anything else.
func claudeBool(settings map[string]json.RawMessage, key string) (*bool, bool) {
	raw, ok := settings[key]
	if !ok || string(raw) == "null" {
		return nil, true
	}
	var v bool
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	return &v, true
}

// claudeManagedFile reports whether file is one of [claudeManagedSettings].
func claudeManagedFile(file string) bool {
	for _, pattern := range claudeManagedSettings {
		if ok, _ := path.Match(pattern, file); ok {
			return true
		}
	}
	return false
}

// claudeHooksOff is why Claude Code in box would run none of a run's hooks
// ([claudeHooksOffVerdict]), notes saying whether they include the notes',
// read from the box as its user reads them (managed files outside the box
// home too, which ReadFile does not reach); "" where nothing switches them off.
func claudeHooksOff(ctx context.Context, box sandbox.Sandbox, paths Paths, notes bool,
	skills []sandbox.Skill) (string, error) {
	start := path.Join(box.Home(), sandbox.WorkspaceSubdir)
	res, err := box.Exec(ctx, boxFilesCommand(claudeHooksOffFiles(paths, start)),
		sandbox.ExecOptions{TimeoutSec: claudeHooksOffWait.Seconds()})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		return "", fmt.Errorf("executor: reading the Claude Code settings that can switch the run's hooks off: %w", err)
	}
	return claudeHooksOffVerdict(boxFilesParse(res.Stdout), notes, skills), nil
}

// claudeHooksOffGate is the part of a run's script that, once its gates have
// started a hook of the engine's (hk), refuses the run where its settings
// switch hooks off ([Paths.ClaudeHooksOff], [claudeHooksOff]), saying why. A
// run whose gates start none, on a release older than every hook's floor, is
// not refused for it.
func claudeHooksOffGate(paths Paths) string {
	f := shellword.Quote(paths.ClaudeHooksOff())
	return `if [ -n "$hk" ] && [ -s ` + f + ` ]; then printf 'crewlet: %s. The run was refused before Claude Code ` +
		`started.\n' "$(cat ` + f + `)" >&2; exit 1; fi` + "\n"
}
