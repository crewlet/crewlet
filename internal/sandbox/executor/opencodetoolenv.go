package executor

import _ "embed"

// openCodeToolEnvPlugin is opencode_toolenv.js, which keeps the variables an
// OpenCode run's harness alone reads out of the commands its model runs.
// Written as its directory's `index` module ([Paths.OpenCodeToolEnvPlugin],
// [openCodeSteerEntry]).
//
//go:embed opencode_toolenv.js
var openCodeToolEnvPlugin []byte

// openCodeHarnessEnv is the variables an OpenCode run's harness alone reads,
// beside its sign-in, which its tools' commands start without
// ([toolEnvOmitted]): what the script hands its server alone ([openCodeScript]),
// the run's configuration and the session its notes plugin admits to, and the
// run's telemetry export, its endpoint carrying the run's token
// ([sandbox.OtelReceiver.RunEnv]). Measured on 2.3.0, OpenCode hands its
// commands all four otherwise.
var openCodeHarnessEnv = []string{"OPENCODE_CONFIG_CONTENT", "CREWLET_STEER_SESSION",
	"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS"}
