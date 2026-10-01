package kv

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// A COMPARE-AND-SET REFUSAL IS MATCHED ON ITS CODE — the DECIDED one.
//
// The leader refuses a conditional write with 10071 once it has compared the
// revision the write named with the one the subject is at, on a solo stream
// and a replicated one alike. 10164 says the same three words and decides
// nothing: another write to the subject is still in process at the leader, and
// it may be this caller's own, already acknowledged. Read as a refusal it gave
// a create race over a just-removed record no winner, so it is NOT one here —
// [leaderBucket.settle] waits it out instead.
//
// The message in each fixture below is DELIBERATELY UNRELATED: a test whose
// error text says "wrong last sequence" passes for a substring match too, and
// would prove nothing about matching on the code.
//
// Mutation: match 10164 as well, and the in-process case fails here.
func TestACompareAndSetRefusalIsMatchedOnItsDecidedCode(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"the decided refusal": {
			&jetstream.APIError{
				ErrorCode:   jetstream.JSErrCodeStreamWrongLastSequence,
				Description: "an entirely unrelated string",
			}, true,
		},
		"wrapped, because callers wrap": {
			fmt.Errorf("publish the activation: %w", &jetstream.APIError{
				ErrorCode: jetstream.JSErrCodeStreamWrongLastSequence,
			}), true,
		},
		"the leader's in-process answer is not a refusal": {
			&jetstream.APIError{
				ErrorCode:   jetstream.JSErrCodeStreamWrongLastSequenceConstant,
				Description: "an entirely unrelated string",
			}, false,
		},
		"another API error is not this one": {
			&jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamNotFound}, false,
		},
		"a plain error saying the words is NOT a refusal": {
			errors.New("wrong last sequence"), false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isWrongLastSequence(tc.err); got != tc.want {
				t.Errorf("isWrongLastSequence(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
