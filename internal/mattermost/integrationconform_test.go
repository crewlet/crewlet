package mattermost_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
)

// chatReconciler drives the REAL pass through the [integration.Reconciler]
// seam, shaped exactly like the engine's own adapter.
//
// The adapter lives here rather than in the engine because that is the only
// place a converged WORLD can be stood up: engine.mattermostPass on a bare
// Engine has no running transport or coordination store, so every clause
// below would pass over a reconciler nothing ever reached, which is the exact
// shape this suite was written to replace.
type chatReconciler struct {
	client *mattermost.Client
	cfg    *config.Mattermost
	org    *org.Organization
	// managed is the bots the access sync's ledger records, by handle.
	managed map[string]string
}

func (*chatReconciler) Kind() integration.Kind { return integration.KindMattermost }

func (r *chatReconciler) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := r.pass(ctx)
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

// pass is one reconcile of the company's half, with the Result the suite's
// seam throws away. Each agent's own bot is the per-seat access sync's, which
// has tests of its own against the same instance.
func (r *chatReconciler) pass(ctx context.Context) (*mattermost.Result, error) {
	return mattermost.Reconcile(ctx, mattermost.Options{
		Client: r.client, Config: r.cfg, Org: r.org, Managed: r.managed,
	})
}

// chatWorld is one instance and the reconciler over it.
type chatWorld struct {
	*chatReconciler
	srv *chatServer
}

// writes is every write this world's instance has received. The company's
// pass seals nothing, so the instance is the whole of what it can change.
func (w *chatWorld) writes() int { return w.srv.mutations() }

// theSeat is the one seat every world here runs, lowercased as its bot is
// named.
const theSeat = "ceo"

// convergedChat stands up a world a real pass has already converged.
//
// CONVERGED BY A REAL RUN rather than by hand-seeding the fixture, which is
// the only way to be sure it is the state this pass actually leaves behind:
// the seat's bot is seeded under a STALE display name, as one the access sync
// created before the role was renamed, and the converging pass renames it.
// The clause that matters, a second pass writes nothing, is then answered
// about the world this pass leaves.
func convergedChat(t *testing.T, tb integrationtest.TB) *chatWorld {
	t.Helper()
	tb.Helper()
	return convergedChatOn(t, tb, nil)
}

// convergedChatOn is the same, over an instance a caller has tuned first.
//
// The tuning exists for ONE caller, the test that proves the guard below can
// actually fire: the only way to reach a world the pass believes it converged
// and did not is an instance that answers a write with success and records
// nothing.
func convergedChatOn(t *testing.T, tb integrationtest.TB, tune func(*chatServer)) *chatWorld {
	t.Helper()
	tb.Helper()
	srv := newChatServer()
	if tune != nil {
		tune(srv)
	}
	cfg := enabledChat()
	srv.mu.Lock()
	id := "bot-" + theSeat
	srv.bots[mattermost.BotUsername(cfg.Provisioning, theSeat)] = id
	srv.names[id] = "Chief Executive" + cfg.Provisioning.DisplayNameSuffix
	srv.mu.Unlock()
	w := &chatWorld{
		chatReconciler: &chatReconciler{
			// The OUTER t, because integrationtest.TB has no Cleanup:
			// the suite calls this once per case, so the servers stand
			// until the test function returns.
			client: chatClient(t, srv),
			cfg:    cfg,
			org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{
				chatSeat("CEO", ""),
			}},
			managed: map[string]string{theSeat: id},
		},
		srv: srv,
	}
	if _, err := w.pass(context.Background()); err != nil {
		tb.Fatalf("converging the world: %v", err)
	}
	if bad := w.vacuous(); len(bad) > 0 {
		tb.Fatalf("the converging pass left a world that is not converged, so "+
			"every clause below would certify nothing:\n  %s",
			strings.Join(bad, "\n  "))
	}
	return w
}

// outstandingChat is the same world after the instance was locked down.
//
// # What is wrong, and why a person owes it
//
// A Mattermost administrator tightened two things after this company was
// provisioned: the account whose token this engine provisions with lost its
// system_admin role, and bot account creation was switched off in the System
// Console. Nothing the engine can do fixes either, and neither shows up until
// the day a seat is turned on, because a pass over a converged company writes
// nothing and so is never refused at all. That window is the whole reason
// this is reported rather than discovered.
//
// # Why it is not quietly converged
//
// The suite has a clause for exactly that ("an outstanding world actually
// reports something"), and [TestTheOutstandingWorldIsBlockedOnAPerson] pins
// the specific shape rather than the mere count.
func outstandingChat(t *testing.T, tb integrationtest.TB) *chatWorld {
	t.Helper()
	tb.Helper()
	w := convergedChat(t, tb)
	w.srv.lockDown()
	return w
}

// chatContract is the harness the suite is driven through, built once here so
// [TestTheWriteCounterSurvivesBeingSampledEarly] can hold the same closures
// the suite holds rather than a second copy of them.
func chatContract(t *testing.T) integrationtest.Reconciler {
	t.Helper()
	// EVERY CONVERGED WORLD THE SUITE BUILDS, summed.
	//
	// Mutations is documented as sampled only around a pass over the
	// CONVERGED world, as a delta — but this closure is the harness's, and
	// a closure that reads "whichever world was built last" is a nil
	// dereference until the first build and answers for the wrong estate
	// afterwards. Summing every converged world can never be nil, and it
	// fails in the SAFE DIRECTION: a clause that sampled before building
	// would see the new world's own converging writes land inside its
	// delta and go red, where a counter following the latest world would
	// answer zero-to-zero and pass over whatever the pass did.
	//
	// The outstanding world is deliberately not in the sum. It is ALLOWED
	// to write — it has work to do — and folding it in would make a
	// converged pass's delta depend on how many outstanding worlds
	// happened to be built between the two samples.
	var converged []*chatWorld
	return integrationtest.Reconciler{
		Converged: func(tb integrationtest.TB) integration.Reconciler {
			w := convergedChat(t, tb)
			converged = append(converged, w)
			return w
		},
		Outstanding: func(tb integrationtest.TB) integration.Reconciler {
			return outstandingChat(t, tb)
		},
		Mutations: func() int {
			n := 0
			for _, w := range converged {
				n += w.writes()
			}
			return n
		},
	}
}

// THE CONTRACT BINDS HERE, over a real Mattermost pass and a world it has
// itself converged.
func TestTheMattermostReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	integrationtest.Run(t, chatContract(t))
}

// THE WRITE COUNTER ANSWERS BEFORE A WORLD EXISTS, AND LOUDLY AFTER ONE IS
// BUILT.
//
// Today every clause samples Mutations only after Converged, inside one
// sequential case — so a counter that followed the latest world would work,
// right up until a clause sampled it first, and then it would PANIC on a nil
// world rather than fail a test. A panic takes the whole test binary with it
// and names nothing.
//
// The two halves are one property. A cold read must answer rather than
// explode, and it must not answer in a way that makes the delta across a
// world's construction look like zero: that is the direction that quietly
// certifies a pass nobody counted, which is the one thing this counter
// exists to prevent.
func TestTheWriteCounterSurvivesBeingSampledEarly(t *testing.T) {
	t.Parallel()
	r := chatContract(t)

	cold := r.Mutations()
	rec := r.Converged(t)
	warm := r.Mutations()
	if warm <= cold {
		t.Fatalf("Mutations went %d -> %d across building a world whose "+
			"converging pass renames a bot; a clause sampling in this order "+
			"would read that pass as free", cold, warm)
	}
	// And the world it built is usable, so the early sample cost nothing.
	if _, err := rec.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if after := r.Mutations(); after != warm {
		t.Errorf("a converged pass made %d write(s)", after-warm)
	}
}

// THE COUNTER SEES A WRITE AT THE INSTANCE.
//
// The counter is invisible on a converged world, which is the whole point of
// the world, so it is not pinned by the clause that reads it. This drives the
// cheapest write the pass makes on its own: a display name the company
// document changed under it, which costs one PUT.
func TestTheWriteCounterSeesAWriteAtTheInstance(t *testing.T) {
	t.Parallel()
	w := convergedChat(t, t)
	before := w.writes()

	// The suffix rather than the role name: the handle is derived from the
	// name, so renaming the role would name a SECOND seat instead.
	w.cfg.Provisioning.DisplayNameSuffix = " (robot)"
	if _, err := w.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := w.writes() - before; got != 1 {
		t.Fatalf("the counter moved by %d over a pass that renamed one bot; "+
			"a counter blind to the instance reads every Mattermost write as "+
			"free", got)
	}
}

// THE OUTSTANDING WORLD IS BLOCKED ON A PERSON, and on these exact things.
//
// The suite's anti-vacuity clause asks only for one finding somebody owes,
// which a world could satisfy by accident — and four clauses read this
// world, so "it reported something" is a weaker claim than it looks. This
// says what, about which setting, who owes it, and that everything else
// converged: a pass that started failing for an unrelated reason would still
// report a person-owed finding and still pass the suite.
func TestTheOutstandingWorldIsBlockedOnAPerson(t *testing.T) {
	t.Parallel()
	w := outstandingChat(t, t)
	findings, err := w.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("the outstanding pass faulted rather than reporting: %v", err)
	}
	want := []integration.Finding{{
		Kind:    integration.FindingCredentialRejected,
		Subject: "integrations.mattermost.provisioning.admin_token",
	}, {
		Kind:    integration.FindingApprovalRequired,
		Subject: "ServiceSettings.EnableBotAccountCreation",
	}}
	if len(findings) != len(want) {
		t.Fatalf("findings = %+v, want the locked-down instance's two", findings)
	}
	for i, got := range findings {
		if got.Kind != want[i].Kind || got.Subject != want[i].Subject {
			t.Errorf("finding %d = %s on %q, want %s on %q", i,
				got.Kind, got.Subject, want[i].Kind, want[i].Subject)
		}
		if _, actor := got.Kind.Verdict(); !actor.WaitsOnAPerson() {
			t.Errorf("%s is owed by %s, so nobody is being asked to act",
				got.Kind, actor)
		}
		if strings.TrimSpace(got.Detail) == "" {
			t.Errorf("%s says nothing about what to do", got.Kind)
		}
	}
	// AND THE REST OF THE PASS WORKED, which is what makes this an
	// outstanding world rather than a broken one: the seat's bot carries
	// its name, so the only things missing really are the two a person
	// owes.
	if bad := w.vacuous(); len(bad) > 0 {
		t.Errorf("the outstanding world is failing for a second reason, so the "+
			"clauses that read it are certifying that one:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// vacuous names what is NOT converged about this world, and is what stands
// between this file and the shape it replaced: a suite driven at a
// Mattermost nothing was ever done to, where "a converged pass writes
// nothing" holds because there is nothing to write about.
//
// IT READS THE WORLD, NOT THE RESULT. A Result says what one pass did; these
// clauses are about what the NEXT pass will find, and the two are the same
// only while the fixture is honest.
//
// COMPLAINTS RATHER THAN Fatalf, so [TestTheHarnessRefusesAConvergedWorldThatIsNotOne]
// can break one thing at a time and read what fired.
func (w *chatWorld) vacuous() []string {
	var bad []string
	username := mattermost.BotUsername(w.cfg.Provisioning, theSeat)
	id := w.srv.botID(username)
	if id == "" {
		return append(bad, fmt.Sprintf(
			"no bot account %q on the instance, so there is no converged "+
				"state to re-run against", username))
	}
	w.srv.mu.Lock()
	name := w.srv.names[id]
	w.srv.mu.Unlock()
	if want := mattermost.BotDisplayName(w.cfg.Provisioning, "CEO"); name != want {
		bad = append(bad, fmt.Sprintf(
			"%s is named %q rather than %q, so the next pass renames it and a "+
				"converged pass would write after all", username, name, want))
	}
	return bad
}

// THE HARNESS REFUSES A CONVERGED WORLD THAT IS NOT ONE.
func TestTheHarnessRefusesAConvergedWorldThatIsNotOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		broken func(t *testing.T, w *chatWorld)
		want   string
	}{{
		name:   "the instance holds no bot for the seat",
		broken: func(_ *testing.T, w *chatWorld) { w.srv.forgetBots() },
		want:   "no bot account",
	}, {
		name: "the bot carries a stale name",
		broken: func(t *testing.T, w *chatWorld) {
			id := w.botID(t)
			w.srv.mu.Lock()
			w.srv.names[id] = "Founder"
			w.srv.mu.Unlock()
		},
		want: "the next pass renames it",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := convergedChat(t, t)
			if bad := w.vacuous(); len(bad) > 0 {
				t.Fatalf("a freshly converged world already complains: %v", bad)
			}
			tc.broken(t, w)
			bad := strings.Join(w.vacuous(), "\n")
			if !strings.Contains(bad, tc.want) {
				t.Errorf("the guard did not name %q; it said:\n%s", tc.want, bad)
			}
		})
	}
}

// recordingTB is an [integrationtest.TB] that remembers instead of failing.
//
// [convergedChat] refuses a world that is not converged by calling Fatalf,
// and a Fatalf on a real *testing.T fails the test doing the proving. So the
// one test that has to watch the refusal happen hands it this instead —
// Fatalf returns here, which is safe because the caller does nothing with the
// world afterwards.
type recordingTB struct{ said []string }

func (*recordingTB) Helper() {}
func (r *recordingTB) Errorf(format string, args ...any) {
	r.said = append(r.said, fmt.Sprintf(format, args...))
}
func (r *recordingTB) Fatalf(format string, args ...any) { r.Errorf(format, args...) }

// THE REFUSAL IS REACHED, not merely written.
//
// [TestTheHarnessRefusesAConvergedWorldThatIsNotOne] proves the guard can
// fire; it calls the method directly, so it says nothing about whether
// anything CALLS it. The world here is one the pass believes it converged:
// the instance answers the rename with success and records nothing, so the
// Result says the bot was renamed and the instance still holds the old name.
func TestTheHarnessRefusesToCertifyAWorldItOnlyThinksItConverged(t *testing.T) {
	t.Parallel()
	tb := &recordingTB{}
	w := convergedChatOn(t, tb, func(s *chatServer) { s.swallowRenames = true })

	said := strings.Join(tb.said, "\n")
	if said == "" {
		t.Fatalf("%s", "the harness certified a world whose bot still carries "+
			"its old name, so nothing calls the guard that would have said so")
	}
	if !strings.Contains(said, "not converged") || !strings.Contains(said, "renames it") {
		t.Errorf("the refusal does not say what it refused: %q", said)
	}
	// The Result really did claim otherwise, which is why reading the
	// instance rather than the Result is the whole of this guard.
	res, err := w.pass(t.Context())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(res.Renamed) != 1 {
		t.Errorf("the pass reported %v, so this case is no longer the one "+
			"where a Result and an instance disagree", res.Renamed)
	}
}

// botID is the seat's account on the instance, for a test that breaks it.
func (w *chatWorld) botID(t *testing.T) string {
	t.Helper()
	id := w.srv.botID(mattermost.BotUsername(w.cfg.Provisioning, theSeat))
	if id == "" {
		t.Fatalf("the converged world has no bot account to break")
	}
	return id
}
