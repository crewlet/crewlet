package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/credential"
)

// SHUTDOWN MUST NOT WAIT ON A PULL THAT WILL NEVER RETURN.
//
// The refresh loop runs on a DETACHED context, and context.WithoutCancel
// strips the deadline as well as the cancellation — so a pull parked inside
// the coordination store answered to nothing at all. stopCooldownRefresh
// waits on that loop with a bare `<-done`, so a wedged broker did not merely
// stall the refresh: it hung the engine's whole shutdown behind it, past the
// drain, with the store and stream still open.
func TestStopEndsARefreshParkedOnAWedgedStore(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	e.cooldowns = &cooldowns{
		stop: make(chan struct{}), done: make(chan struct{}), cancel: cancel,
	}

	var once sync.Once
	entered := make(chan struct{})
	go e.cooldowns.run(loopCtx, func(ctx context.Context) {
		once.Do(func() { close(entered) })
		// The wedged store: this returns only when something cancels it.
		<-ctx.Done()
	})
	<-entered

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		e.stopCooldownRefresh()
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("stopCooldownRefresh is still waiting on a parked pull: the " +
			"stop channel alone cannot reach a pull blocked inside the store, " +
			"so shutdown hangs behind it with the store and stream still open")
	}
}

// AND THE PULL BOUNDS ITSELF, so a store that is slow rather than wedged
// costs one tick instead of every tick after it.
//
// The bound is the interval: a pull that cannot finish before the next one is
// due has fallen behind whatever it does next, and the following tick re-reads
// the whole bench, so nothing is lost by abandoning it. syncSeatMemory states
// the same rule one file over for the same reason.
//
// Observed on the context that actually REACHES the store, because that is
// the only place the bound matters — FleetStore.Since iterates keys, which
// the client's default per-API timeout does not cover.
func TestARefreshPullCarriesItsOwnDeadline(t *testing.T) {
	t.Parallel()
	company, err := NewCompany(mustParseCooldownCompany(t))
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	seen := make(chan bool, 4)
	shared := &deadlineWatcher{seen: seen}
	pools := 0
	for key, provider := range company.Models.All() {
		pooled, ok := provider.(interface{ Pool() *credential.Pool })
		if !ok {
			continue
		}
		pooled.Pool().Share(key, shared)
		pools++
	}
	if pools == 0 {
		t.Fatal("the fixture company configured no pooled provider")
	}

	e := &Engine{}
	e.epoch.current.Store(company)
	// A DETACHED context, exactly as the loop is armed with: no deadline
	// and no cancellation of its own, so anything the store sees is the
	// bound refreshCooldowns added.
	e.refreshCooldowns(context.WithoutCancel(t.Context()))

	select {
	case bounded := <-seen:
		if !bounded {
			t.Fatal("the store was handed a context with no deadline: a pull " +
				"against a wedged broker blocks for ever, and shutdown waits " +
				"on this loop")
		}
	default:
		t.Fatal("the refresh never reached the store")
	}
}

// deadlineWatcher records whether the context reaching the store is bounded.
type deadlineWatcher struct{ seen chan bool }

func (d *deadlineWatcher) Cool(context.Context, string, time.Time) error { return nil }

func (d *deadlineWatcher) Since(ctx context.Context, _ time.Time) (map[string]time.Time, error) {
	_, ok := ctx.Deadline()
	select {
	case d.seen <- ok:
	default:
	}
	return nil, nil
}

func mustParseCooldownCompany(t *testing.T) *config.Company {
	t.Helper()
	c, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}
