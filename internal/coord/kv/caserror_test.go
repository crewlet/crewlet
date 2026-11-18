package kv

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// A COMPARE-AND-SET REFUSAL IS MATCHED ON ITS CODE — the DECIDED one.
//
// 10071 is the leader's refusal once it has compared revisions, on a solo
// stream or a replicated one. 10164 says the same three words and decides
// nothing: another write to the subject, maybe this caller's own and already
// acknowledged, is still in process. Read as a refusal it left a create race
// over a just-removed record with no winner, so it is none here: every bucket
// handle is [settled], which waits it out instead.
//
// Each fixture's message is DELIBERATELY UNRELATED, so a substring match
// cannot pass. Mutation: match 10164 as well, and the in-process case fails.
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
