package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// OpenCode's `run --format json` prints a step's events only sometimes: on
// 2.0.32 and 2.1.4 a step's step_start and step_finish may each be missing
// (the final step's step_finish was in 7 of 20 runs on 2.0.32 and 8 of 20 on
// 2.1.4, measured), and on 2.2.7 and 2.3.0 all three always print. The
// fixtures in testdata/opencode/run-format-json are whole captures of a run
// with tools (`run --server`) against a stub, one per release and sequence, a
// 16 KiB answer each.

// stepFixture is one captured sequence, and whether it closes its step.
type stepFixture struct {
	path     string
	finishes bool
}

var stepFixtures = []stepFixture{
	{"2.0.32/text.jsonl", false},
	{"2.0.32/step_start-text.jsonl", false},
	{"2.0.32/step_start-text-step_finish.jsonl", true},
	{"2.1.4/text.jsonl", false},
	{"2.1.4/step_start-text.jsonl", false},
	{"2.1.4/step_start-text-step_finish.jsonl", true},
	{"2.2.7/step_start-text-step_finish.jsonl", true},
	{"2.3.0/step_start-text-step_finish.jsonl", true},
}

// sessionTotal is a `session.get` report of the counts given, as OpenCode's
// server answers it.
func sessionTotal(input, output, cacheRead int) string {
	return fmt.Sprintf(`{"data":{"tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":%d,"write":0}}}}`,
		input, output, cacheRead)
}

// fixtureAnswer and fixtureCounts read a capture's text and, where it closes
// its step, its step_finish's counts.
func fixtureAnswer(t *testing.T, raw string) (answer string, input, output, cacheRead int, closes bool) {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		var ev struct {
			Type string `json:"type"`
			Part struct {
				Text   string `json:"text"`
				Tokens struct {
					Input  int `json:"input"`
					Output int `json:"output"`
					Cache  struct {
						Read int `json:"read"`
					} `json:"cache"`
				} `json:"tokens"`
			} `json:"part"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("a fixture line does not decode: %v", err)
		}
		switch ev.Type {
		case "text":
			answer = ev.Part.Text
		case "step_finish":
			input, output, cacheRead, closes = ev.Part.Tokens.Input, ev.Part.Tokens.Output, ev.Part.Tokens.Cache.Read, true
		}
	}
	return answer, input, output, cacheRead, closes
}

// EVERY STEP SEQUENCE OPENCODE PRINTS READS AS ITS ANSWER, on both paths. The
// text function reads the answer and the session's counts; the run with tools
// reads the same answer, stop end, no refusal, and tells the host exactly one
// request carrying it. That request's tokens are its step_finish's where the
// step closed, and the session's total where it did not — the remainder is
// the one request without counts of its own — and the run's spend is the
// session's total in every case.
//
// Mutation: drop the remainder's attribution, and a request whose step_finish
// was dropped carries no tokens; stop flushing at finish the request still
// open, and no request is told.
func TestEveryStepSequenceOpenCodePrintsReadsAsItsAnswer(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("opencode")
	if !ok {
		t.Fatal("no builtin opencode profile")
	}
	for _, f := range stepFixtures {
		t.Run(f.path, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(filepath.Join("testdata", "opencode", "run-format-json", f.path))
			if err != nil {
				t.Fatal(err)
			}
			answer, in, out, cached, closes := fixtureAnswer(t, string(raw))
			if closes != f.finishes || answer == "" || !strings.HasSuffix(string(raw), "\n") {
				t.Fatalf("the fixture is not the sequence it is named for (closes %v, answer %d bytes)", closes, len(answer))
			}
			if !closes {
				in, out, cached = 1007, 30, 200
			}
			report := sessionTotal(in, out, cached)
			total := llm.Usage{Input: p.Usage.PromptTokens(in, cached, 0), Output: out, CacheRead: cached}

			prov := &Provider{profile: p, harness: "opencode", key: "oc", model: "stub/stub-model", now: time.Now}
			comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: string(raw)}, report)
			if err != nil {
				t.Fatalf("the text function: %v", err)
			}
			if comp.Content != strings.TrimSpace(answer) || comp.StopReason != llm.StopEnd ||
				comp.InputTokens != total.Input || comp.OutputTokens != total.Output {
				t.Errorf("the text function = %q (%q, %d in, %d out), want the answer, ended, with the session's "+
					"counts", comp.Content, comp.StopReason, comp.InputTokens, comp.OutputTokens)
			}

			host := newTestHost(t)
			reader := newRecordReader(p.Routed(), llm.Request{Tools: host}, "prompt", time.Now)
			for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
				reader.stdoutLine(line)
			}
			reader.finish()
			// TOLD WHEN THE RECORD ENDS, before anything settles the run:
			// a request still open is the record's to tell at its end.
			if turns, _, _, _ := host.snapshot(); len(lastByID(turns)) != 1 {
				t.Fatalf("the record's end told the host %d requests, want the one it showed", len(lastByID(turns)))
			}
			tprov := &Provider{profile: p, harness: "opencode", key: "oc", model: "stub/stub-model", now: time.Now}
			tcomp, err := tprov.toolCompletion(t.Context(), &rawResult{stdout: string(raw)}, report,
				&toolRun{reader: reader, connected: true}, true)
			if err != nil {
				t.Fatalf("the run with tools: %v", err)
			}
			if tcomp.Content != strings.TrimSpace(answer) || tcomp.StopReason != llm.StopEnd {
				t.Errorf("the run with tools = %q (%q), want the answer, ended", tcomp.Content, tcomp.StopReason)
			}
			turns, _, spent, _ := host.snapshot()
			told := lastByID(turns)
			if len(told) != 1 || told[0].Text != answer {
				t.Fatalf("the host was told %d requests, want one carrying the answer", len(told))
			}
			if u := told[0].Usage; u == nil || *u != total {
				t.Errorf("the request's tokens = %+v, want %+v (its step_finish's, else the session's total)", u, total)
			}
			if spent != total {
				t.Errorf("the run spent %+v, want the session's total %+v", spent, total)
			}
		})
	}
}

// lastByID is the requests a host was told, each as it was told last, in the
// order they were first told.
func lastByID(turns []llm.Turn) []llm.Turn {
	var out []llm.Turn
	at := map[string]int{}
	for _, t := range turns {
		if i, seen := at[t.ID]; seen {
			out[i] = t
			continue
		}
		at[t.ID] = len(out)
		out = append(out, t)
	}
	return out
}

// A REMAINDER IS THE ONE COUNTLESS REQUEST'S ALONE. Where the first step closes
// with its counts and the final step prints none, the final request is told the
// exact remainder of the session's total. Where neither step closes, the
// remainder cannot be split honestly: no request is told any tokens, and the
// run keeps the total.
//
// Mutation: give the remainder to the last request whenever any request lacks
// counts.
func TestARemainderIsTheOneCountlessRequestsAlone(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("opencode")
	if !ok {
		t.Fatal("no builtin opencode profile")
	}
	step := func(kind, message string, part map[string]any) string {
		part["messageID"] = message
		raw, _ := json.Marshal(map[string]any{"type": kind, "part": part})
		return string(raw)
	}
	text := func(message, said string) string {
		return step("text", message, map[string]any{"type": "text", "text": said})
	}
	finish := step("step_finish", "msg_1", map[string]any{"type": "step-finish", "reason": "tool-calls",
		"tokens": map[string]any{"input": 100, "output": 10, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}}})
	report := sessionTotal(300, 40, 0)
	total := llm.Usage{Input: 300, Output: 40}
	for _, c := range []struct {
		name  string
		lines []string
		want  []*llm.Usage
	}{
		{"the first step closes", []string{step("step_start", "msg_1", map[string]any{"type": "step-start"}),
			text("msg_1", "Looking."), finish, text("msg_2", "the answer")},
			[]*llm.Usage{{Input: 100, Output: 10}, {Input: 200, Output: 30}}},
		{"neither step closes", []string{step("step_start", "msg_1", map[string]any{"type": "step-start"}),
			text("msg_1", "Looking."), step("step_start", "msg_2", map[string]any{"type": "step-start"}),
			text("msg_2", "the answer")},
			[]*llm.Usage{nil, nil}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			host := newTestHost(t)
			reader := newRecordReader(p.Routed(), llm.Request{Tools: host}, "prompt", time.Now)
			for _, line := range c.lines {
				reader.stdoutLine(line)
			}
			reader.finish()
			prov := &Provider{profile: p, harness: "opencode", key: "oc", model: "stub/stub-model", now: time.Now}
			if _, err := prov.toolCompletion(t.Context(), &rawResult{stdout: strings.Join(c.lines, "\n") + "\n"},
				report, &toolRun{reader: reader, connected: true}, true); err != nil {
				t.Fatalf("toolCompletion: %v", err)
			}
			turns, _, spent, _ := host.snapshot()
			told := lastByID(turns)
			if len(told) != len(c.want) {
				t.Fatalf("the host was told %d requests, want %d", len(told), len(c.want))
			}
			for i, want := range c.want {
				got := told[i].Usage
				if (got == nil) != (want == nil) || got != nil && *got != *want {
					t.Errorf("request %d's tokens = %+v, want %+v", i+1, got, want)
				}
			}
			if spent != total {
				t.Errorf("the run spent %+v, want the session's total %+v", spent, total)
			}
		})
	}
}
