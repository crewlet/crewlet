package queries

import (
	"reflect"
	"slices"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// shownCalls is a run's call log as a person reading it is shown it, a copy:
// each call's output ([sandbox.ShownOutput]) and arguments ([shownArgs])
// REDACTED AT THIS READ, as a session's transcript is. The log itself stays as
// the run's harness was handed it, which a reading of its session compares
// byte for byte.
func shownCalls(calls []sandbox.BridgeCall) []sandbox.BridgeCall {
	if calls == nil {
		return nil
	}
	out := slices.Clone(calls)
	for i := range out {
		out[i].Output, out[i].OutputBytes = sandbox.ShownOutput(out[i].Output, out[i].OutputBytes)
		out[i].Args = shownArgs(out[i].Args)
	}
	return out
}

// shownArgs is a call's arguments as the log keeps them, JSON text, redacted
// as values ([redact.Args]) and written again as the log writes them
// ([tools.RecordArgs]) only where that changed one, so arguments with nothing
// to redact are served byte for byte. The log writes one object or nothing
// (the bridge by tools.RecordArgs, its cut by internal/kept's Args), so text
// that is no JSON object is no arguments it wrote and is redacted as text;
// arguments that cannot be written again are served as none.
func shownArgs(text string) string {
	if text == "" {
		return text
	}
	args := tools.ReadArgs(text)
	if args == nil {
		return redact.Secrets(text)
	}
	shown := redact.Args(args)
	if reflect.DeepEqual(shown, args) {
		return text
	}
	raw, err := tools.RecordArgs(shown)
	if err != nil {
		return "{}"
	}
	return raw
}
