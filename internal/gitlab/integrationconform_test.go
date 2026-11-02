package gitlab_test

import (
	"context"
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
// against the company-level reconcile the loop actually runs: the group, the
// provisioning token's expiry, and the webhook with its signing secret. Each
// seat's account is per-seat access, certified by its own suite.
//
// # TWO WORLDS, because a converged one cannot certify half the clauses
//
// Four of the nine clauses walk the findings list, and a converged pass
// reports none. [outstandingGitLab] is a company that never set its webhook
// address, so the instance has nowhere to deliver to.
//
// # Both hook paths, because the pass has two
//
// One hook on the group where the tier serves group webhooks, one per
// `provisioning.projects` entry where it does not, and the per-project branch
// multiplies whatever it does by the project count. The CONVERGED world is
// driven over both.

// gitlabWorld is one GitLab the suite drives, plus the pass itself as
// [integration.Reconciler] sees it.
type gitlabWorld struct {
	instance *adminInstance
	sink     *recordingSink
	opts     gitlab.Options
}

func (*gitlabWorld) Kind() integration.Kind { return integration.KindGitLab }

// Reconcile runs the pass and reads its findings.
func (w *gitlabWorld) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := gitlab.Reconcile(ctx, w.opts)
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

// writes is everything this pass has done that a person would have to undo:
// requests to the instance AND values sealed in the store. The instance's
// counter cannot see a Record, so a pass that re-sealed the signing secret on
// every converged run while making no request would be invisible to a
// harness that counted one of them.
func (w *gitlabWorld) writes() int { return w.instance.mutations() + w.sink.records() }

// newWorld stands a GitLab up and hands back the pass pointed at it. Both t
// and tb, because [integrationtest.TB] has no Cleanup and the server's
// lifetime hangs off the subtest that owns this world.
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
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: srv.URL, Token: adminToken, HTTP: srv.Client(),
	})
	if err != nil {
		tb.Fatalf("NewClient: %v", err)
	}
	cfg := enabledGitLab()
	cfg.Provisioning.Projects = []string{"nimbus/api", "nimbus/web"}
	signingVar, _ := provision.SoleVar(cfg.SigningSecret)
	sink := newRecordingSink()
	w := &gitlabWorld{instance: f, sink: sink, opts: gitlab.Options{
		Client: client, Config: cfg, Sink: sink,
		WebhookBase: "https://crewlet.example.com", SigningSecret: testSigningSecret,
		SigningSecretVar: signingVar,
		Now:              func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}}
	if adjust != nil {
		adjust(&w.opts)
	}
	return w
}

// convergedGitLab is a world a first pass has already brought in line.
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

// vacuous says why a "converged" world would certify nothing, or "".
func (w *gitlabWorld) vacuous() string {
	if w.instance.mutations() == 0 {
		return "the seeding pass wrote nothing, so there is no converged " +
			"world here to certify a second pass against"
	}
	f := w.instance
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hooks)+len(f.projectHooks) == 0 {
		return "no webhook was registered, so the hook half of the pass is " +
			"not being exercised"
	}
	return ""
}

// outstandingGitLab is a company that never set its webhook address.
func outstandingGitLab(t *testing.T, tb integrationtest.TB) *gitlabWorld {
	t.Helper()
	return newWorld(t, tb, nil, func(o *gitlab.Options) { o.WebhookBase = "" })
}

func TestTheGitLabReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	for _, world := range []struct {
		name string
		tune func(*adminInstance)
	}{
		{"one hook on the group", nil},
		{"one hook per project", func(f *adminInstance) { f.plan = "free" }},
	} {
		t.Run(world.name, func(t *testing.T) {
			t.Parallel()
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

// THE OUTSTANDING WORLD IS BLOCKED ON A PERSON: the one finding names the
// setting to fill in, and no hook was guessed.
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
	if got.Subject != "api.external_url" {
		t.Errorf("subject = %q, want the setting a person has to fill in", got.Subject)
	}
	if _, actor := got.Kind.Verdict(); !actor.WaitsOnAPerson() {
		t.Errorf("the finding is owed by %s, so nobody is being asked to act", actor)
	}
	if len(w.instance.hooks)+len(w.instance.projectHooks) != 0 {
		t.Errorf("a hook was registered with no public base URL: %+v %+v",
			w.instance.hooks, w.instance.projectHooks)
	}
}

// THE HARNESS REFUSES A CONVERGED WORLD THAT IS NOT ONE, so a regression in
// the fixture cannot turn the contract into a test over nothing.
func TestTheHarnessRefusesAConvergedWorldThatIsNotOne(t *testing.T) {
	t.Parallel()
	for _, broken := range []struct {
		name   string
		broken func(w *gitlabWorld)
		want   string
	}{{
		name:   "the seeding pass wrote nothing",
		broken: func(w *gitlabWorld) { w.instance.forget() },
		want:   "wrote nothing",
	}, {
		name: "no webhook was registered",
		broken: func(w *gitlabWorld) {
			w.instance.mu.Lock()
			defer w.instance.mu.Unlock()
			w.instance.hooks, w.instance.projectHooks = nil, nil
		},
		want: "no webhook was registered",
	}} {
		t.Run(broken.name, func(t *testing.T) {
			t.Parallel()
			w := convergedGitLab(t, t, nil)
			broken.broken(w)
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

// A FIRST RUN WITH NO SIGNING SECRET MINTS ONE into the variable the config
// names, and the hook carries it.
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

// THE WRITE COUNTER SEES A VALUE RESEALED WITH NO REQUEST, which is the
// half the instance's own counter is blind to.
func TestTheWriteCounterSeesACredentialResealedWithNoRequest(t *testing.T) {
	t.Parallel()
	w := convergedGitLab(t, t, nil)
	before := w.writes()
	requests := w.instance.mutations()
	if err := w.sink.Record(t.Context(), "GITLAB_SIGNING_SECRET", "whsec_resealed"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if after := w.writes(); after != before+1 {
		t.Fatalf("the counter moved by %d over a re-sealed value, want 1", after-before)
	}
	if w.instance.mutations() != requests {
		t.Fatal("the instance saw a request, so this proves nothing about the sink")
	}
}
