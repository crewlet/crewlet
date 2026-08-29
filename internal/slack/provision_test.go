package slack_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/slack"
)

// sink records what a run mints.
type sink struct {
	mu     sync.Mutex
	values map[string]string
	err    error
}

func newSink() *sink { return &sink{values: map[string]string{}} }

func (s *sink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = value
	return nil
}

func (s *sink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", false, s.err
	}
	return s.values[name], s.values[name] != "", nil
}

func (s *sink) Discard(context.Context) error { return nil }
func (s *sink) Flush(context.Context) error   { return nil }
func (s *sink) Describe() string              { return "a test sink" }
func (s *sink) NextStep() string              { return "restart the engine" }

func (s *sink) value(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[name]
}

func plans() []slack.SeatPlan {
	o := &org.Organization{Name: "nimbus", Roles: []*org.Role{
		{Name: "Engineer", DeclaredHandle: "swe", Slack: org.SlackIdentity{
			BotToken: "${SWE_SLACK_TOKEN}", SigningSecret: "${SWE_SLACK_SIGNING}",
		}},
	}}
	o.Normalize()
	return slack.PlanFor(o)
}

func run(t *testing.T, ws *workspace, mutate func(*slack.Options)) (*slack.Result, *slack.Ledger, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), slack.LedgerName)
	ledger, err := slack.LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	admin := slack.NewAdmin(rewriting(ws.URL))
	opts := slack.Options{
		Admin: admin, Seats: plans(), Ledger: ledger, LedgerPath: path,
		Sink: newSink(), BaseURL: "https://engine.example.com",
		ConfigRefreshToken: "xoxe-1-refresh",
	}
	if mutate != nil {
		mutate(&opts)
	}
	res, err := slack.Reconcile(context.Background(), opts)
	return res, ledger, path, err
}

func withCreate(ws *workspace) {
	ws.replies["tooling.tokens.rotate"] = `{"ok":true,"token":"xoxe.xoxp-config",
		"refresh_token":"xoxe-1-next","exp":4102444800}`
	ws.replies["apps.manifest.create"] = `{"ok":true,"app_id":"A0SWE",
		"credentials":{"client_id":"1.2","client_secret":"cs","signing_secret":"ss"}}`
}

// AN APP IS CREATED AND ITS CREDENTIALS RECORDED BEFORE ANYTHING ELSE.
//
// Slack serves them once, at creation. A process that died in the next
// instruction would leave an app nobody can install, verify or delete.
func TestCreatingAnAppRecordsItsCredentialsImmediately(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)

	res, _, path, err := run(t, ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 || res.Created[0] != "swe" {
		t.Fatalf("created %v", res.Created)
	}
	// Read back OFF DISK, not from the in-memory ledger: what matters is
	// that the file holds them before the run went any further.
	reloaded, err := slack.LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	record := reloaded.Apps["swe"]
	if record.AppID != "A0SWE" || record.ClientSecret != "cs" || record.SigningSecret != "ss" {
		t.Fatalf("ledger holds %+v", record)
	}
	// THE LEDGER IS A SECRETS FILE.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("ledger mode = %o, want 600 — it holds client secrets", mode)
	}
}

// THE SIGNING SECRET REACHES THE CONFIG'S OWN ${VAR}, which is the only way
// the seat's webhook route can ever verify a delivery.
func TestTheSigningSecretIsRecordedIntoItsVariable(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	recorder := newSink()

	if _, _, _, err := run(t, ws, func(o *slack.Options) { o.Sink = recorder }); err != nil {
		t.Fatal(err)
	}
	if got := recorder.value("SWE_SLACK_SIGNING"); got != "ss" {
		t.Fatalf("signing secret = %q", got)
	}
}

// A RE-RUN WHOSE MANIFEST HAS NOT CHANGED PUSHES NOTHING.
//
// The manifest methods are rate limited to roughly one request a minute, so
// a company of seven spends minutes waiting out 429s to achieve nothing.
func TestARerunSkipsAnUnchangedManifest(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)

	_, ledger, path, err := run(t, ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := ws.called("apps.manifest.update")

	res, err := slack.Reconcile(context.Background(), slack.Options{
		Admin: slack.NewAdmin(rewriting(ws.URL)), Seats: plans(),
		Ledger: ledger, LedgerPath: path, Sink: newSink(),
		BaseURL: "https://engine.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Kept) != 1 {
		t.Fatalf("a re-run reported %+v rather than keeping the app", res)
	}
	if ws.called("apps.manifest.update") != before {
		t.Error("an unchanged manifest was pushed anyway")
	}
	if ws.called("apps.manifest.create") != 1 {
		t.Error("a re-run created a second app for the same seat")
	}
}

// A CHANGED MANIFEST IS PUSHED — a new scope has to reach every app, or the
// tool that needs it fails on seats provisioned before the change.
func TestAChangedManifestIsPushed(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)

	_, ledger, path, err := run(t, ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A different public base changes every request URL in the manifest.
	res, err := slack.Reconcile(context.Background(), slack.Options{
		Admin: slack.NewAdmin(rewriting(ws.URL)), Seats: plans(),
		Ledger: ledger, LedgerPath: path, Sink: newSink(),
		BaseURL: "https://moved.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Updated) != 1 {
		t.Fatalf("a moved request URL did not push a manifest: %+v", res)
	}
	body := ws.lastBody("apps.manifest.update")
	if !strings.Contains(body["manifest"].(string), "moved.example.com/webhooks/slack/swe") {
		t.Errorf("the pushed manifest does not carry the new request URL: %v", body)
	}
}

// THE APP-CONFIGURATION TOKEN IS PERSISTED BEFORE IT IS USED.
//
// Slack's rotation is single-use in both directions: the call that returns a
// new refresh token invalidates the one it was given, so a run that rotated
// and did not record the result has locked the operator out of their apps.
func TestTheRotatedConfigTokenIsPersistedFirst(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	// The app create fails, so the run stops right after the rotation.
	ws.replies["apps.manifest.create"] = `{"ok":false,"error":"invalid_manifest"}`

	_, _, path, err := run(t, ws, nil)
	if err == nil {
		t.Fatal("a refused manifest did not fail the run")
	}
	reloaded, loadErr := slack.LoadLedger(path)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if reloaded.ConfigToken.RefreshToken != "xoxe-1-next" {
		t.Fatalf("the rotated refresh token was lost: %+v", reloaded.ConfigToken)
	}
}

// A STILL-GOOD ACCESS TOKEN IS REUSED rather than rotated, because every
// rotation invalidates the previous refresh token.
func TestAValidConfigTokenIsNotRotatedAgain(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)

	_, ledger, path, err := run(t, ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	rotations := ws.called("tooling.tokens.rotate")

	if _, err := slack.Reconcile(context.Background(), slack.Options{
		Admin: slack.NewAdmin(rewriting(ws.URL)), Seats: plans(),
		Ledger: ledger, LedgerPath: path, Sink: newSink(),
		BaseURL: "https://engine.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if ws.called("tooling.tokens.rotate") != rotations {
		t.Error("a run with a live config token rotated it anyway, which " +
			"invalidates the refresh token the operator still holds")
	}
}

// THE INSTALL IS A PERSON'S DECISION. Without an installer the run reports
// the URL and stops, rather than claiming an install it did not do.
func TestWithoutAnInstallerTheRunReportsWhatIsPending(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)

	res, _, _, err := run(t, ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	authorize, pending := res.Pending["swe"]
	if !pending {
		t.Fatalf("nothing was reported as pending: %+v", res)
	}
	for _, want := range []string{"client_id=1.2", "state=swe", "chat%3Awrite"} {
		if !strings.Contains(authorize, want) {
			t.Errorf("the authorize URL is missing %q: %s", want, authorize)
		}
	}
	if len(res.Installed) != 0 {
		t.Errorf("a run with no installer claimed to install %v", res.Installed)
	}
}

// A COMPLETED INSTALL MINTS THE BOT TOKEN into the config's own ${VAR} and
// records the bot user id — which is what a delivery names the seat by.
func TestAnInstallMintsTheBotTokenAndRecordsTheIdentity(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-minted",
		"bot_user_id":"U0BOTSWE","app_id":"A0SWE","team":{"id":"T0ACME"}}`
	recorder := newSink()

	res, _, path, err := run(t, ws, func(o *slack.Options) {
		o.Sink = recorder
		o.Install = func(context.Context, string, string) (string, error) {
			return "the-temporary-code", nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Installed) != 1 {
		t.Fatalf("installed %v", res.Installed)
	}
	if got := recorder.value("SWE_SLACK_TOKEN"); got != "xoxb-minted" {
		t.Fatalf("bot token = %q", got)
	}
	reloaded, err := slack.LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Apps["swe"].Installed() {
		t.Error("the install was not recorded, so a re-run would ask again")
	}
}

// AN OPERATOR WHO SKIPS A SEAT DOES NOT UNDO THE ONES ALREADY DONE.
func TestSkippingAnInstallLeavesTheAppInPlace(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)

	res, _, _, err := run(t, ws, func(o *slack.Options) {
		o.Install = func(context.Context, string, string) (string, error) { return "", nil }
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("the app was not created: %+v", res)
	}
	if _, pending := res.Pending["swe"]; !pending {
		t.Error("a skipped seat was not reported as still needing an install")
	}
}

// A RE-RUN DOES NOT RE-INSTALL A WORKING SEAT: a second install revokes the
// first token, taking a working seat down to fix nothing.
func TestARerunDoesNotReinstallAWorkingSeat(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-minted",
		"bot_user_id":"U0BOTSWE","team":{"id":"T0ACME"}}`
	recorder := newSink()
	installs := 0
	installer := func(context.Context, string, string) (string, error) {
		installs++
		return "the-temporary-code", nil
	}

	_, ledger, path, err := run(t, ws, func(o *slack.Options) {
		o.Sink = recorder
		o.Install = installer
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slack.Reconcile(context.Background(), slack.Options{
		Admin: slack.NewAdmin(rewriting(ws.URL)), Seats: plans(),
		Ledger: ledger, LedgerPath: path, Sink: recorder,
		BaseURL: "https://engine.example.com", Install: installer,
	}); err != nil {
		t.Fatal(err)
	}
	if installs != 1 {
		t.Fatalf("the operator was asked to install %d times", installs)
	}
}

// AN UNREADABLE SINK IS NOT AN UNHELD TOKEN. Treating it as "no token" sends
// the operator through an install they did not need — and that install
// revokes the working token.
func TestAnUnreadableSinkDoesNotTriggerAnInstall(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	recorder := newSink()

	_, _, _, err := run(t, ws, func(o *slack.Options) {
		o.Sink = recorder
		o.Install = func(context.Context, string, string) (string, error) {
			t.Error("an install was attempted against an unreadable sink")
			return "", nil
		}
		recorder.err = errUnreadable
	})
	if err == nil {
		t.Fatal("an unreadable sink was treated as an empty one")
	}
}

// A LITERAL CREDENTIAL CANNOT BE MINTED INTO, and rewriting the company
// config from a provisioning run is not this command's job.
func TestALiteralCredentialIsReportedAndSkipped(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "nimbus", Roles: []*org.Role{
		{Name: "Engineer", DeclaredHandle: "swe", Slack: org.SlackIdentity{
			BotToken: "xoxb-managed-by-hand", SigningSecret: "${SWE_SLACK_SIGNING}",
		}},
	}}
	o.Normalize()
	plan := slack.PlanFor(o)
	if len(plan) != 1 || plan[0].Provisionable() {
		t.Fatalf("a literal token was reported provisionable: %+v", plan)
	}
	if len(plan[0].Notes) == 0 {
		t.Error("a seat that cannot be provisioned says nothing about why")
	}
}

// A RUN WITH NOWHERE TO PUT WHAT IT MINTS IS REFUSED UP FRONT, and so is one
// with no public base — an app made without one delivers nowhere.
func TestARunWithoutItsInputsIsRefusedBeforeAnythingIsMade(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)

	if _, _, _, err := run(t, ws, func(o *slack.Options) { o.Sink = nil }); err == nil {
		t.Error("a run with no sink created apps")
	}
	if _, _, _, err := run(t, ws, func(o *slack.Options) { o.BaseURL = "" }); err == nil {
		t.Error("a run with no public base created apps")
	}
	if ws.called("apps.manifest.create") != 0 {
		t.Errorf("%d app(s) were created by a run that should not have started",
			ws.called("apps.manifest.create"))
	}
}

// THE MANIFEST IS WHAT THE PARSER AND THE EDGE BOTH NEED.
func TestTheManifestSubscribesToWhatTheParserRoutes(t *testing.T) {
	t.Parallel()
	manifest := slack.Manifest("Engineering Lead", "swe", "https://engine.example.com/")
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(encoded)
	for _, want := range []string{
		"https://engine.example.com/webhooks/slack/swe",
		"https://engine.example.com/webhooks/slack-oauth",
		`"app_mention"`, `"message.im"`, `"message.mpim"`,
		`"message.groups"`, `"message.channels"`, `"chat:write"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the manifest is missing %s", want)
		}
	}
	// SOCKET MODE WOULD MEAN NOTHING ARRIVES at the request URL, and
	// token rotation would expire every seat's credential in twelve hours
	// with nothing to refresh it.
	for _, forbidden := range []string{
		`"socket_mode_enabled":true`, `"token_rotation_enabled":true`,
		`"org_deploy_enabled":true`,
	} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("the manifest sets %s", forbidden)
		}
	}
}

// THE APP IS NAMED FOR THE ROLE AND THE BOT FOR THE HANDLE, and the two are
// not interchangeable: one is what an operator picks out of an app
// directory, the other is the identity the rest of the engine addresses.
func TestTheAppAndTheBotAreNamedDifferently(t *testing.T) {
	t.Parallel()
	manifest := slack.Manifest("Engineering Lead", "swe", "https://engine.example.com")
	display := manifest["display_information"].(map[string]any)
	if display["name"] != "Engineering Lead" {
		t.Errorf("app name = %v", display["name"])
	}
	bot := manifest["features"].(map[string]any)["bot_user"].(map[string]any)
	if bot["display_name"] != "swe" {
		t.Errorf("bot display name = %v", bot["display_name"])
	}
	// Slack refuses an app name over 35 characters, and refusing the whole
	// manifest for it would fail a run over a role somebody named
	// carefully.
	long := slack.Manifest(strings.Repeat("x", 80), "swe", "https://e.example.com")
	if got := long["display_information"].(map[string]any)["name"].(string); len(got) != 35 {
		t.Errorf("a long role name produced a %d-character app name", len(got))
	}
}

// THE PASTED CODE IS WHATEVER THE OPERATOR HAD IN THEIR CLIPBOARD.
func TestTheInstallCodeIsReadOutOfWhateverWasPasted(t *testing.T) {
	t.Parallel()
	for pasted, want := range map[string]string{
		"1234.5678":       "1234.5678",
		"  1234.5678  \n": "1234.5678",
		"https://engine.example.com/webhooks/slack-oauth?code=1234.5678&state=swe": "1234.5678",
		"code=1234.5678&state=swe": "1234.5678",
		"":                         "",
	} {
		if got := slack.InstallCode(pasted); got != want {
			t.Errorf("InstallCode(%q) = %q, want %q", pasted, got, want)
		}
	}
}

var errUnreadable = errUnreadableType{}

type errUnreadableType struct{}

func (errUnreadableType) Error() string { return "the store is unreachable" }

var _ provision.TokenSink = (*sink)(nil)

// ONE SEAT'S FAILURE MUST NOT COST THE OTHERS.
//
// Aborting at the first failure is far more expensive here than elsewhere:
// the manifest methods allow about one request a minute, so a run that stops
// at seat three of seven has spent minutes of rate-limit waiting and leaves
// the operator to redo the lot.
func TestOneSeatsFailureDoesNotCostTheOthers(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-minted",
		"bot_user_id":"U0BOTSWE","team":{"id":"T0ACME"}}`

	o := &org.Organization{Name: "nimbus", Roles: []*org.Role{
		{Name: "Engineer", DeclaredHandle: "swe", Slack: org.SlackIdentity{
			BotToken: "${SWE_SLACK_TOKEN}", SigningSecret: "${SWE_SLACK_SIGNING}"}},
		{Name: "Reviewer", DeclaredHandle: "qa", Slack: org.SlackIdentity{
			BotToken: "${QA_SLACK_TOKEN}", SigningSecret: "${QA_SLACK_SIGNING}"}},
	}}
	o.Normalize()
	recorder := newSink()
	path := filepath.Join(t.TempDir(), slack.LedgerName)
	ledger, err := slack.LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}

	res, err := slack.Reconcile(context.Background(), slack.Options{
		Admin: slack.NewAdmin(rewriting(ws.URL)), Seats: slack.PlanFor(o),
		Ledger: ledger, LedgerPath: path, Sink: recorder,
		BaseURL: "https://engine.example.com", ConfigRefreshToken: "xoxe-1-refresh",
		Install: func(_ context.Context, handle, _ string) (string, error) {
			if handle == "swe" {
				return "", errUnreadable
			}
			return "the-temporary-code", nil
		},
	})
	if err == nil {
		t.Fatal("a failed seat did not fail the run's exit status")
	}
	if _, failed := res.Failed["swe"]; !failed {
		t.Errorf("the failing seat is not reported: %+v", res.Failed)
	}
	if len(res.Installed) != 1 || res.Installed[0] != "qa" {
		t.Fatalf("the healthy seat did not finish: %+v", res.Installed)
	}
	if got := recorder.value("QA_SLACK_TOKEN"); got != "xoxb-minted" {
		t.Errorf("the healthy seat's token = %q", got)
	}
	// And what completed is durable, so a re-run resumes rather than
	// starting over.
	reloaded, err := slack.LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Apps) != 2 {
		t.Errorf("the ledger holds %d app(s), so a re-run would duplicate one",
			len(reloaded.Apps))
	}
}

// NARROWING THE RUN COSTS NOTHING FOR THE SEATS IT SKIPS, which is what
// makes fixing one seat in a company of twenty affordable against a method
// that allows about one request a minute.
func TestNarrowingTheRunSkipsEveryOtherSeat(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)

	o := &org.Organization{Name: "nimbus", Roles: []*org.Role{
		{Name: "Engineer", DeclaredHandle: "swe", Slack: org.SlackIdentity{
			BotToken: "${SWE_SLACK_TOKEN}", SigningSecret: "${SWE_SLACK_SIGNING}"}},
		{Name: "Reviewer", DeclaredHandle: "qa", Slack: org.SlackIdentity{
			BotToken: "${QA_SLACK_TOKEN}", SigningSecret: "${QA_SLACK_SIGNING}"}},
	}}
	o.Normalize()
	path := filepath.Join(t.TempDir(), slack.LedgerName)
	ledger, err := slack.LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}

	res, err := slack.Reconcile(context.Background(), slack.Options{
		Admin: slack.NewAdmin(rewriting(ws.URL)), Seats: slack.PlanFor(o),
		Ledger: ledger, LedgerPath: path, Sink: newSink(),
		BaseURL: "https://engine.example.com", ConfigRefreshToken: "xoxe-1-refresh",
		Only: []string{"qa"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 || res.Created[0] != "qa" {
		t.Fatalf("created %v", res.Created)
	}
	if ws.called("apps.manifest.create") != 1 {
		t.Errorf("a narrowed run made %d app(s)", ws.called("apps.manifest.create"))
	}
}

// --- a ledger entry that names a dead app ---------------------------------- //

// AN APP DELETED IN THE CONSOLE LEAVES A LEDGER ENTRY THAT LIES. Its manifest
// hash still matches, so the seat reads as kept — while the token in its
// ${VAR} authenticates as nothing and nobody is told.
func TestADeletedAppIsReplacedRatherThanReportedKept(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	res, ledger, _, err := run(t, ws, nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("first run = %+v", res)
	}
	first := ledger.Apps["swe"].AppID

	// Somebody deletes the app at api.slack.com/apps. The ledger still
	// names it and its manifest hash still matches.
	ws.refuseOnce["apps.manifest.validate"] = "app_not_found"
	ws.replies["apps.manifest.create"] = `{"ok":true,"app_id":"A0REPLACED",
		"credentials":{"client_id":"9.9","client_secret":"cs2","signing_secret":"ss2"}}`
	res, ledger, _, err = runWith(t, ws, ledger, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Created) != 1 || len(res.Kept) != 0 {
		t.Fatalf("second run = %+v", res)
	}
	if got := ledger.Apps["swe"].AppID; got == first || got != "A0REPLACED" {
		t.Errorf("the ledger still names %q", got)
	}
	if !anySlackNote(res.Notes, "no longer exists") {
		t.Errorf("nothing reported the replacement: %v", res.Notes)
	}
}

// THE STALE BOT TOKEN IS THE COMPOUNDING HALF. It belongs to the dead app, it
// satisfies the "already installed" check, and a run that trusted it would
// create a replacement app nobody ever installs.
func TestReplacingADeadAppForcesAFreshInstall(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-first",
		"bot_user_id":"U0BOT","app_id":"A0SWE","team":{"id":"T0"}}`
	s := newSink()
	installer := func(context.Context, string, string) (string, error) { return "code", nil }
	res, ledger, _, err := run(t, ws, func(o *slack.Options) {
		o.Sink = s
		o.Install = installer
	})
	if err != nil || len(res.Installed) != 1 {
		t.Fatalf("first run: %+v %v", res, err)
	}

	ws.refuseOnce["apps.manifest.validate"] = "app_not_found"
	ws.replies["apps.manifest.create"] = `{"ok":true,"app_id":"A0REPLACED",
		"credentials":{"client_id":"9.9","client_secret":"cs2","signing_secret":"ss2"}}`
	ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-second",
		"bot_user_id":"U0BOT2","app_id":"A0REPLACED","team":{"id":"T0"}}`
	res, _, _, err = runWith(t, ws, ledger, func(o *slack.Options) {
		o.Sink = s
		o.Install = installer
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Installed) != 1 {
		t.Fatalf("the replacement was not installed: %+v", res)
	}
	if got := s.value("SWE_SLACK_TOKEN"); got != "xoxb-second" {
		t.Errorf("the seat still holds %q", got)
	}
}

// A REFUSAL IS NOT AN ABSENCE. An app this credential may not touch still
// EXISTS, and replacing it would leave two apps for one seat.
func TestAnAppThisCredentialCannotSeeIsNotReplaced(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	_, ledger, _, err := run(t, ws, nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	ws.refuse["apps.manifest.validate"] = "not_authed"
	res, ledger, _, err := runWith(t, ws, ledger, nil)
	if err == nil {
		t.Fatal("a permission refusal was read as a missing app")
	}
	if len(res.Created) != 0 {
		t.Errorf("a replacement was created: %+v", res)
	}
	if got := ledger.Apps["swe"].AppID; got != "A0SWE" {
		t.Errorf("the ledger entry was dropped: %q", got)
	}
}

// --- adopting an app created by hand --------------------------------------- //

// A HALF-SEEDED ENTRY NAMES WHAT TO COPY. An app id alone builds an authorize
// URL with an empty client_id, which Slack answers with a page saying nothing
// — or an invalid_client_id from the exchange minutes later.
func TestAHandSeededEntryMissingItsOAuthKeysSaysWhatToCopy(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	path := filepath.Join(t.TempDir(), slack.LedgerName)
	ledger, err := slack.LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	// What an operator adopting an app by hand writes: the id, and not
	// much else.
	ledger.Apps["swe"] = slack.AppRecord{AppID: "A0BYHAND"}
	_, _, _, err = runWith(t, ws, ledger, nil)
	if err == nil {
		t.Fatal("a half-seeded entry ran")
	}
	for _, want := range []string{"client_id", "client_secret", "A0BYHAND"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

// A FULLY SEEDED ENTRY IS ADOPTED, which is the supported path: an operator
// who made the app in the console pastes its four values in and this command
// takes over.
func TestAFullyHandSeededEntryIsAdopted(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-adopted",
		"bot_user_id":"U0BOT","app_id":"A0BYHAND","team":{"id":"T0"}}`
	path := filepath.Join(t.TempDir(), slack.LedgerName)
	ledger, err := slack.LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	ledger.Apps["swe"] = slack.AppRecord{
		AppID: "A0BYHAND", ClientID: "9.9", ClientSecret: "cs",
		SigningSecret: "ss",
	}
	s := newSink()
	res, _, _, err := runWith(t, ws, ledger, func(o *slack.Options) {
		o.Sink = s
		o.Install = func(context.Context, string, string) (string, error) {
			return "code", nil
		}
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Created) != 0 {
		t.Errorf("an adopted app was re-created: %+v", res)
	}
	if got := s.value("SWE_SLACK_TOKEN"); got != "xoxb-adopted" {
		t.Errorf("the adopted app's token is %q", got)
	}
}

// --- the cross-app guard --------------------------------------------------- //

// ONE AUTHORIZE URL IS PRINTED PER SEAT AND THEY LOOK ALIKE. Pasting the wrong
// one mints another app's bot token into this seat's ${VAR}, and the seat then
// posts as a colleague with nothing anywhere reporting it.
func TestACodeBelongingToAnotherAppIsRefusedAndRecordsNothing(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-someone-else",
		"bot_user_id":"U0OTHER","app_id":"A0CTO","team":{"id":"T0"}}`
	s := newSink()
	_, _, _, err := run(t, ws, func(o *slack.Options) {
		o.Sink = s
		o.Install = func(context.Context, string, string) (string, error) {
			return "code", nil
		}
	})
	if err == nil {
		t.Fatal("another app's code was accepted")
	}
	if !strings.Contains(err.Error(), "A0CTO") || !strings.Contains(err.Error(), "A0SWE") {
		t.Errorf("the error names neither app: %v", err)
	}
	if got := s.value("SWE_SLACK_TOKEN"); got != "" {
		t.Errorf("the wrong app's token was recorded: %q", got)
	}
}

// AN INSTANCE THAT SERVES NO app_id IS NOT A MISMATCH. The guard has to be a
// comparison, not a requirement — an unknown answer must not fail an install
// that is fine.
func TestAnExchangeThatNamesNoAppStillInstalls(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-fine",
		"bot_user_id":"U0BOT","team":{"id":"T0"}}`
	s := newSink()
	res, _, _, err := run(t, ws, func(o *slack.Options) {
		o.Sink = s
		o.Install = func(context.Context, string, string) (string, error) {
			return "code", nil
		}
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Installed) != 1 || s.value("SWE_SLACK_TOKEN") != "xoxb-fine" {
		t.Errorf("result = %+v, token = %q", res, s.value("SWE_SLACK_TOKEN"))
	}
}

// --- the config-token precedence ------------------------------------------- //

// THE LEDGER'S PAIR BEATS THE SHELL, which is the reverse of the usual rule
// and the reverse is the point: Slack's rotation is single-use in both
// directions, so a value left in an export is dead the moment this command
// first used it. Preferring it trades the only live pair for a retired one.
func TestAStaleExportDoesNotShadowTheLedgersRefreshToken(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	_, ledger, _, err := run(t, ws, nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	// The access token has expired; only a refresh can get in.
	ledger.ConfigToken.Token = ""
	ws.replies["tooling.tokens.rotate"] = `{"ok":true,"token":"xoxe.xoxp-second",
		"refresh_token":"xoxe-1-third","exp":4102444800}`
	if _, _, _, err := runWith(t, ws, ledger, func(o *slack.Options) {
		o.ConfigRefreshToken = "xoxe-1-stale-from-the-shell"
	}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	sent, _ := ws.lastBody("tooling.tokens.rotate")["refresh_token"].(string)
	if sent != "xoxe-1-next" {
		t.Errorf("the run rotated with %q, not the ledger's live pair", sent)
	}
}

// THE FLAG IS A BOOTSTRAP, and it has to work: a ledger holding nothing has
// no pair to prefer.
func TestTheFlagSeedsALedgerThatHoldsNothing(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	if _, _, _, err := run(t, ws, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	sent, _ := ws.lastBody("tooling.tokens.rotate")["refresh_token"].(string)
	if sent != "xoxe-1-refresh" {
		t.Errorf("the bootstrap token was not used: %q", sent)
	}
}

// --- the dry run's manifest validation ------------------------------------- //

// A DRY RUN CHECKS THE MANIFESTS, because apps.manifest.create is rate
// limited to roughly one request a minute: discovering a malformed manifest
// from the create costs a minute per seat and leaves the seats before the bad
// one already created.
func TestADryRunValidatesEveryManifestAndWritesNothing(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	res, err := slack.Validate(context.Background(), validateOptions(t, ws, nil))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(res.Validated) != 1 || res.Validated[0] != "swe" {
		t.Fatalf("result = %+v", res)
	}
	if ws.called("apps.manifest.validate") != 1 {
		t.Errorf("validate was called %d time(s)", ws.called("apps.manifest.validate"))
	}
	for _, method := range []string{"apps.manifest.create", "apps.manifest.update", "oauth.v2.access"} {
		if ws.called(method) != 0 {
			t.Errorf("a dry run called %s", method)
		}
	}
}

// A REFUSED MANIFEST IS NAMED AND THE RUN EXITS NON-ZERO.
func TestADryRunReportsAManifestSlackRefuses(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	ws.refuse["apps.manifest.validate"] = "invalid_manifest"
	res, err := slack.Validate(context.Background(), validateOptions(t, ws, nil))
	if err == nil {
		t.Fatal("a refused manifest passed")
	}
	if len(res.Validated) != 0 || len(res.Failed) != 1 {
		t.Errorf("result = %+v", res)
	}
}

// NO CONFIG TOKEN IS A NOTE, NOT A FAILURE. The plan is the more valuable
// half of a dry run, and an operator who has not made a config token yet is
// exactly the person reading it.
func TestADryRunWithNoConfigTokenStillPrintsThePlan(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	res, err := slack.Validate(context.Background(),
		validateOptions(t, ws, func(o *slack.Options) { o.ConfigRefreshToken = "" }))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Errorf("result = %+v", res)
	}
	if !anySlackNote(res.Notes, "no manifest could be checked") {
		t.Errorf("notes = %v", res.Notes)
	}
}

// A MANIFEST WITHOUT A PUBLIC URL IS NOT THE MANIFEST A REAL RUN SENDS, so
// validating one would prove the wrong thing.
func TestADryRunWithNoPublicURLChecksNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	res, err := slack.Validate(context.Background(),
		validateOptions(t, ws, func(o *slack.Options) { o.BaseURL = "" }))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if ws.called("apps.manifest.validate") != 0 {
		t.Error("a manifest with no base URL was checked")
	}
	if !anySlackNote(res.Notes, "no -public-url") {
		t.Errorf("notes = %v", res.Notes)
	}
}

// A NEW APP'S MANIFEST IS CHECKED BEFORE IT IS CREATED, for the same
// rate-limit reason — and a refusal must not leave an app behind.
func TestAMalformedManifestIsRefusedBeforeTheAppIsCreated(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	withCreate(ws)
	ws.refuse["apps.manifest.validate"] = "invalid_manifest"
	res, ledger, _, err := run(t, ws, nil)
	if err == nil {
		t.Fatal("a malformed manifest created an app")
	}
	if ws.called("apps.manifest.create") != 0 {
		t.Error("the create was attempted anyway")
	}
	if len(res.Created) != 0 || len(ledger.Apps) != 0 {
		t.Errorf("result = %+v, ledger = %+v", res, ledger.Apps)
	}
}

// --- helpers --------------------------------------------------------------- //

// runWith is [run] against a ledger a test has already shaped.
func runWith(t *testing.T, ws *workspace, ledger *slack.Ledger,
	mutate func(*slack.Options),
) (*slack.Result, *slack.Ledger, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), slack.LedgerName)
	opts := slack.Options{
		Admin: slack.NewAdmin(rewriting(ws.URL)), Seats: plans(),
		Ledger: ledger, LedgerPath: path, Sink: newSink(),
		BaseURL: "https://engine.example.com", ConfigRefreshToken: "xoxe-1-refresh",
	}
	if mutate != nil {
		mutate(&opts)
	}
	res, err := slack.Reconcile(context.Background(), opts)
	return res, ledger, path, err
}

func validateOptions(t *testing.T, ws *workspace, mutate func(*slack.Options)) slack.Options {
	t.Helper()
	path := filepath.Join(t.TempDir(), slack.LedgerName)
	ledger, err := slack.LoadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := slack.Options{
		Admin: slack.NewAdmin(rewriting(ws.URL)), Seats: plans(),
		Ledger: ledger, LedgerPath: path,
		BaseURL: "https://engine.example.com", ConfigRefreshToken: "xoxe-1-refresh",
	}
	if mutate != nil {
		mutate(&opts)
	}
	return opts
}

func anySlackNote(notes []string, want string) bool {
	for _, note := range notes {
		if strings.Contains(note, want) {
			return true
		}
	}
	return false
}
