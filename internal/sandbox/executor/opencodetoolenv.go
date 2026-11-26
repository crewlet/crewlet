package executor

import _ "embed"

// openCodeToolEnvPlugin is opencode_toolenv.js, which keeps the variables
// its server alone is handed ([openCodeHarnessEnv]) out of the commands an
// OpenCode run's model runs, on every run.
// Written as its directory's `index` module ([Paths.OpenCodeToolEnvPlugin],
// [openCodeSteerEntry]).
//
//go:embed opencode_toolenv.js
var openCodeToolEnvPlugin []byte

// openCodeHarnessEnv is the variables an OpenCode run's script hands its
// server alone ([openCodeScript]), which its tools' commands start without,
// so that a tool the model runs does not pick one up by accident: the run's
// configuration, which names the bridge's token, and the session its notes
// plugin admits to. Measured on 2.3.2, OpenCode hands its commands both
// otherwise.
var openCodeHarnessEnv = []string{"OPENCODE_CONFIG_CONTENT", "CREWLET_RUN_SESSION"}
