package harness

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// readKimiWire is the shipped kimi-code profile's record reader fed one of the
// wire logs Kimi Code 2.7.3 wrote (testdata/kimi-code/wire/2.7.3), a line at a
// time, then finished, and the host it told.
func readKimiWire(t *testing.T, name string) (*recordReader, *testHost) {
	t.Helper()
	host := newTestHost(t)
	r := newRecordReader(kimiCode(t), llm.Request{Tools: host}, "go", time.Now)
	raw, err := os.ReadFile(filepath.Join("testdata", "kimi-code", "wire", "2.7.3", name))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		r.fileLine(line)
	}
	r.finish()
	return r, host
}

// THE KIMI CODE PROFILE READS THE WIRE LOG 2.7.3 WRITES, AND NAMES NO MODEL IT
// WAS NOT TOLD. Real 2.7.3 records (the vendor's prompt and tool descriptions
// elided): a step that made five calls, two with arguments that are not an
// object and one without the server's prefix, and an answer after a `think`
// part. Each step is one request with its prose, calls, stop and counts; the
// tools offered are the snapshot's, not the whole catalogue; and the model is
// unnamed, as a step's lines name only an alias (`__kimi_env_model__` here)
// and the model's own id comes only with the turn's end (the profile's note).
//
// Mutation: read a model off usage.record or profile.bind, and every request
// names the placeholder; drop the text-type match on content.part, and the
// thought joins the answer; read the tools off tool_select.initialized, and
// the tools the agent file withheld are offered.
func TestKimiCodesProfileReadsTheSupportedReleasesWireLog(t *testing.T) {
	t.Parallel()
	r, host := readKimiWire(t, "tools.wire.jsonl")
	if got, want := r.offeredTools(), []string{"FetchURL", "mcp__crewlet__search", "mcp__crewlet__slow"}; !slices.Equal(got, want) {
		t.Errorf("offered = %q, want %q", got, want)
	}
	turns := host.latestTurns()
	if len(turns) != 2 {
		t.Fatalf("read %d requests, want the run's 2: %+v", len(turns), turns)
	}
	want := llm.Usage{Input: 101, Output: 12, CacheRead: 40}
	for i, turn := range turns {
		if turn.Model != "" {
			t.Errorf("request %d names model %q, which the record never stated", i+1, turn.Model)
		}
		if turn.Usage == nil || *turn.Usage != want {
			t.Errorf("request %d usage = %+v, want %+v", i+1, turn.Usage, want)
		}
	}
	first, second := turns[0], turns[1]
	if first.Text != "checking" || first.Stop != llm.StopToolUse {
		t.Errorf("first request: text %q, stop %q; want the prose before its calls, stopped for them",
			first.Text, first.Stop)
	}
	if second.Text != "pong" || second.Stop != llm.StopEnd || len(second.Calls) != 0 {
		t.Errorf("second request = %+v, want the answer alone", second)
	}
	type call struct {
		id, name  string
		host, bad bool
	}
	var got []call
	for _, c := range first.Calls {
		got = append(got, call{c.ID, c.Name, c.Host, c.ArgumentsError != ""})
	}
	wantCalls := []call{
		{"call_2", "search", true, true},   // "{bad"
		{"call_4", "search", false, false}, // no mcp__crewlet__ prefix
		{"call_3", "search", true, true},   // [1]
		{"call_0", "search", true, false},
		{"call_1", "slow", true, false},
	}
	if !slices.Equal(got, wantCalls) {
		t.Errorf("first request's calls = %+v, want %+v", got, wantCalls)
	}
	for _, c := range first.Calls {
		if c.ID == "call_0" && !maps.Equal(c.Arguments, map[string]any{"q": "a"}) {
			t.Errorf("call_0's arguments = %v, want q=a", c.Arguments)
		}
	}

	_, host = readKimiWire(t, "think.wire.jsonl")
	turns = host.latestTurns()
	if len(turns) != 1 || turns[0].Text != "pong" || turns[0].Model != "" {
		t.Errorf("a run that thought first: %+v, want one request answering pong, its thought and "+
			"model unread", turns)
	}
}
