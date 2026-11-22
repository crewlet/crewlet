package executor

// ClaudeConfigDir is the directory a Claude Code run keeps its state in, which
// the run pins as CLAUDE_CONFIG_DIR ([claudeSessionLayout]): the box home's
// `.claude`, where the harness keeps it by default and where a local box is
// seeded with the entry's login (`.claude/.credentials.json`). Named by the
// box's own home, as every path the runner hands the box is, so no profile the
// login shell sources can move it.
func (p Paths) ClaudeConfigDir() string { return p.Home + "/.claude" }

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
// repository's, which the run edits and a commit carries, and the run reads no
// project settings there. `--settings` is the one settings layer the run reads
// besides a managed policy ([claudeScript]'s `--setting-sources`). Under WorkDir, so a `git clean`
// in the brief cannot remove it from under the run.
func (p Paths) ClaudeSettings() string { return p.WorkDir() + "/claude-settings.json" }

// ClaudeToolEnv is the file a Claude Code run's harness runs ahead of each
// command its model runs, named by CLAUDE_ENV_FILE ([claudeToolEnv]). Under
// WorkDir beside the settings, for their reasons.
func (p Paths) ClaudeToolEnv() string { return p.WorkDir() + "/claude-tool-env.sh" }

// ClaudeNotesSettings is where a Claude Code run wired to its turn's notes has
// its settings written a second time, with the hooks that hand its main agent a
// person's notes between its steps ([claudeNotesSettings]): the file the run's
// script names in place of [Paths.ClaudeSettings] on a release whose note hooks
// it starts ([claudeNotesGate]). Beside it, and in the work directory for the
// reason it is.
func (p Paths) ClaudeNotesSettings() string { return p.WorkDir() + "/claude-settings-notes.json" }

// ClaudeSkillsPlugin is the DIRECTORY of the plugin a Claude Code run is handed
// its own skills in ([sandbox.RunRequest.Skills], [ClaudeCode.WriteConfig]):
// its manifest, a directory per skill, and the hook that reports each load to
// the run's bridge — named on the run's command line with `--plugin-dir`, so
// the skills are the run's alone, under the plugin's name. Under WorkDir, never
// in the checkout or the box's `.claude/skills/`, where a commit could carry
// them or the next run in the box would find them; and its own, removed whole
// before every run ([Runner.ClearArtifacts]).
func (p Paths) ClaudeSkillsPlugin() string { return p.WorkDir() + "/claude-skills" }
