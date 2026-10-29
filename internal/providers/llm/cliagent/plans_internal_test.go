package cliagent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// onPlans builds a claude-code entry on plans, as the engine builds every entry
// of a revision on its one registry, under auth and on stateDir ("" for a
// fresh one).
func onPlans(t *testing.T, plans *Plans, key, model string, auth Auth, stateDir string) *Provider {
	t.Helper()
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	t.Cleanup(func() { forgetWorkspace(stateDir) })
	p, err := New(Config{
		Key: key, Agent: "claude-code", Model: model, StateDir: stateDir,
		Overrides: map[string]any{"binary": filepath.Join(t.TempDir(), "no-cli-here")},
		Timeout:   time.Minute, Auth: auth, Plans: plans,
	})
	if err != nil {
		t.Fatalf("New(%s): %v", key, err)
	}
	return p
}

// keyed is an api-key sign-in with key.
func keyed(key string) Auth { return Auth{Mode: AuthAPIKey, APIKey: key} }

// spentAt is a limit report made at `at`, naming no window.
func spentAt(at time.Time) limitReport {
	return limitReport{reset: at.Add(30 * time.Minute), said: "You've hit your session limit", at: at,
		window: llm.LimitSession}
}

// A SIT-OUT FOLLOWS THE SIGN-IN ACROSS AN APPLY. Every apply rebuilds every
// entry, and the window the vendor named is the account's, not the provider
// object's: a rebuilt entry that calls as the same account finds it, whatever
// else about the entry changed (a timeout, a key) and whatever did not.
//
// Mutation: give each provider a Plan of its own rather than the registry's,
// and the rebuilt entry walks back into the spent plan.
func TestASitOutFollowsTheSignInAcrossAnApply(t *testing.T) {
	t.Parallel()
	plans := NewPlans([]byte("a fleet key"))
	now := time.Date(2026, time.October, 28, 16, 0, 0, 0, time.UTC)
	before := onPlans(t, plans, "default", "sonnet", keyed("sk-ant-api03-one"), "")
	before.now = func() time.Time { return now }
	before.bench(spentAt(now))

	after := onPlans(t, plans, "default", "sonnet", keyed("sk-ant-api03-one"), "")
	after.now = func() time.Time { return now.Add(time.Minute) }
	var refused *llm.Error
	if err := after.SittingOut(); !errors.As(err, &refused) {
		t.Fatal("the rebuilt entry was handed back the spent plan")
	}
	if want := now.Add(30 * time.Minute); !refused.ResetsAt.Equal(want) || refused.Window != llm.LimitSession {
		t.Errorf("the rebuilt entry's refusal names reset %v window %q, want %v %q",
			refused.ResetsAt, refused.Window, want, llm.LimitSession)
	}
}

// A NEW CREDENTIAL UNDER THE SAME NAME STARTS CLEAN. The config spec of an entry
// whose token was rotated in the secret store is unchanged (it names the same
// ${VAR}), and the old carry kept it benched for up to an hour on a credential
// that had never been refused. The sign-in is the value, so a new value is a
// new sign-in, with nothing to sit out.
//
// Mutation: compute the id from the variable's name rather than its value, and
// the rotated entry still sits out.
func TestANewCredentialUnderTheSameNameStartsClean(t *testing.T) {
	t.Parallel()
	plans := NewPlans([]byte("a fleet key"))
	now := time.Date(2026, time.October, 28, 16, 0, 0, 0, time.UTC)
	spent := onPlans(t, plans, "default", "sonnet", keyed("sk-ant-api03-old"), "")
	spent.now = func() time.Time { return now }
	spent.bench(spentAt(now))

	rotated := onPlans(t, plans, "default", "sonnet", keyed("sk-ant-api03-new"), "")
	rotated.now = func() time.Time { return now.Add(time.Minute) }
	if err := rotated.SittingOut(); err != nil {
		t.Fatalf("an entry on a new credential sits out the old one's window: %v", err)
	}
	if spent.SignInID() == rotated.SignInID() {
		t.Fatal("two credentials share one sign-in id")
	}
}

// ENTRIES ON ONE SIGN-IN SIT OUT TOGETHER (D6). A plan-wide limit is the
// account's, and every model on it is refused: an entry that learned it alone
// left every other entry on the same credential to spend a process launch
// learning it again.
//
// Mutation: key the registry by the entry's key, and the second entry answers
// nil.
func TestEntriesOnOneSignInSitOutTogether(t *testing.T) {
	t.Parallel()
	plans := NewPlans([]byte("a fleet key"))
	now := time.Date(2026, time.October, 28, 16, 0, 0, 0, time.UTC)
	opus := onPlans(t, plans, "default", "opus", keyed("sk-ant-api03-shared"), "")
	sonnet := onPlans(t, plans, "aux", "sonnet", keyed("sk-ant-api03-shared"), "")
	other := onPlans(t, plans, "metered", "sonnet", keyed("sk-ant-api03-other"), "")
	for _, p := range []*Provider{opus, sonnet, other} {
		p.now = func() time.Time { return now.Add(time.Minute) }
	}
	opus.bench(spentAt(now))
	if err := sonnet.SittingOut(); err == nil {
		t.Error("an entry on the same sign-in answers through a plan-wide limit")
	}
	if err := other.SittingOut(); err != nil {
		t.Errorf("an entry on another sign-in sits out: %v", err)
	}
	if opus.SignInID().Short() != sonnet.SignInID().Short() || len(opus.SignInID().Short()) != 12 {
		t.Errorf("Short = %q and %q, want one 12-character prefix", opus.SignInID().Short(), sonnet.SignInID().Short())
	}
}

// A MODEL-SCOPED LIMIT HOLDS ONLY THAT MODEL. "Opus limit" (seven_day_opus)
// leaves Sonnet on the same plan answering, so a window whose report names one
// model holds the entries that hand the CLI that model argument and no other.
//
// Mutation: keep a model's window on the plan-wide record, and the Sonnet entry
// sits out an Opus limit.
func TestAModelScopedLimitHoldsOnlyThatModel(t *testing.T) {
	t.Parallel()
	plans := NewPlans([]byte("a fleet key"))
	now := time.Date(2026, time.October, 28, 16, 0, 0, 0, time.UTC)
	opus := onPlans(t, plans, "default", "opus", keyed("sk-ant-api03-shared"), "")
	alsoOpus := onPlans(t, plans, "review", "opus", keyed("sk-ant-api03-shared"), "")
	sonnet := onPlans(t, plans, "aux", "sonnet", keyed("sk-ant-api03-shared"), "")
	for _, p := range []*Provider{opus, alsoOpus, sonnet} {
		p.now = func() time.Time { return now.Add(time.Minute) }
	}
	opus.bench(limitReport{reset: now.Add(48 * time.Hour), said: "You've hit your Opus limit",
		at: now, window: llm.LimitModel})
	if err := alsoOpus.SittingOut(); err == nil {
		t.Error("another entry on the same model answers through its model's limit")
	}
	if err := sonnet.SittingOut(); err != nil {
		t.Errorf("the Sonnet entry sits out the Opus limit: %v", err)
	}
	held, ok := opus.SitOut()
	if !ok || held.Model != "opus" || held.Window != llm.LimitModel {
		t.Errorf("SitOut = %+v, %v, want the opus model's window", held, ok)
	}
	if state := opus.Plan().State(now.Add(time.Minute)); len(state.SittingOut) != 1 || state.SittingOut[0].Model != "opus" {
		t.Errorf("the plan reads %+v, want the one scoped window", state.SittingOut)
	}
}

// A REFRESH WRITE-BACK KEEPS THE SIGN-IN. A login kept in files is refreshed by
// the CLI and carried back by the engine many times a day, and that is the same
// account: an id read off the files' bytes lifted a sit-out at every refresh.
// The login's install record moves only on a new login, so a fresh `crewlet
// llm login` is a clean plan and a refresh is not.
//
// Mutation: hash the login files into the id, and the refreshed entry reads as
// a new sign-in.
func TestARefreshWriteBackKeepsTheSignIn(t *testing.T) {
	t.Parallel()
	plans := NewPlans([]byte("a fleet key"))
	dir := t.TempDir()
	p := onPlans(t, plans, "default", "sonnet", Auth{Mode: AuthSubscription}, dir)
	login := p.ws.sharedPath(p.ws.loginNames()[0])
	if err := os.MkdirAll(filepath.Dir(login), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(login, []byte(`{"claudeAiOauth":{"accessToken":"one"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	signedIn := time.Date(2026, time.October, 28, 9, 0, 0, 0, time.UTC)
	recordInstall(t, p, "one", signedIn)
	first := p.SignInID()
	if first == "" {
		t.Fatal("a login kept in files reads as no sign-in")
	}
	if err := os.WriteFile(login, []byte(`{"claudeAiOauth":{"accessToken":"refreshed"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if refreshed := p.SignInID(); refreshed != first {
		t.Fatal("a refreshed token reads as a new sign-in")
	}
	recordInstall(t, p, "two", signedIn.Add(time.Hour))
	if again := p.SignInID(); again == first {
		t.Fatal("a new login reads as the same sign-in")
	}
}

// AN ID IS THE FLEET'S, NOT THE PROCESS'S. Two registries under one key (two
// nodes, or one node across a restart) name one credential alike, so a run's
// row can be compared after a move; under another key they do not, so the id
// says nothing to anyone without the key.
func TestASignInIDIsComparableOnlyUnderOneKey(t *testing.T) {
	t.Parallel()
	a := onPlans(t, NewPlans([]byte("a fleet key")), "default", "sonnet", keyed("sk-ant-api03-x"), "")
	b := onPlans(t, NewPlans([]byte("a fleet key")), "aux", "sonnet", keyed("sk-ant-api03-x"), "")
	c := onPlans(t, NewPlans([]byte("another key")), "default", "sonnet", keyed("sk-ant-api03-x"), "")
	if a.SignInID() != b.SignInID() {
		t.Error("one credential under one key reads as two sign-ins")
	}
	if a.SignInID() == c.SignInID() {
		t.Error("one credential under two keys reads as one sign-in")
	}
	if none := onPlans(t, NewPlans(nil), "empty", "sonnet", keyed(""), ""); none.SignInID() != "" {
		t.Errorf("an entry nothing signs in has the id %q, want none", none.SignInID())
	}
}

// A SWEEP KEEPS A WINDOW IN FORCE. A credential rotated away leaves its Plan
// behind, which a sweep drops once nothing names it; but one still sitting out
// is kept, so a credential put back before the window ends is still benched.
func TestASweepKeepsAWindowInForce(t *testing.T) {
	t.Parallel()
	plans := NewPlans([]byte("a fleet key"))
	now := time.Date(2026, time.October, 28, 16, 0, 0, 0, time.UTC)
	spent := onPlans(t, plans, "default", "sonnet", keyed("sk-ant-api03-spent"), "")
	idle := onPlans(t, plans, "aux", "sonnet", keyed("sk-ant-api03-idle"), "")
	spent.now = func() time.Time { return now }
	spent.bench(spentAt(now))
	idle.answered(now)
	spentID, idleID := spent.SignInID(), idle.SignInID()

	plans.Sweep(nil, now.Add(time.Minute))
	plans.mu.Lock()
	_, keptSpent := plans.plans[spentID]
	_, keptIdle := plans.plans[idleID]
	plans.mu.Unlock()
	if !keptSpent {
		t.Error("the sweep dropped a sign-in still sitting out")
	}
	if keptIdle {
		t.Error("the sweep kept a sign-in nothing names and nothing holds")
	}
	plans.Sweep(nil, now.Add(time.Hour))
	plans.mu.Lock()
	_, keptSpent = plans.plans[spentID]
	plans.mu.Unlock()
	if keptSpent {
		t.Error("the sweep kept a sign-in whose window is over")
	}
}

// A CLEAR ENDS WHAT WAS REPORTED BEFORE IT, AND NOTHING AFTER. A person's
// "clear" reaches every node at its own moment, so it is a rule on report
// instants rather than a command: windows opened before the request end, and a
// report made after it stands, as the vendor's newer word.
func TestAClearEndsOnlyWhatWasReportedBeforeIt(t *testing.T) {
	t.Parallel()
	plans := NewPlans([]byte("a fleet key"))
	now := time.Date(2026, time.October, 28, 16, 0, 0, 0, time.UTC)
	p := onPlans(t, plans, "default", "opus", keyed("sk-ant-api03-x"), "")
	p.now = func() time.Time { return now.Add(time.Minute) }
	p.bench(spentAt(now))
	p.bench(limitReport{reset: now.Add(time.Hour), said: "Opus limit", at: now, window: llm.LimitModel})
	if !p.Plan().Clear(now.Add(time.Second)) {
		t.Fatal("a clear after the reports ended nothing")
	}
	if err := p.SittingOut(); err != nil {
		t.Fatalf("a cleared entry still sits out: %v", err)
	}
	p.bench(spentAt(now.Add(2 * time.Second)))
	if p.Plan().Clear(now.Add(time.Second)) {
		t.Error("a clear ended a window reported after it")
	}
	if err := p.SittingOut(); err == nil {
		t.Error("a report made after the clear does not stand")
	}
}

// A REFUSED SIGN-IN IS THE SIGN-IN'S LAST FAILURE. What a person is shown about
// an entry is the newest thing its calls met, bounded, and shared by every
// entry on the same credential.
func TestARefusedSignInIsTheSignInsLastFailure(t *testing.T) {
	t.Parallel()
	plans := NewPlans([]byte("a fleet key"))
	now := time.Date(2026, time.October, 28, 16, 0, 0, 0, time.UTC)
	p := onPlans(t, plans, "default", "sonnet", keyed("sk-ant-api03-x"), "")
	q := onPlans(t, plans, "aux", "haiku", keyed("sk-ant-api03-x"), "")
	p.now = func() time.Time { return now }
	q.now = p.now
	long := "Failed to authenticate. API Error: 401 " + string(make([]byte, 400))
	p.remember(p.fail(llm.KindAuth, errors.New(long)))
	state := q.Plan().State(now)
	if state.LastFailure == nil || state.LastFailure.Kind != llm.KindAuth || !state.LastFailure.At.Equal(now) {
		t.Fatalf("LastFailure = %+v, want the refusal", state.LastFailure)
	}
	if len(state.LastFailure.Said) > MaxSaid {
		t.Errorf("the sentence is %d bytes, want at most %d", len(state.LastFailure.Said), MaxSaid)
	}
	if err := q.Unready(t.Context()); llm.KindOf(err) != llm.KindAuth {
		t.Errorf("an entry on the refused sign-in reads ready: %v", err)
	}
}

// recordInstall writes the install record of a login installed at `at`, as
// every login command does.
func recordInstall(t *testing.T, p *Provider, id string, at time.Time) {
	t.Helper()
	raw, err := json.Marshal(LoginInstall{ID: id, At: at})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.ws.CredentialsDir(), loginInstallFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// sitOutUntil is when p's plan-wide window ends.
func sitOutUntil(p *Provider) time.Time {
	plan := p.Plan()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	return plan.wide.until
}

// sitOutSaid is the sentence behind p's plan-wide window.
func sitOutSaid(p *Provider) string {
	plan := p.Plan()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	return plan.wide.said
}

// unnamedStep is the step of [limitBackoff] p's next unnamed report takes.
func unnamedStep(p *Provider) int {
	plan := p.Plan()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	return plan.wide.unnamed
}

// setUnnamed puts p's backoff at step.
func setUnnamed(p *Provider, step int) {
	plan := p.Plan()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	plan.wide.unnamed = step
}

// AN ENV SIGN-IN IS AS MANY SIGN-INS AS THE PLACES IT REACHES. A Bedrock entry
// on ambient AWS credentials hands its CLI one switch whatever its region, so
// keyed by its credentials alone, a throttle on the primary region benched its
// regional fallback; Vertex projects and gateway addresses collided the same
// way. The profile's identity_env names what says where a call goes.
//
// Mutation: drop the identity_env fold from SignInID, and every pair below
// shares one id.
func TestAnEnvSignInIsOneSignInPerPlaceItReaches(t *testing.T) {
	t.Parallel()
	plans := NewPlans([]byte("a fleet key"))
	id := func(key string, vars map[string]string) SignInID {
		t.Helper()
		dir := t.TempDir()
		t.Cleanup(func() { forgetWorkspace(dir) })
		p, err := New(Config{Key: key, Agent: "claude-code", Model: "sonnet", StateDir: dir,
			Timeout: time.Minute, Env: vars, Auth: Auth{Mode: AuthEnv}, Plans: plans})
		if err != nil {
			t.Fatal(err)
		}
		return p.SignInID()
	}
	bedrock := func(region string) map[string]string {
		return map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_REGION": region}
	}
	vertex := func(project string) map[string]string {
		return map[string]string{"CLAUDE_CODE_USE_VERTEX": "1", "ANTHROPIC_VERTEX_PROJECT_ID": project,
			"CLOUD_ML_REGION": "us-east5"}
	}
	gateway := func(url string) map[string]string {
		return map[string]string{"ANTHROPIC_AUTH_TOKEN": "gw-token", "ANTHROPIC_BASE_URL": url}
	}
	for name, pair := range map[string][2]map[string]string{
		"two Bedrock regions": {bedrock("us-east-1"), bedrock("eu-west-1")},
		"two Vertex projects": {vertex("acme-prod"), vertex("acme-dr")},
		"two gateways":        {gateway("https://gw-a.example.com"), gateway("https://gw-b.example.com")},
	} {
		if id("a", pair[0]) == id("b", pair[1]) {
			t.Errorf("%s share one sign-in", name)
		}
	}
	if id("primary", bedrock("us-east-1")) != id("review", bedrock("us-east-1")) {
		t.Error("two entries in one Bedrock region read as two sign-ins")
	}
}
