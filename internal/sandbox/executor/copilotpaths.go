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
// written, the home's own after them: Copilot appends this file to its own
// system prompt after its tool list and before the repository's instructions,
// and renders it afresh on every resume (measured on 1.0.114), so a continued
// run is told by its own launch. The home's file is held and put back.
func (p Paths) CopilotInstructions() string { return p.CopilotHome() + "/copilot-instructions.md" }

// CopilotHeldInstructions is the home's own [Paths.CopilotInstructions], held
// while the run's stand in their place ([holdHome], [Copilot.homeRestore]).
func (p Paths) CopilotHeldInstructions() string { return p.WorkDir() + "/copilot-home-instructions.md" }

// CopilotHeldSettings is the home's own [Paths.CopilotSettings], held while
// the run's keys stand in it ([holdHome], [Copilot.homeRestore]).
func (p Paths) CopilotHeldSettings() string { return p.WorkDir() + "/copilot-home-settings.json" }

// CopilotExtension is the directory of the run's notes extension
// (copilot_steer.js), written as extension.mjs where the run reads notes or has
// a turn cap, and removed where it has neither.
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

// CopilotGuardSettings is where the launch writes the settings the guard asks
// the box's Copilot about ([copilotGuard]), which the guard's shell moves into
// a home of its own. Beside the run's other artefacts, never in the home.
func (p Paths) CopilotGuardSettings() string { return p.WorkDir() + "/copilot-guard-settings.json" }
