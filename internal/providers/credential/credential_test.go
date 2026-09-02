package credential

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

func TestMain(m *testing.M) {
	// The pool warns on every bench, and these tests bench hundreds of
	// keys. Silence keeps a failure readable.
	logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	// The package logger bound its handler at package-var init, which
	// runs before this. Rebind it or every case prints its own log.
	log = logging.Get("providers.credential")
	os.Exit(m.Run())
}

// fakeClock is the pool's monotonic source under test. Guarded because the
// concurrency cases read it from many goroutines.
type fakeClock struct {
	mu sync.Mutex
	d  time.Duration
}

func (c *fakeClock) now() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.d
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.d += d
}

func newTestPool(t *testing.T, keys []string, policy Policy) (*Pool, *fakeClock) {
	t.Helper()
	clock := &fakeClock{}
	return New(Options{Keys: keys, Policy: policy, Clock: clock.now}), clock
}

func coolingOf(t *testing.T, p *Pool, hint string) time.Duration {
	t.Helper()
	for _, s := range p.Stats() {
		if s.Hint == hint {
			return s.Cooling
		}
	}
	t.Fatalf("no pool entry with hint %q", hint)
	return 0
}

// --- construction ------------------------------------------------------

func TestNewNormalisesTheKeyList(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		keys []string
		want int
	}{
		{"plain", []string{"a", "b", "c"}, 3},
		{"drops empties", []string{"a", "", "b"}, 2},
		{"drops duplicates", []string{"a", "b", "a"}, 2},
		{"all empty leaves one placeholder", []string{"", ""}, 1},
		{"none leaves one placeholder", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := New(Options{Keys: tc.keys})
			if got := p.Size(); got != tc.want {
				t.Fatalf("Size() = %d, want %d", got, tc.want)
			}
		})
	}
}

// A duplicated key must not become two entries: the pool would otherwise
// believe it had somewhere to rotate to after benching the first copy, and
// then hand out the same benched credential.
func TestDuplicateKeyIsOneEntry(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k", "k"}, Policy{})
	lease, ok := p.Acquire()
	if !ok {
		t.Fatal("Acquire() found nothing in a fresh pool")
	}
	lease.Fail(t.Context(), llm.KindRateLimit, 0)
	if _, ok := p.Acquire(); ok {
		t.Fatal("Acquire() succeeded after the only distinct key was benched")
	}
}

func TestPlaceholderKeyLeasesAnEmptyString(t *testing.T) {
	t.Parallel()
	p := New(Options{})
	lease, ok := p.Acquire()
	if !ok {
		t.Fatal("Acquire() found nothing")
	}
	if lease.Key() != "" || lease.Hint() != "" {
		t.Fatalf("placeholder lease = %q/%q, want empty", lease.Key(), lease.Hint())
	}
}

// --- selection ---------------------------------------------------------

func TestAcquireSpreadsConcurrentLeasesAcrossKeys(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k0", "k1", "k2"}, Policy{})
	first, _ := p.Acquire()
	second, _ := p.Acquire()
	third, _ := p.Acquire()
	got := []string{first.Key(), second.Key(), third.Key()}
	want := []string{"k0", "k1", "k2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lease %d = %q, want %q (leases: %v)", i, got[i], want[i], got)
		}
	}
}

func TestAcquireIsFillFirstOnceLeasesAreReturned(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k0", "k1"}, Policy{})
	first, _ := p.Acquire()
	first.Succeed()
	second, _ := p.Acquire()
	if second.Key() != "k0" {
		t.Fatalf("second lease = %q, want the head back", second.Key())
	}
	for _, s := range p.Stats() {
		if s.Hint == Hint("k0") && s.UseCount != 2 {
			t.Fatalf("head UseCount = %d, want 2", s.UseCount)
		}
	}
}

func TestAcquireSkipsBenchedKeys(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"bad", "good"}, Policy{})
	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)
	next, ok := p.Acquire()
	if !ok || next.Key() != "good" {
		t.Fatalf("Acquire() = %v/%q, want the live key", ok, next.Key())
	}
}

func TestAcquireReportsNothingWhenEveryKeyIsBenched(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k0", "k1"}, Policy{})
	for range 2 {
		lease, ok := p.Acquire()
		if !ok {
			t.Fatal("Acquire() ran out early")
		}
		lease.Fail(t.Context(), llm.KindRateLimit, 0)
	}
	if _, ok := p.Acquire(); ok {
		t.Fatal("Acquire() succeeded with every key benched")
	}
}

// --- the clock ---------------------------------------------------------

func TestBenchExpiresOnTheInjectedClock(t *testing.T) {
	t.Parallel()
	p, clock := newTestPool(t, []string{"k"}, Policy{RateLimit: time.Minute})
	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)

	clock.advance(59 * time.Second)
	if _, ok := p.Acquire(); ok {
		t.Fatal("key came back one second early")
	}
	clock.advance(2 * time.Second)
	if _, ok := p.Acquire(); !ok {
		t.Fatal("key did not come back after its TTL elapsed")
	}
}

// A frozen clock must freeze the bench, however much real time passes. This
// is what pins that cooldown arithmetic reads NOTHING but the injected
// source: an implementation that consulted time.Now() would let real elapsed
// time move a deadline the pool believes is still in the future.
func TestBenchIgnoresRealTimeWhenTheClockIsFrozen(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k"}, Policy{RateLimit: 5 * time.Millisecond})
	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)

	time.Sleep(25 * time.Millisecond)
	if _, ok := p.Acquire(); ok {
		t.Fatal("a five-millisecond bench expired against real time, not the pool's clock")
	}
	if got := coolingOf(t, p, Hint("k")); got != 5*time.Millisecond {
		t.Fatalf("Cooling = %v, want the full TTL still outstanding", got)
	}
}

// Elapsed must be a duration since process start, not an epoch reading
// dressed as one. An implementation returning time.Duration(time.Now()
// .UnixNano()) would satisfy "non-decreasing" and fail here — and would carry
// a wall clock's jumps straight into the pool.
func TestElapsedIsSinceProcessStartNotTheEpoch(t *testing.T) {
	t.Parallel()
	first := Elapsed()
	if first < 0 || first > time.Hour {
		t.Fatalf("Elapsed() = %v, want a small time-since-start", first)
	}
	time.Sleep(time.Millisecond)
	second := Elapsed()
	if second < first {
		t.Fatalf("Elapsed() went backwards: %v then %v", first, second)
	}
}

// --- cooldown policy ---------------------------------------------------

func TestPolicyDefaultsMatchTheConfigLayer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		policy Policy
		kind   llm.ErrorKind
		want   time.Duration
	}{
		{"rate limit default", Policy{}, llm.KindRateLimit, DefaultRateLimitCooldown},
		{"auth default", Policy{}, llm.KindAuth, DefaultAuthCooldown},
		{"rate limit configured", Policy{RateLimit: 90 * time.Second}, llm.KindRateLimit, 90 * time.Second},
		{"auth configured", Policy{Auth: 90 * time.Second}, llm.KindAuth, 90 * time.Second},
		{"one configured leaves the other defaulted", Policy{Auth: time.Minute}, llm.KindRateLimit, DefaultRateLimitCooldown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, _ := newTestPool(t, []string{"k"}, tc.policy)
			lease, _ := p.Acquire()
			lease.Fail(t.Context(), tc.kind, 0)
			if got := coolingOf(t, p, Hint("k")); got != tc.want {
				t.Fatalf("Cooling = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOnlyCredentialKindsBench(t *testing.T) {
	t.Parallel()
	for _, kind := range []llm.ErrorKind{llm.KindTimeout, llm.KindServer, llm.KindFatal} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			p, _ := newTestPool(t, []string{"k"}, Policy{})
			lease, _ := p.Acquire()
			lease.Fail(t.Context(), kind, 0)
			if got := coolingOf(t, p, Hint("k")); got != 0 {
				t.Fatalf("%s benched the key for %v; a transport failure is not a key failure",
					kind, got)
			}
			if _, ok := p.Acquire(); !ok {
				t.Fatalf("%s made the key unavailable", kind)
			}
		})
	}
}

func TestServerHintOverridesThePolicyTTL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		after time.Duration
		want  time.Duration
	}{
		{"positive hint wins", 20 * time.Second, 20 * time.Second},
		{"zero hint falls back", 0, time.Hour},
		{"negative hint falls back", -5 * time.Second, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, _ := newTestPool(t, []string{"k"}, Policy{RateLimit: time.Hour})
			lease, _ := p.Acquire()
			lease.Fail(t.Context(), llm.KindRateLimit, tc.after)
			if got := coolingOf(t, p, Hint("k")); got != tc.want {
				t.Fatalf("Cooling = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- auth backoff ------------------------------------------------------

func TestRepeatedAuthFailuresBackOffAndOneSuccessResets(t *testing.T) {
	t.Parallel()
	p, clock := newTestPool(t, []string{"bad"}, Policy{Auth: 100 * time.Second})

	fail := func() time.Duration {
		lease, ok := p.Acquire()
		if !ok {
			// Fast-forward past the bench so the next failure can be
			// recorded; the backoff is what is under test, not the wait.
			clock.advance(maxCooldown)
			lease, ok = p.Acquire()
			if !ok {
				t.Fatal("key never came back")
			}
		}
		lease.Fail(t.Context(), llm.KindAuth, 0)
		return coolingOf(t, p, Hint("bad"))
	}

	want := []time.Duration{
		100 * time.Second,  // 2^0
		200 * time.Second,  // 2^1
		400 * time.Second,  // 2^2
		800 * time.Second,  // 2^3
		1600 * time.Second, // 2^4
		3200 * time.Second, // 2^5
		6400 * time.Second, // 2^6, the cap
		6400 * time.Second, // still 2^6
		6400 * time.Second,
	}
	for i, w := range want {
		if got := fail(); got != w {
			t.Fatalf("auth failure %d benched for %v, want %v", i+1, got, w)
		}
	}

	// One success clears the counter.
	clock.advance(maxCooldown)
	lease, ok := p.Acquire()
	if !ok {
		t.Fatal("key never came back for the success")
	}
	lease.Succeed()
	if got := fail(); got != 100*time.Second {
		t.Fatalf("after a success the bench was %v, want the un-multiplied TTL", got)
	}
}

// A rate-limit failure must not feed the auth backoff, and must not clear it
// either: only a success says the key is good.
func TestRateLimitDoesNotTouchTheAuthCounter(t *testing.T) {
	t.Parallel()
	p, clock := newTestPool(t, []string{"k"}, Policy{Auth: 100 * time.Second, RateLimit: time.Second})

	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindAuth, 0)
	clock.advance(2 * time.Hour)

	lease, _ = p.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)
	clock.advance(2 * time.Hour)

	lease, _ = p.Acquire()
	lease.Fail(t.Context(), llm.KindAuth, 0)
	if got := coolingOf(t, p, Hint("k")); got != 200*time.Second {
		t.Fatalf("Cooling = %v, want the second auth failure's 2x bench", got)
	}
}

// A bench shifted six places overflows int64 into a NEGATIVE duration, and a
// negative bench reads as "ready" — the permanently-bad key the backoff exists
// to bench comes back INSTANTLY on the sixth failure.
//
// The starting value has to be enormous for that (above about four and a half
// years), which is why this uses one rather than the day-long config maximum:
// a mutation removing the saturation survived a 24h policy untouched, because
// 24h shifted six places is nowhere near int64's ceiling and the final clamp
// caught it. Policy is a plain struct in this package, so a caller can build
// the value that does overflow, and min() of a negative is the negative.
func TestAuthBackoffSaturatesInsteadOfOverflowing(t *testing.T) {
	t.Parallel()
	huge := time.Duration(math.MaxInt64 / 8) // ~36 years; << 6 wraps negative
	if huge<<maxAuthDoublings > 0 {
		t.Fatalf("premise broken: %v shifted %d places did not overflow",
			huge, maxAuthDoublings)
	}
	p, clock := newTestPool(t, []string{"bad"}, Policy{Auth: huge})
	for i := range 8 {
		lease, ok := p.Acquire()
		if !ok {
			clock.advance(2 * maxCooldown)
			lease, ok = p.Acquire()
			if !ok {
				t.Fatalf("key never came back before failure %d", i+1)
			}
		}
		lease.Fail(t.Context(), llm.KindAuth, 0)
		got := coolingOf(t, p, Hint("bad"))
		if got != maxCooldown {
			t.Fatalf("failure %d benched for %v, want the %v cap", i+1, got, maxCooldown)
		}
	}
}

func TestBenchIsCappedAtADay(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k"}, Policy{})
	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 400*time.Hour)
	if got := coolingOf(t, p, Hint("k")); got != maxCooldown {
		t.Fatalf("Cooling = %v, want the %v cap", got, maxCooldown)
	}
}

// --- lease lifecycle ---------------------------------------------------

func TestSecondReleaseIsANoOp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		second func(*Lease)
	}{
		{"fail then succeed", func(l *Lease) { l.Succeed() }},
		{"fail then fail", func(l *Lease) { l.Fail(t.Context(), llm.KindAuth, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, _ := newTestPool(t, []string{"k"}, Policy{RateLimit: time.Hour})
			lease, _ := p.Acquire()
			lease.Fail(t.Context(), llm.KindRateLimit, 0)
			tc.second(lease)

			stats := p.Stats()
			if stats[0].InFlight != 0 {
				t.Fatalf("InFlight = %d after a double release, want 0", stats[0].InFlight)
			}
			if stats[0].Cooling != time.Hour {
				t.Fatalf("Cooling = %v, want the first release's bench intact", stats[0].Cooling)
			}
		})
	}
}

func TestSucceedReleasesTheInFlightCount(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k"}, Policy{})
	lease, _ := p.Acquire()
	if p.Stats()[0].InFlight != 1 {
		t.Fatalf("InFlight = %d while leased, want 1", p.Stats()[0].InFlight)
	}
	lease.Succeed()
	if p.Stats()[0].InFlight != 0 {
		t.Fatalf("InFlight = %d after release, want 0", p.Stats()[0].InFlight)
	}
}

// --- Hint --------------------------------------------------------------

func TestHint(t *testing.T) {
	t.Parallel()
	if got := Hint(""); got != "" {
		t.Fatalf("Hint(\"\") = %q, want empty", got)
	}
	const key = "key_a"
	first, second := Hint(key), Hint(key)
	if first != second {
		t.Fatalf("Hint is not stable: %q then %q", first, second)
	}
	if len(first) != 12 {
		t.Fatalf("Hint = %q (%d chars), want 12", first, len(first))
	}
	for _, r := range first {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("Hint = %q, want lowercase hex", first)
		}
	}
	// The obvious alternative — a suffix of the key — leaks most of a
	// short one.
	if strings.Contains(first, key) {
		t.Fatalf("Hint %q contains the key it is hiding", first)
	}
	if Hint("key_b") == first {
		t.Fatal("Hint collided on two different keys")
	}
}

// --- Rotate ------------------------------------------------------------

// classifier is the backend-supplied half of Rotate: a map from the error a
// call returned to the contract's classification.
func classifier(kind llm.ErrorKind, retryAfter time.Duration) func(error) *llm.Error {
	return func(err error) *llm.Error {
		return &llm.Error{Kind: kind, Provider: "test", Model: "m", RetryAfter: retryAfter, Err: err}
	}
}

func TestRotateReturnsTheFirstSuccess(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k0", "k1"}, Policy{})
	var seen []string
	got, err := Rotate(t.Context(), p, Identity{Provider: "test"}, classifier(llm.KindFatal, 0),
		func(key string) (string, error) {
			seen = append(seen, key)
			return "answer from " + key, nil
		})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if got != "answer from k0" {
		t.Fatalf("Rotate = %q", got)
	}
	if len(seen) != 1 {
		t.Fatalf("called %d keys, want 1", len(seen))
	}
}

func TestRotateWalksPastBenchingFailures(t *testing.T) {
	t.Parallel()
	for _, kind := range []llm.ErrorKind{llm.KindRateLimit, llm.KindAuth} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			p, _ := newTestPool(t, []string{"k0", "k1", "k2"}, Policy{})
			var seen []string
			got, err := Rotate(t.Context(), p, Identity{Provider: "test"},
				func(err error) *llm.Error {
					return &llm.Error{Kind: kind, Err: err}
				},
				func(key string) (string, error) {
					seen = append(seen, key)
					if key == "k0" {
						return "", errors.New("refused")
					}
					return key, nil
				})
			if err != nil {
				t.Fatalf("Rotate: %v", err)
			}
			if got != "k1" {
				t.Fatalf("Rotate = %q, want the second key's answer", got)
			}
			if len(seen) != 2 {
				t.Fatalf("consulted %v, want to stop at the first live key", seen)
			}
			if coolingOf(t, p, Hint("k0")) == 0 {
				t.Fatal("the refusing key was not benched")
			}
			if coolingOf(t, p, Hint("k1")) != 0 {
				t.Fatal("the answering key was benched")
			}
		})
	}
}

func TestRotateStopsAtANonCredentialFailure(t *testing.T) {
	t.Parallel()
	for _, kind := range []llm.ErrorKind{llm.KindFatal, llm.KindTimeout, llm.KindServer} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			p, _ := newTestPool(t, []string{"k0", "k1"}, Policy{})
			calls := 0
			_, err := Rotate(t.Context(), p, Identity{Provider: "test"},
				classifier(kind, 0),
				func(string) (string, error) {
					calls++
					return "", errors.New("boom")
				})
			if calls != 1 {
				t.Fatalf("made %d calls, want 1: %s says nothing about the key", calls, kind)
			}
			var classified *llm.Error
			if !errors.As(err, &classified) || classified.Kind != kind {
				t.Fatalf("Rotate error = %v, want a classified %s", err, kind)
			}
			if errors.Is(err, ErrExhausted) {
				t.Fatal("a single non-credential failure was reported as an exhausted pool")
			}
			for _, s := range p.Stats() {
				if s.Cooling != 0 {
					t.Fatalf("%s benched %s", kind, s.Hint)
				}
			}
		})
	}
}

func TestRotateExhaustionCarriesTheLastKind(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k0", "k1"}, Policy{})
	calls := 0
	_, err := Rotate(t.Context(), p, Identity{Provider: "anthropic", Model: "claude"},
		classifier(llm.KindAuth, 0),
		func(string) (string, error) {
			calls++
			return "", errors.New("nope")
		})
	if calls != 2 {
		t.Fatalf("made %d calls, want one per key", calls)
	}
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("error %v does not answer to ErrExhausted", err)
	}
	if got := llm.KindOf(err); got != llm.KindAuth {
		t.Fatalf("KindOf = %s, want the kind that did the benching", got)
	}
	var classified *llm.Error
	if !errors.As(err, &classified) {
		t.Fatalf("error %v is not an *llm.Error", err)
	}
	if classified.Provider != "anthropic" || classified.Model != "claude" {
		t.Fatalf("error names %s/%s, want the caller's identity",
			classified.Provider, classified.Model)
	}
}

// A pool already fully benched when Rotate arrives never calls anything, and
// the honest hint is the kind that says "try elsewhere" without claiming the
// key is bad.
func TestRotateOnAnAlreadyBenchedPoolReportsRateLimit(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k"}, Policy{})
	lease, _ := p.Acquire()
	lease.Fail(t.Context(), llm.KindAuth, 0)

	calls := 0
	_, err := Rotate(t.Context(), p, Identity{Provider: "test"}, classifier(llm.KindFatal, 0),
		func(string) (string, error) { calls++; return "", nil })
	if calls != 0 {
		t.Fatalf("made %d calls against a fully benched pool", calls)
	}
	if got := llm.KindOf(err); got != llm.KindRateLimit {
		t.Fatalf("KindOf = %s, want rate_limit", got)
	}
	if !llm.KindOf(err).Retryable() {
		t.Fatal("an exhausted pool must stay retryable for the chain")
	}
}

func TestRotatePassesTheServerHintToTheBench(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k"}, Policy{RateLimit: time.Hour})
	_, _ = Rotate(t.Context(), p, Identity{Provider: "test"},
		classifier(llm.KindRateLimit, 20*time.Second),
		func(string) (string, error) { return "", errors.New("429") })
	if got := coolingOf(t, p, Hint("k")); got != 20*time.Second {
		t.Fatalf("Cooling = %v, want the server's hint", got)
	}
}

// A backend that cannot classify its own failure must not panic in the middle
// of a turn, and the contract already names the safe answer for an
// unrecognised error.
func TestRotateSurvivesAClassifierThatAnswersNothing(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k0", "k1"}, Policy{})
	calls := 0
	_, err := Rotate(t.Context(), p, Identity{Provider: "test", Model: "m"},
		func(error) *llm.Error { return nil },
		func(string) (string, error) {
			calls++
			return "", errors.New("boom")
		})
	if calls != 1 {
		t.Fatalf("made %d calls, want the unclassified failure to stop the walk", calls)
	}
	if got := llm.KindOf(err); got != llm.KindFatal {
		t.Fatalf("KindOf = %s, want fatal", got)
	}
	for _, s := range p.Stats() {
		if s.Cooling != 0 {
			t.Fatal("an unclassified failure benched a credential")
		}
	}
}

func TestRotateIsSafeUnderConcurrency(t *testing.T) {
	t.Parallel()
	p, _ := newTestPool(t, []string{"k0", "k1", "k2"}, Policy{RateLimit: time.Millisecond})
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			_, _ = Rotate(t.Context(), p, Identity{Provider: "test"},
				classifier(llm.KindRateLimit, 0),
				func(key string) (string, error) {
					if i%3 == 0 {
						return "", fmt.Errorf("429 on %s", key)
					}
					return key, nil
				})
		})
	}
	wg.Wait()
	for _, s := range p.Stats() {
		if s.InFlight != 0 {
			t.Fatalf("key %s left %d leases in flight", s.Hint, s.InFlight)
		}
	}
}
