package cliagent

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// Captured from Claude Code 2.1.312's `-p --output-format stream-json
// --verbose` on 2026-10-28, cut to the fields the profile reads (and ids
// removed). The rate-limit event is the one every run writes; a refused one
// differs in `status`, which is what the profile matches.
const (
	// allowedLimitEvent is a run's rate-limit event while the plan still
	// answers: written on every run, and no limit.
	allowedLimitEvent = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning",` +
		`"resetsAt":1793707200,"rateLimitType":"seven_day","utilization":0.51,"overageStatus":"rejected",` +
		`"isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0.25,"resetsAt":1793259000,` +
		`"status":"allowed"},"seven_day":{"utilization":0.51,"resetsAt":1793707200,"status":"allowed"}}}}`
	// keyRefusedResult is the result a refused API key ends a run on.
	keyRefusedResult = `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,` +
		`"api_error_origin":"first_party","result":"Invalid API key · Fix external API key",` +
		`"terminal_reason":"api_error"}`
	// tokenRetry is what a refused OAuth token writes before it gives up.
	tokenRetry = `{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,` +
		`"retry_delay_ms":588,"error_status":401,"error":"authentication_failed"}`
	// tokenRefusedAnswer is the assistant event a refused token's run writes,
	// carrying the sentence -p prints.
	tokenRefusedAnswer = `{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"text",` +
		`"text":"Failed to authenticate. API Error: 401 OAuth access token is invalid."}]},` +
		`"error":"authentication_failed","api_error_status":401}`
)

// rejected is allowedLimitEvent as the CLI writes it once the window it
// names is spent.
func rejected(window string, resetsAt time.Time) string {
	return `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":` +
		strconv.FormatInt(resetsAt.Unix(), 10) + `,"rateLimitType":"` + window + `"}}`
}

// A SPENT PLAN IS DATED AND NAMED BY THE CLI'S OWN EVENT. The result line says
// a limit was met (429) and the sentence may say when, in words; the rejected
// rate-limit event says when in an epoch and which window by name, and is read
// first. An allowed one, which every run writes, dates nothing.
//
// Mutation: read the first matching event rather than the last, or match
// allowed events, and the window and reset come from the wrong report; drop
// the event reading, and a weekly limit reads as an unknown window reset at
// the sentence's time.
func TestALimitIsDatedAndNamedByTheCLIsEvent(t *testing.T) {
	t.Parallel()
	p := claudeCode(t)
	weekly := reportedAt.Add(52 * time.Hour)
	session := reportedAt.Add(3 * time.Hour)
	const spent = `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,` +
		`"result":"You've hit your session limit · resets 4:30pm (America/Vancouver)"}`
	stdout := strings.Join([]string{allowedLimitEvent, rejected("five_hour", session),
		rejected("seven_day", weekly), spent}, "\n")
	hit, ok := classifyMarkers(p, cliOutput{stdout: stdout, failed: true, at: reportedAt})
	if !ok || hit.Kind != llm.KindRateLimit {
		t.Fatalf("classified %+v, %v; want a limit", hit, ok)
	}
	if !hit.Reset.Equal(weekly) || hit.Window != llm.LimitWeekly {
		t.Errorf("reset %v window %q, want %v %q (the last rejected event)", hit.Reset, hit.Window,
			weekly, llm.LimitWeekly)
	}

	allowedOnly := strings.Join([]string{allowedLimitEvent, spent}, "\n")
	hit, _ = classifyMarkers(p, cliOutput{stdout: allowedOnly, failed: true, at: reportedAt})
	if want := time.Date(2026, time.October, 27, 23, 30, 0, 0, time.UTC); !hit.Reset.Equal(want) ||
		hit.Window != llm.LimitSession {
		t.Errorf("with only an allowed event: reset %v window %q, want the sentence's %v and %q",
			hit.Reset, hit.Window, want, llm.LimitSession)
	}

	opus := strings.Join([]string{rejected("seven_day_opus", weekly), spent}, "\n")
	if hit, _ = classifyMarkers(p, cliOutput{stdout: opus, failed: true, at: reportedAt}); hit.Window != llm.LimitModel {
		t.Errorf("an Opus window reads %q, want %q", hit.Window, llm.LimitModel)
	}
	unmapped := strings.Join([]string{rejected("ninety_day", weekly), spent}, "\n")
	if hit, _ = classifyMarkers(p, cliOutput{stdout: unmapped, failed: true, at: reportedAt}); hit.Window != llm.LimitSession {
		t.Errorf("a window the map does not hold reads %q, want the sentence's %q", hit.Window, llm.LimitSession)
	}
}

// A REFUSED SIGN-IN IS READ FROM THE CLI'S OWN FIELDS, FIRST. The 401 is a
// status, which survives the rewording that retires a sentence, and the
// `api_retry` event names `authentication_failed` before the CLI gives up; both
// were measured on 2.1.312. Only in a run the CLI reported failed: a CLI that
// retries a 401 and then answers was signed in.
//
// Mutation: drop auth_events, and the refused token's run is read by its
// sentence alone, which "Failed to authenticate" must then carry; read them in
// a run that answered, and the answer is thrown away.
func TestARefusedSignInIsReadFromTheCLIsFields(t *testing.T) {
	t.Parallel()
	p := claudeCode(t)
	for name, stdout := range map[string]string{
		"a refused key":   keyRefusedResult,
		"a refused token": strings.Join([]string{tokenRetry, tokenRetry, tokenRefusedAnswer}, "\n"),
	} {
		hit, ok := classifyMarkers(p, cliOutput{stdout: stdout, failed: true, at: reportedAt})
		if !ok || hit.Kind != llm.KindAuth || hit.Source != sourceEvent {
			t.Errorf("%s: classified %+v, %v; want auth from the CLI's event", name, hit, ok)
		}
	}
	if hit, ok := classifyMarkers(p, cliOutput{stdout: keyRefusedResult, failed: true}); !ok ||
		hit.Said != "Invalid API key · Fix external API key" {
		t.Errorf("the refusal says %q, want the result's sentence", hit.Said)
	}
	answered := strings.Join([]string{tokenRetry, `{"type":"result","is_error":false,"result":"ready"}`}, "\n")
	if hit, ok := classifyMarkers(p, cliOutput{said: "ready", stdout: answered, at: reportedAt}); ok {
		t.Errorf("a run that retried a 401 and answered classified as %+v", hit)
	}
	// The sentence alone, as a run cut short before its events prints it.
	said := "Failed to authenticate. API Error: 401 OAuth access token is invalid."
	if hit, ok := classifyMarkers(p, cliOutput{said: said, failed: true}); !ok || hit.Kind != llm.KindAuth {
		t.Errorf("the -p sentence classified %+v, %v; want auth", hit, ok)
	}
}

// CODEX'S RESET IS READ FROM ITS SENTENCE, IN THE ZONE IT WAS HANDED. Codex
// writes no rate-limit event on `exec --json` and names its reset as a local
// time with no zone (0.150.1), so the engine pins TZ on the child and reads the
// time in it. "for <model>" names a limit of one model.
//
// Mutation: read the time in UTC, and the reset lands seven hours off; drop
// limit_windows from the profile, and a model's limit holds the whole plan.
func TestCodexsResetIsReadInTheZoneItWasHanded(t *testing.T) {
	t.Parallel()
	p, err := cliprofile.Load("codex", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	vancouver, err := time.LoadLocation("America/Vancouver")
	if err != nil {
		t.Fatal(err)
	}
	stderr := "You've hit your usage limit for gpt-6.1-sol. Visit https://chatgpt.com/codex/settings/usage " +
		"to purchase more credits or try again at 6:02 PM.\n"
	hit, ok := classifyMarkers(p, cliOutput{stderr: stderr, failed: true, at: reportedAt, zone: vancouver})
	if !ok || hit.Kind != llm.KindRateLimit {
		t.Fatalf("classified %+v, %v; want a limit", hit, ok)
	}
	if want := time.Date(2026, time.October, 28, 1, 2, 0, 0, time.UTC); !hit.Reset.Equal(want) {
		t.Errorf("reset %v, want %v (6:02 PM in the child's zone)", hit.Reset, want)
	}
	if hit.Window != llm.LimitModel {
		t.Errorf("window %q, want %q", hit.Window, llm.LimitModel)
	}
}

// EVERY CHILD IS HANDED THE ENGINE'S ZONE BY NAME. Inherited only where the
// host had one, a CLI wrote its reset in whatever zone the host or a box's
// image defaulted to, which the engine could only guess.
//
// Mutation: drop the TZ line from childEnv or SandboxEnv, and the child is
// handed no zone.
func TestEveryChildIsHandedTheEnginesZone(t *testing.T) {
	t.Setenv("TZ", "")
	p := shipped(t, "claude-code", "sonnet")
	if got := childEnv(p.profile, &Checkout{}, p.env, p.auth)["TZ"]; got != "UTC" {
		t.Errorf("with no TZ the child is handed %q, want UTC", got)
	}
	t.Setenv("TZ", "America/Vancouver")
	if got := childEnv(p.profile, &Checkout{}, p.env, p.auth)["TZ"]; got != "America/Vancouver" {
		t.Errorf("the child is handed %q, want the engine's America/Vancouver", got)
	}
	if got := p.SandboxEnv()["TZ"]; got != "America/Vancouver" {
		t.Errorf("a box is handed %q, want the engine's America/Vancouver", got)
	}
	t.Setenv("TZ", "Mars/Olympus")
	if name, zone := childZone(); name != "UTC" || zone != time.UTC {
		t.Errorf("a zone the system does not know reads %q, want UTC", name)
	}
}

// AN ENV-MODE ENTRY HANDS ITS CLI NEITHER THE PLAN TOKEN NOR A KEY (B1). A
// Bedrock or gateway entry signs in through its env; under the default mode the
// convention's plan token reached its CLI, and its box, where the cloud switch
// outranks it and it was never used. Its sign-in reads as the env, not as none.
//
// Mutation: drop the AuthEnv case from applyAuth, and the token reaches the
// child and the box.
func TestAnEnvModeEntryHandsItsCLINoTokenOrKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "bedrock", Agent: "claude-code", Model: "sonnet", StateDir: dir, Timeout: time.Minute,
		Env: map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_REGION": "us-east-1",
			"ANTHROPIC_API_KEY": "sk-ant-api03-from-env"},
		Auth: Auth{Mode: AuthEnv, Token: "sk-ant-oat01-plan", APIKey: "sk-ant-api03-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := childEnv(p.profile, &Checkout{}, p.env, p.auth)
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if _, ok := env[name]; ok {
			t.Errorf("the child is handed %s", name)
		}
		if _, ok := p.SandboxEnv()[name]; ok {
			t.Errorf("the box is handed %s", name)
		}
	}
	if env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
		t.Error("the env's own sign-in did not reach the child")
	}
	if state := p.signIn(t.Context()).state(); state != "env" {
		t.Errorf("the sign-in reads %q, want env", state)
	}
	if err := p.missingSignIn(t.Context()); err != nil {
		t.Errorf("an entry its env signs in reads as having no sign-in: %v", err)
	}
	if !p.signsInWithKey() {
		t.Error("an env-mode call would be seeded with a login")
	}
	if p.SignInID() == "" {
		t.Error("an entry its env signs in has no sign-in id")
	}
}

// AN EXECUTOR RUN REFUSED ITS SIGN-IN SAYS SO, AND IS THE SIGN-IN'S. At a run's
// collection the engine asks how it ended (EndedKind, which records nothing),
// and RunEnded records a refused sign-in as the sign-in's verdict and last
// failure, as a call's refusal is, without sitting anything out.
//
// Mutation: leave the auth branch out of RunEnded, and the plan has no last
// failure; classify without the profile's auth_events, and EndedKind says
// nothing.
func TestARunRefusedItsSignInSaysSoAndIsTheSignIns(t *testing.T) {
	t.Parallel()
	p := shipped(t, "claude-code", "sonnet")
	end := RunEnd{Event: keyRefusedResult, Failed: true, At: reportedAt}
	if kind, ok := p.EndedKind(end); !ok || kind != llm.KindAuth {
		t.Fatalf("EndedKind = %v, %v; want auth", kind, ok)
	}
	if state := p.Plan().State(reportedAt); state.LastFailure != nil {
		t.Fatal("EndedKind recorded something on the plan")
	}
	if p.RunEnded(t.Context(), end) {
		t.Error("a refused sign-in sat the entry out")
	}
	if state := p.Plan().State(reportedAt); state.LastFailure == nil || state.LastFailure.Kind != llm.KindAuth {
		t.Errorf("the plan's last failure is %+v, want the refusal", state.LastFailure)
	}
	if _, ok := p.EndedKind(RunEnd{Answer: "done", At: reportedAt}); ok {
		t.Error("an answer classified as a failure")
	}
}

// THE ROUTE A STATUS READ PUBLISHES IS THE ONE `crewlet llm list` PRINTS, in the
// status read's words, and a login kept in files is the one the engine asks the
// CLI about.
func TestTheSignInRouteIsTheListsFirstRoute(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	env, err := New(Config{Key: "bedrock", Agent: "claude-code", Model: "sonnet", StateDir: dir,
		Timeout: time.Minute, Env: map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}, Auth: Auth{Mode: AuthEnv}})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		p       *Provider
		route   SignInRoute
		resolve bool
		login   bool
	}{
		"a key":    {p: shipped(t, "claude-code", "sonnet"), route: "api-key", resolve: true},
		"the env":  {p: env, route: "env", resolve: true},
		"no login": {p: onPlans(t, nil, "sub", "sonnet", Auth{Mode: AuthSubscription}, ""), route: "none", login: true},
		"a token": {p: onPlans(t, nil, "tok", "sonnet", Auth{Mode: AuthSubscription, Token: "sk-ant-oat01-x"}, ""),
			route: "token", resolve: true},
	} {
		route, resolves := tc.p.SignInRoute(t.Context())
		if !route.Valid() {
			t.Errorf("%s: SignInRoute = %q, which is not one of %v", name, route, SignInRoutes)
		}
		if route != tc.route || resolves != tc.resolve {
			t.Errorf("%s: SignInRoute = %q, %v; want %q, %v", name, route, resolves, tc.route, tc.resolve)
		}
		if got := tc.p.SignsInWithLogin(); got != tc.login {
			t.Errorf("%s: SignsInWithLogin = %v, want %v", name, got, tc.login)
		}
	}
}

// A BOX IS SEEDED WITH A LOGIN ONLY FOR AN ENTRY THAT SIGNS IN WITH ONE (B1). A
// call on this host under api-key or env runs in a key home no login is seeded
// into; a local box on the same entry was still mapped the plan's credential
// files, so a Bedrock or gateway run carried the company's plan login beside
// its own sign-in.
//
// Mutation: drop the signsInWithKey guard from SandboxCredentials, and the
// api-key and env entries are mapped the login.
func TestABoxIsSeededWithALoginOnlyWhereTheEntrySignsInWithOne(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	for _, tc := range []struct {
		mode  AuthMode
		env   map[string]string
		login bool
	}{
		{mode: AuthSubscription, login: true},
		{mode: AuthAPIKey},
		{mode: AuthEnv, env: map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}},
		{mode: AuthInheritEnv, login: true},
	} {
		dir := t.TempDir()
		t.Cleanup(func() { forgetWorkspace(dir) })
		auth := Auth{Mode: tc.mode}
		if tc.mode == AuthAPIKey {
			auth.APIKey = "sk-ant-api03-not-real"
		}
		p, err := New(Config{Key: "e", Agent: "claude-code", Model: "sonnet", StateDir: dir,
			Timeout: time.Minute, Env: tc.env, Auth: auth})
		if err != nil {
			t.Fatal(err)
		}
		if got := len(p.SandboxCredentials()) > 0; got != tc.login {
			t.Errorf("%s: a box is mapped the login = %v, want %v", tc.mode, got, tc.login)
		}
	}
}

// A RUN'S LAST WORD DECIDES HOW IT ENDED, AND AN ANSWER IS NEVER A REFUSAL. A
// token Claude Code retried on a 401 and then refreshed can go on to meet a
// spent plan: the run ended on the plan. And a clean answer that opens with
// "Failed to authenticate" is the model writing about someone's error, not the
// vendor refusing this sign-in, which would set a verdict on every entry on it.
//
// Mutation: read auth events before limit events, and the retried run reads as
// a refusal; match auth markers in a clean answer, and the answer is thrown
// away as one.
func TestARunsLastWordDecidesAndAnAnswerIsNeverARefusal(t *testing.T) {
	t.Parallel()
	p := claudeCode(t)
	const spent = `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,` +
		`"result":"You've hit your session limit · resets 4:30pm (America/Vancouver)"}`
	retried := strings.Join([]string{tokenRetry, tokenRetry, spent}, "\n")
	if hit, ok := classifyMarkers(p, cliOutput{stdout: retried, failed: true, at: reportedAt}); !ok ||
		hit.Kind != llm.KindRateLimit {
		t.Errorf("a run retried on a 401 that ended on a spent plan classified %+v, %v; want a limit", hit, ok)
	}
	answer := "Failed to authenticate is what the deploy log says; the fix is to rotate the key."
	stdout := `{"type":"result","subtype":"success","is_error":false,"result":"` + answer + `"}`
	if hit, ok := classifyMarkers(p, cliOutput{said: answer, stdout: stdout, at: reportedAt}); ok {
		t.Errorf("a clean answer opening with an auth sentinel classified as %+v", hit)
	}
}

// EVERY ROUTE IS ONE THE SET NAMES, AND THE SET NOTHING ELSE. The Models screen
// draws a route by a closed vocabulary held to this one, so a word the reading
// can produce must be in the set and a value off the wire outside it is not.
func TestASignInRouteIsOneOfTheClosedSet(t *testing.T) {
	t.Parallel()
	for _, r := range SignInRoutes {
		if !r.Valid() {
			t.Errorf("%q is in the set and not Valid", r)
		}
	}
	for _, r := range []SignInRoute{"", "device", "api key", "inherited token"} {
		if r.Valid() {
			t.Errorf("%q is Valid and not in the set", r)
		}
	}
}
