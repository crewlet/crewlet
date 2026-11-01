package chain

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// limitedAt is a member refusing for a spent plan, tried again at retry and
// lifting at reset (zero where its vendor named none).
func limitedAt(model string, retry, reset time.Time) *fake {
	return &fake{model: model, err: &llm.Error{
		Kind: llm.KindRateLimit, Provider: "p", Model: model, RetryAt: retry, ResetsAt: reset,
		Agent: "claude-code", Subscription: true,
		Err: errors.New("You've hit your 5-hour limit"),
	}}
}

// A CHAIN IS LIMITED ONLY WHEN EVERY MEMBER IS. A seat whose head is spent and
// whose fallback failed for a reason nothing says will last is not waited out:
// its retry may well find the fallback answering. And the instant a person is
// told is the earliest any member's vendor named, unless one named none.
//
// Mutation: read only the last member (the chain's Err), and the case with a
// server failure behind a spent head reads as limited; or only the first, and
// the one with a spent fallback behind a failing head does.
func TestAChainIsLimitedOnlyWhenEveryMemberIs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 27, 23, 16, 0, 0, time.UTC)
	soon, later := now.Add(14*time.Minute), now.Add(3*time.Hour)
	for name, tc := range map[string]struct {
		members   []Member
		limited   bool
		retry     time.Time
		resets    time.Time
		wantFirst string
	}{
		"one spent member": {
			members: []Member{member("plan", limitedAt("opus", soon, soon))},
			limited: true, retry: soon, resets: soon,
		},
		"both spent, the earliest named reset": {
			members: []Member{member("plan", limitedAt("opus", soon, later)),
				member("key", limitedAt("sonnet", later, soon))},
			limited: true, retry: soon, resets: soon,
		},
		"one member named no reset": {
			members: []Member{member("plan", limitedAt("opus", soon, soon)),
				member("key", limitedAt("sonnet", later, time.Time{}))},
			limited: true, retry: soon,
		},
		"a spent head and a fallback that failed otherwise": {
			members: []Member{member("plan", limitedAt("opus", soon, soon)),
				member("key", failing("sonnet", llm.KindServer))},
		},
		"a failing head and a spent fallback": {
			members: []Member{member("plan", failing("opus", llm.KindTimeout)),
				member("key", limitedAt("sonnet", soon, soon))},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := build(t, Options{}, tc.members...).Complete(t.Context(), llm.Request{})
			limit, limited := Limited(err)
			if limited != tc.limited {
				t.Fatalf("Limited = %v, want %v (err %v)", limited, tc.limited, err)
			}
			if !limited {
				return
			}
			if !limit.RetryAt.Equal(tc.retry) || !limit.ResetsAt.Equal(tc.resets) {
				t.Errorf("limit retries %v and resets %v, want %v and %v",
					limit.RetryAt, limit.ResetsAt, tc.retry, tc.resets)
			}
			keys := make([]string, 0, len(tc.members))
			for _, m := range tc.members {
				keys = append(keys, m.Key)
			}
			if !slices.Equal(limit.Keys, keys) {
				t.Errorf("keys %v, want %v", limit.Keys, keys)
			}
			if limit.Said != "You've hit your 5-hour limit" {
				t.Errorf("said %q, want the head's own sentence", limit.Said)
			}
			if limit.Agent != "claude-code" || !limit.Subscription {
				t.Errorf("limit names %q (subscription %v), want the head's CLI on a plan",
					limit.Agent, limit.Subscription)
			}
		})
	}
}

// AN ERROR THAT IS NOT AN EXHAUSTED CHAIN IS NOT A LIMIT, even a rate limit
// returned on its own: only a chain says every member was asked.
func TestOnlyAnExhaustedChainIsLimited(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for name, err := range map[string]error{
		"nil":               nil,
		"a bare rate limit": &llm.Error{Kind: llm.KindRateLimit, RetryAt: now, ResetsAt: now},
		"a fatal":           &llm.Error{Kind: llm.KindFatal, Err: errors.New("no binary")},
	} {
		if _, limited := Limited(err); limited {
			t.Errorf("%s: Limited = true, want false", name)
		}
	}
}
