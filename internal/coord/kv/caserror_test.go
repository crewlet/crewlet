package kv

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// A COMPARE-AND-SET REFUSAL IS MATCHED ON ITS CODE, both of them.
//
// The server answers 10071 on a solo stream and 10164 on a REPLICATED one for
// the same refusal — so a fleet, which is the only topology where this race is
// common, matched neither code and fell through to a substring test on a
// message the client is free to reword.
//
// The message in each fixture below is DELIBERATELY UNRELATED: a test whose
// error text says "wrong last sequence" passes for the substring version too,
// and would have proved nothing about the fix.
func TestACompareAndSetRefusalIsMatchedOnItsCode(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"the solo code": {
			&jetstream.APIError{
				ErrorCode:   jetstream.JSErrCodeStreamWrongLastSequence,
				Description: "an entirely unrelated string",
			}, true,
		},
		"the replicated code": {
			&jetstream.APIError{
				ErrorCode:   jetstream.JSErrCodeStreamWrongLastSequenceConstant,
				Description: "an entirely unrelated string",
			}, true,
		},
		"wrapped, because callers wrap": {
			fmt.Errorf("publish the activation: %w", &jetstream.APIError{
				ErrorCode: jetstream.JSErrCodeStreamWrongLastSequenceConstant,
			}), true,
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
