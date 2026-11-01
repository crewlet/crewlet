package builtin

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// A MERGE REFUSAL CARRIES THE CODE HOST'S OWN WORD beneath its class, as a
// value. Several of the host's refusals share one class — a head that moved
// and a train whose base moved are both stale_version — and a person's
// surface answers each in its own words by reading the word (the act route's
// `refusal`, operator.Fail), never by parsing the sentence written for a
// model; a gate refusal, answered not_ready, carries it as much as any.
//
// Mutation: have writeRefusalError return a plain error again, and this
// fails.
func TestAMergeRefusalCarriesTheHostsOwnWord(t *testing.T) {
	t.Parallel()
	for _, refusal := range []code.MergeRefusal{code.MergeRefusalHeadMoved, code.MergeRefusalTargetMoving,
		code.MergeRefusalGateUnmet} {
		res := mergeRefused(CodeMergeChangeTool, "web!42", "main", refusal, "the host's sentence", nil)
		var host *HostRefusal
		if !errors.As(res.Cause, &host) {
			t.Fatalf("%s's refusal carries no host word beneath its class: %v", refusal, res.Cause)
		}
		if host.Refusal != string(refusal) || host.Detail != "the host's sentence" {
			t.Errorf("%s's refusal carries %+v, want its own word and sentence", refusal, host)
		}
	}
}
