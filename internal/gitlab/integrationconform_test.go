package gitlab_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/provision"
)

// The GitLab half of [integration.Reconciler]'s safety contract, certified
// against the reconcile the loop actually runs.
//
// # Why this file exists at all
//
// integrationtest states the contract in nine clauses and its package doc
// calls the write-counting hook required, because that clause is "the one
// most likely to be wrong". Its only caller was a stub whose pass returned
// (nil, nil), so "a converged pass writes nothing" held because nothing
// happened — a green test over a world nobody had built and a reconciler
// nobody had run. This points the same clauses at [gitlab.Reconcile] and
// counts what the pass writes.
//
// # TWO WORLDS, because a converged one cannot certify half the clauses
//
// Four of the nine walk the findings list. A converged pass reports NO
// findings — that is what converged means — so every one of them walked an
// empty list and passed whatever this package did. The suite now requires an
// OUTSTANDING world as well, and [outstandingGitLab] is GitLab's: a company
// that never set integrations.public_base_url, so the pass provisions every
// seat and registers no webhook, and the instance has nowhere to deliver to.
//
// # Both hook paths, because the pass has two
//
// [gitlab.Reconcile] registers ONE hook on the group where the tier serves
// group webhooks, and one hook per `provisioning.projects` entry where it
// does not — and the per-project branch multiplies whatever it does by the
// project count. A harness pinned to the group path certifies half the
// surface, so the CONVERGED world is driven over both.
//
// The outstanding world is the same under both tunes, deliberately: a run
// with no public base URL never reaches the tier branch at all, because
// there is no address to put in a hook. Driving it twice would not be a
// second world, it would be the same one twice.

// gitlabWorld is one GitLab the suite drives, plus the pass itself as
// [integration.Reconciler] sees it.
type gitlabWorld struct {
	instance *adminInstance
	sink     *recordingSink
	opts     gitlab.Options
}

// Kind names the surface. [integration.KindGitLab] is a constant rather than
// anything derived, which is what makes the suite's stability clause a real
// assertion about the adapter rather than about a field.
func (*gitlabWorld) Kind() integration.Kind { return integration.KindGitLab }

// Reconcile runs the real pass and maps its answer the way the loop's own
// adapter does.
//
// IT MIRRORS internal/engine's gitlabPass.Run DELIBERATELY, including the
// [gitlab.ErrNameReserved] arm — which lives in the engine rather than in
// this package. A harness that skipped it would certify a slightly different
// reconciler from the one the loop drives, and the clause it would get wrong
// is the one about a fault versus a finding.
func (w *gitlabWorld) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := gitlab.Reconcile(ctx, w.opts)
	if errors.Is(err, gitlab.ErrNameReserved) {
		return []integration.Finding{{
			Kind: integration.FindingIdentityMissing,
			Detail: "GitLab is still releasing the name of an account it is " +
				"deleting, so this seat's account cannot be created yet: " +
				"the next pass makes it, usually within a minute",
		}}, nil
	}
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

// writes is what [integrationtest.Reconciler.Mutations] asks for: everything
// this pass has done that a PERSON WOULD HAVE TO UNDO.
//
// TWO COUNTERS SUMMED, because the pass writes to two places and the suite's
// package doc says so in as many words: "a pass that re-seals a seat's
// credential through the fleet's sealed store on every converged run is
// writing just as surely, and the harness has to see it." The instance's
// counter cannot see a Record — the sealed store is not an HTTP route here —
// so a regression that re-sealed every seat token on every converged pass
// while making no request at all would have been invisible to a harness that
// counted only one of them. That is not a hypothetical shape: it is what the
// pass does the moment [gitlab.Options.Rotate] is true, and one wrong
// default is all it takes.
func (w *gitlabWorld) writes() int { return w.instance.mutations() + w.sink.records() }

// newWorld stands a GitLab up and hands back the pass pointed at it.
//
// BOTH t AND tb, deliberately. [integrationtest.TB] is the narrow interface
// the suite's own tests drive its cases with and it has no Cleanup, so the
// case's own reporter is tb while the server's lifetime hangs off the real
// *testing.T of the subtest that owns this world.
func newWorld(t *testing.T, tb integrationtest.TB, tune func(*adminInstance),
	adjust func(*gitlab.Options),
) *gitlabWorld {
	t.Helper()
	f := newAdminInstance()
	if tune != nil {
		tune(f)
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	// THE SERVER'S OWN CLIENT, whose transport dies with it — see
	// reconcileWith for why a shared pool breaks parallel tests.
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: srv.URL, Token: adminToken, HTTP: srv.Client(),
	})
	if err != nil {
		tb.Fatalf("NewClient: %v", err)
	}

	cfg := enabledGitLab()
	cfg.Provisioning.Projects = []string{"nimbus/api", "nimbus/web"}
	plan := &provision.Plan{}
	for _, seat := range []struct{ handle, tokenVar string }{
		{"swe", "GITLAB_TOKEN_SWE"},
		{"cto", "GITLAB_TOKEN_CTO"},
	} {
		plan.Add(provision.Seat{
			Handle: seat.handle, Role: strings.ToUpper(seat.handle),
			TokenVar: seat.tokenVar,
			Email:    seat.handle + "@noreply.crewlet.invalid",
		})
	}
	// THE VARIABLE THE ENGINE DERIVES, by the call the engine makes:
	// internal/engine's gitlabPass.Run reads provision.SoleVar over the
	// config's own signing_secret. Left empty, the harness was driving an
	// option the loop never leaves empty — inert on the converged path,
	// because PlanSigningSecret answers SigningReuse on a resolved secret
	// before it ever consults the variable, but "inert today" is not the
	// claim this file makes about these fields.
	signingVar, _ := provision.SoleVar(cfg.SigningSecret)
	sink := newRecordingSink()
	w := &gitlabWorld{instance: f, sink: sink, opts: gitlab.Options{
		Client: client, Config: cfg, Plan: plan, Sink: sink,
		// EVERY ONE OF THESE THE WAY THE ENGINE SETS IT. Each has a
		// setting that makes the pass do LESS than the loop really does,
		// and each would make the converged clause pass for the wrong
		// reason: an empty WebhookBase touches no hook at all, empty
		// Projects adds no project memberships, and a sink where
		// provision.CanMint is false short-circuits the whole pass to
		// NoKeyring without one request. recordingSink has no Mints
		// method, so CanMint answers true.
		//
		// The three the engine never sets — Mode, Decommission and Rotate
		// (its PassInput carries no Recreate) — are left at their zero
		// values for the same reason.
		WebhookBase: "https://crewlet.example.com", SigningSecret: testSigningSecret,
		SigningSecretVar: signingVar,
		Now:              func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}}
	if adjust != nil {
		adjust(&w.opts)
	}
	return w
}

// convergedGitLab stands the world up and converges it BY RUNNING THE PASS.
//
// # Converged by the pass rather than by a fixture
//
// The alternative is a hand-written fixture that seeds the accounts,
// memberships and hook a converged company has. It is the tempting one and it
// is weaker in the direction that matters: it encodes what the harness author
// BELIEVES a converged GitLab looks like, so a pass whose idea of converged
// has drifted from that belief writes on every run and the fixture calls it
// converged anyway. Running the real pass makes the world converged by
// definition, and then the only question left is the one worth asking —
// whether the SECOND pass writes.
//
// The seeding pass's own writes are not counted: [integrationtest.Run] takes
// its baseline from Mutations() after Converged returns.
//
// TWO SEATS AND TWO PROJECTS, because every write this pass made on a
// converged world was per-seat or per-seat-per-project. One of each would
// have counted 1 where the bug's shape is N and N×M, and a harness cannot
// tell "wrote once" from "wrote once per seat" with one seat.
func convergedGitLab(t *testing.T, tb integrationtest.TB, tune func(*adminInstance)) *gitlabWorld {
	t.Helper()
	w := newWorld(t, tb, tune, nil)

	findings, err := w.Reconcile(context.Background())
	if err != nil {
		tb.Fatalf("the pass that converges the world failed: %v", err)
	}
	if len(findings) != 0 {
		tb.Fatalf("the world is not converged: the seeding pass reported %+v", findings)
	}
	if why := w.vacuous(); why != "" {
		tb.Fatalf("%s", why)
	}
	return w
}

// vacuous says why this is not a converged GitLab worth certifying against,
// or "" when it is.
//
// # Why it is a method returning a reason rather than four tb.Fatalf lines
//
// These are the harness's own anti-vacuity guards — the local twin of the
// suite's "an outstanding world actually reports something" clause. Written
// inline they were UNPINNED, in the exact way this round exists to catch:
// deleting the write check left the whole package green, because a guard
// only fires in a world the fixture does not currently produce, and a guard
// nothing can make fire is a claim rather than coverage. As a function over
// the world it is drivable, and
// [TestTheHarnessRefusesAConvergedWorldThatIsNotOne] breaks each thing it
// protects one at a time.
func (w *gitlabWorld) vacuous() string {
	// IT ACTUALLY DID THE WORK. A seeding pass that wrote nothing would
	// leave every case in the suite true about an empty GitLab, which is
	// the exact shape of vacuous pass this file exists to replace.
	if w.instance.mutations() == 0 {
		return "the seeding pass wrote nothing, so there is no converged " +
			"world here to certify a second pass against"
	}
	for _, name := range []string{"GITLAB_TOKEN_SWE", "GITLAB_TOKEN_CTO"} {
		if w.sink.value(name) == "" {
			return name + " holds no token after the seeding pass, so the " +
				"second pass would mint rather than keep"
		}
	}
	f := w.instance
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.groupMembers) != 2 {
		return fmt.Sprintf("group roster = %v, want both seats", f.groupMembers)
	}
	if len(f.hooks)+len(f.projectHooks) == 0 {
		return "no webhook was registered, so the hook half of the pass is " +
			"not being exercised"
	}
	return ""
}

// outstandingGitLab is a GitLab where something is genuinely wrong AND A
// PERSON HAS TO ACT: the company never set integrations.public_base_url, so
// there is no address to point a webhook at and the instance delivers
// nothing to anybody.
//
// # Why this one
//
// It is the only person-owed finding this pass reaches that is also a FULL
// pass. The alternative — handing it [provision.ReadOnly], so the node has no
// keyring — reports a person-owed finding too, and returns it before one
// HTTP request is made: every clause downstream would then be certifying an
// early return rather than the reconciler. This world resolves the group,
// resolves both projects, reads both rosters, creates two accounts, adds four
// memberships, mints and seals two tokens, and THEN reports.
//
// # Why it is not quietly converged
//
// The suite has a clause for exactly that ("an outstanding world actually
// reports something"), and [TestTheOutstandingWorldIsBlockedOnAPerson] pins
// the specific shape rather than the mere count — because "at least one
// person-owed finding" is satisfiable by accident and this is the world four
// other clauses read.
//
// # It is NOT pre-converged, deliberately
//
// The first pass creates accounts and mints; the second keeps. So the suite's
// "two passes over an outstanding world agree" clause is driven across that
// transition, which is where a findings list actually churns — a converged
// twin of it compares two stable lists and could not see it.
func outstandingGitLab(t *testing.T, tb integrationtest.TB) *gitlabWorld {
	t.Helper()
	// NOWHERE TO DELIVER TO, which is what the loop passes whenever
	// integrations.public_base_url is unset: setup.PassInput.WebhookBase is
	// that value verbatim.
	return newWorld(t, tb, nil, func(o *gitlab.Options) { o.WebhookBase = "" })
}

// THE CONTRACT IS CERTIFIED AGAINST THE REAL RECONCILER.
func TestTheGitLabReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	for _, world := range []struct {
		name string
		tune func(*adminInstance)
	}{
		// The Premium / self-managed shape: one hook on the group.
		{"one hook on the group", nil},
		// And the free tier, where a group webhook is accepted and never
		// delivered, so the pass registers one hook per declared project
		// — a branch whose every write is multiplied by the project
		// count.
		{"one hook per project", func(f *adminInstance) { f.plan = "free" }},
	} {
		t.Run(world.name, func(t *testing.T) {
			t.Parallel()
			// Assigned by Converged and read by Mutations, which the
			// suite calls in that order within one sequential case.
			// Outstanding never touches it: Mutations is only ever
			// sampled around a pass over the converged world, and a
			// counter that followed whichever world was built last would
			// answer for the wrong one.
			var converged *gitlabWorld
			integrationtest.Run(t, integrationtest.Reconciler{
				Converged: func(tb integrationtest.TB) integration.Reconciler {
					converged = convergedGitLab(t, tb, world.tune)
					return converged
				},
				Outstanding: func(tb integrationtest.TB) integration.Reconciler {
					return outstandingGitLab(t, tb)
				},
				Mutations: func() int { return converged.writes() },
			})
		})
	}
}

// THE OUTSTANDING WORLD IS BLOCKED ON A PERSON, and on this exact thing.
//
// The suite's anti-vacuity clause asks only for one finding somebody owes,
// which a world could satisfy by accident — and four clauses read this world,
// so "it reported something" is a weaker claim than it looks. This says what,
// about which setting, and that everything else converged: a pass that
// started failing for an unrelated reason would still report a person-owed
// finding and still pass the suite.
func TestTheOutstandingWorldIsBlockedOnAPerson(t *testing.T) {
	t.Parallel()
	w := outstandingGitLab(t, t)
	findings, err := w.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("the outstanding pass faulted rather than reporting: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly the blocked delivery path", findings)
	}
	got := findings[0]
	if got.Kind != integration.FindingIngressBlocked {
		t.Errorf("kind = %q, want %q", got.Kind, integration.FindingIngressBlocked)
	}
	if got.Subject != "integrations.public_base_url" {
		t.Errorf("subject = %q, want the setting a person has to fill in", got.Subject)
	}
	if _, actor := got.Kind.Verdict(); !actor.WaitsOnAPerson() {
		t.Errorf("the finding is owed by %s, so nobody is being asked to act", actor)
	}
	// AND THE REST OF THE PASS WORKED, which is what makes this an
	// outstanding world rather than a broken one: the seats were
	// provisioned, so the only thing missing really is the delivery path.
	for _, name := range []string{"GITLAB_TOKEN_SWE", "GITLAB_TOKEN_CTO"} {
		if w.sink.value(name) == "" {
			t.Errorf("%s holds no token, so this world is failing for a second "+
				"reason and the clauses that read it are certifying that one", name)
		}
	}
	if len(w.instance.hooks)+len(w.instance.projectHooks) != 0 {
		t.Errorf("a hook was registered with no public base URL: %+v %+v",
			w.instance.hooks, w.instance.projectHooks)
	}
}

// THE HARNESS REFUSES A CONVERGED WORLD THAT IS NOT ONE.
//
// [gitlabWorld.vacuous] is what stands between this file and the shape it
// replaced: a suite driven at a GitLab nothing was ever done to, where "a
// converged pass writes nothing" holds because there is nothing to write
// about. Every clause in the suite reads that world, so a guard that cannot
// fire puts nine green ticks on nothing.
//
// Each thing it protects is broken SEPARATELY here. Together they mask each
// other — an empty instance has no roster and no hook either — and a single
// break that reddens the lot proves only that one of them works.
func TestTheHarnessRefusesAConvergedWorldThatIsNotOne(t *testing.T) {
	t.Parallel()
	for _, broken := range []struct {
		name   string
		broken func(t *testing.T, w *gitlabWorld)
		want   string
	}{{
		// A pass that made no request at all: the instance received
		// nothing, so there is no converged state here to re-run against.
		name:   "the seeding pass wrote nothing",
		broken: func(_ *testing.T, w *gitlabWorld) { w.instance.forget() },
		want:   "wrote nothing",
	}, {
		// A seat whose credential was not sealed. The second pass would
		// then MINT rather than keep, so "a converged pass writes
		// nothing" would be a claim about a world that was never
		// converged in the only dimension that costs a credential.
		name: "no seat token was sealed",
		broken: func(t *testing.T, w *gitlabWorld) {
			if err := w.sink.Discard(t.Context()); err != nil {
				t.Fatalf("Discard: %v", err)
			}
		},
		want: "holds no token",
	}, {
		// An empty roster: the memberships half of the pass never ran, so
		// the N and N×M writes this harness exists to count could not
		// have happened.
		name: "neither seat is in the group",
		broken: func(_ *testing.T, w *gitlabWorld) {
			w.instance.mu.Lock()
			defer w.instance.mu.Unlock()
			w.instance.groupMembers = map[int]int{}
		},
		want: "group roster",
	}, {
		// And no hook anywhere, which is the half that is a write per
		// project per pass when it regresses.
		name: "no webhook was registered",
		broken: func(_ *testing.T, w *gitlabWorld) {
			w.instance.mu.Lock()
			defer w.instance.mu.Unlock()
			w.instance.hooks, w.instance.projectHooks = nil, nil
		},
		want: "no webhook was registered",
	}} {
		t.Run(broken.name, func(t *testing.T) {
			t.Parallel()
			w := convergedGitLab(t, t, nil)
			if why := w.vacuous(); why != "" {
				t.Fatalf("a real converged world was refused: %s", why)
			}
			broken.broken(t, w)
			why := w.vacuous()
			if why == "" {
				t.Fatalf("%s, and the harness accepted the world anyway", broken.name)
			}
			if !strings.Contains(why, broken.want) {
				t.Errorf("refused with %q, which does not name %q", why, broken.want)
			}
		})
	}
}

// THE HARNESS CARRIES THE VARIABLE A MINTED SIGNING SECRET IS SEALED INTO.
//
// [gitlab.Options.SigningSecretVar] is one of the fields the file's comment
// block claims is set "the way the engine sets it", and it was the one that
// was not: it was left empty. Inert on the converged path — PlanSigningSecret
// takes SigningReuse on a resolved secret before it ever consults the
// variable — which is exactly why nothing noticed, and exactly why a claim
// like that needs a test rather than a comment.
//
// So it is driven over the posture where the field IS load-bearing: a company
// whose signing_secret resolves to nothing, where the pass must MINT one and
// seal it under the name the config's ${VAR} gives. With the field empty that
// pass does not mint — PlanSigningSecret answers SigningBlocked and the whole
// run is refused. That posture is also the first run of every real company,
// and until this it was not driven through this harness's options at all.
func TestTheHarnessMintsASigningSecretIntoTheConfiguredVariable(t *testing.T) {
	t.Parallel()
	w := newWorld(t, t, nil, func(o *gitlab.Options) { o.SigningSecret = "" })
	if _, err := w.Reconcile(t.Context()); err != nil {
		t.Fatalf("a first run with no signing secret was refused: %v", err)
	}
	got := w.sink.value("GITLAB_SIGNING_SECRET")
	if !strings.HasPrefix(got, gitlab.SigningSecretPrefix) {
		t.Fatalf("GITLAB_SIGNING_SECRET = %q, want a minted whsec_ value sealed "+
			"under the name integrations.gitlab.signing_secret points at", got)
	}
	if len(w.instance.hooks)+len(w.instance.projectHooks) == 0 {
		t.Error("nothing was hooked, so the minted secret reached no hook")
	}
}

// THE WRITE COUNTER SEES THE SEALED STORE, not just the instance.
//
// [integrationtest]'s package doc defines a write as anything a person would
// have to undo and names a re-sealed credential as one. The instance's own
// counter cannot see a Record — nothing goes over HTTP — so a harness that
// summed only that would report zero for a pass that rotated every seat's
// token on every run, which is the single worst thing this loop can do.
func TestTheWriteCounterSeesACredentialResealedWithNoRequest(t *testing.T) {
	t.Parallel()
	w := convergedGitLab(t, t, nil)
	before := w.writes()
	requests := w.instance.mutations()

	if err := w.sink.Record(t.Context(), "GITLAB_TOKEN_SWE", "glpat-resealed"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if after := w.writes(); after != before+1 {
		t.Fatalf("the counter moved by %d over a re-sealed credential, want 1",
			after-before)
	}
	if w.instance.mutations() != requests {
		t.Fatal("the instance saw a request, so this proves nothing about the sink")
	}
}
