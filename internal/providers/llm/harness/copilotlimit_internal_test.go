package harness

import (
	"testing"
	"time"
)

// copilotRateLimited is the session.error Copilot 1.0.114 ended a run on after a
// 429, as the in-box runner collects it off the run's stream
// (internal/sandbox/executor's testdata/copilot/1.0.114/ratelimit.stream.jsonl):
// measured against a stand-in model (BYOK), not GitHub's service.
const copilotRateLimited = `{"type":"session.error","data":{"errorType":"rate_limit","message":"Failed to get ` +
	`response from the AI model; retried 5 times (total retry wait time: 5.48 seconds) Last error: 429 Rate ` +
	`limit reached for requests","statusCode":429,"providerErrorType":"rate_limit_exceeded",` +
	`"providerErrorCode":"rate_limit_exceeded","originatingMessageId":"6383cfe9-d589-4df8-aaf6-924a22bb23f5"},` +
	`"id":"aba5a236-e07b-48c7-8c22-74938d81c7f4","timestamp":"2026-11-21T20:40:54.289Z",` +
	`"parentId":"2493fbe5-0d70-477b-9356-11a3235aeb58"}`

// copilotPastContext is the session.error of a provider's 400 "prompt is too
// long" (refused-note.stream.jsonl there): a failure, and no limit.
const copilotPastContext = `{"type":"session.error","data":{"errorType":"context_limit","message":"Failed to ` +
	`get response from the AI model; retried 5 times (total retry wait time: 5.00 seconds) Last error: 400 ` +
	`prompt is too long","statusCode":400,"providerErrorType":"invalid_request_error",` +
	`"providerErrorCode":"context_length_exceeded","originatingMessageId":"ef710bdb-c166-45c3-b33e-ff9708a88fa0"},` +
	`"id":"acfbbf55-ecc2-4cd9-aea8-e3d5560bb163","timestamp":"2026-11-21T20:46:03.668Z",` +
	`"parentId":"51af637d-5f01-4a88-9777-c94bc7bc2d29"}`

// A COPILOT EXECUTOR RUN THAT MET A RATE LIMIT SITS ITS ENTRY OUT, read off the
// harness's own session.error by its errorType, which no sentinel matched
// ("Rate limit", "rate_limit"). Only in a run that failed, since the line is a
// file in the box; a context failure is no limit.
//
// Mutation: drop the copilot profile's limit_events, or read an executor's
// terminal lines as no record — the first case benches nothing; read the
// record for a run that answered — the last case benches.
func TestACopilotRunThatMetARateLimitSitsItsEntryOut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		line        string
		failed      bool
		wantBenched bool
	}{
		{name: "a rate limit", line: copilotRateLimited, failed: true, wantBenched: true},
		{name: "a conversation past its context", line: copilotPastContext, failed: true},
		{name: "a rate limit's line on a run that answered", line: copilotRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := shipped(t, "copilot", "gpt-5.2")
			p.now = func() time.Time { return reportedAt }
			end := RunEnd{Error: "the run failed", Event: tc.line, Limits: tc.line, Failed: tc.failed}
			if got := p.RunEnded(t.Context(), end); got != tc.wantBenched {
				t.Fatalf("RunEnded = %v, want %v", got, tc.wantBenched)
			}
		})
	}
}
