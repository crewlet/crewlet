package executor_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A RUN WHOSE HARNESS THE KERNEL KILLED FOR MEMORY ENDED AT ITS CEILING; ONE
// THAT SURVIVED A KILLED COMMAND DID NOT, AND STILL SAYS THE KILL. The kills
// are the count the kernel kept in the job's memory domain between the
// script's two records of it, never a guess from the status alone: a SIGKILL
// with no kill counted is no memory ceiling.
//
// Mutation: class a run by its status alone, ignore the count, or drop the
// kills of a run that survived them, and a row goes red.
func TestAHarnessKilledForMemoryEndedAtItsCeiling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		code          string
		before, after string
		kills         int64
		ceiling       int64
		outOfMemory   bool
	}{
		{name: "the harness killed for memory", code: "137",
			before: "oom_kill=2\nlimit=2147483648\ntotal_kb=8388608\n",
			after:  "oom_kill=3\nlimit=2147483648\ntotal_kb=8388608\n",
			kills:  1, ceiling: 2 << 30, outOfMemory: true},
		{name: "a command killed, the harness failing on", code: "1",
			before: "oom_kill=0\nlimit=max\ntotal_kb=4194304\n",
			after:  "oom_kill=2\nlimit=max\ntotal_kb=4194304\n",
			kills:  2, ceiling: 4 << 30},
		{name: "a SIGKILL with no kill counted", code: "137",
			before: "oom_kill=5\nlimit=2147483648\n", after: "oom_kill=5\nlimit=2147483648\n"},
		{name: "a domain that keeps no count", code: "137",
			before: "total_kb=4194304\n", after: "total_kb=4194304\n"},
		{name: "a count that went back", code: "137",
			before: "oom_kill=9\n", after: "oom_kill=1\n"},
		{name: "a job whose script recorded nothing", code: "137"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := executor.NewClaudeCode()
			b := box(t, runner)
			p := paths(b)
			b.Put(p.ExitCode(), tc.code)
			if tc.before != "" {
				b.Put(p.MemoryBefore(), tc.before)
				b.Put(p.MemoryAfter(), tc.after)
			}
			res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if res.MemoryKills != tc.kills || res.MemoryBytes != tc.ceiling || res.OutOfMemory != tc.outOfMemory {
				t.Fatalf("collected kills %d, ceiling %d, out of memory %v; want %d, %d, %v",
					res.MemoryKills, res.MemoryBytes, res.OutOfMemory, tc.kills, tc.ceiling, tc.outOfMemory)
			}
		})
	}
}

// THE SCRIPT'S MEMORY READ RUNS UNDER A PLAIN SH and prints only what the
// collection parses: whatever the machine, it never fails the job, and where it
// reads a domain it says something of it.
//
// Mutation: break the function's syntax, or print a line the parser does not
// read, and this goes red.
func TestTheMemoryReadRunsUnderAPlainShell(t *testing.T) {
	t.Parallel()
	out, err := exec.Command("sh", "-c", sandbox.MemoryDomainScript+"\ncrewlet_memory").CombinedOutput()
	if err != nil {
		t.Fatalf("the memory read failed under sh: %v\n%s", err, out)
	}
	for line := range strings.Lines(string(out)) {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || !strings.Contains(" oom_kill limit total_kb total ", " "+key+" ") {
			t.Errorf("the memory read printed %q, which nothing parses", line)
		}
	}
	if m := sandbox.ParseMemoryDomain(string(out)); !m.Known() {
		t.Errorf("the memory read on this machine said nothing of it: %q", out)
	}
}

// THE WRAPPER READS THE JOB'S MEMORY EITHER SIDE OF ITS HARNESS: before the
// harness starts and after its status is read, so a kill counted between them
// is the job's and the read cannot replace the harness's status.
//
// Mutation: read the memory once, or after the status is recorded, and this
// goes red.
func TestTheWrapperReadsTheJobsMemoryEitherSideOfItsHarness(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	start(t, runner, b)
	script := lastBackground(t, b)
	before := strings.Index(script, p.MemoryBefore())
	exited := strings.LastIndex(script, "code=$?")
	after := strings.Index(script, p.MemoryAfter())
	recorded := strings.Index(script, "echo $code > ")
	if !strings.Contains(script, "crewlet_memory()") || before < 0 || exited < before || after < exited ||
		recorded < after {
		t.Fatalf("want the memory read, the harness's status, the memory read again, then the status recorded:\n%s",
			script)
	}
}
