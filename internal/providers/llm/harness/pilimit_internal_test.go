package harness

import (
	"testing"
	"time"
)

// piRateLimited is the error a Pi 1.6.0 executor run's last request ended on
// after a 429 outlasted its retries, as the in-box runner collects it off the
// run's stream (internal/sandbox/executor's testdata/pi/1.6.0/ratelimit.stream.jsonl):
// the assistant message's `errorMessage`, measured against a stand-in model.
const piRateLimited = `429 {"type":"error","error":{"type":"rate_limit_error","message":"Number of request ` +
	`tokens has exceeded your per-minute rate limit"}}`

// A PI EXECUTOR RUN THAT FAILED ON A RATE LIMIT SITS ITS ENTRY OUT, read off the
// provider's own error the run failed with; the same words in a run that
// answered, which are the model's, bench nothing.
//
// Mutation: drop the pi profile's rate_limit_error sentinel, or match its
// markers against a run's answer: this goes red.
func TestAPiRunThatFailedOnARateLimitSitsItsEntryOut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		end         RunEnd
		wantBenched bool
	}{
		{name: "a rate limit", end: RunEnd{Error: piRateLimited, Failed: true}, wantBenched: true},
		{name: "the words in an answer", end: RunEnd{Answer: "The log said " + piRateLimited}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := shipped(t, "pi", "anthropic/claude-sonnet-4-5")
			p.now = func() time.Time { return reportedAt }
			if got := p.RunEnded(t.Context(), tc.end); got != tc.wantBenched {
				t.Fatalf("RunEnded = %v, want %v", got, tc.wantBenched)
			}
		})
	}
}
