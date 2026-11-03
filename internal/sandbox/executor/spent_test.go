package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A LIVE READING SAYS WHAT THE RUN HAS SPENT SO FAR, so a caller can stop a
// run at its token cap while it works rather than learn the total once it
// ended. Each model call is counted once, by its message id, however many
// lines the stream repeats its usage on, and every token of the call (the
// uncached prompt, the cache's reads and writes, and the reply) is counted.
func TestALiveReadingCountsEachModelCallOnce(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	usage := func(id string, in, out, read, write int, text string) string {
		return claudeLine(map[string]any{"type": "assistant", "message": map[string]any{
			"id": id, "content": []map[string]any{{"type": "text", "text": text}},
			"usage": map[string]any{"input_tokens": in, "output_tokens": out,
				"cache_read_input_tokens": read, "cache_creation_input_tokens": write},
		}})
	}
	stream := strings.Join([]string{
		usage("msg_1", 10, 5, 1000, 200, "reading the brand draft"),
		usage("msg_1", 10, 5, 1000, 200, "and its colours"),
		usage("msg_2", 20, 40, 1200, 0, "drawing the logo"),
	}, "\n") + "\n"
	b.Put(executor.ClaudeCode{}.Output(paths(b)).Stdout, stream)
	got := read(t, runner.Follow(sandbox.RunHandle{}), b)
	if want := (10 + 5 + 1000 + 200) + (20 + 40 + 1200); got.Spent != want {
		t.Fatalf("Spent = %d, want %d", got.Spent, want)
	}
}
