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

// ClaudeHooksOff is why the settings a Claude Code run loads would run none of
// its hooks ([claudeHooksOff]), empty where nothing switches them off: written
// for a run that may start a hook of the engine's, read by the script once its
// gates have started one ([claudeHooksOffGate]).
func (p Paths) ClaudeHooksOff() string { return p.WorkDir() + "/claude-hooks-off" }

// ClaudeHooksOffBy is each file and key that has Claude Code run no hook of a
// plugin's, `; `-separated, empty where none: written beside
// [Paths.ClaudeHooksOff], read where the checkouts' hook would be the run's
// only one ([claudeCheckoutsGate]).
func (p Paths) ClaudeHooksOffBy() string { return p.WorkDir() + "/claude-hooks-off-by" }

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

// ClaudeSkillsPlugin is the DIRECTORY of the plugin a Claude Code run is handed
// its own skills in ([sandbox.RunRequest.Skills], [ClaudeCode.WriteConfig]):
// its manifest, a directory per skill, and the hook that reports each load to
// the run's bridge — named on the run's command line with `--plugin-dir`, so
// the skills are the run's alone, under the plugin's name. Under WorkDir, never
// in the checkout or the box's `.claude/skills/`, where a commit could carry
// them or the next run in the box would find them; and its own, removed whole
// before every run ([Runner.ClearArtifacts]).
func (p Paths) ClaudeSkillsPlugin() string { return p.WorkDir() + "/claude-skills" }

// ClaudeRepo is the run's checkout state ([ClaudeCode.checkoutHome]): the checkouts
// handed, the lock and the nested hook's config, remade by every run.
func (p Paths) ClaudeRepo() string { return p.WorkDir() + "/claude-repo" }

// ClaudeRepoSkills lists, a name a line, the checkouts' skills a run copied
// into the home's `.claude/skills` ([ClaudeCode.checkoutHome]), so the run's end
// removes them and none of the home's, or the next run's gate where the run
// was killed first.
func (p Paths) ClaudeRepoSkills() string { return p.WorkDir() + "/claude-repo-skills" }

// ClaudeHomeInstructions is the home's own `.claude/CLAUDE.md`, held in the
// work directory while the run's, the home's with its checkouts' after it,
// stands in its place ([ClaudeCode.checkoutHome], [ClaudeCode.homeRestore],
// [putBackHome]).
func (p Paths) ClaudeHomeInstructions() string { return p.WorkDir() + "/claude-home-CLAUDE.md" }

// ClaudeRepoInstructions is where checkout-launch writes the instructions of
// a run's checkouts ([ClaudeCode.checkoutHome]), which the gate then writes after
// the home's own into `.claude/CLAUDE.md` through a link as the home keeps it.
func (p Paths) ClaudeRepoInstructions() string { return p.WorkDir() + "/claude-repo-CLAUDE.md" }

// ClaudeNoteHelper is where a Claude Code run's launch writes the note helper
// its checkout gate and checkouts' hook run ([ClaudeCode.checkoutHome]).
func (p Paths) ClaudeNoteHelper() string { return p.WorkDir() + "/claude-notehelper" }

// ClaudeCheckoutLaunch is the run's checkout-launch config, which its launch
// writes ([claudeCheckouts]) and its script hands the note helper.
func (p Paths) ClaudeCheckoutLaunch() string { return p.WorkDir() + "/claude-checkout-launch.json" }

// ClaudeCheckoutsPlugin is the DIRECTORY of the plugin that holds a Claude
// Code run's checkouts' hook ([writeClaudeCheckouts]), named with
// `--plugin-dir` where the run's script starts it ([claudeCheckoutsGate]).
func (p Paths) ClaudeCheckoutsPlugin() string { return p.WorkDir() + "/claude-checkouts" }

// ClaudeSettingsGuarded is the guarded copy of a run's settings, handed to a
// run with a bridge in place of every run's and
// the skills' pinned copies ([claudePinGate]): the pin, hooks on, and the
// bridge's guard ([claudeGuardHooks]). Beside them, for their reason.
func (p Paths) ClaudeSettingsGuarded() string { return p.WorkDir() + "/claude-settings-guarded.json" }

// ClaudeNotesSettingsGuarded is [Paths.ClaudeNotesSettingsPinned]'s guarded
// copy: the notes' hooks beside the guard's ([Paths.ClaudeSettingsGuarded]).
func (p Paths) ClaudeNotesSettingsGuarded() string {
	return p.WorkDir() + "/claude-settings-notes-guarded.json"
}

// ClaudeBridgeGuard is the script the guard's PreToolUse hook runs
// ([claudeGuardScript]). Under WorkDir, never in the checkout.
func (p Paths) ClaudeBridgeGuard() string { return p.WorkDir() + "/claude-bridge-guard.sh" }

// ClaudeUnbridged is where the guard records the server's source and the agent
// of a call it refused, which the run's script reads once the harness exits
// ([claudeGuardLines]).
func (p Paths) ClaudeUnbridged() string { return p.WorkDir() + "/claude-unbridged" }
