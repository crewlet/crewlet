package executor_test

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

// probeCount runs the count plugin under node with options (JSON) and the
// server's events (JSON), answering what the record it names holds.
func probeCount(t *testing.T, options, events string) string {
	t.Helper()
	node := nodeForNotes(t)
	cmd := exec.CommandContext(t.Context(), node, "opencode_count_probe.mjs", "opencode_count.js", options, events) //nolint:gosec // the probe under test
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the probe: %v\n%s", err, out)
	}
	var got struct {
		File string `json:"file"`
	}
	if err = json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the probe's report: %v\n%s", err, out)
	}
	return got.File
}

// AN OPENCODE COUNT PLUGIN RECORDS EACH ANSWERED REQUEST AND NOTHING ELSE:
// driven under node with the events OpenCode's server hands it, it writes a
// start line as it loads and a line for each `session.usage.updated`, every
// session's, holding the event's kind and session alone, never a word the
// event carries; any other event writes nothing, and a plugin handed no file
// writes nowhere.
//
// Mutation: count another event, copy the event's data into the line, write
// no start line, or write where no file is named, and the record differs.
func TestAnOpenCodeCountPluginRecordsEachAnsweredRequestAlone(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "count.jsonl")
	options, _ := json.Marshal(map[string]string{"file": file})
	events := `[{"type":"session.step.started","data":{"sessionID":"ses_a"}},` +
		`{"type":"session.usage.updated","data":{"sessionID":"ses_a","tokens":{"input":1}}},` +
		`{"type":"session.retry.scheduled","data":{"sessionID":"ses_a"}},` +
		`{"type":"session.usage.updated","data":{"sessionID":"ses_b","text":"a word of the session's"}}]`
	want := `{"type":"start"}` + "\n" + `{"type":"request","session":"ses_a"}` + "\n" +
		`{"type":"request","session":"ses_b"}` + "\n"
	if got := probeCount(t, string(options), events); got != want {
		t.Errorf("the record holds %q, want %q", got, want)
	}
	if got := probeCount(t, `{}`, events); got != "" {
		t.Errorf("a plugin handed no file recorded %q", got)
	}
}
