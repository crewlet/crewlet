package executor

// CopilotHome is the directory a Copilot run keeps its state in, pinned as
// COPILOT_HOME ([Copilot.Pins]): the box home's `.copilot`, where the harness
// keeps it by default and where a local box is seeded with the entry's login
// (`config.json`'s copilotTokens and authTokens). Its sessions are under
// `session-state/`, and the run's own settings, standing instructions and
// extension are written there ([Copilot.WriteConfig]), the one place Copilot
// reads each from.
func (p Paths) CopilotHome() string { return p.Home + "/.copilot" }

// CopilotSettings is the home's user-level settings file, which every run
// sets its own keys in, keeping the rest ([copilotSettings]), and puts back
// as the home had it once it ends ([Copilot.homeRestore]).
func (p Paths) CopilotSettings() string { return p.CopilotHome() + "/settings.json" }

// CopilotInstructions is where a Copilot run's standing instructions are
// written, the home's own and its checkouts' sections after them
// ([copilotCheckoutHome]): Copilot appends this file to its own system prompt
// after its tool list and before the repository's instructions, and renders it
// afresh on every resume (measured on 1.0.114), so a continued run is told by
// its own launch. The home's file is held and put back.
func (p Paths) CopilotInstructions() string { return p.CopilotHome() + "/copilot-instructions.md" }

// CopilotHeldInstructions is the home's own [Paths.CopilotInstructions], held
// while the run's stand in their place ([holdHome], [Copilot.homeRestore]).
func (p Paths) CopilotHeldInstructions() string { return p.WorkDir() + "/copilot-home-instructions.md" }

// CopilotHeldSettings is the home's own [Paths.CopilotSettings], held while
// the run's keys stand in it ([holdHome], [Copilot.homeRestore]).
func (p Paths) CopilotHeldSettings() string { return p.WorkDir() + "/copilot-home-settings.json" }

// CopilotExtension is the directory of the run's notes extension
// (copilot_steer.js), written as extension.mjs where the run reads notes, has
// a turn cap or a bridge to guard, and removed where it has none of them.
func (p Paths) CopilotExtension() string { return p.CopilotHome() + "/extensions/crewlet-notes" }

// CopilotSessionDir is session's directory under the run's home: its
// `events.jsonl`, the conversation, and `session.db`, the sql tool's tables,
// which a pack never takes ([copilotPack]).
func (p Paths) CopilotSessionDir(session string) string {
	return p.CopilotHome() + "/" + copilotSessionState + "/" + session
}

// CopilotExtensionFresh is where a continuing run's extension is written a
// second time, wired to the job's other notes route, for a session its script
// starts in the continued one's place ([sandbox.RunRequest.FreshWiring]), which
// the script moves over the extension's file where it starts one and removes
// where it continues ([copilotSessionGate]). Beside the run's other artefacts,
// never in the extension's directory, which Copilot reads.
func (p Paths) CopilotExtensionFresh() string { return p.WorkDir() + "/copilot-extension-fresh.mjs" }

// CopilotNotesState is the file the run's notes extension keeps its requests
// counted, its cursor and the notes it sent in (copilot_steer.js), so that one
// Copilot restarts in the session (extensions_reload) resumes them. Beside the
// run's other artefacts, cleared before every run ([Copilot.artifactFiles]).
func (p Paths) CopilotNotesState() string { return p.WorkDir() + "/copilot-notes-state.json" }

// CopilotGuardSettings is where the launch writes the settings the guard asks
// the box's Copilot about ([copilotGuard]), which the guard's shell moves into
// a home of its own. Beside the run's other artefacts, never in the home.
func (p Paths) CopilotGuardSettings() string { return p.WorkDir() + "/copilot-guard-settings.json" }

// CopilotSkills is the home's skills directory, which Copilot lists every
// `<dir>/SKILL.md` of at launch (measured on 1.0.114), where a run copies its
// checkouts' skills ([copilotCheckoutHome]).
func (p Paths) CopilotSkills() string { return p.CopilotHome() + "/skills" }

// CopilotNoteHelper is the note helper a Copilot run hands its checkouts
// with (internal/notehelper), written at every launch.
func (p Paths) CopilotNoteHelper() string { return p.WorkDir() + "/copilot-notehelper" }

// CopilotRepo is the run's checkout state ([copilotCheckoutHome]): the
// checkouts handed, the lock, the nested adapter's config and each launch
// attempt's directories and kept sets, remade by every run.
func (p Paths) CopilotRepo() string { return p.WorkDir() + "/copilot-repo" }

// CopilotCheckoutLaunch is the run's checkout-launch config, which its launch
// writes ([copilotCheckouts]) and its script hands the note helper.
func (p Paths) CopilotCheckoutLaunch() string { return p.WorkDir() + "/copilot-checkout-launch.json" }

// CopilotRepoInstructions is where a launch attempt's checkout sections are
// written, which the script writes after the home's own into
// [Paths.CopilotInstructions].
func (p Paths) CopilotRepoInstructions() string { return p.WorkDir() + "/copilot-repo-instructions.md" }

// CopilotRepoSkills lists, a name a line, the checkouts' skills a run copied
// into [Paths.CopilotSkills], so the run's restore and the next run remove
// them and none of the home's.
func (p Paths) CopilotRepoSkills() string { return p.WorkDir() + "/copilot-repo-skills" }

// CopilotRunInstructions is the run's standing instructions, which its script
// writes first into [Paths.CopilotInstructions], the home's own after them.
func (p Paths) CopilotRunInstructions() string { return p.WorkDir() + "/copilot-run-instructions.md" }

// CopilotAttempt is the file a run's script writes each launch attempt's
// number to before it starts Copilot, which the extension keys its state by
// ([copilotExtensionSettings]).
func (p Paths) CopilotAttempt() string { return p.WorkDir() + "/copilot-attempt" }

// CopilotStoreCheck is the run's copy of copilot_storecheck.mjs, which its
// script runs after a refused launch attempt ([copilotAttempts]).
func (p Paths) CopilotStoreCheck() string { return p.WorkDir() + "/copilot-storecheck.mjs" }

// CopilotExtensionOff is why the run's extension does no checkout work, a
// line its script adds to the run's skip log ([copilotExtensionOff]); absent
// where it does.
func (p Paths) CopilotExtensionOff() string { return p.WorkDir() + "/copilot-extension-off" }
