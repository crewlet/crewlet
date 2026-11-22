package executor

import _ "embed"

// openCodeToolEnvPlugin is opencode_toolenv.js, which keeps the variables an
// OpenCode run's harness alone reads out of the commands its model runs.
// Written as its directory's `index` module ([Paths.OpenCodeToolEnvPlugin],
// [openCodeSteerEntry]).
//
//go:embed opencode_toolenv.js
var openCodeToolEnvPlugin []byte

// openCodeServerEnv is the variables the script hands an OpenCode run's server
// alone ([openCodeScript]): the run's configuration and the session its notes
// plugin admits to. Its tools' commands are started without them
// ([toolEnvOmitted]).
var openCodeServerEnv = []string{"OPENCODE_CONFIG_CONTENT", "CREWLET_STEER_SESSION"}
