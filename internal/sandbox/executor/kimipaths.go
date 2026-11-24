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

// KimiMCPConfig is the home's MCP servers file, written whole by every run with
// its seat's servers: the first of the layers Kimi merges, and the only one an
// untrusted workspace reads ([kimiSwitchesOff]).
func (p Paths) KimiMCPConfig() string { return p.KimiHome() + "/mcp.json" }

// KimiSkills and KimiInstructions are the home's skills directory and personal
// AGENTS.md, and KimiUserSkills and KimiUserInstructions the ones Kimi reads
// beside them under `~/.agents`: 2.7.3 hands its model every skill and both
// AGENTS.md files there (measured). No run writes them; the clear removes them
// ([Kimi.artifacts]).
func (p Paths) KimiSkills() string { return p.KimiHome() + "/skills" }

// KimiInstructions is the home's personal AGENTS.md ([Paths.KimiSkills]).
func (p Paths) KimiInstructions() string { return p.KimiHome() + "/AGENTS.md" }

// KimiUserSkills is `~/.agents/skills` ([Paths.KimiSkills]).
func (p Paths) KimiUserSkills() string { return p.Home + "/.agents/skills" }

// KimiUserInstructions is every AGENTS.md name Kimi reads under `~/.agents`
// ([Paths.KimiSkills]).
func (p Paths) KimiUserInstructions() []string {
	return []string{p.Home + "/.agents/AGENTS.md", p.Home + "/.agents/agents.md"}
}

// KimiConfig is the home's config.toml: where a local box is seeded with the
// entry's login, and where Kimi also reads hooks, plugins, permission rules
// and tool settings, so every run rewrites it to its login's tables first
// ([writeKimiLoginConfig]).
func (p Paths) KimiConfig() string { return p.KimiHome() + "/config.toml" }

// KimiPlugins is where Kimi keeps the plugins installed in the home: their
// record (`installed.json`) and packages. 2.7.3 runs an installed plugin's
// hooks and adds its system prompt with no step of a run's (measured), so the
// clear removes it whole ([Kimi.artifacts]); no run installs one.
func (p Paths) KimiPlugins() string { return p.KimiHome() + "/plugins" }

// KimiTrust is where Kimi keeps the workspaces trusted in the home. A run's
// workspace is never trusted ([kimiSwitchesOff]), but a trust saved through
// Kimi's own API from inside a run would hold for the next run in the box and
// load the repository's MCP servers (measured on 2.7.3), so the clear removes
// it ([Kimi.artifacts]).
func (p Paths) KimiTrust() string { return p.KimiHome() + "/workspace-trust" }

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
