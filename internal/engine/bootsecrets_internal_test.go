package engine

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/secrets"
)

// fleetUnderTest is coord.Fleet under a name of its own, because the interface
// has a method called Fleet that an embedded field of that name would hide.
type fleetUnderTest = coord.Fleet

// silentFleet is a fleet whose secret listing does not answer the first fails
// times it is asked, as a coordination group still electing its leader does
// not, and counts every time it is asked.
type silentFleet struct {
	fleetUnderTest

	mu    sync.Mutex
	fails int
	asked int
}

func (f *silentFleet) SecretValues(ctx context.Context) ([]coord.SecretRecord, error) {
	f.mu.Lock()
	f.asked++
	silent := f.asked <= f.fails
	f.mu.Unlock()
	if silent {
		return nil, fmt.Errorf("%w: the secrets bucket's group has no leader yet", coord.ErrUnavailable)
	}
	return f.fleetUnderTest.SecretValues(ctx)
}

func (f *silentFleet) timesAsked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

// nodeBeforeItsSnapshot is a node holding the fleet's secret store, its
// listing silent fails times, whose environment holds the stale value of
// ACME_TOKEN and of ACME_MODEL, and whose fleet holds the current ones —
// sealed under seal, which is the node's own keyring unless a test says
// otherwise.
func nodeBeforeItsSnapshot(t *testing.T, fails int, seal secrets.Cipher) (*Engine, *silentFleet) {
	t.Helper()
	_, cipher := testKeyring(t)
	if seal == nil {
		seal = cipher
	}
	inner := coordmem.NewFleet()
	for name, value := range map[string]string{"ACME_TOKEN": "from-the-store", "ACME_MODEL": "claude-from-the-store"} {
		if err := fleetsecrets.New(inner, seal).Set(t.Context(), name, value,
			testAuthor, secrets.SourceCLI, time.Now().UTC()); err != nil {
			t.Fatalf("Set %s: %v", name, err)
		}
	}
	fleet := &silentFleet{fleetUnderTest: inner, fails: fails}
	return &Engine{
		backends: &Backends{Fleet: fleet},
		cipher:   cipher,
		environ: config.MapSource{
			"ACME_TOKEN": "stale-from-the-environment",
			"ACME_MODEL": "stale-model-from-the-environment",
		},
	}, fleet
}

// A BOOT WAITS FOR THE SECRET STORE rather than resolving its company from the
// environment. The store not answering is what a boot meets while a
// coordination group is still electing its leader, and the boot's read used to
// log it and go on, building the company from whatever the process was started
// with: a stale token wherever the environment held one, nothing wherever it
// did not.
//
// Mutation: let bootSecrets return after a failed read, and ${ACME_TOKEN} is
// the environment's stale value.
func TestABootWaitsForTheSecretStoreRatherThanTheEnvironment(t *testing.T) {
	t.Parallel()
	e, fleet := nodeBeforeItsSnapshot(t, 1, nil)
	if err := e.bootSecrets(t.Context()); err != nil {
		t.Fatalf("bootSecrets: %v", err)
	}
	if asked := fleet.timesAsked(); asked != 2 {
		t.Errorf("the store was asked %d times, want twice: once unanswered and once answered", asked)
	}
	if got := e.resolver().Value("${ACME_TOKEN}"); got != "from-the-store" {
		t.Fatalf("${ACME_TOKEN} = %q, want the store's value: the boot built its "+
			"resolver before the store answered", got)
	}
}

// A BOOT FAILS ON A SECRET ITS KEYRING CANNOT OPEN, naming it, and at once.
// Every node runs the same Tier A keyring, so a value it cannot open is a key
// dropped from it: no wait changes the answer, and a boot that waited for one
// would sit `preparing` with nothing to restart it.
//
// Mutation: treat ErrDecrypt as a store that did not answer, and the boot is
// still waiting when the case's own deadline ends it.
func TestABootFailsOnASecretItsKeyringCannotOpen(t *testing.T) {
	t.Parallel()
	_, stranger := testKeyring(t)
	e, fleet := nodeBeforeItsSnapshot(t, 0, stranger)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := e.bootSecrets(ctx)
	if !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("bootSecrets = %v, want the keyring's refusal", err)
	}
	for _, want := range []string{"ACME_", "secrets.keys", "crewlet secrets set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if asked := fleet.timesAsked(); asked != 1 {
		t.Errorf("the store was asked %d times, want once: a value no key opens is not "+
			"a store that did not answer", asked)
	}
	if e.env.Load() != nil {
		t.Error("a snapshot was installed from a store that could not be opened")
	}
}

// A BOOT STOPPED WHILE THE STORE IS SILENT RETURNS, rather than waiting out its
// next attempt: the signal that stops a boot must stop this one too.
func TestABootStoppedWhileTheStoreIsSilentReturns(t *testing.T) {
	t.Parallel()
	e, fleet := nodeBeforeItsSnapshot(t, 1<<30, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.bootSecrets(ctx) }()
	for fleet.timesAsked() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("bootSecrets = %v, want the cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bootSecrets did not return when its context ended")
	}
	if e.env.Load() != nil {
		t.Error("a snapshot was installed although the store never answered")
	}
}

// AN APPLY NEVER BUILDS FROM THE ENVIRONMENT ALONE on a node holding a secret
// store it has never read. It used to: the read's failure was logged, the
// epoch was built from the environment and recorded as applied, and nothing
// read the store again for that revision. [New] does not return without a
// snapshot, so this is a node built some other way — and it is refused before
// anything changed.
//
// Mutation: drop the guard in Apply, and the revision is built from the
// environment's model.
func TestAnApplyNeverBuildsFromTheEnvironmentAlone(t *testing.T) {
	t.Parallel()
	e, _ := nodeBeforeItsSnapshot(t, 1<<30, nil)
	status, stages, err := e.Apply(t.Context(), parseBundled(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    main:
      model: ${ACME_MODEL}
      auth: {mode: api-key, api_key: "${ACME_TOKEN}"}
roles:
  - name: CEO
    handle: ceo
    llm: main
`), time.Now())
	if status != configplane.StatusError || err == nil ||
		!strings.Contains(err.Error(), "never read its secret store") {
		t.Fatalf("Apply = (%s, %v, %v), want the revision refused for want of a snapshot",
			status, stages, err)
	}
	if e.Company() != nil {
		t.Error("an epoch was published from a resolver that never read the store")
	}
}

// THE COMPANY'S HALF OF THE BOOT IS BELOW THE BOUNDARY.
//
// A startup probe is sized for the store a node holds (Options.Recovered), so
// nothing whose time grows with what the company configures may run before
// the boundary: the secret snapshot grows with the company's secrets, and a
// bundle restore waits on the shared login's lock. And the bundles are
// restored last, after every step that can still fail the boot, on Apply's
// rule.
//
// Read out of the source, because none of the order is visible from outside
// a running node. Mutation: move any of the four calls back above
// opts.Recovered(), or the restore above equip, and this is red.
func TestTheCompanysHalfOfTheBootIsBelowTheBoundary(t *testing.T) {
	t.Parallel()
	fn := engineFunction(t, "New")
	boundary := firstCall(fn, "Recovered")
	if boundary == token.NoPos {
		t.Fatal("New no longer calls opts.Recovered(), so this guard asserts nothing — " +
			"point it at whatever replaced the boundary")
	}
	for _, name := range []string{"bootSecrets", "NewCompanyWith", "stampActivation", "restoreBundles"} {
		if at := firstCall(fn, name); at == token.NoPos || at < boundary {
			t.Errorf("New calls %s at %v, want it below the boundary at %v: it is the company's, "+
				"and a startup probe sized for the store knows nothing of it", name, at, boundary)
		}
	}
	if snapshot, build := firstCall(fn, "bootSecrets"), firstCall(fn, "NewCompanyWith"); snapshot > build {
		t.Error("New builds its company before it holds a secret snapshot, so the company " +
			"resolves its references from the environment alone")
	}
	if restore, equip := firstCall(fn, "restoreBundles"), firstCall(fn, "equip"); restore < equip {
		t.Error("New restores the bundles before equip, a step that can still fail the boot, " +
			"so a boot that fails there leaves a login behind")
	}
	if refresh := firstCall(fn, "refreshSecrets"); refresh != token.NoPos {
		t.Error("New takes its snapshot through refreshSecrets, which keeps the previous " +
			"snapshot on a failed read — and a booting node has none, so it falls back to the " +
			"environment; take it through bootSecrets")
	}
}
