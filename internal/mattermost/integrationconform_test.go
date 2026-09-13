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
	"github.com/crewlet/crewlet/internal/provision"
)

// chatReconciler drives the REAL pass through the [integration.Reconciler]
// seam, shaped exactly like the engine's own adapter.
//
// The adapter lives here rather than in the engine because that is the only
// place a converged WORLD can be stood up: engine.mattermostPass on a bare
// Engine falls back to a read-only sink, [provision.CanMint] answers false,
// and [mattermost.Reconcile] returns before its first HTTP request — so
// every clause below would pass over a reconciler nothing ever reached,
// which is the exact shape this suite was written to replace.
type chatReconciler struct {
	client *mattermost.Client
	cfg    *config.Mattermost
	org    *org.Organization
	sink   provision.TokenSink
}

func (*chatReconciler) Kind() integration.Kind { return integration.KindMattermost }

func (r *chatReconciler) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := r.pass(ctx)
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

// pass is one reconcile, with the Result the suite's seam throws away.
//
// THE PLAN IS REBUILT ON EVERY PASS, exactly as engine.mattermostPass builds
// it: registration is static and the company document is edited live, so a
// pass reads the company as it is at that moment. A harness holding one plan
// across passes would also hold the notes a run appends to it, and the second
// pass would report the first one's caveats as its own.
func (r *chatReconciler) pass(ctx context.Context) (*mattermost.Result, error) {
	plan, err := mattermost.PlanFor(r.org, r.cfg)
	if err != nil {
		return nil, err
	}
	return mattermost.Reconcile(ctx, mattermost.Options{
		Client: r.client, Config: r.cfg, Org: r.org, Plan: plan, Sink: r.sink,
	})
}

// chatWorld is one instance, one sealed store and the reconciler over both.
//
// The three travel together because every clause needs all three: the suite
// drives the reconciler, counts writes at BOTH estates, and the guards below
// read the instance and the store directly to prove the world is the one it
// claims to be.
type chatWorld struct {
	*chatReconciler
	srv  *chatServer
	sink *chatSink
}

// writes is every write this world has received, at the instance and in this
// deployment's own sealed store.
//
// BOTH ESTATES. A write is anything a person would have to undo, which is
// wider than "a request to Mattermost": a pass that re-seals a seat's
// credential on every converged run is writing just as surely, and the value
// an operator would have to put back lives in the store rather than on the
// instance. Counting only the instance would miss exactly the failure
// [mattermost.Options.Rotate] exists to keep deliberate.
func (w *chatWorld) writes() int { return w.srv.mutations() + w.sink.mutations() }

// theSeat is the one seat every world here runs, lowercased as the plan
// keys it.
const theSeat = "ceo"

// convergedChat stands up a world a real pass has already converged.
//
// CONVERGED BY A REAL RUN rather than by hand-seeding the fixture, which is
// the only way to be sure it is the state this pass actually leaves behind:
// a hand-built world can be converged in a way the provisioner never
// produces, and then the clause that matters — a second pass writes nothing
// — is answered about a world nobody runs.
//
// Neither Rotate nor Decommission is set, because the engine's pass sets
// neither: both are deliberate command-line gestures that take working
// agents down, and certifying the loop means certifying what the loop runs.
func convergedChat(t *testing.T, tb integrationtest.TB) *chatWorld {
	t.Helper()
	tb.Helper()
	return convergedChatOn(t, tb, nil)
}

// convergedChatOn is the same, over an instance a caller has tuned first.
//
// The tuning exists for ONE caller — the test that proves the guard below can
// actually fire — because the only way to reach a world the pass believes it
// converged and did not is an instance that answers a write with success and
// records nothing.
func convergedChatOn(t *testing.T, tb integrationtest.TB, tune func(*chatServer)) *chatWorld {
	t.Helper()
	tb.Helper()
	srv, sink := newChatServer(), newChatSink()
	if tune != nil {
		tune(srv)
	}
	w := &chatWorld{
		chatReconciler: &chatReconciler{
			// The OUTER t, because integrationtest.TB has no Cleanup:
			// the suite calls this once per case, so the servers stand
			// until the test function returns.
			client: chatClient(t, srv),
			cfg:    enabledChat(),
			org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{
				chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
			}},
			sink: sink,
		},
		srv: srv, sink: sink,
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
// Console. Nothing the engine can do fixes either — every pass from here is
// refused identically — and neither shows up until the day a seat is added,
// because a pass over a converged company writes nothing and so is never
// refused at all. That window is the whole reason this is reported rather
// than discovered.
//
// # Why this one rather than the alternatives
//
// It is the only person-owed shape this pass reaches that is also a FULL
// pass. The alternative — handing it [provision.ReadOnly] so the node has no
// keyring — reports a person-owed finding too, and returns it from
// [mattermost.Reconcile]'s third guard, before one HTTP request is made:
// every clause reading that world would be certifying an early return rather
// than the reconciler. This world resolves the team, lists the bots, reads
// the team and channel rosters, verifies the sealed token against the
// account it authenticates as, and THEN reports.
//
// # Why it is not quietly converged
//
// The suite has a clause for exactly that ("an outstanding world actually
// reports something"), and [TestTheOutstandingWorldIsBlockedOnAPerson] pins
// the specific shape rather than the mere count — "at least one person-owed
// finding" is satisfiable by accident, and four clauses read this world.
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
		t.Fatalf("Mutations went %d -> %d across building a world that "+
			"creates an account, joins two channels and mints a token; a "+
			"clause sampling in this order would read that pass as free",
			cold, warm)
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
// Both halves of [chatWorld.writes] are invisible on a converged world — that
// is the whole point of the world — so neither half is pinned by the clause
// that reads it. Drop either and "a converged pass writes nothing" goes on
// passing while the harness has stopped watching one of the two places this
// pass can write. This is the half at Mattermost, driven by the cheapest
// write the pass makes on its own: a display name the company document
// changed under it, which costs one PUT and touches no credential.
func TestTheWriteCounterSeesAWriteAtTheInstance(t *testing.T) {
	t.Parallel()
	w := convergedChat(t, t)
	before, storeBefore := w.writes(), w.sink.mutations()

	// The suffix rather than the role name: the handle is derived from the
	// name, so renaming the role would create a SECOND bot instead of
	// renaming this one.
	w.cfg.Provisioning.DisplayNameSuffix = " (robot)"
	if _, err := w.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := w.writes() - before; got != 1 {
		t.Fatalf("the counter moved by %d over a pass that renamed one bot; "+
			"a counter blind to the instance reads every Mattermost write as "+
			"free", got)
	}
	if got := w.sink.mutations() - storeBefore; got != 0 {
		t.Errorf("the rename also wrote %d time(s) to the sealed store, so "+
			"this case no longer isolates the instance half", got)
	}
}

// AND THE COUNTER SEES A WRITE INTO THIS DEPLOYMENT'S OWN SEALED STORE.
//
// The other half, and the one a harness counting requests would miss: the
// value an operator has to put back after a credential is rotated lives in
// the store, not on the instance. -rotate is the gesture that writes to both,
// so the counter's total has to be the sum rather than either side of it.
func TestTheWriteCounterSeesAWriteIntoTheSealedStore(t *testing.T) {
	t.Parallel()
	w := convergedChat(t, t)
	before, instanceBefore, storeBefore := w.writes(), w.srv.mutations(), w.sink.mutations()

	plan, err := mattermost.PlanFor(w.org, w.cfg)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if _, err := mattermost.Reconcile(t.Context(), mattermost.Options{
		Client: w.client, Config: w.cfg, Org: w.org, Plan: plan, Sink: w.sink,
		Rotate: true,
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	instance, store := w.srv.mutations()-instanceBefore, w.sink.mutations()-storeBefore
	if store == 0 {
		t.Fatalf("a rotation sealed nothing, so this case cannot see the store half")
	}
	if instance == 0 {
		t.Fatalf("a rotation wrote nothing at the instance, so this case cannot " +
			"tell the sum from either side")
	}
	if got := w.writes() - before; got != instance+store {
		t.Fatalf("the counter moved by %d over %d instance write(s) and %d "+
			"store write(s); it is watching one estate rather than both",
			got, instance, store)
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
	// outstanding world rather than a broken one: the seat is provisioned
	// and its credential sealed, so the only things missing really are the
	// two a person owes.
	if bad := w.vacuous(); len(bad) > 0 {
		t.Errorf("the outstanding world is failing for a second reason, so the "+
			"clauses that read it are certifying that one:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// vacuous names what is NOT converged about this world, and is what stands
// between this file and the shape it replaced: a suite driven at a
// Mattermost nothing was ever done to, where "a converged pass writes
// nothing" holds because there is nothing to write about. Every clause reads
// that world, so a guard that cannot fire puts nine green ticks on nothing.
//
// IT READS THE WORLD, NOT THE RESULT. A Result says what one pass did; these
// clauses are about what the NEXT pass will find, and the two are the same
// only while the fixture is honest. Each check is also the precondition for
// exactly one thing the converged pass must not do: a bot that is missing is
// created, a membership that is absent is joined, a seal that does not match
// the live token is minted over.
//
// COMPLAINTS RATHER THAN Fatalf, so [TestTheHarnessRefusesAConvergedWorldThatIsNotOne]
// can break one thing at a time and read what fired. A guard that can only
// kill its own test cannot be tested.
func (w *chatWorld) vacuous() []string {
	var bad []string
	username := mattermost.BotUsername(w.cfg.Provisioning, theSeat)
	id := w.srv.botID(username)
	if id == "" {
		bad = append(bad, fmt.Sprintf(
			"no bot account %q on the instance, so the converging pass wrote "+
				"nothing and there is no converged state to re-run against",
			username))
		// Everything below is about that account; without it they would
		// all fire and say the same thing three more times.
		return bad
	}
	if !w.srv.inTeam(id) {
		bad = append(bad, fmt.Sprintf(
			"%s is not in team %q, so the next pass joins it and the team "+
				"write this harness counts has not happened yet",
			username, w.cfg.Team))
	}
	for _, channel := range []string{"general", "leadership"} {
		if !w.srv.inChannel(channel, id) {
			bad = append(bad, fmt.Sprintf(
				"%s is not in %q, so the next pass joins it and a converged "+
					"pass would write after all", username, channel))
		}
	}
	live := w.srv.mintedToken(id, mattermost.TokenDescription(theSeat))
	if live == "" {
		bad = append(bad, fmt.Sprintf(
			"%s holds no token this run minted, so the next pass mints one "+
				"and the clause about a credential rotated on a timer is "+
				"answered about a seat that never had one", username))
	}
	// THE SEAL MUST BE THE LIVE TOKEN, not merely present. The keep-or-mint
	// decision takes the value the variable holds and asks the server who it
	// is, so a sealed value that is not the live token verifies as somebody
	// else — or as nobody — and the next pass MINTS. A store holding any
	// non-empty string would pass a presence check and certify the opposite
	// of what it claims.
	if sealed := w.sink.value("MM_TOKEN_CEO"); sealed != live || sealed == "" {
		bad = append(bad, fmt.Sprintf(
			"MM_TOKEN_CEO does not hold the token live on %s, so the next "+
				"pass cannot verify it and mints a fresh one", username))
	}
	return bad
}

// THE HARNESS REFUSES A CONVERGED WORLD THAT IS NOT ONE.
//
// Each thing [chatWorld.vacuous] protects is broken SEPARATELY. Together
// they mask each other — an instance with no bot has no membership and no
// token either — and a single break that reddens the lot proves only that
// one of them works.
func TestTheHarnessRefusesAConvergedWorldThatIsNotOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		broken func(t *testing.T, w *chatWorld)
		want   string
	}{{
		// A pass that made no request at all: the instance received
		// nothing, so there is no converged state here to re-run against.
		name:   "the seeding pass created no account",
		broken: func(_ *testing.T, w *chatWorld) { w.srv.forgetBots() },
		want:   "no bot account",
	}, {
		// Out of the team, which is where a channel membership cannot
		// exist: the next pass rejoins, so the team write this harness
		// counts has not happened yet.
		name:   "the bot is not in the team",
		broken: func(_ *testing.T, w *chatWorld) { w.srv.leaveTeam(w.botID(t)) },
		want:   "not in team",
	}, {
		// In the team and in only one of its two channels. A converged
		// pass reads the roster once and joins the difference, so a
		// world short one channel writes on every run for ever.
		name: "the bot is short one channel",
		broken: func(_ *testing.T, w *chatWorld) {
			w.srv.leaveChannel("leadership", w.botID(t))
		},
		want: `not in "leadership"`,
	}, {
		// No token on the account at all, which is the world where "a
		// converged pass writes nothing" would be a claim about a seat
		// that never had a credential to protect.
		name:   "no token was minted for the seat",
		broken: func(_ *testing.T, w *chatWorld) { w.srv.forgetTokens(w.botID(t)) },
		want:   "holds no token this run minted",
	}, {
		// The store was emptied. The next pass reads nothing held, mints,
		// and seals — so the loudest clause in the suite would be
		// certified over a world that was never converged in the one
		// dimension that costs a credential.
		name: "the seat's credential was not sealed",
		broken: func(t *testing.T, w *chatWorld) {
			if err := w.sink.Discard(t.Context()); err != nil {
				t.Fatalf("Discard: %v", err)
			}
		},
		want: "does not hold the token live on",
	}, {
		// A TOKEN ON THE ACCOUNT THAT THIS TOOL DID NOT MINT. The
		// description is the only thing separating a credential this
		// engine owns from one an administrator created by hand, and the
		// keep-or-mint decision and the retire step both key on it — so a
		// world whose only token is somebody else's is one the next pass
		// mints into, however live that token is.
		name: "the account's only token was minted by hand",
		broken: func(_ *testing.T, w *chatWorld) {
			w.srv.relabelTokens(w.botID(t), "set up by an admin")
		},
		want: "holds no token this run minted",
	}, {
		// AND THE SUBTLE ONE: the store holds a value, just not the live
		// one. An operator restoring an older env file leaves exactly
		// this, and a presence check passes it while the next pass
		// verifies the stale value as nobody and mints over it.
		name: "the sealed value is a stale token",
		broken: func(_ *testing.T, w *chatWorld) {
			w.sink.seed("MM_TOKEN_CEO", "mmtok-from-an-older-env-file")
		},
		want: "does not hold the token live on",
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
// [TestTheHarnessRefusesAConvergedWorldThatIsNotOne] proves every clause of
// [chatWorld.vacuous] can fire; it calls the method directly, so it says
// nothing about whether anything CALLS it. Delete the check from
// [convergedChat] and that test stays green while the suite quietly starts
// certifying whatever world the fixture happened to leave — which is the
// shape this whole file exists to remove.
//
// The world here is one the pass believes it converged: the instance accepts
// every channel join with a 201 and records none of them, so the Result says
// the seat joined both channels and the seat is in neither.
func TestTheHarnessRefusesToCertifyAWorldItOnlyThinksItConverged(t *testing.T) {
	t.Parallel()
	tb := &recordingTB{}
	w := convergedChatOn(t, tb, func(s *chatServer) { s.swallowJoins = true })

	said := strings.Join(tb.said, "\n")
	if said == "" {
		t.Fatalf("%s", "the harness certified a world whose seat is in no "+
			"channel, so nothing calls the guard that would have said so")
	}
	if !strings.Contains(said, "not converged") {
		t.Errorf("the refusal does not say what it refused: %q", said)
	}
	// And it named the thing that is wrong, so whoever reads it is not
	// sent looking through a pass that reported success.
	if !strings.Contains(said, `not in "leadership"`) {
		t.Errorf("the refusal does not name the missing membership: %q", said)
	}
	// The Result really did claim otherwise, which is why reading the
	// instance rather than the Result is the whole of this guard.
	res, err := w.pass(t.Context())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(res.Joined[theSeat]) != 2 {
		t.Errorf("the pass reported %v, so this case is no longer the one "+
			"where a Result and an instance disagree", res.Joined)
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
