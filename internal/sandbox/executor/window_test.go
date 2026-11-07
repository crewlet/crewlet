package executor_test

import (
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A CLAUDE CODE RUN IS PINNED TO THE WINDOW OF THE MODEL IT WORKS UNDER, and one
// on a model this build does not know to the fallback, which its launch logs.
// A fixed window below a model's own left a fresh session too little room above
// its first request, so compaction refilled at once and the harness stopped it.
//
// NOT PARALLEL: it reads the process's log, which every parallel case writes to.
//
// Mutation: pin a fixed window, or drop the log, and a row goes red.
func TestAClaudeRunIsPinnedToTheWindowOfItsModel(t *testing.T) {
	logs := &lockedBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatText, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	runner := executor.NewClaudeCode()
	for _, c := range []struct {
		model   string
		want    int
		unknown bool
	}{
		{model: "claude-opus-5-5", want: 1_000_000},
		{model: "sonnet", want: 1_000_000},
		{model: "us.anthropic.claude-haiku-4-5-20251001-v1:0", want: 200_000},
		{model: "claude-sonnet-4-5[1m]", want: 1_000_000},
		{model: "claude-next-preview", want: sandbox.FallbackExecutorWindow, unknown: true},
	} {
		from := len(logs.String())
		script := startAndScript(t, runner, box(t, runner), sandbox.RunRequest{Brief: "x", Model: c.model})
		if pins := windowPin.FindAllStringSubmatch(script, -1); len(pins) != 1 || pins[0][1] != strconv.Itoa(c.want) {
			t.Errorf("a run on %s pins %v, want the one window %d:\n%s", c.model, pins, c.want, script)
		}
		logged := strings.Contains(logs.String()[from:], "executor_context_window_unknown")
		if logged != c.unknown {
			t.Errorf("a run on %s logged an unknown window: %v, want %v", c.model, logged, c.unknown)
		}
	}
}

// windowPin is the window pin in a run's script, read through the script's quoting.
var windowPin = regexp.MustCompile(`export CLAUDE_CODE_AUTO_COMPACT_WINDOW=[\\']*(\d+)`)
