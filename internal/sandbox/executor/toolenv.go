package executor

import (
	"slices"
	"strings"
)

// shellOff is what the commands a run's model runs start without: Crewlet's
// own variables alone, never the harness's sign-in nor anything the seat set,
// so that a tool the model runs does not pick one up by accident. It is no
// protection: the harness's own files, which the model can read, hold the
// run's bridge token (code-sandbox.md, "Credentials and the run environment").
// Every runner applies it, through its harness's own channel for the shell's
// environment, or by starting a harness that reads none of them without them.
type shellOff struct {
	// names go by name, prefixes every variable whose name starts so.
	names, prefixes []string
}

// crewletShellOff is every run's [shellOff]: its telemetry, every OTEL_
// variable (the endpoint carries the run's token) and the trace context,
// TRACEPARENT and TRACESTATE ([sandbox.OtelReceiver.RunEnv]); and own, the
// runner's own variables that carry or lead to the run's bridge token.
func crewletShellOff(own ...string) shellOff {
	names := slices.Concat([]string{"TRACEPARENT", "TRACESTATE"}, own)
	slices.Sort(names)
	return shellOff{names: slices.Compact(names), prefixes: []string{"OTEL_"}}
}

// script is s as POSIX shell, run in the environment it clears: one unset of
// the names, and every variable under a prefix unset whatever its name, as an
// image or an operator may add any. Its loop's variable goes too.
func (s shellOff) script() string {
	var b strings.Builder
	b.WriteString("unset " + strings.Join(s.names, " ") + "\n")
	for _, p := range s.prefixes {
		b.WriteString(`for crewlet_v in $(env | sed -n 's/^\(` + p + `[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$crewlet_v"; done; unset crewlet_v` + "\n")
	}
	return b.String()
}

// globs is s as glob patterns: each name, and each prefix followed by `*`.
func (s shellOff) globs() []string {
	out := slices.Clone(s.names)
	for _, p := range s.prefixes {
		out = append(out, p+"*")
	}
	return out
}
