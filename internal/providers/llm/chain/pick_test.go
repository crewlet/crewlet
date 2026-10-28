package chain

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// readiness answers [Pick]'s ready from a table keyed by member, so each case
// states which member refuses and how.
func readiness(refusals map[string]error) func(Member) error {
	return func(m Member) error { return refusals[m.Key] }
}

func sittingOut(key string) error {
	return &llm.Error{Kind: llm.KindRateLimit, Provider: "cli-agent/claude-code",
		Err:     fmt.Errorf("Usage limit reached on %s — sitting out until 2026-10-23T10:01:00Z", key),
		RetryAt: sitOutUntil}
}

// sitOutUntil is when every member [sittingOut] refuses is tried again.
var sitOutUntil = time.Date(2026, time.October, 23, 10, 1, 0, 0, time.UTC)

// A LAUNCH SKIPS A MEMBER SITTING OUT AND LANDS ON THE NEXT, with no special
// case: the refusal is a retryable classification, as a call's is, and
// that alone moves the walk.
//
// Mutation: return the head whatever ready says, and the launch lands on the
// spent entry.
func TestALaunchSkipsAMemberSittingOut(t *testing.T) {
	t.Parallel()
	members := []Member{member("primary", answering("a", "")), member("backup", answering("b", ""))}
	got, err := Pick(context.Background(), members,
		readiness(map[string]error{"primary": sittingOut("primary")}), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Key != "backup" {
		t.Fatalf("picked %q, want the member after the one sitting out", got.Key)
	}
}

// A READY HEAD IS PICKED AND NOTHING AFTER IT IS ASKED — a launch that walked
// further would ask entries it never needed and could announce hand-offs that
// did not happen.
func TestAReadyHeadIsPickedAndNothingElseIsAsked(t *testing.T) {
	t.Parallel()
	asked := 0
	members := []Member{member("primary", answering("a", "")), member("backup", answering("b", ""))}
	got, err := Pick(context.Background(), members, func(Member) error { asked++; return nil }, nil)
	if err != nil || got.Key != "primary" {
		t.Fatalf("Pick = %q, %v; want the head", got.Key, err)
	}
	if asked != 1 {
		t.Fatalf("asked %d members, want only the head", asked)
	}
}

// THE HAND-OFF IS ANNOUNCED NAMING WHO TOOK OVER, and the last member's names
// nobody — the same Fallback a call publishes, so provider_fallback reads
// identically whether a call or a launch fell through.
func TestALaunchHandOffNamesTheMemberThatTookOver(t *testing.T) {
	t.Parallel()
	var seen []Fallback
	members := []Member{
		member("primary", answering("a", "")),
		member("secondary", answering("b", "")),
		member("backup", answering("c", "")),
	}
	refusals := map[string]error{"primary": sittingOut("primary"), "secondary": sittingOut("secondary")}
	got, err := Pick(context.Background(), members, readiness(refusals),
		func(f Fallback) { seen = append(seen, f) })
	if err != nil || got.Key != "backup" {
		t.Fatalf("Pick = %q, %v", got.Key, err)
	}
	if len(seen) != 2 {
		t.Fatalf("fired %d hooks, want one per member skipped", len(seen))
	}
	if seen[0].From != "primary" || seen[0].To != "secondary" || seen[0].Kind != llm.KindRateLimit {
		t.Errorf("first hand-off = %+v", seen[0])
	}
	if seen[1].From != "secondary" || seen[1].To != "backup" || seen[1].Err == nil {
		t.Errorf("second hand-off = %+v", seen[1])
	}
}

// EVERY MEMBER SITTING OUT IS AN EXHAUSTED CHAIN, the same [*Error] an
// exhausted call reports, each member's refusal on it — so a launch reads
// exactly as a call does, with no second code path: a chain every member of
// which is at a limit ([Limited]), which parks the seat until the first is
// tried again, where a launch that read as a plain failure was retried into the
// limit. The last hand-off names no successor.
//
// Mutation: return the last refusal bare, and errors.As misses; leave the
// refusals off the error, and the launch is no limited chain.
func TestEveryMemberSittingOutIsAnExhaustedChain(t *testing.T) {
	t.Parallel()
	var seen []Fallback
	members := []Member{member("primary", answering("a", "")), member("backup", answering("b", ""))}
	_, err := Pick(context.Background(), members,
		readiness(map[string]error{"primary": sittingOut("primary"), "backup": sittingOut("backup")}),
		func(f Fallback) { seen = append(seen, f) })
	var chainErr *Error
	if !errors.As(err, &chainErr) {
		t.Fatalf("err = %v, want a *chain.Error", err)
	}
	if got := fmt.Sprint(chainErr.Attempted); got != "[primary backup]" {
		t.Errorf("Attempted = %v", got)
	}
	if llm.KindOf(err) != llm.KindRateLimit {
		t.Errorf("KindOf = %s, want the last member's", llm.KindOf(err))
	}
	if len(seen) != 2 || seen[1].To != "" {
		t.Errorf("hand-offs = %+v, want the last to name nobody", seen)
	}
	if limit, ok := Limited(err); !ok || !limit.RetryAt.Equal(sitOutUntil) ||
		fmt.Sprint(limit.Keys) != "[primary backup]" {
		t.Errorf("Limited = %+v, %v; want every member at its limit until %v", limit, ok, sitOutUntil)
	}
}

// A FATAL REFUSAL STOPS THE WALK and comes back as it came: a launch the host
// cannot make on one member is the operator's to fix, not an outage for the
// next to carry, and it is announced as no hand-off.
func TestAFatalRefusalStopsTheLaunchWalk(t *testing.T) {
	t.Parallel()
	fatal := &llm.Error{Kind: llm.KindFatal, Err: errors.New("no runner")}
	fired := 0
	members := []Member{member("primary", answering("a", "")), member("backup", answering("b", ""))}
	_, err := Pick(context.Background(), members,
		readiness(map[string]error{"primary": fatal}), func(Fallback) { fired++ })
	if !errors.Is(err, fatal) {
		t.Fatalf("err = %v, want the member's own refusal", err)
	}
	var chainErr *Error
	if errors.As(err, &chainErr) {
		t.Fatal("a fatal refusal was dressed up as an exhausted chain")
	}
	if fired != 0 {
		t.Fatalf("fired %d hooks for a refusal that did not hand off", fired)
	}
}

// NO MEMBER AT ALL IS A FATAL NAMING ITSELF, never an exhausted chain whose
// last error is nil.
func TestALaunchWithNoMemberIsRefused(t *testing.T) {
	t.Parallel()
	_, err := Pick(context.Background(), nil, func(Member) error { return nil }, nil)
	if llm.KindOf(err) != llm.KindFatal {
		t.Fatalf("err = %v, want a fatal", err)
	}
}
