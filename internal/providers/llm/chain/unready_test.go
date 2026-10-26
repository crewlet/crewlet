package chain

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// knowing is a member that can say, without a call, what its next call would
// meet.
type knowing struct {
	unready error
}

func (k knowing) Model() string { return "knowing" }

func (k knowing) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return nil, errors.New("knowing: a readiness case must not call a member")
}

func (k knowing) Unready(context.Context) error { return k.unready }

func spent(at time.Time) error {
	return &llm.Error{Kind: llm.KindRateLimit, RetryAt: at, Err: errors.New("plan spent")}
}

func refused(kind llm.ErrorKind, err error) error {
	return &llm.Error{Kind: kind, Err: err}
}

// THE CHAIN IS UNREADY ONLY WHEN EVERY MEMBER IS, and then it reports the
// failure that clears first: a spent plan (the earliest reset, a named one
// before an unnamed one) over a login over a CLI that cannot start. A member
// that cannot say may answer, so it makes the chain ready.
//
// Mutation: return the first member's failure without sorting, and the mixed
// cases name the wrong one; return a failure when one member is ready, and the
// "one ready" case fails.
func TestAChainIsUnreadyOnlyWhenEveryMemberIs(t *testing.T) {
	t.Parallel()
	soon := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	later := soon.Add(time.Hour)
	auth := refused(llm.KindAuth, errors.New("signed out"))
	missing := refused(llm.KindFatal, llm.ErrNotRunnable)
	cases := map[string]struct {
		members []llm.Provider
		kind    llm.ErrorKind
		at      time.Time
		ready   bool
	}{
		"no members":                {ready: true},
		"one ready":                 {members: []llm.Provider{knowing{auth}, knowing{}}, ready: true},
		"a member that cannot say":  {members: []llm.Provider{knowing{auth}, answering("m", "x")}, ready: true},
		"every member signed out":   {members: []llm.Provider{knowing{auth}, knowing{auth}}, kind: llm.KindAuth},
		"spent beside signed out":   {members: []llm.Provider{knowing{auth}, knowing{spent(later)}}, kind: llm.KindRateLimit, at: later},
		"the earliest reset":        {members: []llm.Provider{knowing{spent(later)}, knowing{spent(soon)}}, kind: llm.KindRateLimit, at: soon},
		"a named reset first":       {members: []llm.Provider{knowing{spent(time.Time{})}, knowing{spent(later)}}, kind: llm.KindRateLimit, at: later},
		"signed out before missing": {members: []llm.Provider{knowing{missing}, knowing{auth}}, kind: llm.KindAuth},
		"every member cannot start": {members: []llm.Provider{knowing{missing}}, kind: llm.KindFatal},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := Unready(t.Context(), tc.members)
			if tc.ready {
				if err != nil {
					t.Fatalf("Unready = %v, want nil", err)
				}
				return
			}
			var failure *llm.Error
			if !errors.As(err, &failure) || failure.Kind != tc.kind {
				t.Fatalf("Unready = %v, want kind %v", err, tc.kind)
			}
			if !failure.RetryAt.Equal(tc.at) {
				t.Errorf("RetryAt = %v, want %v", failure.RetryAt, tc.at)
			}
		})
	}
}
