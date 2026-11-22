package executor

// KimiHome is the directory a Kimi Code run keeps its state in, pinned as
// KIMI_CODE_HOME ([Kimi.Pins]): the box home's `.kimi-code`, where the harness
// keeps it by default and where a local box is seeded with the entry's login
// (`credentials/oauth/kimi-code.json` and `config.toml`). Its sessions are
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
