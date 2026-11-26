package executor

// KimiHome is the directory a Kimi Code run keeps its state in, pinned as
// KIMI_CODE_HOME ([Kimi.Pins]): the box home's `.kimi-code`, where the harness
// keeps it by default and where a local box is seeded with the entry's login
// (`credentials/kimi-code.json` and `config.toml`). Its sessions are
// under `sessions/`, and the run's agent profile and MCP servers are written
// there ([Kimi.WriteConfig]), the one place `kimi web` reads each from.
func (p Paths) KimiHome() string { return p.Home + "/.kimi-code" }

// KimiAgents is the directory of the home's agent profiles, where a run's own
// profile is written under a name of its own ([kimiProfileName]).
func (p Paths) KimiAgents() string { return p.KimiHome() + "/agents" }

// KimiMCPConfig is the home's MCP servers file, the first of the layers Kimi
// merges: while a run's servers stand in it beside the home's own
// ([kimiMCPOver]), the home's own file is held at [Paths.KimiHomeMCPConfig].
func (p Paths) KimiMCPConfig() string { return p.KimiHome() + "/mcp.json" }

// KimiHomeMCPConfig is the home's own `mcp.json`, held in the run's work
// directory while the run's servers stand in its place, and put back once
// Kimi has stopped ([kimiRestoreMCP]): empty where the home had none.
func (p Paths) KimiHomeMCPConfig() string { return p.WorkDir() + "/kimi-home-mcp.json" }

// KimiMCPOurs is the names the run's servers stand under in the home's
// `mcp.json`, recorded before they are written there and removed once the
// home's own file is back: a run cut off between the two left them, and the
// next run's start takes them out of the file as it then stands
// ([kimiMCPLeftOut]).
func (p Paths) KimiMCPOurs() string { return p.WorkDir() + "/kimi-mcp-ours.json" }

// KimiSessions is the directory Kimi files each session's directory under,
// `wd_<cwd>_<hash>/session_<uuid>/`.
func (p Paths) KimiSessions() string { return p.KimiHome() + "/sessions" }

// KimiDriver is where the run's driver is written (kimi_driver.js), beside the
// run's other artefacts.
func (p Paths) KimiDriver() string { return p.WorkDir() + "/kimi-driver.mjs" }

// KimiSettings is the driver's settings ([kimiSettings]): its profile, model,
// notes route and the files it writes. It holds the notes route's token, so it
// is the run's own, removed by the next run's clear.
func (p Paths) KimiSettings() string { return p.WorkDir() + "/kimi-run.json" }

// KimiServerPid is where the driver records the pid of the `kimi web` it
// started, which the run's script ends if it outlived the driver ([kimiScript]).
func (p Paths) KimiServerPid() string { return p.WorkDir() + "/kimi-server.pid" }

// KimiMCPConfigFresh is where a continuing run's MCP servers are written a
// second time, wired to the job's other bridge token, for a session its script
// starts in the continued one's place ([sandbox.RunRequest.FreshWiring]),
// which the script moves over [Paths.KimiMCPConfig] where it starts one and
// removes where it continues ([kimiSessionGate]).
func (p Paths) KimiMCPConfigFresh() string { return p.WorkDir() + "/kimi-mcp-fresh.json" }

// KimiSettingsFresh is the driver's settings written a second time, for the
// same session ([Paths.KimiMCPConfigFresh]): their notes route is the job's
// other one.
func (p Paths) KimiSettingsFresh() string { return p.WorkDir() + "/kimi-run-fresh.json" }

// KimiLinesBefore is where a continuing run's script records how many lines
// each agent's wire.jsonl held before its driver started ([kimiLinesBefore]).
func (p Paths) KimiLinesBefore() string { return p.WorkDir() + "/kimi-lines-before" }

// KimiNoteHelper is where a Kimi run's note helper is written, which hands the
// checkouts below its start directory theirs ([kimiCheckoutHome]).
func (p Paths) KimiNoteHelper() string { return p.WorkDir() + "/kimi-notehelper" }

// KimiCheckouts is the run's checkout state, which the note helper's
// checkout-launch remakes and its kimi-nested firings read.
func (p Paths) KimiCheckouts() string { return p.WorkDir() + "/kimi-checkouts" }

// KimiCheckoutLaunch is the run's checkout-launch config ([kimiCheckouts]).
func (p Paths) KimiCheckoutLaunch() string { return p.WorkDir() + "/kimi-checkout-launch.json" }

// KimiCheckoutSections is where checkout-launch writes the checkouts'
// instructions, which the gate writes after the home's own into
// [Paths.KimiHomeAgents].
func (p Paths) KimiCheckoutSections() string { return p.WorkDir() + "/kimi-checkout-sections.md" }

// KimiCheckoutSkillsCopied lists, a name a line, the checkouts' skills a run
// copied into [Paths.KimiSkills], for its end, or the next run's gate, to
// remove.
func (p Paths) KimiCheckoutSkillsCopied() string { return p.WorkDir() + "/kimi-checkout-skills" }

// KimiHomeAgents is the home's instruction file, which Kimi Code 2.7.2 loads
// whole ahead of a project's: KIMI_CODE_HOME/AGENTS.md.
func (p Paths) KimiHomeAgents() string { return p.KimiHome() + "/AGENTS.md" }

// KimiHomeInstructions is the home's own [Paths.KimiHomeAgents], held beside
// the run while the checkouts' sections stand after it.
func (p Paths) KimiHomeInstructions() string { return p.WorkDir() + "/kimi-home-instructions.md" }

// KimiSkills is the home's skills directory, the first of the user skill
// roots Kimi Code 2.7.2 reads, which the checkouts' skills are copied into.
func (p Paths) KimiSkills() string { return p.KimiHome() + "/skills" }
