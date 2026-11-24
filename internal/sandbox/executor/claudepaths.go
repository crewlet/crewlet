package executor

// ClaudeConfigDir is the directory a Claude Code run keeps its state in, which
// the run pins as CLAUDE_CONFIG_DIR ([claudeSessionLayout]): the box home's
// `.claude`, where the harness keeps it by default and where a local box is
// seeded with the entry's login (`.claude/.credentials.json`). Named by the
// box's own home, as every path the runner hands the box is, so no profile the
// login shell sources can move it.
func (p Paths) ClaudeConfigDir() string { return p.Home + "/.claude" }

// ClaudeState is the home's own `.claude.json`, where Claude Code keeps its
// state, user and local MCP servers among it, when CLAUDE_CONFIG_DIR is unset;
// the run's [Paths.ClaudeConfigDir] links its own copy there ([claudeStateLink]).
func (p Paths) ClaudeState() string { return p.Home + "/.claude.json" }

// ClaudeNotesSystem is where a Claude Code run wired to its turn's notes has
// its standing instructions written a second time, with its harness's instruction
// about notes after them ([ClaudeCodeSteerInstruction]): the text the run's
// script names in place of [Paths.System] on a release whose note hooks it
// starts ([claudeNotesGate]), so the instruction reaches a run its notes can
// reach and no other. Beside it, and a file for the reason it is.
func (p Paths) ClaudeNotesSystem() string { return p.WorkDir() + "/system-notes.md" }

// ClaudeSettings is where every Claude Code run's own settings file is written
// — the setting that holds the harness's own retention sweep off
// ([claudeSettings]) — named on its command line with `--settings`, unless the
// run is started with [Paths.ClaudeNotesSettings] in its place.
//
// BESIDE mcp.json, never in the checkout's `.claude/`: the checkout is the
// repository's, which the run edits and a commit carries, and a file there would
// replace the repository's own project settings rather than sit above them.
// `--settings` adds a layer the harness ranks above the repository's settings and
// below a managed policy, so the repository keeps its own and the run's settings
// are there whatever it holds. Under WorkDir, so a `git clean` in the brief
// cannot remove it from under the run.
func (p Paths) ClaudeSettings() string { return p.WorkDir() + "/claude-settings.json" }

// ClaudeNoProxy is the NO_PROXY and no_proxy entries of the settings files a
// Claude Code run loads, comma-joined, which its script merges into its own
// settings ([claudeNoProxyMerge]); empty where no file sets either.
func (p Paths) ClaudeNoProxy() string { return p.WorkDir() + "/claude-no-proxy" }

// ClaudeNotesSettings is where a Claude Code run wired to its turn's notes has
// its settings written a second time, with the hooks that hand its main agent a
// person's notes between its steps ([claudeNotesSettings]): the file the run's
// script names in place of [Paths.ClaudeSettings] on a release whose note hooks
// it starts ([claudeNotesGate]). Beside it, and in the work directory for the
// reason it is.
func (p Paths) ClaudeNotesSettings() string { return p.WorkDir() + "/claude-settings-notes.json" }

// ClaudeSkillsSettings is where a Claude Code run handed skills of its own has
// its settings written with hooks switched on ([claudeHooksOnSettings]), so
// the plugin's hook that reports each load runs whatever the repository's own
// settings say: the file the run's script names in place of
// [Paths.ClaudeSettings] on a release that runs that hook ([claudeSkillsGate]),
// unless it starts the notes' hooks, whose own settings switch hooks on too
// ([Paths.ClaudeNotesSettings]). Beside the others, for their reason.
func (p Paths) ClaudeSkillsSettings() string { return p.WorkDir() + "/claude-settings-skills.json" }

// ClaudeSettingsPinned is [Paths.ClaudeSettings]'s pinned copy: the same
// settings with the compaction point and a resume's load pinned besides
// ([claudeSettingsPin]), which the run's script names in that file's place on
// a release that takes the pin ([claudePinGate]). Beside it, for its reason.
func (p Paths) ClaudeSettingsPinned() string { return p.WorkDir() + "/claude-settings-pinned.json" }

// ClaudeNotesSettingsPinned is [Paths.ClaudeNotesSettings]'s pinned copy
// ([Paths.ClaudeSettingsPinned]).
func (p Paths) ClaudeNotesSettingsPinned() string {
	return p.WorkDir() + "/claude-settings-notes-pinned.json"
}

// ClaudeSkillsSettingsPinned is [Paths.ClaudeSkillsSettings]'s pinned copy
// ([Paths.ClaudeSettingsPinned]).
func (p Paths) ClaudeSkillsSettingsPinned() string {
	return p.WorkDir() + "/claude-settings-skills-pinned.json"
}

// ClaudeSkillsPlugin is the DIRECTORY of the plugin a Claude Code run is handed
// its own skills in ([sandbox.RunRequest.Skills], [ClaudeCode.WriteConfig]):
// its manifest, a directory per skill, and the hook that reports each load to
// the run's bridge — named on the run's command line with `--plugin-dir`, so
// the skills are the run's alone, under the plugin's name. Under WorkDir, never
// in the checkout or the box's `.claude/skills/`, where a commit could carry
// them or the next run in the box would find them; and its own, removed whole
// before every run ([Runner.ClearArtifacts]).
func (p Paths) ClaudeSkillsPlugin() string { return p.WorkDir() + "/claude-skills" }
