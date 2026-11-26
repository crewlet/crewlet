package executor_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// grokCompaction is a Grok Build compaction_requests file of attempts, its
// error refused where refused is set, a key of its chat history naming an
// attempt too, as a model's call may.
func grokCompaction(attempts int, refused bool) string {
	details := make([]string, attempts)
	for i := range details {
		details[i] = fmt.Sprintf(`{"attempt":%d,"outcome":"degenerate"}`, i+1)
	}
	errorField := ""
	if refused {
		errorField = `"error":"compact failed: degenerate summary",`
	}
	return `{"schema_version":2,"chat_history":[{"type":"assistant","tool_calls":[{"arguments":{"attempt":1}}]}],` +
		errorField + fmt.Sprintf(`"attempts":%d,"attempt_details":[%s]}`, attempts, strings.Join(details, ","))
}

// grokSideBox is a box of a Grok Build run whose stream shows streamed
// requests and whose session directory holds, past an earlier run's
// compaction file, two of this run's (3 attempts refused, 2 kept), two
// checkpoints and, where titled, a title it gained.
func grokSideBox(t *testing.T, runner *executor.Runner, streamed int, startTitled, titled bool) *sandbox.FakeSandbox {
	t.Helper()
	b := box(t, runner)
	p := executor.PathsFor(b)
	ids := []string{"00000000-0000-4000-8000-00000000000a", "00000000-0000-4000-8000-00000000000b",
		"00000000-0000-4000-8000-00000000000c", "00000000-0000-4000-8000-00000000000d",
		"00000000-0000-4000-8000-00000000000e"}
	start := "session " + fixtureSession + "\ncompaction_requests " + ids[0] + ".json\n"
	if startTitled {
		start += "titled\n"
	}
	b.Put(p.GrokRunStart(), start)
	dir := p.GrokSessionsKey() + "/" + fixtureSession
	b.Put(dir+"/compaction_requests/"+ids[0]+".json", grokCompaction(3, true))
	b.Put(dir+"/compaction_requests/"+ids[1]+".json", grokCompaction(3, true))
	b.Put(dir+"/compaction_requests/"+ids[2]+".json", grokCompaction(2, false))
	if titled {
		b.Put(dir+"/summary.json", `{"info":{"id":"x"},"generated_title": "Cap run"}`)
	}
	listing := "compaction_requests " + ids[0] + ".json\ncompaction_requests " + ids[1] + ".json\n" +
		"compaction_requests " + ids[2] + ".json\ncompaction_requests notes.json\n" +
		"compaction_checkpoints " + ids[3] + ".json\ncompaction_checkpoints " + ids[4] + ".json\n"
	b.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if strings.Contains(cmd, "compaction_checkpoints") {
			return sandbox.ExecResult{Stdout: listing}, nil
		}
		return sandbox.ExecResult{}, nil
	}
	out := executor.Grok{}.Output(p)
	stream := requestStreams[executor.GrokName](streamed)
	b.Put(out.Stdout, stream)
	if out.Result != "" && out.Result != out.Stdout {
		b.Put(out.Result, stream[strings.LastIndex(stream, "\n")+1:])
	}
	b.Put(p.Findings(), "Outcome: succeeded")
	b.Put(p.ExitCode(), "0")
	return b
}

// A GROK BUILD RUN IS COUNTED BY ITS STREAM AND WHAT ITS SESSION KEEPS BESIDE
// IT, live and at its collection alike: each attempt a refused compaction's
// file lists, all but the last of a kept one's, which its checkpoint counts,
// each checkpoint, and the title the session gained, added to the stream's
// usage lines, which show none of them; never an earlier run's file, a title
// the session already had, or a key of a file's chat history. Each is a
// request the run made, so the sum never counts more than it did.
//
// Mutation: take the larger instead of the sum, count a kept compaction's last
// attempt, an earlier run's file, a title held at the start, or every
// "attempt" key, and a row goes red.
func TestAGrokRunIsCountedByItsStreamAndItsSessionsSideRecords(t *testing.T) {
	t.Parallel()
	runner := executor.New(executor.Grok{})
	for _, c := range []struct {
		name                string
		startTitled, titled bool
		want                int
	}{
		{"a title gained", false, true, 4 + 3 + 1 + 2 + 1},
		{"a title held at the start", true, true, 4 + 3 + 1 + 2},
		{"no title yet", false, false, 4 + 3 + 1 + 2},
	} {
		b := grokSideBox(t, runner, 4, c.startTitled, c.titled)
		if got, err := runner.Count(sandbox.RunHandle{}).Read(t.Context(), b); err != nil || got.Requests != c.want {
			t.Errorf("%s: the live count reads %+v (%v), want %d", c.name, got, err, c.want)
		}
		for turnCap, past := range map[int]bool{c.want: false, c.want - 1: true} {
			res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{TurnCap: turnCap}, queue.NewAnswerBudget(nil))
			if err != nil || res.TurnCapExceeded != past {
				t.Errorf("%s: collected under a cap of %d as exceeded %v (%v), want %v", c.name, turnCap,
					res.TurnCapExceeded, err, past)
			}
		}
	}
	b := grokSideBox(t, runner, 4, false, true)
	b.Put(executor.PathsFor(b).GrokRunStart(), "")
	if got, err := runner.Count(sandbox.RunHandle{}).Read(t.Context(), b); err != nil || got.Requests != 4 {
		t.Errorf("a run that recorded no start reads %+v (%v), want its stream's 4 alone", got, err)
	}
}
