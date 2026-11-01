package harness

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A USAGE REPORT THAT ARRIVED AND COULD NOT BE READ IS SAID OUT LOUD.
//
// A report the harness produced and this profile cannot read — one that does not
// decode, or in which the input and output counts no longer resolve, which is
// what a release renaming `data.tokens` leaves — falls back to what stdout
// reported, an estimate for a harness that reports none there. It used to fall back
// SILENTLY, so every call was charged a guess and there was no event to search
// for. A command that succeeded and printed nothing is the same failure from
// the other side: it reported nothing, and the empty report handed on looked
// like no report at all. A report that was never asked for, or arrived whole,
// says nothing.
//
// NOT PARALLEL: it reads the process's log, which every parallel case writes
// to, and a sequential test runs with none of them.
//
// Mutation: drop the warning from completion(), or treat an empty report as
// none at the call site, and a case here is red.
func TestAnUnreadableUsageReportIsSaidOutLoud(t *testing.T) {
	logs := captureLogs(t)
	const report = "usage-report"
	usageCommand := map[string]any{
		"session_args": []any{"--session", "{session}"},
		"usage_args":   []any{"-test.run=TestFakeHarness", report, "{session}"},
		"usage": map[string]any{
			"input":  []any{[]any{"tokens", "input"}},
			"output": []any{[]any{"tokens", "output"}},
		},
	}
	usageFile := map[string]any{
		"usage_file_args": []any{"--usage-file", "{usage_file}"},
		"usage": map[string]any{
			"input":  []any{[]any{"tokens", "input"}},
			"output": []any{[]any{"tokens", "output"}},
		},
	}
	for _, tc := range []struct {
		name      string
		overrides map[string]any
		env       map[string]string
		event     string // "" for no warning at all
		reason    string
		reported  bool
	}{
		{
			name: "a command that printed nothing", overrides: usageCommand,
			env:   map[string]string{"FAKE_USAGE_COMMAND": report, "FAKE_USAGE_REPORT": ""},
			event: "harness_usage_report_failed", reason: "exited 0 and printed nothing",
		},
		{
			name: "a command's report that does not decode", overrides: usageCommand,
			env:   map[string]string{"FAKE_USAGE_COMMAND": report, "FAKE_USAGE_REPORT": "tokens: 12 in, 3 out"},
			event: "harness_usage_report_unreadable", reason: "not one JSON object",
		},
		{
			name: "a command's report whose counts moved", overrides: usageCommand,
			env:   map[string]string{"FAKE_USAGE_COMMAND": report, "FAKE_USAGE_REPORT": `{"usage":{"input":12,"output":3}}`},
			event: "harness_usage_report_unreadable", reason: "do not both resolve",
		},
		{
			name: "a command's report read whole", overrides: usageCommand,
			env:      map[string]string{"FAKE_USAGE_COMMAND": report, "FAKE_USAGE_REPORT": `{"tokens":{"input":12,"output":3}}`},
			reported: true,
		},
		{
			name: "a usage file written empty", overrides: usageFile,
			env:   map[string]string{"FAKE_USAGE_FILE": " \n"},
			event: "harness_usage_file_unreadable", reason: "the file is empty",
		},
		{
			name: "a usage file that does not decode", overrides: usageFile,
			env:   map[string]string{"FAKE_USAGE_FILE": "garbage"},
			event: "harness_usage_report_unreadable", reason: "not one JSON object",
		},
		{
			// The vendor writes it "even when the run fails", so its
			// absence says the run did not get that far — nothing to warn of.
			name: "no usage file at all", overrides: usageFile,
			env: map[string]string{"FAKE_USAGE_FILE": "-"},
		},
	} {
		logs.Reset()
		env := map[string]string{"FAKE_STDOUT": "the answer"}
		for k, v := range tc.env {
			env[k] = v
		}
		p := fakeProvider(t, env, tc.overrides)
		comp, err := ask(t, p, llm.Request{})
		if err != nil {
			t.Fatalf("%s: Complete: %v — a usage report is never a failed call", tc.name, err)
		}
		got := logs.String()
		for _, event := range []string{
			"harness_usage_report_failed", "harness_usage_report_unreadable", "harness_usage_file_unreadable",
		} {
			if logged := strings.Contains(got, event); logged != (event == tc.event) {
				t.Errorf("%s: %s logged = %v, want %v:\n%s", tc.name, event, logged, event == tc.event, got)
			}
		}
		if tc.reason != "" && !strings.Contains(got, tc.reason) {
			t.Errorf("%s: the warning does not say why (%q):\n%s", tc.name, tc.reason, got)
		}
		if tc.event == "harness_usage_report_unreadable" && !strings.Contains(got, "an estimate") {
			t.Errorf("%s: the warning does not say what the counts fell back to:\n%s", tc.name, got)
		}
		if tc.reported {
			if comp.InputTokens != 12 || comp.OutputTokens != 3 {
				t.Errorf("%s: tokens = %d/%d, want the report's 12/3", tc.name, comp.InputTokens, comp.OutputTokens)
			}
		} else if comp.OutputTokens != EstimateTokens("the answer") {
			t.Errorf("%s: output tokens = %d, want the estimate for the answer", tc.name, comp.OutputTokens)
		}
	}
}

// A FAILED RUN'S REPORT IS NOT DRIFT. A call the harness said failed is refused and
// charged nothing, so a report holding no counts is no fallback to warn of —
// and Hermes writes its counts as null for exactly the calls an operator reads
// the log about, a refused key and a spent plan (measured on 0.21.5). The same
// report on a run that exited 0 still warns, which is what keeps this from
// being a rule about nulls.
//
// NOT PARALLEL: it reads the process's log, as the case above does.
//
// Mutation: drop the failed-run condition from completion()'s warning and the
// first case is red.
func TestAFailedRunsEmptyUsageReportIsNotCalledDrift(t *testing.T) {
	logs := captureLogs(t)
	const nulls = `{"input_tokens":null,"output_tokens":null,"completed":false,"failed":true}`
	for _, tc := range []struct {
		name, exit string
		failed     bool
	}{
		{name: "a run that exited 2", exit: "2", failed: true},
		{name: "a run that exited 0", exit: "0", failed: false},
	} {
		logs.Reset()
		p := fakeProvider(t, map[string]string{
			"FAKE_STDOUT": "the answer", "FAKE_USAGE_FILE": nulls, "FAKE_EXIT": tc.exit,
		}, usageProfile())
		_, err := ask(t, p, llm.Request{})
		if (err != nil) != tc.failed {
			t.Fatalf("%s: err = %v, want a failed call = %v", tc.name, err, tc.failed)
		}
		warned := strings.Contains(logs.String(), "harness_usage_report_unreadable")
		if warned == tc.failed {
			t.Errorf("%s: harness_usage_report_unreadable logged = %v, want %v:\n%s",
				tc.name, warned, !tc.failed, logs.String())
		}
	}
}

// WHAT A REPORT WAS MADE OF IS THREE WAYS OF TAKING NOTHING, and only the two
// that mean the harness's report arrived count as unreadable: nothing obtained is
// already reported where it was obtained, or is a profile that reads none.
func TestApplyUsageReportSaysWhyItTookNothing(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin("opencode")
	for raw, want := range map[string]usageOutcome{
		"":     usageNoReport,
		"  \n": usageNoReport,
		"[]":   usageUndecodable,
		"data": usageUndecodable,
		`{"data":{"usage":{"input":1,"output":2}}}`:  usageUnresolved,
		`{"data":{"tokens":{"input":1}}}`:            usageUnresolved,
		`{"data":{"tokens":{"input":1,"output":2}}}`: usageApplied,
	} {
		var out extracted
		if got := out.applyUsageReport(p, raw); got != want {
			t.Errorf("report %q: %v, want %v", raw, got, want)
		}
		if out.reported != (want == usageApplied) {
			t.Errorf("report %q: reported = %v", raw, out.reported)
		}
	}
	var out extracted
	if got := out.applyUsageReport(harnessprofile.Profile{}, `{"x":1}`); got != usageNoReport {
		t.Errorf("a profile that reads no report: %v, want %v", got, usageNoReport)
	}
}

// captureLogs points the process's log at a buffer for one sequential test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatText, buf)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	return buf
}

// syncBuffer is a log sink the logger may write to from any goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

var _ io.Writer = (*syncBuffer)(nil)

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
