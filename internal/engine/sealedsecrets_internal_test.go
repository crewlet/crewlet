package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/secrets"
)

// founderParty is a person who pressed Connect through their own machine
// token, so every part of what a rebuild is credited to is distinct; and
// testParty is the party a case that is not about attribution runs a pass as.
var (
	founderParty = iam.Actor{Name: "founder", Kind: iam.ActorHuman,
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}
	testParty = iam.Actor{Name: "test-operator", Kind: iam.ActorOperator,
		OperatorID: "token:test-operator"}
	testAuthor = secrets.Author{Name: testParty.Name, Kind: string(testParty.Kind),
		OperatorID: testParty.OperatorID}
)

// WHAT A PROVISIONING PASS SEALS HAS TO REACH THE WIRING ALREADY BUILT, and
// sealing alone does not: `${VAR}` resolves from a snapshot taken at apply
// time, so every consumer of a minted seat token (seat identities, parsers,
// transports, provider clients, MCP children) resolved it to nothing at the
// last apply. Refreshing the snapshot fixes only the next read, and every
// vendor reconciler runs only from an apply, so the tracker would route the
// agent's issues to its project lead while reporting itself ready.

// jiraInstance is a tracker that answers /myself for exactly one credential.
//
// A seat's account is whatever its OWN token authenticates as, so answering
// 401 to any other makes the case below turn on the token reaching the
// lookup, not on the lookup happening at all.
func jiraInstance(t *testing.T, token, account string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/2/myself" {
			http.Error(w, "no such route", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"name":%q,"displayName":"Agent"}`, account)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sealingEngine is a node with a fleet secret store and a keyring, and a
// company whose Jira block points every seat at a variable not yet set: the
// state a connect dialog leaves just before its first pass.
func sealingEngine(t *testing.T, instance string) (*Engine, *Company) {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1",
		Keys:     map[string][]byte{"k1": []byte("crewlet-test-sealing-key-32bytes")},
	})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	e := &Engine{backends: &Backends{Fleet: coordmem.NewFleet()}, cipher: cipher}

	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  jira:
    url: ` + instance + `
    token: "${JIRA_ORG_TOKEN}"
    webhook_secret: "whsec_Y3Jld2xldC10ZXN0LWppcmEtd2ViaG9vay1rZXktMzI="
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      jira: {JIRA_API_TOKEN: "${JIRA_AGENT_TOKEN}"}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	return e, company
}

// A SEALED SEAT TOKEN REACHES THE TRACKER'S ROUTING WITH NO FURTHER CONFIG
// CHANGE.
//
// The apply is the two calls [Engine.applyEpoch] makes for this surface,
// [Engine.refreshParties] and [Engine.startJira], wired to
// [ConfigWriter.Reload], which moves the activation pointer that triggers them
// in production. It fails without [Engine.rebuildForSealedSecrets]: the
// snapshot carries the token, the registry nothing, and no further operator
// gesture is coming.
func TestASealedSeatTokenReachesTheTrackerWithoutAConfigChange(t *testing.T) {
	t.Parallel()
	const token = "seat-token-minted-by-the-pass"
	instance := jiraInstance(t, token, "agent-ceo")
	e, company := sealingEngine(t, instance.URL)

	// GUARDED AND AWAITED: the re-activation runs off the asking pass's
	// goroutine (see [reloadingWriter]'s mu).
	var appliesMu sync.Mutex
	var applies int
	countApplies := func() int {
		appliesMu.Lock()
		defer appliesMu.Unlock()
		return applies
	}
	apply := func(ctx context.Context) error {
		e.refreshParties(ctx, company)
		if _, err := e.startJira(ctx, company, company.Config.Integrations.Jira); err != nil {
			return err
		}
		// COUNTED LAST, so a count of two means two applies that FINISHED
		// and the registry read below does not race them.
		appliesMu.Lock()
		applies++
		appliesMu.Unlock()
		return nil
	}
	e.UseConfigWriter(&reloadingWriter{reload: apply})

	// THE APPLY THAT CAME FIRST: a company connects its tracker before any
	// seat has an account on it, so the running wiring resolved the
	// variable to nothing.
	if err := apply(t.Context()); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if _, ok := e.Registry().ByExternalID(jira.Backend, "agent-ceo"); ok {
		t.Fatal("precondition: the seat resolved before its token was ever minted")
	}

	sink, err := e.SetupSink(founderParty)
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	if err := sink.Record(t.Context(), "JIRA_AGENT_TOKEN", token); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if got := e.Resolve("${JIRA_AGENT_TOKEN}"); got != token {
		t.Fatalf("the snapshot did not pick the sealed token up: %q", got)
	}
	for deadline := time.Now().Add(2 * time.Second); countApplies() < 2; {
		if time.Now().After(deadline) {
			t.Fatalf("applies = %d: sealing a credential rebuilt nothing, so "+
				"everything holding the old value still holds it", countApplies())
		}
		time.Sleep(time.Millisecond)
	}
	party, ok := e.Registry().ByExternalID(jira.Backend, "agent-ceo")
	if !ok {
		t.Fatal("the tracker still routes this account to nobody, so every " +
			"issue naming the agent falls through to its project lead")
	}
	if party.Handle != "ceo" {
		t.Errorf("account agent-ceo resolved to %q, want ceo", party.Handle)
	}

	// AND THE REVISION IS THEIRS: Reload writes a new revision so the
	// reload can be found later, credited to the person who pressed
	// Connect, whose name the sink carries.
	writer, _ := e.configWriterOrNil().(*reloadingWriter)
	if writer == nil {
		t.Fatal("the config surface is not the one this test installed")
	}
	// WHOLE: the person, their kind and the credential they pressed
	// Connect through.
	if got := writer.lastOperator(); got != founderParty {
		t.Errorf("the rebuild was credited to %+v, want the party the pass "+
			"ran for, %+v", got, founderParty)
	}
}

// A PASS THAT SEALED NOTHING RE-ACTIVATES NOTHING, which is what makes the
// automatic rebuild safe: an apply brings the next pass forward, so an
// unconditional reload would loop for ever. `sealed` is set by an actual
// Record alone, and every reconciler is certified against integrationtest's
// "a converged pass writes nothing".
func TestAPassThatSealedNothingDoesNotReactivateTheRevision(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	w := &reloadingWriter{}
	e.UseConfigWriter(w)

	sink, err := e.SetupSink(testParty)
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// SETTLED BEFORE THE ABSENCE IS CLAIMED: the re-activation is
	// asynchronous, so a read straight after Flush would pass either way.
	time.Sleep(50 * time.Millisecond)
	if got := w.reloads(); got != 0 {
		t.Errorf("Reload calls = %d: a converged pass re-activated the "+
			"revision, which wakes the loop that runs the next pass", got)
	}
}

// A FLUSH THAT FAILED STILL REBUILDS, and a second one does not rebuild
// again. The sink is write-through, so every recorded value is durable
// whatever Flush reports, and leaving it unpublished strands it.
func TestAFailedFlushStillRebuildsAndDoesNotRebuildTwice(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	w := &reloadingWriter{}
	e.UseConfigWriter(w)

	sink := &refreshingSink{TokenSink: &failingFlushSink{}, engine: e}
	if err := sink.Record(t.Context(), "JIRA_AGENT_TOKEN", "value"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := sink.Flush(t.Context()); err == nil {
		t.Fatal("precondition: this sink's Flush must fail")
	}
	w.awaitReloads(t, 1)
	if err := sink.Flush(t.Context()); err == nil {
		t.Fatal("precondition: this sink's Flush must fail")
	}
	time.Sleep(50 * time.Millisecond)
	if got := w.reloads(); got != 1 {
		t.Errorf("Reload calls = %d after a second flush, want 1", got)
	}
}

// THE REBUILD OUTLIVES THE PASS THAT EARNED IT, AND IS STILL BOUNDED.
//
// It runs at a pass's tail, on a context that may be spent or cancelled as the
// pass returns; the credentials are durable by then, so abandoning the
// re-activation leaves the stale wiring running. Detached is not unbounded: a
// re-activation against an unreachable control plane must end, or each pass
// leaks a goroutine.
func TestTheRebuildSurvivesTheCancelledPassAndStaysBounded(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	w := &reloadingWriter{reload: func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("the rebuild inherited the pass's cancellation: %w", err)
		}
		if _, ok := ctx.Deadline(); !ok {
			return fmt.Errorf("the rebuild carried no deadline")
		}
		return nil
	}}
	e.UseConfigWriter(w)

	sink, err := e.SetupSink(testParty)
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	if err := sink.Record(t.Context(), "JIRA_AGENT_TOKEN", "value"); err != nil {
		t.Fatalf("record: %v", err)
	}

	// The pass ends where its context dies, which is BEFORE Flush returns:
	// a run whose deadline expired still sealed everything it recorded.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	w.awaitReloads(t, 1)
	if err := w.failure(); err != nil {
		t.Error(err)
	}
}

// A NODE WITH NO CONFIG SURFACE DOES NOT DIE ON THE REBUILD IT CANNOT DO: a
// worker-only node, or one before the API installs the surface. The values
// are durable and the snapshot refreshed; the operator is told which gesture
// supplies the rebuild.
func TestSealingOnANodeWithNoConfigSurfaceIsLoggedRatherThanFatal(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	if e.configWriterOrNil() != nil {
		t.Fatal("precondition: this node must have no config surface")
	}

	sink, err := e.SetupSink(testParty)
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	if err := sink.Record(t.Context(), "JIRA_AGENT_TOKEN", "value"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := e.Resolve("${JIRA_AGENT_TOKEN}"); got != "value" {
		t.Errorf("the snapshot did not pick the sealed value up: %q", got)
	}
}

// reloadingWriter counts re-activations and runs whatever an apply would.
type reloadingWriter struct {
	noopWriter
	reload func(context.Context) error

	// mu guards everything below: the re-activation runs off the asking
	// goroutine, since the asker is a pass inside the surface lease and the
	// minute [setup.PassDeadline] leaves inside [setup.LeaseTTL] is that
	// pass's own status write ([republisher.request]).
	mu    sync.Mutex
	calls int

	// operator is who the last re-activation was credited to, written on a
	// timer's goroutine: read it only through [reloadingWriter.lastOperator].
	operator iam.Actor

	// err is what the reload reported: the engine logs a failed
	// re-activation rather than returning it, so a case reads it back here.
	err error
}

// reloads is how many re-activations this writer has served, read under the
// lock. [reloadingWriter.awaitReloads] is how a case waits for one.
func (w *reloadingWriter) reloads() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// awaitReloads blocks until this writer has served n re-activations.
//
// A poll rather than a channel: the cases also assert that a second flush
// does NOT re-activate, an absence that needs a settle either way.
func (w *reloadingWriter) awaitReloads(t *testing.T, n int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if w.reloads() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%d re-activation(s) served, want %d", w.reloads(), n)
}

// lastOperator is who the most recent re-activation was credited to.
//
// Under the lock: the re-activation runs on a timer's goroutine, so reading
// [reloadingWriter.operator] directly races the write.
func (w *reloadingWriter) lastOperator() iam.Actor {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.operator
}

// failure is what the reload reported, read under the lock.
func (w *reloadingWriter) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *reloadingWriter) Reload(ctx context.Context, summary string, operator iam.Actor) error {
	w.mu.Lock()
	w.calls++
	w.operator = operator
	w.mu.Unlock()
	// The summary is what an operator reads on the revision this creates;
	// a blank one is a revision nobody can account for.
	if strings.TrimSpace(summary) == "" {
		return fmt.Errorf("the re-activation carried no summary")
	}
	if w.reload == nil {
		return nil
	}
	err := w.reload(ctx)
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
	return err
}

// failingFlushSink records durably and fails to complete.
type failingFlushSink struct{ recorded map[string]string }

func (s *failingFlushSink) Record(_ context.Context, name, value string) error {
	if s.recorded == nil {
		s.recorded = map[string]string{}
	}
	s.recorded[name] = value
	return nil
}

func (s *failingFlushSink) Value(_ context.Context, name string) (string, bool, error) {
	v, ok := s.recorded[name]
	return v, ok, nil
}

func (*failingFlushSink) Discard(context.Context) error { return nil }

// Forget implements [provision.TokenSink].
func (*failingFlushSink) Forget(context.Context, ...string) error { return nil }
func (*failingFlushSink) Flush(context.Context) error {
	return fmt.Errorf("the run could not be completed")
}
func (*failingFlushSink) Describe() string { return "a test sink" }
func (*failingFlushSink) NextStep() string { return "nothing" }

// A CREDENTIAL A PASS SEALED IS ONE EVERY SURFACE CAN SEE.
//
// `${VAR}` resolves from a snapshot taken at apply time, and minting a
// credential advances no epoch, so without a refresh every surface goes on
// reporting the seat as waiting for an account, and each tick's pass mints
// another token. This is only the read half: wiring built at the last apply
// needs the rebuild [TestASealedSeatTokenReachesTheTrackerWithoutAConfigChange]
// holds.
func TestASealedCredentialIsVisibleWithoutAnApply(t *testing.T) {
	t.Parallel()
	e, _ := engineWithSecrets(t)
	// The fleet's store: where a sealed credential goes and the snapshot
	// is read from.
	e.backends.Fleet = coordmem.NewFleet()

	sink, err := e.SetupSink(testParty)
	if err != nil {
		// NOT A SKIP: the fixture always opens a store and installs a
		// cipher, so a sink that cannot be built is a regression, and a skip
		// would turn the mint-every-tick guard green.
		t.Fatalf("SetupSink: %v", err)
	}
	if got := e.resolver().Value("${SEAT_TOKEN}"); got != "" {
		t.Fatalf("before the pass, ${SEAT_TOKEN} = %q", got)
	}

	if err := sink.Record(t.Context(), "SEAT_TOKEN", "glpat-minted"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// NOT YET, and deliberately: a run that seals five credentials and is
	// then rolled back should leave the snapshot where it was.
	if got := e.resolver().Value("${SEAT_TOKEN}"); got != "" {
		t.Errorf("mid-run, ${SEAT_TOKEN} = %q: a rollback would leave a value "+
			"resolving that no longer exists", got)
	}

	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := e.resolver().Value("${SEAT_TOKEN}"); got != "glpat-minted" {
		t.Fatalf("after the pass, ${SEAT_TOKEN} = %q, so every surface still "+
			"reports this seat as waiting for a credential it already has", got)
	}
}

// A RUN THAT SEALED NOTHING DOES NOT REBUILD THE SNAPSHOT. Most passes read
// and report, and rebuilding on each of those would put the whole secret
// store on the reconcile loop's tick.
func TestAPassThatSealedNothingLeavesTheSnapshotAlone(t *testing.T) {
	t.Parallel()
	e, sv := engineWithSecrets(t)
	e.backends.Fleet = coordmem.NewFleet()
	sink, err := e.SetupSink(testParty)
	if err != nil {
		// NOT A SKIP: the fixture always opens a store and installs a
		// cipher, so a sink that cannot be built is a regression, and a skip
		// would turn the mint-every-tick guard green.
		t.Fatalf("SetupSink: %v", err)
	}

	// Written behind the resolver's back, so a rebuild is observable.
	if err := sv.Set(t.Context(), "UNSEEN", "value", testAuthor, secrets.SourceCLI, time.Now().UTC()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := e.resolver().Value("${UNSEEN}"); got != "" {
		t.Errorf("a read-only pass rebuilt the snapshot: ${UNSEEN} = %q", got)
	}
}

// TWO PASSES SEALING IN ONE BURST RE-ACTIVATE ONCE.
//
// The rebuild goes through [republisher], not straight to
// [Engine.rebuildForSealedSecrets], and that is the whole of the coalescing:
// one dashboard connect writes a request per surface, and each seal is a
// whole-company rebuild and a permanent config revision. An apply also brings
// the reconcile tick forward, so an unconverging pass would seal (and mint) as
// fast as applies complete. The sink's `flushed` flag stops one sink
// rebuilding twice; this is two sinks, which is what a burst is.
func TestASecondPassSealingInTheSameBurstDoesNotRebuildAgain(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	w := &reloadingWriter{}
	e.UseConfigWriter(w)

	for _, name := range []string{"JIRA_AGENT_TOKEN", "CONFLUENCE_AGENT_TOKEN"} {
		// A SINK PER PASS, as SetupSink builds for every request.
		sink, err := e.SetupSink(founderParty)
		if err != nil {
			t.Fatalf("sink: %v", err)
		}
		if err := sink.Record(t.Context(), name, "value"); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
		if err := sink.Flush(t.Context()); err != nil {
			t.Fatalf("flush %s: %v", name, err)
		}
	}

	w.awaitReloads(t, 1)
	// SETTLED, because the claim is that a SECOND one does not happen:
	// the coalescing window is fifteen seconds, so any later one the
	// burst asked for is owed, not immediate.
	time.Sleep(50 * time.Millisecond)
	if got := w.reloads(); got != 1 {
		t.Errorf("re-activations = %d from one burst of two seals, want 1: "+
			"each is a permanent config revision, and an apply brings the "+
			"reconcile tick that seals again forward", got)
	}
}
