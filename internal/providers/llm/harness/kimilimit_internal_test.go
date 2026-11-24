package harness

import (
	"testing"
	"time"
)

// kimiRateLimited is the turn end Kimi Code 2.7.3 recorded in its session's
// wire.jsonl after a 429, as the in-box runner's script adds it to the run's
// stream (internal/sandbox/executor's testdata/kimi/2.7.3/ratelimit.stream.jsonl):
// measured against a stand-in model, not Moonshot's service.
const kimiRateLimited = `{"type":"turn.ended","agentId":"main","turnId":0,"reason":"failed","error":{"code":` +
	`"provider.rate_limit","message":"You have reached your 5-hour usage limit. Resets in 2h.","name":` +
	`"APIProviderRateLimitError","details":{"requestId":null,"statusCode":429},"retryable":true},` +
	`"durationMs":308,"modelSteps":[{"model":"m","provider":"openai","isKfc":false,"step":1,"role":"step"}],"time":1795511715510}`

// kimiPastContext is the turn end of a provider's 400 "exceeded model token
// limit" (overflow.stream.jsonl there): a failure, and no limit.
const kimiPastContext = `{"type":"turn.ended","agentId":"main","turnId":0,"reason":"failed","error":{"code":` +
	`"compaction.failed","message":"APIContextOverflowError: Invalid request: Your request exceeded model token ` +
	`limit: 262144 (requested: 300000)","name":"Error2","retryable":false,"cause":{"code":` +
	`"context.overflow","message":"Invalid request: Your request exceeded model token limit: 262144 (requested: ` +
	`300000)","name":"APIContextOverflowError","details":{"statusCode":400,"requestId":null,"traceId":null,` +
	`"diagnostics":{}},"retryable":false}},"durationMs":331,"modelSteps":[{"model":"m","provider":"openai","isKfc":false,"step":1,"role":"step"}],"time":1795511732807}`

// A KIMI CODE EXECUTOR RUN THAT MET A RATE LIMIT SITS ITS ENTRY OUT, read off
// the record's own turn end by the kimi-code profile's limit event (`in:
// record`), the line the text phases read off the same file. Only in a run that
// failed; a context failure is no limit.
//
// Mutation: drop the kimi-code profile's limit_events, or read an executor's
// terminal lines as no record — the first case benches nothing; have
// classifyEvents read the record for a run that answered — the last benches.
func TestAKimiRunThatMetARateLimitSitsItsEntryOut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		line        string
		failed      bool
		wantBenched bool
	}{
		{name: "a rate limit", line: kimiRateLimited, failed: true, wantBenched: true},
		{name: "a conversation past its context", line: kimiPastContext, failed: true},
		{name: "a rate limit's line on a run that answered", line: kimiRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := shipped(t, "kimi-code", "kimi-for-coding")
			p.now = func() time.Time { return reportedAt }
			end := RunEnd{Error: "the run failed", Event: tc.line, Limits: tc.line, Failed: tc.failed}
			if got := p.RunEnded(t.Context(), end); got != tc.wantBenched {
				t.Fatalf("RunEnded = %v, want %v", got, tc.wantBenched)
			}
		})
	}
}
