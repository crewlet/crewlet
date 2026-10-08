package codingagent_test

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// THE STREAM IS SAID TO STAND IN FOR THE RESULT FILE ONLY WHERE IT DID. The
// wrapper writes the result file whatever the CLI did, so a CLI that exited at
// once — not installed, a bad flag — leaves it empty beside an empty stream,
// and a warning that "the result file was never written" stood beside every
// such run's real failure, false twice over. The line is logged where the
// stream's last line was read in the file's place, and says what the file was.
//
// NOT PARALLEL: it reads the process's log, which every parallel case writes
// to, and a sequential test runs with none of them.
//
// Mutation: log whenever the file reads empty, and the first case is red.
func TestTheStreamIsSaidToStandInForTheResultFileOnlyWhereItDid(t *testing.T) {
	logs := &lockedBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatText, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	runner := codingagent.NewClaudeCode()

	exited := box(t, runner)
	p := paths(exited)
	exited.Put(p.Stream(), "")
	exited.Put(p.Result(), "")
	exited.Put(p.ExitCode(), "127")
	exited.Put(p.Err(), "sh: 1: claude: not found")
	if _, err := runner.Collect(t.Context(), exited, sandbox.RunHandle{}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := logs.String(); strings.Contains(got, "coding_agent_result_from_stream") {
		t.Errorf("a run whose stream said nothing logged a stand-in:\n%s", got)
	}

	died := box(t, runner)
	p = paths(died)
	died.Put(p.Stream(), strings.Join(claudeRunStream[:len(claudeRunStream)-1], "\n")+"\n")
	died.Put(p.Result(), "")
	if _, err := runner.Collect(t.Context(), died, sandbox.RunHandle{}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := logs.String(); !strings.Contains(got, "coding_agent_result_from_stream") ||
		!strings.Contains(got, "the result file was empty") {
		t.Errorf("a run read from its stream did not say so, or not why:\n%s", got)
	}
}

// lockedBuffer is a log sink the logger may write to from any goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
