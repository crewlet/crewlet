package executor_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// countLineBound is the line bound the live count's cases read at: past a
// fixture's ordinary lines and short of the long one each case builds.
const countLineBound = 4 << 10

// countedStream is h's stream of made requests ([requestStreams]) as a running
// harness writes it, each line ended, with its second line grown past
// [countLineBound] in its middle, so its ends are all a reading keeps of it.
func countedStream(t *testing.T, h executor.Harness, made int) string {
	t.Helper()
	lines := strings.Split(requestStreams[h.Name()](made), "\n")
	if len(lines) < 3 {
		t.Fatalf("%s's stream of %d requests has %d lines; a case needs three", h.Name(), made, len(lines))
	}
	long := lines[1]
	lines[1] = long[:len(long)/2] + strings.Repeat("x", 3*countLineBound) + long[len(long)/2:]
	return strings.Join(lines, "\n") + "\n"
}

// A LIVE COUNT IS ITS COLLECTION'S COUNT, HOWEVER THE STREAM ARRIVES: for
// every runner counted off its stream, the stream read as it grows (split
// mid-line, and inside a line past the bound), a few bytes a read, and after a
// rewrite that shrank it, ends on exactly the requests, in-flight
// allowance and undecided lines [executor.Runner.Collect] counts of the
// whole; no read's count is ever below the one before; and a read takes only
// the bytes the stream grew by since the last.
//
// Mutation: read from the stream's end as the display does, drop a read's
// backlog past its bound, keep no partial line between reads, read a line
// past the bound otherwise than Collect, let a shrink lower the count, or
// read the stream from its start each time, and a runner's row fails.
func TestALiveCountIsItsCollectionsCountHoweverTheStreamArrives(t *testing.T) {
	t.Parallel()
	const made = 6
	for _, h := range executor.Registered() {
		if executor.RecordCounted(h) {
			continue // TestALiveRecordCountIsItsCollectionsCountHoweverTheRecordArrives holds it
		}
		t.Run(h.Name(), func(t *testing.T) {
			t.Parallel()
			runner := executor.Bounded(executor.New(h), sandbox.MaxFailureBytes, countLineBound)
			stream := countedStream(t, h, made)
			want, err := executor.CollectedCount(runner, []byte(stream))
			if err != nil {
				t.Fatalf("the collection's count: %v", err)
			}
			if want.Requests < made-1 {
				t.Fatalf("the premise: the collection counts %+v of a stream of %d requests", want, made)
			}
			b := box(t, runner)
			out := h.Output(paths(b))
			read := func(c sandbox.Counter, last sandbox.RequestCount, what string) sandbox.RequestCount {
				t.Helper()
				got, err := c.Read(t.Context(), b)
				if err != nil {
					t.Fatalf("%s: Read: %v", what, err)
				}
				if got.Requests < last.Requests || got.Undecided < last.Undecided {
					t.Fatalf("%s: the count went from %+v down to %+v", what, last, got)
				}
				return got
			}

			t.Run("as it grows", func(t *testing.T) {
				c := executor.CountingBy(runner, executor.CountReadBytes)
				var got sandbox.RequestCount
				for end := 0; end < len(stream); end += 97 {
					b.Put(out.Stdout, stream[:end])
					got = read(c, got, "grown to "+strconv.Itoa(end))
				}
				b.Put(out.Stdout, stream)
				if got = read(c, got, "whole"); got != want {
					t.Fatalf("read as it grew: %+v; the collection counts %+v", got, want)
				}
			})
			t.Run("a few bytes a read", func(t *testing.T) {
				c := executor.CountingBy(runner, 61)
				b.Put(out.Stdout, stream)
				var got sandbox.RequestCount
				for range len(stream)/61 + 1 {
					got = read(c, got, "a read")
				}
				if got != want {
					t.Fatalf("read 61 bytes at a time: %+v; the collection counts %+v", got, want)
				}
			})
			t.Run("past the display's window", func(t *testing.T) {
				// THE REQUESTS ARE ALL BEFORE THE LAST WINDOW a display read
				// begins from, as a long run's early requests are.
				noise := strings.Repeat(`{"type":"crewlet.noise"}`+"\n", executor.LiveWindow/16)
				long := stream + noise
				whole, err := executor.CollectedCount(runner, []byte(long))
				if err != nil || whole != want {
					t.Fatalf("the premise: the collection counts %+v, %v of the stream and its noise; want %+v",
						whole, err, want)
				}
				c := executor.CountingBy(runner, executor.CountReadBytes)
				b.Put(out.Stdout, long)
				if got := read(c, sandbox.RequestCount{}, "whole"); got != want {
					t.Fatalf("read past the display's window: %+v; the collection counts %+v", got, want)
				}
			})
			t.Run("a read takes what grew", func(t *testing.T) {
				// A RUN HOURS LONG costs each tick what it wrote since the
				// last, never its whole stream again.
				c := executor.CountingBy(runner, executor.CountReadBytes)
				noise := strings.Repeat(`{"type":"crewlet.noise"}`+"\n", executor.LiveWindow/16)
				b.Put(out.Stdout, stream+noise)
				got := read(c, sandbox.RequestCount{}, "whole")
				grew := `{"type":"crewlet.noise","grew":true}` + "\n"
				before := b.Delivered()
				b.Put(out.Stdout, stream+noise+grew)
				read(c, got, "grown")
				if took := b.Delivered() - before; took != int64(len(grew)) {
					t.Fatalf("a read after the stream grew %d bytes took %d of its %d", len(grew), took,
						len(stream+noise+grew))
				}
			})
			t.Run("rewritten shorter", func(t *testing.T) {
				c := executor.CountingBy(runner, executor.CountReadBytes)
				b.Put(out.Stdout, stream)
				before := read(c, sandbox.RequestCount{}, "whole")
				b.Put(out.Stdout, stream[:len(stream)/4])
				read(c, before, "shrunk")
				b.Put(out.Stdout, stream)
				if got := read(c, before, "written again"); got != want {
					t.Fatalf("read again after a shrink: %+v; the collection counts %+v", got, want)
				}
			})
		})
	}
}
