//go:build unix

package codingagent_test

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// A PIECE THAT IS NOT A REGULAR FILE DEGRADES ONLY ITSELF, as a piece past the
// size cap does. What a path names is the box's own doing — a coding agent can
// make a pipe where its report goes — so a collection that answered it with an
// error was retried until the run was settled as lost: its spend uncharged and
// its record unpublished, every attempt refused again. The piece is described
// for what it is, the run reads as not succeeded with that as the reason, and
// what the rest of the box holds — its spend among it — is still collected: a
// refused result file is read from the stream's last line, which is what the
// wrapper copies into it.
//
// On a real local box, because it is the engine host's own open that refuses
// a named pipe, and every piece a collection reads is covered: the whole
// files, the event stream and the error stream.
//
// Mutation: answer the refusal as an error in any one reader, and its case
// fails the collection; skip the stream's line for a refused result file, and
// the result case loses the spend.
func TestAPieceThatIsNotARegularFileDegradesOnlyItself(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewClaudeCode()
	resultLine := claudeRunStream[len(claudeRunStream)-1]
	for _, tc := range []struct {
		name string
		at   func(codingagent.Paths) string
		what string
	}{
		{"report", codingagent.Paths.Findings, "the report the coding agent wrote"},
		{"error stream", codingagent.Paths.Err, "the error stream"},
		{"exit status", codingagent.Paths.ExitCode, "its exit status"},
		{"question", codingagent.Paths.Ask, "the question the coding agent recorded"},
		// The spend is in both of the next two, so each case finds it in
		// the other: the stream's last line is what the result file holds.
		{"event stream", codingagent.Paths.Stream, "the event stream"},
		{"result", codingagent.Paths.Result, "the result its CLI printed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
			if err != nil {
				t.Fatalf("NewLocal: %v", err)
			}
			box, err := local.Create(t.Context(), sandbox.Spec{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			t.Cleanup(func() { _ = box.Close(t.Context()) })
			p := codingagent.PathsFor(box)
			for path, content := range map[string]string{
				p.Stream():   strings.Join(claudeRunStream, "\n") + "\n",
				p.Result():   resultLine + "\n",
				p.Findings(): "Fixed the race.",
				p.Err():      "a warning",
				p.ExitCode(): "0",
			} {
				if err := box.WriteFile(t.Context(), path, []byte(content)); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
			}
			// The direct box's paths are the host's own.
			target := tc.at(p)
			_ = os.Remove(target)
			if err := syscall.Mkfifo(target, 0o600); err != nil {
				t.Fatalf("mkfifo %s: %v", target, err)
			}

			type collected struct {
				res sandbox.Result
				err error
			}
			answered := make(chan collected, 1)
			go func() {
				res, err := runner.Collect(t.Context(), box, sandbox.RunHandle{})
				answered <- collected{res, err}
			}()
			var got collected
			select {
			case got = <-answered:
			case <-time.After(10 * time.Second):
				t.Fatal("the collection has not answered in 10 s")
			}
			if got.err != nil {
				t.Fatalf("Collect: %v; want the refusal kept inside its piece", got.err)
			}
			res := got.res
			if res.Success {
				t.Error("a run one of whose pieces could not be read reads as a success")
			}
			if want := tc.what + " (" + target + ") is a named pipe, not a regular file"; !strings.Contains(res.Error, want) {
				t.Errorf("the failure = %q; want it to say %q", res.Error, want)
			}
			// THE SPEND IS STILL CHARGED: it is what a collection that
			// answered an error lost along with the record.
			if res.InputTokens != 30+2400+51200 || res.CostUSD != 0.1834 {
				t.Errorf("input %d, cost %v; want the run's spend still collected", res.InputTokens, res.CostUSD)
			}
		})
	}
}
