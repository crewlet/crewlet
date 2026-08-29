package engine_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The engine's half of the fleet's credential cooldowns.
//
// internal/coord shipped the Cooldowns contract, three backends implemented it
// and the certified suite passed — and nothing ever called it. Every case here
// failed before this wiring existed, and none of them by erroring: a fleet
// simply paid one 429 per node for what the first node already knew, which
// looks from the outside exactly like a vendor being slow.

// poolOf reaches the credential pool behind one configured provider.
func poolOf(t *testing.T, e *engine.Engine, key string) *credential.Pool {
	t.Helper()
	for k, provider := range e.Company().Models.All() {
		if k != key {
			continue
		}
		pooled, ok := provider.(interface{ Pool() *credential.Pool })
		if !ok {
			t.Fatalf("provider %q rotates no credentials", key)
		}
		return pooled.Pool()
	}
	t.Fatalf("no provider configured under %q", key)
	return nil
}

// coolingOf is what the pool says is left on one key's bench.
func coolingOf(t *testing.T, pool *credential.Pool, key string) time.Duration {
	t.Helper()
	hint := credential.Hint(key)
	for _, stat := range pool.Stats() {
		if stat.Hint == hint {
			return stat.Cooling
		}
	}
	t.Fatalf("the pool holds no key hinted %q", hint)
	return 0
}

// cooldownCompany names its credential outright rather than through a ${VAR},
// so a test can compute the hint the fleet's ledger will be keyed by.
const cooldownCompany = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
    alpha:
      type: anthropic
      model: claude-haiku-4-5
      api_keys: ["sk-ant-fake-zulu-key"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`

const cooldownKey = "sk-ant-fake-zulu-key"

// A BENCH THIS NODE TAKES REACHES THE FLEET.
//
// Without it every peer pays its own 429 to discover the same exhausted quota
// — and with a two-key bag on four nodes that is eight wasted calls for one
// rate-limit window.
func TestABenchTakenHereIsPublishedToTheFleet(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, cooldownCompany)})

	lease, ok := poolOf(t, e, "zulu").Acquire()
	if !ok {
		t.Fatal("the pool had nothing to lease")
	}
	lease.Fail(t.Context(), llm.KindRateLimit, 0)

	cooled, err := e.Backends().Fleet.Since(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if _, ok := cooled["zulu:"+credential.Hint(cooldownKey)]; !ok {
		t.Fatalf("the bench never reached the fleet; ledger = %v", cooled)
	}
}

// THE SCOPE IS THE CONFIG ENTRY'S KEY, which is what keeps one entry's quota
// out of another's. Two entries can list the same credential against different
// models, and those are two rate-limit buckets at the vendor — a shared record
// with no scope would turn one model's burst into a company-wide outage.
func TestOneEntrysBenchDoesNotReachAnotherOnTheSameKey(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, cooldownCompany)})

	lease, _ := poolOf(t, e, "zulu").Acquire()
	lease.Fail(t.Context(), llm.KindRateLimit, 0)

	applied, err := poolOf(t, e, "alpha").Refresh(t.Context())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if applied != 0 {
		t.Fatal("one entry's rate limit benched the same key under the other entry")
	}
}

// A NODE THAT HAS JUST STARTED INHERITS THE FLEET'S BENCH.
//
// This is the moment the whole subsystem exists for and the one a ticker alone
// would miss: a fresh process has an empty bench, and waiting a full interval
// before its first pull spends its first turns rediscovering exactly what the
// fleet already recorded. So the loop pulls immediately and then on the tick.
func TestABootingNodeInheritsWhatItsPeersBenched(t *testing.T) {
	t.Parallel()
	boot := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	back, err := openBackends(t, boot)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	// A PEER's record, written before this node exists.
	until := time.Now().Add(30 * time.Minute)
	if err := back.Fleet.Cool(t.Context(), "zulu:"+credential.Hint(cooldownKey), until); err != nil {
		t.Fatalf("Cool: %v", err)
	}

	e := newEngine(t, engine.Options{
		Bootstrap: boot, Backends: back,
		Company: parsedCompany(t, cooldownCompany),
	})

	pool := poolOf(t, e, "zulu")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if coolingOf(t, pool, cooldownKey) > 0 {
			if _, ok := pool.Acquire(); ok {
				t.Fatal("the key was benched and leased anyway")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the node came up with an empty bench: the fleet's cooldown never " +
		"reached the pool, so this node's first calls will rediscover it")
}

// A NODE WITH NO COORDINATION STORE KEEPS ITS COOLDOWNS AND RUNS. That is the
// single-node deployment, where there is no peer to tell — and it must not
// need a store to bench a key at all.
func TestANodeWithNoFleetStoreStillBenchesLocally(t *testing.T) {
	t.Parallel()
	boot := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	back, err := openBackends(t, boot)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })
	back.Fleet = nil

	e := newEngine(t, engine.Options{
		Bootstrap: boot, Backends: back,
		Company: parsedCompany(t, cooldownCompany),
	})

	pool := poolOf(t, e, "zulu")
	lease, ok := pool.Acquire()
	if !ok {
		t.Fatal("the pool had nothing to lease")
	}
	lease.Fail(t.Context(), llm.KindRateLimit, 0)
	if _, ok := pool.Acquire(); ok {
		t.Fatal("the key stayed live on a node with no fleet store")
	}
}
