package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// appGitHub stands in for an Enterprise Server's token endpoint: every mint
// is a fresh, numbered token good for an hour, unless refuse is set.
type appGitHub struct {
	mu     sync.Mutex
	mints  int
	refuse bool
	url    string

	// asked is the permissions each mint requested, in order.
	asked []map[string]string
}

func newAppGitHub(t *testing.T) *appGitHub {
	t.Helper()
	g := &appGitHub{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.refuse {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"This installation has been suspended"}`))
			return
		}
		var body struct {
			Permissions map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("a mint request was not the token endpoint's JSON: %v", err)
		}
		g.asked = append(g.asked, body.Permissions)
		g.mints++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_test_" + strings.Repeat("x", g.mints),
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

func (g *appGitHub) minted() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mints
}

// appCompany is a company on GitHub at server, where `builder` holds an app
// that is created and installed and `planner` one that is created and not
// installed yet. Both name their own token for the GitHub MCP server, and
// `builder` also names `planner`'s, which must answer nothing for it.
func appCompany(t *testing.T, server string, enabled bool) *Company {
	t.Helper()
	return appCompanyWith(t, server, enabled, "")
}

// appCompanyWith is [appCompany] with builderGitHub appended to `builder`'s
// own `integrations.github` block.
func appCompanyWith(t *testing.T, server string, enabled bool, builderGitHub string) *Company {
	t.Helper()
	state := "true"
	if !enabled {
		state = "false"
	}
	return specCompany(t, `
name: GoPhrase
providers:
  llm:
    zulu:
      model: claude-sonnet-5
  sandbox: {fake: true}
integrations:
  github:
    enabled: `+state+`
    url: "`+server+`"
    webhook_secret: "${GITHUB_WEBHOOK_SECRET}"
    provisioning:
      org: gophrase
mcp_servers:
  - name: github
    transport: http
    shared: false
    url: "https://mcp.example.com/"
  - name: borrowed
    transport: http
    shared: false
    url: "https://mcp.example.com/"
roles:
  - name: Owner
    handle: owner
    kind: human
  - name: Builder
    handle: builder
    llm: zulu
    mcp_env:
      github:
        Authorization: "Bearer ${GITHUB_APP_TOKEN_BUILDER}"
      borrowed:
        Authorization: "Bearer ${GITHUB_APP_TOKEN_PLANNER}"
    integrations:
      github:
        tier: full_access
        repos: [gophrase/backend]
        app_id: 101
        app_slug: gophrase-builder
        installation_id: 9001
        private_key: "${GITHUB_APP_KEY_BUILDER}"
`+builderGitHub+`  - name: Planner
    handle: planner
    llm: zulu
    mcp_env:
      github:
        Authorization: "Bearer ${GITHUB_APP_TOKEN_PLANNER}"
    integrations:
      github:
        tier: review
        app_id: 102
        app_slug: gophrase-planner
        private_key: "${GITHUB_APP_KEY_PLANNER}"
`)
}

// appEngine is an engine whose secret store holds both apps' keys.
func appEngine(t *testing.T, c *Company) *Engine {
	t.Helper()
	e := &Engine{environ: config.MapSource{
		"GITHUB_APP_KEY_BUILDER": testAppKey(t),
		"GITHUB_APP_KEY_PLANNER": testAppKey(t),
		"GITHUB_WEBHOOK_SECRET":  "s3cret",
	}}
	e.epoch.current.Store(c)
	return e
}

// A SEAT'S APP TOKEN IS ANSWERED BY ITS OWN RESOLUTION AND NO OTHER.
//
// An app is one agent's identity. The token is laid in front of the
// company's sources for that seat's reads alone, so a seat naming another
// seat's variable names something nothing answers, and the company's chain,
// which every shared server and every other seat reads, never holds it.
func TestASeatsAppTokenIsAnsweredOnlyByItsOwnResolution(t *testing.T) {
	t.Parallel()
	gh := newAppGitHub(t)
	c := appCompany(t, gh.url, true)
	e := appEngine(t, c)
	builder := c.Org.AgentSeatByHandle("builder")

	env, token := e.seatResolver(t.Context(), c, builder, github.ToolTokenLife)
	own := env.Value("${GITHUB_APP_TOKEN_BUILDER}")
	if !strings.HasPrefix(own, "ghs_test_") {
		t.Fatalf("the seat's own token resolved to %q", own)
	}
	if issued, ok := token.issued(); !ok || issued.Token != own {
		t.Error("the resolution does not report the token it handed out")
	}
	if other := env.Value("${GITHUB_APP_TOKEN_PLANNER}"); other != "" {
		t.Error("a seat's resolution answered another seat's app token")
	}
	if shared := e.resolver().Value("${GITHUB_APP_TOKEN_BUILDER}"); shared != "" {
		t.Error("the company's own chain answered a seat's app token")
	}

	// ONE MINT for a seat's whole pass, and for the next pass inside the
	// hour: the issuer is the engine's.
	_, again := e.seatResolver(t.Context(), c, builder, github.ToolTokenLife)
	again.Lookup(again.name)
	if gh.minted() != 1 {
		t.Errorf("two reads of one seat's token minted %d, want one", gh.minted())
	}
}

// NOTHING IS ISSUED WHILE GITHUB IS SWITCHED OFF. Switching the block off is
// the gesture an operator makes after a leak, and it has to stop every token
// the engine hands out, at the next read.
func TestADisabledGitHubIssuesNoToken(t *testing.T) {
	t.Parallel()
	gh := newAppGitHub(t)
	c := appCompany(t, gh.url, false)
	e := appEngine(t, c)

	env, token := e.seatResolver(t.Context(), c, c.Org.AgentSeatByHandle("builder"), github.ToolTokenLife)
	if token != nil || env.Value("${GITHUB_APP_TOKEN_BUILDER}") != "" {
		t.Fatal("a company with GitHub off was issued a seat's app token")
	}
	if _, declared := e.mintedFor(c, "builder")["GITHUB_APP_TOKEN_BUILDER"]; declared {
		t.Error("the token is declared as on its way while nothing will issue it")
	}
	if gh.minted() != 0 {
		t.Errorf("%d tokens were minted for a company with GitHub off", gh.minted())
	}
}

// THE TOKEN IS DECLARED AS THE GITHUB INTEGRATION'S FOR EVERY AGENT SEAT, and
// for no person: a person holds no app, and an agent naming its token before
// its app exists is waiting on the step the card asks for.
func TestEveryAgentSeatsTokenIsDeclaredAsGitHubs(t *testing.T) {
	t.Parallel()
	c := appCompany(t, "https://github.example.com", true)
	e := &Engine{}
	for handle, want := range map[string]string{
		"builder": "GITHUB_APP_TOKEN_BUILDER",
		"planner": "GITHUB_APP_TOKEN_PLANNER",
	} {
		if kind := e.mintedFor(c, handle)[want]; kind != integration.KindGitHub {
			t.Errorf("%s's %s is declared as %q, want github", handle, want, kind)
		}
	}
	if minted := e.mintedFor(c, "owner"); len(minted) != 0 {
		t.Errorf("a person's seat is declared %v", minted)
	}
}

// A SEAT WHOSE APP IS NOT INSTALLED YET WAITS FOR IT, and says what for.
//
// The variable is the GitHub integration's to issue, so the server naming it
// is held back as waiting, not reported as a secret the operator forgot, and
// the sentence names the app's installation rather than a seal that will
// never come.
func TestAServerNamingAnUninstalledAppsTokenWaitsForTheInstall(t *testing.T) {
	t.Parallel()
	gh := newAppGitHub(t)
	c := appCompany(t, gh.url, true)
	e := appEngine(t, c)
	planner := c.Org.AgentSeatByHandle("planner")

	env, token := e.seatResolver(t.Context(), c, planner, github.ToolTokenLife)
	specs, outcomes := seatSpecs(c, planner, env,
		&seatMint{kinds: e.mintedFor(c, "planner"), token: token})
	if len(specs) != 0 || len(outcomes) != 1 {
		t.Fatalf("specs = %+v, outcomes = %+v: want the server held back", specs, outcomes)
	}
	var missing *credentialMissing
	if !errors.As(outcomes[0].err, &missing) || !missing.waiting() {
		t.Fatalf("outcome = %v, want a server waiting", outcomes[0].err)
	}
	says := missing.Error()
	for _, want := range []string{
		"the github integration to issue ${GITHUB_APP_TOKEN_PLANNER}",
		"once the agent's GitHub App is installed",
	} {
		if !strings.Contains(says, want) {
			t.Errorf("the sentence does not say %q: %s", want, says)
		}
	}
	if strings.Contains(says, "seal") {
		t.Errorf("the sentence promises a seal that never comes: %s", says)
	}
	if gh.minted() != 0 {
		t.Error("an app installed nowhere was asked for a token")
	}
}

// A TOKEN GITHUB WOULD NOT ISSUE IS A FAILURE WITH GITHUB'S REASON, never a
// wait: the app is installed, so there is no step outstanding, and the reason
// is what an operator acts on. It carries no token.
func TestATokenGitHubRefusesIsAFailureWithItsReason(t *testing.T) {
	t.Parallel()
	gh := newAppGitHub(t)
	gh.refuse = true
	c := appCompany(t, gh.url, true)
	e := appEngine(t, c)
	builder := c.Org.AgentSeatByHandle("builder")

	env, token := e.seatResolver(t.Context(), c, builder, github.ToolTokenLife)
	_, outcomes := seatSpecs(c, builder, env,
		&seatMint{kinds: e.mintedFor(c, "builder"), token: token})
	var refused *credentialMissing
	for _, o := range outcomes {
		var m *credentialMissing
		if errors.As(o.err, &m) && m.server == "github" {
			refused = m
		}
	}
	if refused == nil {
		t.Fatalf("outcomes = %+v, want the github server refused", outcomes)
	}
	if refused.waiting() {
		t.Error("a refused mint reads as waiting for a step nobody owes")
	}
	says := refused.Error()
	if !strings.Contains(says, "${GITHUB_APP_TOKEN_BUILDER} could not be issued") ||
		!strings.Contains(says, "403") {
		t.Errorf("the sentence does not carry GitHub's refusal: %s", says)
	}
	if token.unissued(token.name) == "" {
		t.Error("the resolution does not report the refusal it met")
	}
}

// A RUN'S ENVIRONMENT AND ITS FILES CARRY THE SEAT'S TOKEN, so a setup step's
// credential helper and `gh` act as the agent's own app from the first
// command.
func TestARunEnvironmentCarriesTheSeatsAppToken(t *testing.T) {
	t.Parallel()
	gh := newAppGitHub(t)
	c := appCompany(t, gh.url, true)
	e := appEngine(t, c)
	builder := c.Org.AgentSeatByHandle("builder")

	defaults := []sandbox.SetupStep{{
		Name:  "git-auth",
		Files: map[string]string{".config/crewlet/github-token": "${GITHUB_APP_TOKEN_BUILDER}"},
	}}
	run := e.runEnvironment(t.Context(), c, builder,
		&org.RoleSandbox{Env: map[string]string{"GITHUB_TOKEN": "${GITHUB_APP_TOKEN_BUILDER}"}},
		defaults)
	if run.gap != nil {
		t.Fatalf("the run lacks %s", run.gap.reason())
	}
	token := run.env["GITHUB_TOKEN"]
	if !strings.HasPrefix(token, "ghs_test_") {
		t.Fatalf("GITHUB_TOKEN = %q, want the seat's app token", token)
	}
	if got := run.setup[0].Files[".config/crewlet/github-token"]; got != token {
		t.Errorf("the token file holds %q, want the token the environment holds", got)
	}
}

// A LONG RUN'S TOKEN FILE IS KEPT CURRENT, AND A QUIET POLL WRITES NOTHING.
//
// A coding run is never stopped on a clock, so the token it launched with
// expires under it. The poll rewrites the file a credential helper reads when
// the token behind it is renewed, and leaves it alone otherwise, asking the
// box rather than remembering, so the fleet waiter's duty can move between
// nodes, and a node can restart, with nothing to carry over.
func TestALongRunsTokenFileIsKeptCurrent(t *testing.T) {
	t.Parallel()
	gh := newAppGitHub(t)
	c := appCompany(t, gh.url, true)
	e := appEngine(t, c)
	const path = ".config/crewlet/github-token"
	defaults := []sandbox.SetupStep{{
		Name: "git-auth", Files: map[string]string{path: "${GITHUB_APP_TOKEN_BUILDER}"},
	}}
	box := sandbox.NewFakeSandbox("box-1")
	box.Put(path, "ghs_expired")
	run := sandbox.PendingRun{TurnID: "turn-1", AgentHandle: "builder"}

	e.refreshTokenFiles(t.Context(), defaults, run, box)
	held, err := box.ReadFile(t.Context(), path)
	if err != nil || !strings.HasPrefix(string(held), "ghs_test_") {
		t.Fatalf("the token file holds %q (%v), want the issued token", held, err)
	}
	e.refreshTokenFiles(t.Context(), defaults, run, box)
	if again, _ := box.ReadFile(t.Context(), path); string(again) != string(held) {
		t.Error("a poll with nothing renewed changed the token")
	}
	if gh.minted() != 1 {
		t.Errorf("a quiet poll minted %d tokens, want the held one", gh.minted())
	}

	// A SEAT NAMING NO TOKEN FILE IS NEVER ASKED ABOUT.
	other := sandbox.NewFakeSandbox("box-2")
	e.refreshTokenFiles(t.Context(), nil, run, other)
	if b, _ := other.ReadFile(t.Context(), path); len(b) != 0 {
		t.Error("a box provisioned with no token file was given one")
	}
}

// BOTH OF A NODE'S WAITERS KEEP A LONG RUN'S TOKEN FILE CURRENT.
//
// Each box is polled by one waiter alone: a `direct` or `container` box by its
// launching node's own, and an `e2b` box by the fleet's (ADR-0059). The
// refresh rides the poll, so a waiter built without it leaves every box of
// its kind holding the token it launched with, which GitHub refuses within
// the hour, halfway through a long run's work.
func TestBothWaitersKeepARunsTokenFileCurrent(t *testing.T) {
	t.Parallel()
	gh := newAppGitHub(t)
	c := appCompany(t, gh.url, true)
	e := appEngine(t, c)
	e.backends = &Backends{}
	const path = ".config/crewlet/github-token"
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
		Runners:   map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
		DefaultSetup: []sandbox.SetupStep{{
			Name: "git-auth", Files: map[string]string{path: "${GITHUB_APP_TOKEN_BUILDER}"},
		}},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	for _, drives := range sandbox.AllDrives {
		opts := e.sandboxWaiterOptions(&sandboxRuntime{}, drives, time.Second)
		if opts.Drives != drives {
			t.Fatalf("the options built for the %s waiter drive %s", drives, opts.Drives)
		}
		if opts.Refresh == nil {
			t.Errorf("the %s waiter refreshes nothing it polls", drives)
			continue
		}
		box := sandbox.NewFakeSandbox("box-" + string(drives))
		box.Put(path, "ghs_expired")
		opts.Refresh(t.Context(), manager, sandbox.PendingRun{
			TurnID: "turn-" + string(drives), LaunchID: "job-1", AgentHandle: "builder",
		}, box)
		if held, _ := box.ReadFile(t.Context(), path); !strings.HasPrefix(string(held), "ghs_test_") {
			t.Errorf("the %s waiter's refresh left the token file holding %q", drives, held)
		}
	}
}

// A RUN LAUNCHED WITHOUT ITS TOKEN IS HANDED THE FILE ONCE THE APP ISSUES ONE,
// AND THE WAIT BEFORE THAT IS TOLD AS ONE, NOT AS A FAILURE ON EVERY POLL.
//
// Every agent turn is an executor run, which starts without a value its seat
// cannot resolve yet rather than being refused for it, so a running box can
// name a token file its launch could not write. A file that is missing reads
// back as nothing, which the issued token differs from, so the first poll
// after the app can issue one writes it. Until then the seat's app is a
// person's step away: a job that never held a token is waiting on it, one
// whose file holds a token the app no longer renews has lost it, and the two
// are different stories, each told once for the job rather than once a poll.
func TestARunLaunchedWithoutItsTokenGetsTheFileOnceTheAppIssuesOne(t *testing.T) {
	t.Parallel()
	gh := newAppGitHub(t)
	c := appCompany(t, gh.url, true)
	e := appEngine(t, c)
	const path = ".config/crewlet/github-token"
	steps := func(handle string) []sandbox.SetupStep {
		return []sandbox.SetupStep{{Name: "git-auth", Files: map[string]string{
			path: "${" + github.SeatVar(handle, github.FieldAppToken) + "}"}}}
	}

	// INSTALLED: the box was launched before it was, and holds no file; an
	// earlier poll found the job waiting on the install.
	installed := sandbox.NewFakeSandbox("box-installed")
	run := sandbox.PendingRun{TurnID: "turn-1", LaunchID: "job-1", AgentHandle: "builder"}
	e.tokenWaits.first(run.TurnID+"/"+run.LaunchID, tokenAwaited, time.Hour)
	e.refreshTokenFiles(t.Context(), steps("builder"), run, installed)
	if held, err := installed.ReadFile(t.Context(), path); err != nil || !strings.HasPrefix(string(held), "ghs_test_") {
		t.Fatalf("the file a run was launched without holds %q (%v) once the app issues a token", held, err)
	}
	if _, waiting := e.tokenWaits.said[run.TurnID+"/"+run.LaunchID]; waiting {
		t.Error("a job whose token was issued is still remembered as waiting, so " +
			"losing it again would go unsaid")
	}

	// NOT INSTALLED: nothing is written, and the job is waiting on the app.
	awaited := sandbox.NewFakeSandbox("box-awaited")
	planner := sandbox.PendingRun{TurnID: "turn-2", LaunchID: "job-2", AgentHandle: "planner"}
	e.refreshTokenFiles(t.Context(), steps("planner"), planner, awaited)
	if held, _ := awaited.ReadFile(t.Context(), path); len(held) != 0 {
		t.Fatalf("a seat whose app is not installed had %q written", held)
	}
	if got := e.tokenWaits.said[planner.TurnID+"/"+planner.LaunchID].story; got != tokenAwaited {
		t.Errorf("a job launched without the token is told as %q, want %q", got, tokenAwaited)
	}

	// A FILE HOLDING A TOKEN THE APP NO LONGER RENEWS is the other story.
	lapsed := sandbox.NewFakeSandbox("box-lapsed")
	lapsed.Put(path, "ghs_before_the_uninstall")
	gone := sandbox.PendingRun{TurnID: "turn-3", LaunchID: "job-3", AgentHandle: "planner"}
	e.refreshTokenFiles(t.Context(), steps("planner"), gone, lapsed)
	if got := e.tokenWaits.said[gone.TurnID+"/"+gone.LaunchID].story; got != tokenLapsed {
		t.Errorf("a job holding a token its app no longer renews is told as %q, want %q", got, tokenLapsed)
	}
	if held, _ := lapsed.ReadFile(t.Context(), path); string(held) != "ghs_before_the_uninstall" {
		t.Errorf("a refresh that issued nothing rewrote the file to %q", held)
	}
	if gh.minted() != 1 {
		t.Errorf("%d mints, want the installed app's one", gh.minted())
	}
}

// A JOB'S WAIT IS TOLD ONCE: again only when its story changes, once it has
// resolved a token and waits anew, or once no poll on this node has found it
// for longer than a live job goes unpolled.
func TestATokenWaitIsToldOncePerJob(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	w := &tokenWaits{now: func() time.Time { return now }}
	const forget = time.Minute
	steps := []struct {
		job   string
		story tokenWait
		after time.Duration
		tell  bool
		why   string
	}{
		{"t/1", tokenAwaited, 0, true, "a job's first wait"},
		{"t/1", tokenAwaited, 15 * time.Second, false, "the same wait on the next poll"},
		{"t/1", tokenAwaited, 15 * time.Second, false, "the same wait on a third"},
		{"t/2", tokenAwaited, 0, true, "another job's wait"},
		{"t/1", tokenLapsed, 15 * time.Second, true, "a changed story"},
		{"t/1", tokenLapsed, 50 * time.Second, false, "a job polled within the horizon"},
		{"t/2", tokenAwaited, 0, true, "a job no poll found for longer than the horizon"},
	}
	for _, step := range steps {
		now = now.Add(step.after)
		if got := w.first(step.job, step.story, forget); got != step.tell {
			t.Errorf("%s: told = %v, want %v", step.why, got, step.tell)
		}
	}
	w.settle("t/1")
	if !w.first("t/1", tokenLapsed, forget) {
		t.Error("a job that resolved its token and lost it again was not told")
	}
}

// tokenFiles finds only whole references to the seat's own variable: a file
// that mentions it inside other text is the box's own content, expanded
// there, and a file naming another seat's is not this seat's to keep.
func TestTokenFilesAreTheWholeReferencesToTheSeatsOwnVariable(t *testing.T) {
	t.Parallel()
	steps := []sandbox.SetupStep{
		{Name: "a", Files: map[string]string{
			"one":   "${GITHUB_APP_TOKEN_BUILDER}",
			"inner": "token=${GITHUB_APP_TOKEN_BUILDER}",
			"other": "${GITHUB_APP_TOKEN_PLANNER}",
		}},
		{Name: "b", Files: map[string]string{"two": " ${GITHUB_APP_TOKEN_BUILDER} "}},
	}
	got := tokenFiles(steps, "GITHUB_APP_TOKEN_BUILDER")
	if strings.Join(got, ",") != "one,two" {
		t.Errorf("tokenFiles = %v, want [one two]", got)
	}
}

// A SEAT'S CHILDREN ARE RENEWED BEFORE THEIR TOKEN RUNS SHORT, and a seat
// whose children hold none arms nothing.
func TestARenewalIsArmedOnlyForAnIssuedToken(t *testing.T) {
	t.Parallel()
	gh := newAppGitHub(t)
	c := appCompany(t, gh.url, true)
	e := appEngine(t, c)
	s := &seatChildren{handle: "builder", lease: t.Context()}
	s.idle.L = &s.mu

	_, token := e.seatResolver(t.Context(), c, c.Org.AgentSeatByHandle("builder"), github.ToolTokenLife)
	e.armRenewal(s, token)
	if s.renew != nil {
		t.Fatal("a pass that was never asked for the token armed a renewal")
	}
	token.Lookup(token.name)
	e.armRenewal(s, token)
	if s.renew == nil {
		t.Fatal("children started with an issued token have no renewal armed")
	}
	s.renew.Stop()

	e.armRenewal(s, nil)
	if s.renew != nil {
		t.Error("a pass that issued nothing left a renewal armed")
	}
}

// A SEAT'S OPT-IN TO WORKFLOW WRITES REACHES THE MINT, and its silence does
// too. The document is the one place the opt-in is written, and a token is
// the one place GitHub reads it: a seat that opted in and is minted without it
// has every push touching `.github/workflows` refused, and one that did not
// opt in must never be handed it.
func TestASeatsWorkflowsOptInIsWhatItsTokenAsksFor(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		builder string
		want    string
	}{
		"opted in": {"        workflows: true\n", "write"},
		"silent":   {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			gh := newAppGitHub(t)
			company := appCompanyWith(t, gh.url, true, c.builder)
			e := appEngine(t, company)
			env, _ := e.seatResolver(t.Context(), company, company.Org.AgentSeatByHandle("builder"), github.ToolTokenLife)
			if token := env.Value("${GITHUB_APP_TOKEN_BUILDER}"); token == "" {
				t.Fatal("the seat's own token did not resolve")
			}
			gh.mu.Lock()
			defer gh.mu.Unlock()
			if len(gh.asked) != 1 {
				t.Fatalf("%d mints, want one", len(gh.asked))
			}
			if got := gh.asked[0][github.PermissionWorkflows]; got != c.want {
				t.Errorf("the mint asked for workflows=%q, want %q", got, c.want)
			}
			if got := gh.asked[0]["contents"]; got != "write" {
				t.Errorf("a full_access mint asked for contents=%q", got)
			}
		})
	}
}
