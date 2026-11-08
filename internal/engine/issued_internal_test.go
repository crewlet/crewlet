package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mcp"
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
// refuse while it is set. With settles, an agent not ready waits on the fake
// itself settling its grant rather than on a person's step.
type fakeIssuer struct {
	mu      sync.Mutex
	ready   map[string]bool
	refuse  error
	passes  bool
	settles bool
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
		case !f.ready[handle] && f.settles:
			return issuedToken{}, fmt.Errorf("fakecloud: the agent's grant is still being applied: %w", errIssueSettling)
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
	switch {
	case errors.Is(err, errIssueSettling):
		return "", true
	case errors.Is(err, errFakeWaiting):
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
// rig: refused builds it with the vendor refusing every issue. GitHub's and
// Google Cloud's run through their own fakes; the fake cloud's proves no path
// keys on a kind.
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
	{"aws", awsRig},
	{"google cloud", googleCloudRig},
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
				now, lives := time.Now(), livesOf(issuer)
				if issued.ExpiresAt.Sub(issued.RenewAt) < lives.renewBefore || issued.ExpiresAt.Before(now.Add(lives.run())) ||
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
				r.e.refreshTokenFiles(t.Context(), defaults, sandbox.PendingRun{TurnID: "t", AgentHandle: r.ready}, box, takesEveryToken)
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
				if m == nil || !m.waiting() || tokens.refused() || tokens.settling() {
					t.Fatalf("missing = %v, refused = %v, settling = %v: want a wait on a person's step",
						m, tokens.refused(), tokens.settling())
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
				if m == nil || m.waiting() || !tokens.refused() || tokens.settling() {
					t.Fatalf("missing = %v, refused = %v, settling = %v: want a failure",
						m, tokens.refused(), tokens.settling())
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

	e.refreshTokenFiles(t.Context(), defaults, run, box, takesEveryToken)
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
	e.refreshTokenFiles(t.Context(), defaults, run, box, takesEveryToken)
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

// A VENDOR'S BOX FILE IS WRITTEN AT LAUNCH, KEPT BY THE POLL, AND NEVER IN THE
// ENVIRONMENT. The launch resolves it through the seat's own resolution, so an
// issued variable is issued, in a step of its own ahead of every other; a
// person's file at the same path is dropped for it, being a credential, where
// a person's file at a setting file's path is theirs. It is in
// neither the run's env nor its box defaults, which are all a sign-in is judged
// on, and only its path is kept for the launch, under its variable. It is
// scrubbed as a credential. The poll rewrites a file marked rewritten once its
// token renews, through the refresh that keeps a person's token files, and
// leaves the rest to the launch.
func TestAVendorsBoxFileIsWrittenAtLaunchAndKeptByThePoll(t *testing.T) {
	t.Parallel()
	e, c, fake := twoIssuers(t)
	const path, setting = ".config/crewlet/fake-token", "FAKE_CLI_TOKEN_FILE"
	file := &seataccess.BoxFile{Path: path, Ref: "${" + builderFake + "}", PathEnv: setting, Rewritten: true}
	e.boxSupply = func(_ *config.Company, handle string) []seataccess.BoxDefault {
		if handle != "builder" {
			return nil
		}
		return []seataccess.BoxDefault{{Kind: fakeCloud, File: file}}
	}
	builder := c.Org.AgentSeatByHandle("builder")
	run := e.runEnvironment(t.Context(), c, builder, nil, nil, e.boxDefaults(c.Config, "builder"))
	if len(run.setup) == 0 || run.setup[0].Name != integrationFilesStep {
		t.Fatalf("setup = %+v, want the integrations' files first", run.setup)
	}
	token := run.setup[0].Files[path]
	if !strings.HasPrefix(token, "fake_issued_") {
		t.Fatalf("the vendor's file holds %q, want the issued token", token)
	}
	for _, env := range []map[string]string{run.env, run.defaults} {
		for name, value := range env {
			if value == token {
				t.Errorf("the file's token reached the environment as %s", name)
			}
		}
	}
	if run.homePaths[setting] != path || run.env[setting] != "" || run.defaults[setting] != "" {
		t.Errorf("the file's variable is %q kept, %q in the env, %q in the defaults: want only its "+
			"path under the home, kept for the launch", run.homePaths[setting], run.env[setting], run.defaults[setting])
	}
	if !slices.Contains(run.secrets, token) {
		t.Error("the file's token is not scrubbed from the run's output")
	}
	// A CREDENTIAL FILE IS THE INTEGRATION'S, so a person's file at its path,
	// spelt from the home or not, is dropped for it; a setting file is theirs.
	mine := []sandbox.SetupStep{{Name: "mine", Files: map[string]string{"~/" + path: "a person's own"}}}
	own := e.runEnvironment(t.Context(), c, builder, nil, mine, e.boxDefaults(c.Config, "builder"))
	if own.setup[0].Name != integrationFilesStep || !strings.HasPrefix(own.setup[0].Files[path], "fake_issued_") {
		t.Errorf("setup = %+v, want the vendor's credential file over a person's at its path", own.setup)
	}
	if _, kept := own.setup[1].Files["~/"+path]; kept || own.homePaths[setting] != path {
		t.Errorf("a person's file at a credential file's path was kept (%v), or its variable lost (%v)",
			own.setup[1].Files, own.homePaths)
	}
	literal := *file
	literal.Ref, literal.Rewritten = "[default]\nregion = fr-par\n", false
	settingSupply := []seataccess.BoxDefault{{Kind: fakeCloud, File: &literal}}
	setting2 := e.runEnvironment(t.Context(), c, builder, nil, mine, settingSupply)
	if setting2.setup[0].Name == integrationFilesStep || setting2.homePaths != nil {
		t.Error("the vendor's setting file was written over a person's file at the same path")
	}

	box := sandbox.NewFakeSandbox("box-1")
	box.Put(path, token)
	run2 := sandbox.PendingRun{TurnID: "turn-1", LaunchID: "job-1", AgentHandle: "builder"}
	renew := func() {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		held := fake.held["builder"]
		held.ExpiresAt = time.Now().Add(time.Minute)
		fake.held["builder"] = held
	}
	renew()
	e.refreshTokenFiles(t.Context(), nil, run2, box, takesEveryToken)
	renewed, _ := box.ReadFile(t.Context(), path)
	if !strings.HasPrefix(string(renewed), "fake_issued_") || string(renewed) == token {
		t.Fatalf("the poll left %q in the vendor's file once its token renewed, want the new token", renewed)
	}
	file.Rewritten = false
	renew()
	e.refreshTokenFiles(t.Context(), nil, run2, box, takesEveryToken)
	if again, _ := box.ReadFile(t.Context(), path); string(again) != string(renewed) {
		t.Error("the poll rewrote a vendor's file the vendor left to the launch")
	}
}

// A BOX FILE'S VARIABLE NAMES A FILE THE BOX HOLDS: one the launch writes, or
// a rewritten one whose token waits, which the poll writes once issued; never
// one left out for good, and never a name the seat's own box settings set,
// which are theirs. A file of literal text is written as it is, names no
// variable, and the poll leaves it alone. Mutation: keep every file's
// variable, or keep it over the seat's own line, and this goes red.
func TestABoxFilesVariableNamesAFileTheBoxHolds(t *testing.T) {
	t.Parallel()
	e, c, fake := twoIssuers(t)
	const issued, unset, literal = ".config/crewlet/issued", ".config/crewlet/unset", ".config/crewlet/config"
	const text = "[default]\nregion = fr-par\n"
	e.boxSupply = func(*config.Company, string) []seataccess.BoxDefault {
		return []seataccess.BoxDefault{
			{Kind: fakeCloud, File: &seataccess.BoxFile{Path: issued, Ref: "${" + builderFake + "}",
				PathEnv: "CLI_TOKEN_FILE", Rewritten: true}},
			{Kind: fakeCloud, File: &seataccess.BoxFile{Path: unset, Ref: "${FAKECLOUD_NEVER_SET}",
				PathEnv: "CLI_OTHER_FILE"}},
			{Kind: fakeCloud, File: &seataccess.BoxFile{Path: literal, Ref: text}},
		}
	}
	builder := c.Org.AgentSeatByHandle("builder")
	launch := func(settings *org.RoleSandbox) runEnv {
		return e.runEnvironment(t.Context(), c, builder, settings, nil, e.boxDefaults(c.Config, "builder"))
	}

	run := launch(nil)
	if want := map[string]string{"CLI_TOKEN_FILE": issued}; !maps.Equal(run.homePaths, want) {
		t.Errorf("the kept variables are %v, want %v: the written file's alone", run.homePaths, want)
	}
	if got := run.setup[0].Files[literal]; got != text {
		t.Errorf("the literal file holds %q, want its text as written", got)
	}
	if _, written := run.setup[0].Files[unset]; written {
		t.Error("a file whose reference does not resolve was written")
	}

	box := sandbox.NewFakeSandbox("box-1")
	box.Put(literal, text)
	e.refreshTokenFiles(t.Context(), nil, sandbox.PendingRun{TurnID: "turn-1", LaunchID: "job-1", AgentHandle: "builder"}, box, takesEveryToken)
	if got, _ := box.ReadFile(t.Context(), literal); string(got) != text {
		t.Errorf("the poll wrote %q over the literal file", got)
	}

	own := launch(&org.RoleSandbox{Env: map[string]string{"CLI_TOKEN_FILE": "/elsewhere"}})
	if own.homePaths != nil || own.env["CLI_TOKEN_FILE"] != "/elsewhere" {
		t.Errorf("the seat's own line gave env %q and kept %v, want the line alone",
			own.env["CLI_TOKEN_FILE"], own.homePaths)
	}

	fake.mu.Lock()
	delete(fake.ready, "builder")
	fake.mu.Unlock()
	waiting := launch(nil)
	if waiting.homePaths["CLI_TOKEN_FILE"] != issued {
		t.Errorf("a rewritten file waiting on its token kept %v, want its variable for the poll's write",
			waiting.homePaths)
	}
	if _, written := waiting.setup[0].Files[issued]; written {
		t.Error("a file whose token waits was written at launch")
	}
}

// A SETTLING ISSUE IS RETRIED UNTIL ITS TOOL GOES LIVE, with no revision, no
// seal and nobody acting: the integration's own wait clears by itself, so the
// seat's children ask again on the restart schedule ([seatToken.settling]).
// Meanwhile the server reads waiting, never failed, and nothing is written at
// error. Mutation: retry only a refusal, and the server waits for an apply
// that never comes.
func TestASettlingIssuersToolGoesLiveOnTheNextRetry(t *testing.T) {
	logs := captureEngineLogs(t)
	company, _ := helperCompany(t, false, nil)
	fake := newFakeIssuer()
	fake.settles = true
	company.Org.AgentSeatByHandle("ceo").MCPEnv["tracker"]["SEAT_TOKEN"] = "${" + fake.variable(company, "ceo") + "}"
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), mcpRetryBase: 50 * time.Millisecond,
		issuers: []seatIssuer{fake}}
	e.epoch.current.Store(company)
	ceo := heldSeatChildren(t, e, "ceo", func() bool { return true })

	e.convergeSeat(t.Context(), ceo, true)
	readings := e.mcpReadings(t.Context())
	if len(readings) != 1 || readings[0].state != coord.MCPInstanceWaiting {
		t.Fatalf("the server while its grant settles reads %+v, want one instance waiting", readings)
	}
	ceo.mu.Lock()
	armed := ceo.retry != nil
	ceo.mu.Unlock()
	if !armed {
		t.Fatal("a settling issue armed no retry, so only an apply would start the server")
	}

	fake.mu.Lock()
	fake.ready["ceo"] = true
	fake.mu.Unlock()
	for deadline := time.Now().Add(10 * time.Second); !seatChild(ceo).Serving; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the server never went live once its grant settled: %+v", e.mcpReadings(t.Context()))
		}
	}
	logs.mu.Lock()
	written := logs.buf.String()
	logs.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(written), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) && strings.Contains(line, `"seat":"ceo"`) {
			t.Errorf("a settling issue was written at error: %s", line)
		}
	}
	if awaiting := logs.records("mcp_server_awaiting_credential", "ceo"); len(awaiting) == 0 ||
		awaiting[0]["level"] != "INFO" {
		t.Errorf("mcp_server_awaiting_credential = %v, want the wait said at info", awaiting)
	}
}

// takesEveryToken is a recorder that takes every token, as the set beside a
// running job's row does: the refresh writes whatever it is handed.
func takesEveryToken(context.Context, string, time.Time) error { return nil }

// A TOKEN THE POLL RENEWS IS RECORDED BEFORE IT IS WRITTEN, once for every file
// of its variable, with the expiry its issuer gave it: the job's sealed set,
// which every capture of its session is scrubbed by, holds the token before any
// file of the box does. A poll that finds the files current records nothing.
//
// Mutation: write a file before its token is recorded, record the token once
// per file, record it with no expiry, or record it on a poll that writes
// nothing, and a check below goes red.
func TestATokenThePollRenewsIsRecordedBeforeItIsWritten(t *testing.T) {
	t.Parallel()
	e, _, fake := twoIssuers(t)
	e.issuers = []seatIssuer{fake}
	const first, second = ".config/crewlet/fake-token", ".config/other/fake-token"
	paths := []string{first, second}
	defaults := []sandbox.SetupStep{{Name: "auth", Files: map[string]string{
		first: "${" + builderFake + "}", second: "${" + builderFake + "}",
	}}}
	box := sandbox.NewFakeSandbox("box-1")
	for _, path := range paths {
		box.Put(path, "fake_expired")
	}
	run := sandbox.PendingRun{TurnID: "turn-1", LaunchID: "job-1", AgentHandle: "builder"}
	type recorded struct {
		value   string
		expires time.Time
		held    []string
	}
	var records []recorded
	record := func(ctx context.Context, value string, expires time.Time) error {
		var held []string
		for _, path := range paths {
			got, _ := box.ReadFile(ctx, path)
			held = append(held, string(got))
		}
		records = append(records, recorded{value: value, expires: expires, held: held})
		return nil
	}

	e.refreshTokenFiles(t.Context(), defaults, run, box, record)
	if len(records) != 1 {
		t.Fatalf("the renewed token was recorded %d times, want once for both its files", len(records))
	}
	got := records[0]
	for i, path := range paths {
		if written, _ := box.ReadFile(t.Context(), path); string(written) != got.value {
			t.Errorf("%s holds %q, want the token recorded, %q", path, written, got.value)
		}
		if got.held[i] == got.value {
			t.Errorf("%s held the token when it was recorded: written before its record", path)
		}
	}
	fake.mu.Lock()
	issued := fake.held["builder"]
	fake.mu.Unlock()
	if got.value != issued.Token || !got.expires.Equal(issued.ExpiresAt) {
		t.Errorf("recorded %q lapsing at %v, want the issued %q at its expiry, %v",
			got.value, got.expires, issued.Token, issued.ExpiresAt)
	}

	e.refreshTokenFiles(t.Context(), defaults, run, box, record)
	if len(records) != 1 {
		t.Errorf("a poll that found the files current recorded the token again (%d records)", len(records))
	}
}

// A RENEWAL THE STORE DID NOT TAKE IS NOT WRITTEN: none of its variable's
// files, on this poll or any poll after, until a record lands. A token in the
// box that its job's set does not hold is one no capture of the job's session is
// scrubbed of. The refusal is told once while it lasts and settled by the record
// that lands; and a job the run's record no longer holds running is written
// nothing and told nothing, since the poll that reached it is a moment old.
// Not parallel: it adds a destination to the process-wide logger.
//
// Mutation: write the files whatever the record answered, tell the refusal on
// every poll, or tell a job that moved on, and a check below goes red.
func TestARenewalTheStoreDidNotTakeIsNotWritten(t *testing.T) {
	logs := captureEngineLogs(t)
	e, _, fake := twoIssuers(t)
	e.issuers = []seatIssuer{fake}
	const path = ".config/crewlet/fake-token"
	defaults := []sandbox.SetupStep{{Name: "auth", Files: map[string]string{path: "${" + builderFake + "}"}}}
	box := sandbox.NewFakeSandbox("box-1")
	box.Put(path, "fake_expired")
	run := sandbox.PendingRun{TurnID: "turn-1", LaunchID: "job-1", AgentHandle: "builder"}
	unanswered := func(context.Context, string, time.Time) error {
		return errors.New("sandbox: record what run turn-1's box is handed: coord: the store did not answer")
	}

	for range 3 {
		e.refreshTokenFiles(t.Context(), defaults, run, box, unanswered)
	}
	if held, _ := box.ReadFile(t.Context(), path); string(held) != "fake_expired" {
		t.Fatalf("a token its record did not take was written: the file holds %q", held)
	}
	told := logs.records("sandbox_token_refresh_failed", "builder")
	if len(told) != 1 {
		t.Fatalf("three polls whose record the store did not take logged %d lines, want one", len(told))
	}
	detail, _ := told[0]["detail"].(string)
	for _, want := range []string{"could not be recorded", "did not answer", "keep the token they hold"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the line does not say %q: %s", want, detail)
		}
	}

	e.refreshTokenFiles(t.Context(), defaults, run, box, takesEveryToken)
	if held, _ := box.ReadFile(t.Context(), path); !strings.HasPrefix(string(held), "fake_issued_") {
		t.Fatalf("the poll whose record landed left %q in the file", held)
	}
	if _, said := e.tokenWaits.said[tokenRecordJob(run.TurnID+"/"+run.LaunchID, builderFake)]; said {
		t.Error("the record that landed did not settle the refusal")
	}

	box.Put(path, "fake_expired")
	moved := func(context.Context, string, time.Time) error { return sandbox.ErrJobNotRunning }
	e.refreshTokenFiles(t.Context(), defaults, run, box, moved)
	if held, _ := box.ReadFile(t.Context(), path); string(held) != "fake_expired" {
		t.Errorf("a token was written into the box of a job its row no longer holds running: %q", held)
	}
	if got := len(logs.records("sandbox_token_refresh_failed", "builder")); got != 1 {
		t.Errorf("a job that moved on was told as a failure: %d lines in all, want the one", got)
	}
}
