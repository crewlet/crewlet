package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/secrets"
)

// sharedFleet is the fleet the race cases wrap, under a name that does not
// collide with the interface's own Fleet method.
type sharedFleet = coord.Fleet

// racedKey is the name the race cases mint.
const racedKey = "iam/test-company-key"

// racingFleet reproduces the interleaving that split a company's key: the
// FIRST write of the key is held back until a second write has landed (or a
// short grace passes), so the first writer's value is the one the store keeps
// while the second writer has already read its own back.
//
// Without an exclusion around the mint, two minters both reach the write and
// that ordering is certain. With one, a second write never comes, and the
// grace lets the only writer through.
type racingFleet struct {
	sharedFleet

	mu     sync.Mutex
	writes int
	second chan struct{}
	once   sync.Once
}

func newRacingFleet() *racingFleet {
	return &racingFleet{sharedFleet: coordmem.NewFleet(), second: make(chan struct{})}
}

func (f *racingFleet) PutSecret(ctx context.Context, rec coord.SecretRecord) error {
	_, err := f.race(ctx, rec.Name, func() (bool, error) {
		return true, f.sharedFleet.PutSecret(ctx, rec)
	})
	return err
}

// CreateSecret is held back exactly as a put is, so a mint written either way
// meets the same interleaving.
func (f *racingFleet) CreateSecret(ctx context.Context, rec coord.SecretRecord) (bool, error) {
	return f.race(ctx, rec.Name, func() (bool, error) {
		return f.sharedFleet.CreateSecret(ctx, rec)
	})
}

// race holds the first write of the raced key back until a second has landed.
func (f *racingFleet) race(ctx context.Context, name string, write func() (bool, error)) (
	bool, error) {

	if name != racedKey {
		return write()
	}
	f.mu.Lock()
	f.writes++
	first := f.writes == 1
	f.mu.Unlock()
	if !first {
		wrote, err := write()
		f.once.Do(func() { close(f.second) })
		return wrote, err
	}
	select {
	case <-f.second:
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return write()
}

// keyNode is a hand-built node over a shared fleet and coordination store:
// everything [Engine.companyKey] reads, and nothing else.
func keyNode(t *testing.T, id string, fleet coord.Fleet, backend coord.Backend) *Engine {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1",
		Keys:     map[string][]byte{"k1": []byte("crewlet-test-sealing-key-32bytes")},
	})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return &Engine{
		backends:    &Backends{Fleet: fleet, Coord: backend},
		cipher:      cipher,
		id:          id,
		incarnation: id + ":incarnation",
	}
}

// mintConcurrently has every node ask for the key at once and returns what
// each was told, beside what the store kept.
func mintConcurrently(t *testing.T, nodes []*Engine, fleet coord.Fleet) ([]string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got := make([]string, len(nodes))
	errs := make([]error, len(nodes))
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Go(func() {
			got[i], errs[i] = node.companyKey(ctx, racedKey, "test", noEstate)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
	}
	stored, err := fleetsecrets.New(fleet, nodes[0].cipher).Estate().Get(ctx, racedKey)
	if err != nil {
		t.Fatalf("read the stored key: %v", err)
	}
	return got, stored
}

// TWO NODES THAT FIND A COMPANY KEY ABSENT AT ONCE END UP WITH ONE KEY.
//
// The store is last-write-wins, and the read-back the mint used to rely on did
// not make that harmless: a node that read its own write back before the other
// node's landed went on deriving under a key the store no longer held — and a
// blind is opaque, so nothing reports it. A fleet booting every node at once is
// exactly when this happens.
func TestACompanyKeyIsMintedOnceWhenTwoNodesRaceForIt(t *testing.T) {
	t.Parallel()
	fleet := newRacingFleet()
	backend := coordmem.New()
	nodes := []*Engine{
		keyNode(t, "node-a", fleet, backend),
		keyNode(t, "node-b", fleet, backend),
	}
	got, stored := mintConcurrently(t, nodes, fleet)
	for i, key := range got {
		if key != stored {
			t.Errorf("node %d derives under a key the store does not hold, so "+
				"no peer can match anything it blinds", i)
		}
	}
}

// AND WHEN THE HOLD KEEPS NOBODY APART, THE STORE STILL DOES.
//
// The hold is a lease, and a lease cannot fence a write: a holder paused past
// it — a GC stop, a coordination write that hangs and then lands — writes after
// a second holder has minted and read its own key back. Two nodes with no
// coordination store between them are that state with the timing removed: each
// holds, each mints, and the first write lands after the second. A mint that
// PUT its key split the company there; a mint that CREATES it leaves the second
// write refused and both nodes deriving under the one key the store holds.
func TestACompanyKeyIsMintedOnceWhenTheHoldKeepsNobodyApart(t *testing.T) {
	t.Parallel()
	fleet := newRacingFleet()
	nodes := []*Engine{
		keyNode(t, "node-a", fleet, nil),
		keyNode(t, "node-b", fleet, nil),
	}
	if nodes[0].workerHold("company-key-"+racedKey, companyKeyHoldTTL) != nil {
		t.Fatal("a node with no coordination store holds something, so this " +
			"case is not the one it names")
	}
	got, stored := mintConcurrently(t, nodes, fleet)
	for i, key := range got {
		if key != stored {
			t.Errorf("node %d derives under a key the store does not hold — a "+
				"mint that put its key over the one that landed first", i)
		}
	}
}

// AND TWO GOROUTINES OF ONE NODE. The fleet's hold does not keep them apart —
// a claim by the owner that already holds it answers yes — so the in-process
// gate is the half that does.
func TestTwoCallersOnOneNodeMintOneKey(t *testing.T) {
	t.Parallel()
	fleet := newRacingFleet()
	node := keyNode(t, "node-a", fleet, coordmem.New())
	got, stored := mintConcurrently(t, []*Engine{node, node}, fleet)
	for i, key := range got {
		if key != stored {
			t.Errorf("caller %d derives under a key the store does not hold", i)
		}
	}
}
