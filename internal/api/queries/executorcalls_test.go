package queries_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// callPlant is a credential planted in a run's calls, as redact's own corpus
// spells it: the text planted, and the value no read may hold any piece of.
type callPlant struct{ name, text, value string }

func callPlants(t *testing.T) []callPlant {
	t.Helper()
	sample := map[string]string{}
	for _, s := range redacttest.Shapes() {
		sample[s.Name] = s.Sample
	}
	return []callPlant{
		{"a provider's key", sample["anthropic-key"], sample["anthropic-key"]},
		{"a password", sample["password"], strings.TrimPrefix(sample["password"], "password: ")},
		{"a bearer token", "Authorization: Bearer " + sample["jwt"], sample["jwt"]},
	}
}

// showsNone fails where text holds six bytes of p's value, or a marker the
// redaction wrote cut in part, which reads as the text's own words.
func showsNone(t *testing.T, where, text string, p callPlant) {
	t.Helper()
	for i := 0; i+6 <= len(p.value); i++ {
		if strings.Contains(text, p.value[i:i+6]) {
			t.Errorf("%s holds %q of %s", where, p.value[i:i+6], p.name)
			return
		}
	}
	for _, words := range redact.OwnWords(text) {
		if strings.Contains(words, "REDACTED") {
			t.Errorf("%s holds a marker cut in part", where)
			return
		}
	}
}

// AN EXECUTOR RUN'S CALL LOG IS SHOWN WITH NO SECRET IT CARRIED. A provider's
// key, a password and a bearer token, planted in a call's whole output and in
// its arguments, and in outputs the log kept only the opening of, its cut
// falling at every offset of the credential, are read on `bridge_calls` as
// markers: the words around them kept, the arguments one JSON object, a cut
// output the redaction of its opening's complete lines whose size beside it
// reads it back. The log itself is as the run's harness was handed it.
//
// Mutation: serve the log as it stands (no shownCalls), or redact a cut
// output's opening as it stands (no RedactedOpening in ShownOutput), and the
// read holds the plant or the head the log's cut left of it.
func TestAnExecutorRunsCallLogIsShownWithNoSecretItCarried(t *testing.T) {
	t.Parallel()
	const runLog kept.Record = "the run's record"
	for _, p := range callPlants(t) {
		store := seedRuns(t, sandbox.PendingRun{TurnID: "t-1", Role: "Dev", Status: sandbox.StatusRunning})
		run, _, err := store.Get(context.Background(), "t-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		said := "fetched the page\n" + p.text + "\nthen check the logs"
		args, err := tools.RecordArgs(map[string]any{"command": said, "password": p.value, "path": "a.go"})
		if err != nil {
			t.Fatal(err)
		}
		calls := []sandbox.BridgeCall{{Name: "fetch", Args: args, Output: said, At: runBase}}
		// THE LOG'S CUT, for an output of this size of no byte it escapes.
		lead, size := "fetched the page\n", 3*sandbox.MaxBridgeOutputBytes
		probe, _ := runLog.Text(lead+strings.Repeat("a", size-len(lead)), sandbox.MaxBridgeOutputBytes)
		opening, ok := sandbox.OutputOpening(probe, size)
		if !ok {
			t.Fatal("the run's log no longer reads back the cut this test writes as its own")
		}
		for at := 1; at < len(p.text); at++ {
			raw := lead + strings.Repeat("b", len(opening)-at-len(lead)) + p.text + "\n"
			raw += strings.Repeat("c", size-len(raw))
			calls = append(calls, sandbox.BridgeCall{Name: "fetch", Output: raw, At: runBase})
		}
		for _, call := range calls {
			if _, err := store.AppendBridgeCall(context.Background(), "t-1", sandbox.BridgeAppend{
				Launch: run.LaunchID, Call: call}); err != nil {
				t.Fatalf("AppendBridgeCall: %v", err)
			}
		}
		log, err := store.BridgeLog(context.Background(), run)
		if err != nil {
			t.Fatal(err)
		}
		for i, c := range log.Calls[1:] {
			if cut, _ := sandbox.OutputOpening(c.Output, c.OutputBytes); len(cut) != len(opening) {
				t.Fatalf("%s: call %d's log kept %d bytes, not the %d the cases place its cut by", p.name, i+1,
					len(cut), len(opening))
			}
		}

		shown, ok := askRuns(t, store)[0]["bridge_calls"].([]sandbox.BridgeCall)
		if !ok || len(shown) != len(calls) {
			t.Fatalf("%s: bridge_calls holds %d calls, want %d", p.name, len(shown), len(calls))
		}
		wire, err := json.Marshal(shown)
		if err != nil {
			t.Fatal(err)
		}
		showsNone(t, p.name+" on the read", string(wire), p)
		whole := shown[0]
		if !strings.Contains(whole.Output, "then check the logs") || !strings.Contains(whole.Output, "[REDACTED:") ||
			tools.ReadArgs(whole.Args)["path"] != "a.go" {
			t.Errorf("%s: the whole call reads %q with %s, want the words around a marker and one object", p.name,
				whole.Output, whole.Args)
		}
		for i, c := range shown[1:] {
			where := p.name + " cut " + strconv.Itoa(i+1) + " bytes in"
			back, ok := sandbox.OutputOpening(c.Output, c.OutputBytes)
			if !ok || !strings.HasPrefix(back, lead) {
				t.Errorf("%s: the output reads %.60q… of %d bytes, want the lines before it, read back by its size",
					where, c.Output, c.OutputBytes)
			}
		}
		if after, err := store.BridgeLog(context.Background(), run); err != nil ||
			!strings.Contains(after.Calls[0].Output, p.value) {
			t.Errorf("%s: the log itself was changed: it must stay as the run's harness was handed it", p.name)
		}
	}
}
