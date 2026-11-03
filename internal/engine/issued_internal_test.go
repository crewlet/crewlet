package engine

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// fakeCloud is the kind of [fakeIssuer], which no integration of this build
// is: the shared paths serve it knowing nothing of it but the contract.
const fakeCloud integration.Kind = "fakecloud"

// errFakeWaiting is the fake's waiting story: the agent's account at the
// vendor is not made yet, a person's step its card would ask for.
var errFakeWaiting = errors.New("fakecloud: the agent's account is not made yet")

// fakeIssuer is a second integration that issues at use, standing in for any
// issuer the shared paths must serve without a branch for its kind. It issues
// for the seats in ready, holding each seat's token while it is good for the
// life asked, waits for every other agent, and refuses every issue with
// refuse while it is set.
type fakeIssuer struct {
	mu      sync.Mutex
	ready   map[string]bool
	refuse  error
	passes  bool
	renewIn time.Duration
	issues  int
	held    map[string]issuedToken
}

func newFakeIssuer(ready ...string) *fakeIssuer {
	f := &fakeIssuer{ready: map[string]bool{}, renewIn: 50 * time.Minute, held: map[string]issuedToken{}}
	for _, handle := range ready {
		f.ready[handle] = true
	}
	return f
}

func (*fakeIssuer) kind() integration.Kind { return fakeCloud }

func (*fakeIssuer) variable(c *Company, handle string) string {
	if c == nil || c.Org == nil || c.Org.AgentSeatByHandle(handle) == nil {
		return ""
	}
	return seataccess.SinkName(fakeCloud, "TOKEN", handle)
}

func (f *fakeIssuer) issuing(_ *Company, seat *org.Role, _ *config.Resolver) issueFunc {
	handle := seat.Handle()
	return func(_ context.Context, life time.Duration) (issuedToken, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case f.refuse != nil:
			return issuedToken{}, f.refuse
		case !f.ready[handle]:
			return issuedToken{}, errFakeWaiting
		}
		now := time.Now()
		if held, ok := f.held[handle]; ok && held.ExpiresAt.After(now.Add(life)) {
			return held, nil
		}
		f.issues++
		f.held[handle] = issuedToken{Token: "fake_issued_" + strconv.Itoa(f.issues),
			ExpiresAt: now.Add(time.Hour), RenewAt: now.Add(f.renewIn)}
		return f.held[handle], nil
	}
}

func (f *fakeIssuer) refusal(err error) (string, bool) {
	if errors.Is(err, errFakeWaiting) {
		return "", false
	}
	return err.Error(), f.passes
}

func (*fakeIssuer) words() issuedWords {
	return issuedWords{
		source:  "the fake cloud token the engine issues for this seat",
		verb:    "issue",
		arrival: "the agent's fake cloud account is made",
		refused: "it is not issued until somebody grants the agent's fake cloud account",
		retried: "the fake cloud is asked for it again on the restart schedule",
	}
}

// issuerRig is one issuer on a company where it issues for the seat ready and
// waits on a person's step for the seat waiting.
type issuerRig struct {
	e              *Engine
	c              *Company
	kind           integration.Kind
	ready, waiting string
	// says is what a refused issue's reason carries, for a rig built refused.
	says string
}

// issuerCases is every issuer the shared paths serve, each built fresh by its
// rig: refused builds it with the vendor refusing every issue. GitHub's runs
// through its own fakes; the fake cloud's proves no path keys on a kind.
var issuerCases = []struct {
	name string
	rig  func(t *testing.T, refused bool) issuerRig
}{
	{"github", func(t *testing.T, refused bool) issuerRig {
		gh := newAppGitHub(t)
		if refused {
			gh.status = http.StatusForbidden
		}
		c := appCompany(t, gh.url, true)
		return issuerRig{e: appEngine(t, c), c: c, kind: integration.KindGitHub,
			ready: "builder", waiting: "planner", says: "403"}
	}},
	{"a second issuer", func(t *testing.T, refused bool) issuerRig {
		// GITHUB OFF, so the fake is the only issuer that issues anything.
		c := appCompany(t, "https://github.example.com", false)
		e := appEngine(t, c)
		fake := newFakeIssuer("builder")
		if refused {
			fake.refuse = errors.New("fakecloud: POST /token: 403: the grant is not applied")
		}
		e.issuers = append(e.seatIssuers(), fake)
		return issuerRig{e: e, c: c, kind: fakeCloud, ready: "builder", waiting: "planner",
			says: "the grant is not applied"}
	}},
}

// issuerOf is r's issuer as its engine lists it.
func (r issuerRig) issuerOf(t *testing.T) seatIssuer {
	t.Helper()
	for _, issuer := range r.e.seatIssuers() {
		if issuer.kind() == r.kind {
			return issuer
		}
	}
	t.Fatalf("the engine lists no %s issuer", r.kind)
	return nil
}

// EVERY ISSUER IS SERVED BY THE SAME SHARED PATHS, and holds up its half: its
// seat's own variable issued at the first lookup and answered by that seat's
// resolution alone, good for the lives the paths ask, renewed before it runs
// short, written into a running box's token file, and told as waiting on its
// integration or refused with its vendor's reason.
//
// Mutation: drop an issuer from mintedFor, the resolver or the refresh, or
// read a story's words off a kind, and its row goes red.
func TestEveryIssuerMeetsTheContract(t *testing.T) {
	t.Parallel()
	for _, tc := range issuerCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			t.Run("issues its seat's own variable at use", func(t *testing.T) {
				t.Parallel()
				r := tc.rig(t, false)
				issuer := r.issuerOf(t)
				seat := r.c.Org.AgentSeatByHandle(r.ready)
				name := issuer.variable(r.c, r.ready)
				if name == "" || r.e.mintedFor(r.c, r.ready)[name] != r.kind {
					t.Fatalf("%q is not declared as the %s integration's: %v", name, r.kind,
						r.e.mintedFor(r.c, r.ready))
				}
				if got := issuer.variable(r.c, "owner"); got != "" {
					t.Errorf("a person's seat is issued %q", got)
				}
				env, tokens := r.e.seatResolver(t.Context(), r.c, seat, toolTokenLife)
				own := tokens.link(name)
				if own == nil || own.SourceName() != issuer.words().source {
					t.Fatalf("the seat's resolution holds no source of its own for %s", name)
				}
				if _, armed := tokens.renewAt(); armed {
					t.Error("a resolution that read nothing reports a token to renew")
				}
				value := env.Value("${" + name + "}")
				issued, ok := own.issued()
				if value == "" || !ok || issued.Token != value {
					t.Fatalf("the seat's own variable resolved to %q and the source reports %v", value, ok)
				}
				now := time.Now()
				if issued.ExpiresAt.Sub(issued.RenewAt) < toolTokenLife || issued.ExpiresAt.Before(now.Add(runTokenLife)) ||
					!issued.RenewAt.After(now) {
					t.Errorf("an issue expiring %v and renewed %v does not serve the lives the paths ask",
						issued.ExpiresAt.Sub(now), issued.RenewAt.Sub(now))
				}
				if at, armed := tokens.renewAt(); !armed || !at.Equal(issued.RenewAt) {
					t.Errorf("the resolution renews at %v (%v), want the issue's %v", at, armed, issued.RenewAt)
				}
				other, _ := r.e.seatResolver(t.Context(), r.c, r.c.Org.AgentSeatByHandle(r.waiting), toolTokenLife)
				if other.Value("${"+name+"}") != "" || r.e.resolver().Value("${"+name+"}") != "" {
					t.Error("another seat's resolution, or the company's chain, answered the seat's variable")
				}
				s := &seatChildren{handle: r.ready, lease: t.Context()}
				r.e.armRenewal(s, tokens)
				if s.renew == nil {
					t.Fatal("children started with an issued credential have no renewal armed")
				}
				s.renew.Stop()
			})
			t.Run("keeps a running box's token file", func(t *testing.T) {
				t.Parallel()
				r := tc.rig(t, false)
				name := r.issuerOf(t).variable(r.c, r.ready)
				const path = ".config/crewlet/token"
				defaults := []sandbox.SetupStep{{Name: "auth", Files: map[string]string{path: "${" + name + "}"}}}
				box := sandbox.NewFakeSandbox("box-1")
				box.Put(path, "expired")
				r.e.refreshTokenFiles(t.Context(), defaults, sandbox.PendingRun{TurnID: "t", AgentHandle: r.ready}, box)
				_, tokens := r.e.seatResolver(t.Context(), r.c, r.c.Org.AgentSeatByHandle(r.ready), refreshTokenLife)
				want, _ := tokens.Lookup(name)
				if held, _ := box.ReadFile(t.Context(), path); want == "" || string(held) != want {
					t.Errorf("the token file holds %q, want the issued token", held)
				}
			})
			t.Run("tells a value waiting on its step as waiting", func(t *testing.T) {
				t.Parallel()
				r := tc.rig(t, false)
				issuer := r.issuerOf(t)
				name := issuer.variable(r.c, r.waiting)
				env, tokens := r.e.seatResolver(t.Context(), r.c, r.c.Org.AgentSeatByHandle(r.waiting), toolTokenLife)
				if env.Value("${"+name+"}") != "" {
					t.Fatal("a seat waiting on its integration's step was issued a credential")
				}
				m := credentialGap(r.waiting, []string{name}, env,
					&seatMint{kinds: r.e.mintedFor(r.c, r.waiting), token: tokens})
				if m == nil || !m.waiting() || tokens.refused() {
					t.Fatalf("missing = %v, refused = %v: want a wait", m, tokens.refused())
				}
				says := m.Error()
				for _, want := range []string{
					"the " + string(r.kind) + " integration to " + issuer.words().verb + " ${" + name + "}",
					"it starts once " + issuer.words().arrival,
				} {
					if !strings.Contains(says, want) {
						t.Errorf("the sentence does not say %q: %s", want, says)
					}
				}
			})
			t.Run("tells a refused issue as a failure with its reason", func(t *testing.T) {
				t.Parallel()
				r := tc.rig(t, true)
				issuer := r.issuerOf(t)
				name := issuer.variable(r.c, r.ready)
				env, tokens := r.e.seatResolver(t.Context(), r.c, r.c.Org.AgentSeatByHandle(r.ready), toolTokenLife)
				if env.Value("${"+name+"}") != "" {
					t.Fatal("a refused issue resolved to a value")
				}
				m := credentialGap(r.ready, []string{name}, env,
					&seatMint{kinds: r.e.mintedFor(r.c, r.ready), token: tokens})
				if m == nil || m.waiting() || !tokens.refused() {
					t.Fatalf("missing = %v, refused = %v: want a failure", m, tokens.refused())
				}
				says := m.Error()
				for _, want := range []string{"${" + name + "} could not be issued", r.says, issuer.words().refused} {
					if !strings.Contains(says, want) {
						t.Errorf("the sentence does not say %q: %s", want, says)
					}
				}
			})
		})
	}
}

// twoIssuers is a company on GitHub whose `builder` is issued its GitHub App
// token and the fake cloud's token, and whose `planner` waits on its app.
func twoIssuers(t *testing.T) (*Engine, *Company, *fakeIssuer) {
	t.Helper()
	gh := newAppGitHub(t)
	c := appCompany(t, gh.url, true)
	e := appEngine(t, c)
	fake := newFakeIssuer("builder", "planner")
	e.issuers = append(e.seatIssuers(), fake)
	return e, c, fake
}

const (
	builderGitHub = "GITHUB_APP_TOKEN_BUILDER"
	builderFake   = "FAKECLOUD_TOKEN_BUILDER"
)

// TWO ISSUERS ON ONE SEAT ARE ONE RESOLUTION: each variable answered by its
// own source, both declared as their integrations', in the registry's order.
func TestTwoIssuersOnOneSeatAreOneResolution(t *testing.T) {
	t.Parallel()
	e, c, _ := twoIssuers(t)
	got := e.mintedFor(c, "builder")
	if got[builderGitHub] != integration.KindGitHub || got[builderFake] != fakeCloud {
		t.Fatalf("mintedFor = %v, want both variables, each its integration's", got)
	}
	env, tokens := e.seatResolver(t.Context(), c, c.Org.AgentSeatByHandle("builder"), toolTokenLife)
	var chain []string
	for link := tokens; link != nil; link = link.next {
		chain = append(chain, link.name)
	}
	if strings.Join(chain, ",") != builderGitHub+","+builderFake {
		t.Errorf("the resolution chains %v, want GitHub's then the fake's", chain)
	}
	if !strings.HasPrefix(env.Value("${"+builderGitHub+"}"), "ghs_test_") ||
		!strings.HasPrefix(env.Value("${"+builderFake+"}"), "fake_issued_") {
		t.Error("one resolution did not answer both of the seat's issued variables")
	}
	if tokens.link(builderFake).SourceName() == tokens.link(builderGitHub).SourceName() {
		t.Error("the two sources are named alike, so a shadow warning cannot say which hid a value")
	}
}

// A SEAT'S CHILDREN ARE RENEWED AT THE EARLIEST RENEW TIME of what they were
// issued, whichever issuer's it is, so neither token outlives its holder.
//
// Mutation: renew at the first source's time, or the last's, and one order
// goes red.
func TestRenewalArmsAtTheEarliestRenewTime(t *testing.T) {
	t.Parallel()
	for name, renewIn := range map[string]time.Duration{"the second first": 5 * time.Minute, "the first first": 55 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, c, fake := twoIssuers(t)
			fake.renewIn = renewIn
			_, tokens := e.seatResolver(t.Context(), c, c.Org.AgentSeatByHandle("builder"), toolTokenLife)
			tokens.Lookup(builderGitHub)
			tokens.link(builderFake).Lookup(builderFake)
			gh, _ := tokens.link(builderGitHub).issued()
			fk, _ := tokens.link(builderFake).issued()
			want := gh.RenewAt
			if fk.RenewAt.Before(want) {
				want = fk.RenewAt
			}
			if at, ok := tokens.renewAt(); !ok || !at.Equal(want) {
				t.Errorf("the resolution renews at %v, want the earlier of %v and %v", at, gh.RenewAt, fk.RenewAt)
			}
		})
	}
}

// A RUNNING BOX'S TOKEN FILES ARE KEPT PER VARIABLE: a variable that cannot be
// issued now leaves its file as it is, and is told as the job's wait, while the
// other's is written; once it is issued, its file is written and the wait is
// settled.
func TestTokenFilesAreRefreshedPerVariable(t *testing.T) {
	t.Parallel()
	e, _, fake := twoIssuers(t)
	delete(fake.ready, "builder")
	const ghPath, fakePath = ".config/crewlet/github-token", ".config/crewlet/fake-token"
	defaults := []sandbox.SetupStep{{Name: "auth", Files: map[string]string{
		ghPath: "${" + builderGitHub + "}", fakePath: "${" + builderFake + "}",
	}}}
	box := sandbox.NewFakeSandbox("box-1")
	box.Put(ghPath, "ghs_expired")
	box.Put(fakePath, "fake_expired")
	run := sandbox.PendingRun{TurnID: "turn-1", LaunchID: "job-1", AgentHandle: "builder"}
	job := run.TurnID + "/" + run.LaunchID

	e.refreshTokenFiles(t.Context(), defaults, run, box)
	if held, _ := box.ReadFile(t.Context(), ghPath); !strings.HasPrefix(string(held), "ghs_test_") {
		t.Errorf("GitHub's file holds %q while the other variable waits, want its issued token", held)
	}
	if held, _ := box.ReadFile(t.Context(), fakePath); string(held) != "fake_expired" {
		t.Errorf("a file whose variable was not issued was rewritten to %q", held)
	}
	if got := e.tokenWaits.said[job].story; got != (tokenWait{held: true}) {
		t.Errorf("the job's wait is told as %+v, want the waiting variable's file holding a token", got)
	}

	fake.mu.Lock()
	fake.ready["builder"] = true
	fake.mu.Unlock()
	e.refreshTokenFiles(t.Context(), defaults, run, box)
	if held, _ := box.ReadFile(t.Context(), fakePath); !strings.HasPrefix(string(held), "fake_issued_") {
		t.Errorf("the second variable's file holds %q once it is issued", held)
	}
	if _, waiting := e.tokenWaits.said[job]; waiting {
		t.Error("a job issued every token is still remembered as waiting")
	}
}

// EACH VARIABLE'S STORY IS ITS OWN ISSUER'S: one waiting on GitHub's app and
// one its vendor refused are told in one sentence, each in its issuer's words,
// and two waits arrive with both issuers' steps.
func TestEachIssuersStoryIsToldInItsWords(t *testing.T) {
	t.Parallel()
	e, c, fake := twoIssuers(t)
	fake.refuse = errors.New("fakecloud: POST /token: 503: unavailable")
	fake.passes = true
	planner := c.Org.AgentSeatByHandle("planner")
	ghPlanner, fakePlanner := "GITHUB_APP_TOKEN_PLANNER", "FAKECLOUD_TOKEN_PLANNER"
	env, tokens := e.seatResolver(t.Context(), c, planner, toolTokenLife)
	if env.Value("${"+ghPlanner+"} ${"+fakePlanner+"}") != " " {
		t.Fatal("a seat waiting on its app, refused by the other vendor, was issued a credential")
	}
	m := credentialGap("planner", []string{ghPlanner, fakePlanner}, env,
		&seatMint{kinds: e.mintedFor(c, "planner"), token: tokens})
	says := m.Error()
	for _, want := range []string{
		"${" + fakePlanner + "} could not be issued: fakecloud: POST /token: 503: unavailable; " +
			fake.words().retried,
		"waiting for the github integration to issue ${" + ghPlanner + "}",
	} {
		if !strings.Contains(says, want) {
			t.Errorf("the sentence does not say %q: %s", want, says)
		}
	}
	if m.waiting() {
		t.Error("a refused issue beside a wait reads as only waiting")
	}

	fake.refuse = nil
	delete(fake.ready, "planner")
	env, tokens = e.seatResolver(t.Context(), c, planner, toolTokenLife)
	env.Value("${" + ghPlanner + "} ${" + fakePlanner + "}")
	m = credentialGap("planner", []string{ghPlanner, fakePlanner}, env,
		&seatMint{kinds: e.mintedFor(c, "planner"), token: tokens})
	want := "it starts once the agent's fake cloud account is made and the agent's GitHub App is installed"
	if !m.waiting() || !strings.Contains(m.Error(), want) {
		t.Errorf("two waits are told as %q, want it to say %q", m.Error(), want)
	}
}
