package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/secrets"
)

// WHAT A PROVISIONING PASS SEALS HAS TO REACH THE WIRING THAT WAS ALREADY
// BUILT, and nothing about sealing a credential does that on its own.
//
// `${VAR}` resolves from a snapshot taken at apply time. A pass that mints a
// seat's tracker token therefore lands in a company where every consumer of
// that token — the seat identities, the parsers, the transports, the provider
// clients, the MCP children — already resolved it, to nothing, minutes ago.
// Refreshing the snapshot fixes the NEXT read and not one of those, and there
// is no next read: every vendor reconciler is called from the apply and from
// nowhere else.
//
// Measured on a fresh single-node install, which is what these cases are
// built out of: connecting Atlassian created the agent's service account and
// sealed its API token, the reconcile reported the surface ready from its own
// independent check, and Jira's live routing held seat_identities=0 for ever.
// Every issue naming that agent fell through to its project lead while the
// dashboard said the integration was fine.

// jiraInstance is a tracker that answers /myself for exactly one credential.
//
// The credential is the whole point: a seat's account is not declared
// anywhere, it is whatever the seat's OWN token authenticates as, so a lookup
// made with the wrong token (or with none) resolves to nobody rather than to
// the wrong person. Answering 401 for anything else is what makes the case
// below turn on the token reaching the lookup, rather than on the lookup
// happening at all.
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

// sealingEngine is a node with a fleet secret store, a keyring to seal with,
// and nothing resolved yet — a company whose Jira block points every seat at
// a variable that does not exist, which is the state a connect dialog leaves
// behind the instant before its first pass runs.
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
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
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
// The apply here is the two calls [Engine.applyEpoch] makes for this surface —
// [Engine.refreshParties] and [Engine.startJira] — behind the one thing that
// triggers them in production, the activation pointer moving. That is what
// [ConfigWriter.Reload] does, and wiring it to the same two calls is what
// makes this case the field report rather than a mock of it: the pass seals a
// credential, and the question is whether anything re-resolves.
//
// It fails without [Engine.rebuildForSealedSecrets]: the snapshot carries the
// token, the registry carries nothing, and no further gesture from the
// operator is coming.
func TestASealedSeatTokenReachesTheTrackerWithoutAConfigChange(t *testing.T) {
	const token = "seat-token-minted-by-the-pass"
	instance := jiraInstance(t, token, "agent-ceo")
	e, company := sealingEngine(t, instance.URL)

	var applies int
	apply := func(ctx context.Context) error {
		applies++
		e.refreshParties(company)
		if _, err := e.startJira(ctx, company, company.Config.Integrations.Jira); err != nil {
			return err
		}
		return nil
	}
	e.UseConfigWriter(&reloadingWriter{reload: apply})

	// THE APPLY THAT CAME FIRST. A company connects its tracker before any
	// seat has an account on it — that is what connecting is for — so the
	// wiring this node is running was built against a variable that
	// resolved to nothing.
	if err := apply(t.Context()); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if _, ok := e.Registry().ByExternalID(jira.Backend, "agent-ceo"); ok {
		t.Fatal("precondition: the seat resolved before its token was ever minted")
	}

	sink, err := e.SetupSink("founder@example.com")
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
	if applies < 2 {
		t.Errorf("applies = %d: sealing a credential rebuilt nothing, so "+
			"everything holding the old value still holds it", applies)
	}
	party, ok := e.Registry().ByExternalID(jira.Backend, "agent-ceo")
	if !ok {
		t.Fatal("the tracker still routes this account to nobody, so every " +
			"issue naming the agent falls through to its project lead")
	}
	if party.Handle != "ceo" {
		t.Errorf("account agent-ceo resolved to %q, want ceo", party.Handle)
	}

	// AND THE REVISION IS THEIRS. Reload writes a NEW revision rather than
	// re-pointing the old one so that "the credentials were reloaded at
	// 04:12" is a fact somebody can find later — which a revision credited
	// to a constant answers only half of. This pass ran because a person
	// pressed Connect, and the sink is already carrying their name.
	writer, _ := e.configWriterOrNil().(*reloadingWriter)
	if writer == nil {
		t.Fatal("the config surface is not the one this test installed")
	}
	if writer.operator != "founder@example.com" {
		t.Errorf("the rebuild was credited to %q, want the operator the pass "+
			"ran for", writer.operator)
	}
}

// A PASS THAT SEALED NOTHING RE-ACTIVATES NOTHING.
//
// This is the whole of why the rebuild is safe to do automatically. An apply
// marks the reconcile loop stale, which brings the next pass forward — so a
// pass that reloaded unconditionally would apply, wake itself, apply again,
// for the life of the deployment. What stops it is that `sealed` is set by an
// actual Record and by nothing else, and that every reconciler is certified
// against integrationtest's "a converged pass writes nothing".
func TestAPassThatSealedNothingDoesNotReactivateTheRevision(t *testing.T) {
	e, _ := sealingEngine(t, "https://jira.example.com")
	w := &reloadingWriter{}
	e.UseConfigWriter(w)

	sink, err := e.SetupSink("test-operator")
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if w.calls != 0 {
		t.Errorf("Reload calls = %d: a converged pass re-activated the "+
			"revision, which wakes the loop that runs the next pass", w.calls)
	}
}

// A FLUSH THAT FAILED STILL REBUILDS, and a second one does not rebuild
// again.
//
// The sink is write-through: by the time Flush is reached every value it
// recorded is already durable in the fleet's store, so what a failing Flush
// reports is an incomplete COMPLETION over credentials that are out there
// either way. Leaving them unpublished would be the field report again, with
// an error log to explain it.
func TestAFailedFlushStillRebuildsAndDoesNotRebuildTwice(t *testing.T) {
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
	if w.calls != 1 {
		t.Fatalf("Reload calls = %d after a failed flush, want 1", w.calls)
	}
	if err := sink.Flush(t.Context()); err == nil {
		t.Fatal("precondition: this sink's Flush must fail")
	}
	if w.calls != 1 {
		t.Errorf("Reload calls = %d after a second flush, want 1", w.calls)
	}
}

// THE REBUILD OUTLIVES THE PASS THAT EARNED IT, AND IS STILL BOUNDED.
//
// This runs at the tail of a provisioning pass, on that pass's own context —
// which carries the pass's deadline, may have all but spent it, and is
// cancelled the moment the pass returns. The credentials are durable by then,
// so abandoning the re-activation because the work that produced them finished
// is exactly how the stale wiring survives: sealed, resolvable, and read by
// nothing that is running.
//
// Detached is not unbounded, though. A re-activation against an unreachable
// control plane must end, or a pass leaks a goroutine per run.
func TestTheRebuildSurvivesTheCancelledPassAndStaysBounded(t *testing.T) {
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

	sink, err := e.SetupSink("test-operator")
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

	if w.calls != 1 {
		t.Errorf("Reload calls = %d, want 1", w.calls)
	}
	if w.err != nil {
		t.Error(w.err)
	}
}

// A NODE WITH NO CONFIG SURFACE DOES NOT DIE ON THE REBUILD IT CANNOT DO.
//
// A worker-only node, or any node in the few hundred milliseconds between the
// engine being constructed and the API installing the surface. The values are
// durable and the snapshot is refreshed; what is missing is the rebuild, and
// the operator is told which gesture supplies it.
func TestSealingOnANodeWithNoConfigSurfaceIsLoggedRatherThanFatal(t *testing.T) {
	e, _ := sealingEngine(t, "https://jira.example.com")
	if e.configWriterOrNil() != nil {
		t.Fatal("precondition: this node must have no config surface")
	}

	sink, err := e.SetupSink("test-operator")
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
	calls  int

	// operator is who the last re-activation was credited to.
	operator string

	// err is what the reload reported. The engine LOGS a failed
	// re-activation rather than returning it — nothing above Flush can act
	// on one — so a case that asserts something about the call itself has
	// to read it back from here or assert nothing at all.
	err error
}

func (w *reloadingWriter) Reload(ctx context.Context, summary, operator string) error {
	w.calls++
	w.operator = operator
	// The summary is what an operator reads on the revision this gesture
	// creates, beside the ones a person wrote. A blank one is a revision
	// nobody can account for.
	if strings.TrimSpace(summary) == "" {
		return fmt.Errorf("the re-activation carried no summary")
	}
	if w.reload == nil {
		return nil
	}
	w.err = w.reload(ctx)
	return w.err
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
func (*failingFlushSink) Flush(context.Context) error {
	return fmt.Errorf("the run could not be completed")
}
func (*failingFlushSink) Describe() string { return "a test sink" }
func (*failingFlushSink) NextStep() string { return "nothing" }

// A CREDENTIAL A PASS SEALED IS ONE EVERY SURFACE CAN SEE.
//
// `${VAR}` resolves from a snapshot taken at apply time, which keeps the
// secret store off the path of every config read, and nothing about minting a
// credential advances an epoch. So a pass created a GitLab service account,
// minted its token and sealed it under the name the seat pointed at, and
// every surface went on reporting that seat as waiting for an account: the
// resolver was still holding the snapshot from before the seal. The pass then
// ran again on the next tick, found the same unresolved variable, and minted
// a second token, for ever.
//
// THIS IS THE FIRST OF TWO HALVES and was shipped as though it were both.
// Making the value resolvable fixes every read that happens FROM NOW ON, and
// the wiring a company actually routes through is not one of those: it was
// built at the last apply out of the snapshot standing then. See
// [TestASealedSeatTokenReachesTheTrackerWithoutAConfigChange] for the half
// that was missing, and what it cost in production.
func TestASealedCredentialIsVisibleWithoutAnApply(t *testing.T) {
	e, _ := engineWithSecrets(t)
	// THE FLEET'S STORE, which is where a sealed credential goes and where
	// the resolver's snapshot is read from.
	e.backends.Fleet = coordmem.NewFleet()

	sink, err := e.SetupSink("test")
	if err != nil {
		// NOT A SKIP. Nothing here is environmental: the fixture always
		// opens a temp-dir store and always installs a cipher, so a sink
		// that cannot be built is a regression in the code under test —
		// and skipping turned the whole mint-every-tick guard green.
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
	e, sv := engineWithSecrets(t)
	e.backends.Fleet = coordmem.NewFleet()
	sink, err := e.SetupSink("test")
	if err != nil {
		// NOT A SKIP. Nothing here is environmental: the fixture always
		// opens a temp-dir store and always installs a cipher, so a sink
		// that cannot be built is a regression in the code under test —
		// and skipping turned the whole mint-every-tick guard green.
		t.Fatalf("SetupSink: %v", err)
	}

	// Written behind the resolver's back, so a rebuild is observable.
	if err := sv.Set(t.Context(), "UNSEEN", "value", "op", "cli", time.Now().UTC()); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := e.resolver().Value("${UNSEEN}"); got != "" {
		t.Errorf("a read-only pass rebuilt the snapshot: ${UNSEEN} = %q", got)
	}
}
