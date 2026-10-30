package slack_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/slack"
)

const engineBase = "https://engine.example.com"

var swe = seataccess.SeatRef{Handle: "swe", Name: "Engineer"}

func accessVendor(t *testing.T) seataccess.Vendor {
	t.Helper()
	vendor, ok := seataccess.Lookup(integration.KindSlack)
	if !ok {
		t.Fatal("the Slack vendor is not registered")
	}
	return vendor
}

// THE CATALOGUE: on or off, the token bound to the Slack tool server, and an
// install that is a person's every time.
func TestSlackCatalog(t *testing.T) {
	t.Parallel()
	cat := accessVendor(t).Catalog()
	if !slices.Equal(cat.Credentials, []string{"BOT_TOKEN", "SIGNING_SECRET", "CLIENT_SECRET", "CLIENT_ID"}) {
		t.Errorf("credentials = %v", cat.Credentials)
	}
	if cat.Tool == nil || cat.Tool.Server != "slack" || cat.Tool.Keys[0].Field != "BOT_TOKEN" ||
		cat.Tool.Keys[0].Env != "SLACK_MCP_XOXB_TOKEN" {
		t.Errorf("tool = %+v", cat.Tool)
	}
	spec, _ := cat.Lane(seataccess.LaneSlack)
	if spec.Levels || spec.Scoped || !spec.Rotatable || spec.EnabledBy != seataccess.ByYou ||
		spec.CreatedBy != seataccess.ByCrewlet {
		t.Errorf("lane = %+v", spec)
	}
}

// EVERY OP DRAWS SLACK'S OWN STEPS.
func TestSlackStepsPerOp(t *testing.T) {
	t.Parallel()
	vendor := accessVendor(t)
	type drawn struct {
		verb seataccess.StepVerb
		by   seataccess.StepActor
	}
	crewlet, you := seataccess.ByCrewlet, seataccess.ByYou
	missing := slack.ManifestDiff{MissingScopes: []string{"chat:write"}}.Describe()
	extra := slack.ManifestDiff{ExtraScopes: []string{"admin"}}.Describe()
	for name, tc := range map[string]struct {
		op   seataccess.Op
		have seataccess.Observed
		out  []drawn
	}{
		"create then install":   {op: seataccess.OpCreate, out: []drawn{{seataccess.VerbCreate, crewlet}, {seataccess.VerbApprove, you}}},
		"back on is an install": {op: seataccess.OpEnable, out: []drawn{{seataccess.VerbApprove, you}}},
		"a lost token is an install": {op: seataccess.OpMint, out: []drawn{{seataccess.VerbApprove, you}}},
		"a scope removed at Slack needs a reinstall": {op: seataccess.OpLevel,
			have: seataccess.Observed{Level: missing},
			out:  []drawn{{seataccess.VerbRestore, crewlet}, {seataccess.VerbApprove, you}}},
		"a scope added at Slack is only restored": {op: seataccess.OpLevel,
			have: seataccess.Observed{Level: extra}, out: []drawn{{seataccess.VerbRestore, crewlet}}},
		"off revokes":            {op: seataccess.OpDisable, out: []drawn{{seataccess.VerbRevoke, crewlet}}},
		"rotate is a reinstall":  {op: seataccess.OpRotate, out: []drawn{{seataccess.VerbRevoke, crewlet}, {seataccess.VerbApprove, you}}},
		"remove deletes the app": {op: seataccess.OpRemove, out: []drawn{{seataccess.VerbRemove, crewlet}}},
		"no scope per seat":      {op: seataccess.OpScope},
	} {
		var got []drawn
		for i, step := range vendor.Steps(swe, seataccess.LaneSlack, tc.op, seataccess.Desired{}, tc.have) {
			got = append(got, drawn{step.Verb, step.By})
			if step.Op != tc.op || step.Index != i || step.Text == "" {
				t.Errorf("%s: step %d = %+v", name, i, step)
			}
		}
		if !slices.Equal(got, tc.out) {
			t.Errorf("%s: drew %v, want %v", name, got, tc.out)
		}
	}
}

// slackWorld is one seat's Slack world: a fake workspace, the company
// document, the sealed values, and a sink for the configuration token.
type slackWorld struct {
	ws     *workspace
	cfg    *config.Company
	sealed config.MapSource
	sink   *sink
	base   string
}

func newSlackWorld(t *testing.T) *slackWorld {
	t.Helper()
	on := true
	pair, err := slack.EncodeConfigToken(slack.ConfigToken{
		Token: "xoxe.xoxp-config", RefreshToken: "xoxe-1-refresh",
		ExpiresAt: time.Now().Add(6 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &slackWorld{
		ws: newWorkspace(t),
		cfg: &config.Company{
			Integrations: config.Integrations{Slack: &config.Slack{}},
			Roles: []config.Role{{Name: "Engineer", Handle: "swe",
				Integrations: config.RoleIntegrations{Slack: &config.RoleSlack{Enabled: &on}}}},
		},
		sealed: config.MapSource{slack.ConfigTokenVar: pair},
		sink:   newSink(),
		base:   engineBase,
	}
}

// installed seals an app's OAuth client and a bot token for swe.
func (w *slackWorld) installed() *slackWorld {
	w.sealed["SLACK_CLIENT_ID_SWE"] = "1.2"
	w.sealed["SLACK_CLIENT_SECRET_SWE"] = "cs"
	w.sealed["SLACK_SIGNING_SECRET_SWE"] = "ss"
	w.sealed["SLACK_BOT_TOKEN_SWE"] = "xoxb-swe"
	return w
}

func (w *slackWorld) open(t *testing.T) seataccess.Session {
	t.Helper()
	session, err := slack.OpenThrough(t.Context(), seataccess.OpenInput{
		Config: w.cfg, Env: config.NewResolver(w.sealed), Sink: w.sink, WebhookBase: w.base,
	}, rewriting(t, w.ws))
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func appRecord(state seataccess.AccountState) *seataccess.Record {
	return &seataccess.Record{
		Kind: integration.KindSlack, Handle: "swe",
		Account: &seataccess.Account{ID: "A0SWE", State: state},
	}
}

func (w *slackWorld) exportsItsOwnManifest(t *testing.T) {
	t.Helper()
	manifest, err := slack.ManifestJSON("Engineer", "swe", engineBase)
	if err != nil {
		t.Fatal(err)
	}
	w.ws.replies["apps.manifest.export"] = `{"ok":true,"manifest":` + manifest + `}`
}

func slackApply(t *testing.T, s seataccess.Session, op seataccess.Op, index int, record *seataccess.Record) (seataccess.StepResult, error) {
	t.Helper()
	steps := accessVendor(t).Steps(swe, seataccess.LaneSlack, op, seataccess.Desired{}, seataccess.Observed{})
	step := seataccess.Step{Op: op, Index: index, By: seataccess.ByCrewlet}
	if index < len(steps) {
		step = steps[index]
	}
	return s.Apply(t.Context(), seataccess.ApplyInput{
		Seat: swe, Lane: seataccess.LaneSlack, Step: step, Record: record,
	})
}

// THE LOOP CREATES THE APP, and hands the once-served values to the sync to
// seal, each secret carrying the undo that deletes the app again.
func TestCreateHandsTheOnceServedValuesToTheSync(t *testing.T) {
	t.Parallel()
	w := newSlackWorld(t)
	w.ws.replies["apps.manifest.create"] = `{"ok":true,"app_id":"A0SWE",
		"credentials":{"client_id":"1.2","client_secret":"cs","signing_secret":"ss"}}`
	res, err := slackApply(t, w.open(t), seataccess.OpCreate, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountID != "A0SWE" || res.Account != "Engineer" || len(res.Minted) != 3 {
		t.Fatalf("result = %+v", res)
	}
	if n := w.ws.called("apps.manifest.validate"); n != 1 {
		t.Errorf("the manifest was validated %d times before the create", n)
	}
	byField := map[string]seataccess.Minted{}
	for _, minted := range res.Minted {
		byField[minted.Field] = minted
	}
	if byField["CLIENT_SECRET"].Value != "cs" || byField["SIGNING_SECRET"].Value != "ss" || byField["CLIENT_ID"].Value != "1.2" {
		t.Errorf("minted = %+v", res.Minted)
	}
	if byField["CLIENT_ID"].Revoke != nil {
		t.Error("the client id carries an undo, and it grants nothing by itself")
	}
	// A SEAL THAT FAILS UNDOES THE CREATE.
	if err := byField["CLIENT_SECRET"].Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body := w.ws.lastBody("apps.manifest.delete"); body["app_id"] != "A0SWE" {
		t.Errorf("the undo deleted %v", body)
	}
	if token := lastToken(w.ws); token != "xoxe.xoxp-config" {
		t.Errorf("the undo authenticated with %q, not the configuration token", token)
	}
}

// AN APP SLACK DID NOT HAND THE SECRETS FOR is deleted rather than kept, and
// a refused create names Slack's reason and the remedy.
func TestACreateThatCannotFinishLeavesNoApp(t *testing.T) {
	t.Parallel()
	w := newSlackWorld(t)
	w.ws.replies["apps.manifest.create"] = `{"ok":true,"app_id":"A0SWE","credentials":{"client_id":"1.2"}}`
	if _, err := slackApply(t, w.open(t), seataccess.OpCreate, 0, nil); err == nil {
		t.Fatal("a create without secrets succeeded")
	}
	if w.ws.called("apps.manifest.delete") != 1 {
		t.Error("the half-made app was kept")
	}

	refused := newSlackWorld(t)
	refused.ws.refuse["apps.manifest.create"] = "invalid_manifest"
	_, err := slackApply(t, refused.open(t), seataccess.OpCreate, 0, nil)
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Said, "invalid_manifest") || refusal.Remedy == "" {
		t.Errorf("a refused create answered %v", err)
	}
}

// NO CONFIGURATION TOKEN IS A STEP FOR A PERSON, said on the step.
func TestNoConfigurationTokenIsSaidOnTheStep(t *testing.T) {
	t.Parallel()
	w := newSlackWorld(t)
	delete(w.sealed, slack.ConfigTokenVar)
	_, err := slackApply(t, w.open(t), seataccess.OpCreate, 0, nil)
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) || refusal.Actor != seataccess.ByYou || refusal.ActionURL != "https://api.slack.com/apps" {
		t.Errorf("no token answered %v", err)
	}
	if w.ws.called("apps.manifest.create") != 0 {
		t.Error("a create was attempted with no token")
	}
}

// AN EXPIRING CONFIGURATION TOKEN IS ROTATED AND SEALED BEFORE USE; a
// preview has no sink and rotates nothing.
func TestOpenRotatesAnExpiringConfigurationToken(t *testing.T) {
	t.Parallel()
	w := newSlackWorld(t)
	stale, _ := slack.EncodeConfigToken(slack.ConfigToken{
		Token: "xoxe.xoxp-old", RefreshToken: "xoxe-1-refresh", ExpiresAt: time.Now().Add(-time.Minute),
	})
	w.sealed[slack.ConfigTokenVar] = stale
	w.ws.replies["tooling.tokens.rotate"] = `{"ok":true,"token":"xoxe.xoxp-new","refresh_token":"xoxe-1-next","exp":4102444800}`
	w.open(t)
	sealed, err := slack.DecodeConfigToken(w.sink.value(slack.ConfigTokenVar))
	if err != nil || sealed.Token != "xoxe.xoxp-new" || sealed.RefreshToken != "xoxe-1-next" {
		t.Fatalf("sealed %+v, %v", sealed, err)
	}

	preview := newSlackWorld(t)
	preview.sealed[slack.ConfigTokenVar] = stale
	session, err := slack.OpenThrough(t.Context(), seataccess.OpenInput{
		Config: preview.cfg, Env: config.NewResolver(preview.sealed),
	}, rewriting(t, preview.ws))
	if err != nil {
		t.Fatal(err)
	}
	if preview.ws.called("tooling.tokens.rotate") != 0 {
		t.Error("a preview rotated the configuration token")
	}
	_, err = slackApply(t, session, seataccess.OpCreate, 0, nil)
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) {
		t.Errorf("a preview's create answered %v", err)
	}
}

// AN INSTALL WAITS ON A PERSON, and is done once the sealed token works.
func TestTheInstallWaitsUntilTheTokenWorks(t *testing.T) {
	t.Parallel()
	w := newSlackWorld(t).installed()
	delete(w.sealed, "SLACK_BOT_TOKEN_SWE")
	res, err := slackApply(t, w.open(t), seataccess.OpCreate, 1, appRecord(seataccess.AccountActive))
	if err != nil || res.Waiting == nil || !strings.Contains(res.Waiting.Detail, "Install") {
		t.Fatalf("not installed: %+v, %v", res, err)
	}
	if res.Waiting.ActionURL != "" {
		t.Errorf("the plan carried an install link of its own: %q", res.Waiting.ActionURL)
	}

	done := newSlackWorld(t).installed()
	done.exportsItsOwnManifest(t)
	done.ws.replies["auth.test"] = `{"ok":true,"user_id":"U0SWE","team_id":"T0ACME","user":"swe"}`
	res, err = slackApply(t, done.open(t), seataccess.OpEnable, 0, appRecord(seataccess.AccountActive))
	if err != nil || res.Waiting != nil {
		t.Errorf("installed: %+v, %v", res, err)
	}
}

// OFF AND ROTATE REVOKE THE SEAT'S OWN TOKEN, and hand its field back for
// clearing; only off records the disable.
func TestOffAndRotateRevokeTheBotToken(t *testing.T) {
	t.Parallel()
	for _, op := range []seataccess.Op{seataccess.OpDisable, seataccess.OpRotate} {
		w := newSlackWorld(t).installed()
		w.ws.replies["auth.revoke"] = `{"ok":true,"revoked":true}`
		res, err := slackApply(t, w.open(t), op, 0, appRecord(seataccess.AccountActive))
		if err != nil || !slices.Equal(res.Revoked, []string{"BOT_TOKEN"}) || res.Disabled != (op == seataccess.OpDisable) {
			t.Errorf("%s: %+v, %v", op, res, err)
		}
		if token := lastToken(w.ws); token != "xoxb-swe" {
			t.Errorf("%s: revoked with %q", op, token)
		}
	}
	refused := newSlackWorld(t).installed()
	refused.ws.refuse["auth.revoke"] = "fatal_error"
	_, err := slackApply(t, refused.open(t), seataccess.OpDisable, 0, appRecord(seataccess.AccountActive))
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Said, "fatal_error") {
		t.Errorf("a refused revoke answered %v", err)
	}
}

// REMOVE DELETES THE APP, every sealed value is dead after it, and without a
// configuration token it waits with the app's page.
func TestRemoveDeletesTheApp(t *testing.T) {
	t.Parallel()
	w := newSlackWorld(t).installed()
	res, err := slackApply(t, w.open(t), seataccess.OpRemove, 0, appRecord(seataccess.AccountDisabled))
	if err != nil || !res.Gone || len(res.Revoked) != 4 {
		t.Fatalf("remove: %+v, %v", res, err)
	}
	if body := w.ws.lastBody("apps.manifest.delete"); body["app_id"] != "A0SWE" {
		t.Errorf("deleted %v", body)
	}

	noToken := newSlackWorld(t).installed()
	delete(noToken.sealed, slack.ConfigTokenVar)
	res, err = slackApply(t, noToken.open(t), seataccess.OpRemove, 0, appRecord(seataccess.AccountDisabled))
	if err != nil || res.Waiting == nil || res.Waiting.ActionURL != "https://api.slack.com/apps/A0SWE" || res.Gone {
		t.Errorf("remove with no token: %+v, %v", res, err)
	}
}

// OBSERVE: the app, its manifest and its token, read back every pass.
func TestSlackObserve(t *testing.T) {
	t.Parallel()
	observe := func(t *testing.T, w *slackWorld, state seataccess.AccountState) seataccess.Observed {
		t.Helper()
		obs, err := w.open(t).Observe(t.Context(), seataccess.ObserveInput{
			Seats:   []seataccess.SeatRef{swe},
			Records: map[string]seataccess.Record{"swe": *appRecord(state)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return obs["swe"][seataccess.LaneSlack]
	}

	w := newSlackWorld(t).installed()
	w.exportsItsOwnManifest(t)
	w.ws.replies["auth.test"] = `{"ok":true,"user_id":"U0SWE","team_id":"T0ACME","user":"swe"}`
	got := observe(t, w, seataccess.AccountActive)
	if !got.Exists || !got.Enabled || got.Level != "" || got.AccountID != "A0SWE" || got.Account != "swe" ||
		got.Credential == nil || got.Credential.Variable != "SLACK_BOT_TOKEN_SWE" {
		t.Errorf("in step: %+v", got)
	}

	// A SCOPE REMOVED IN SLACK'S CONSOLE is drift, said in Slack's words.
	drifted := newSlackWorld(t).installed()
	manifest, _ := slack.ManifestJSON("Engineer", "swe", engineBase)
	drifted.ws.replies["apps.manifest.export"] = `{"ok":true,"manifest":` +
		strings.Replace(manifest, `"chat:write",`, ``, 1) + `}`
	drifted.ws.replies["auth.test"] = `{"ok":true,"user_id":"U0SWE","user":"swe"}`
	if got := observe(t, drifted, seataccess.AccountActive); !strings.Contains(got.Level, "chat:write") {
		t.Errorf("drift = %q", got.Level)
	}

	// A DEAD TOKEN is off: by this engine where the ledger says so, by
	// somebody at Slack otherwise (D3).
	for state, want := range map[seataccess.AccountState]seataccess.DisabledBy{
		seataccess.AccountDisabled: seataccess.DisabledByCrewlet,
		seataccess.AccountActive:   seataccess.DisabledByHand,
	} {
		dead := newSlackWorld(t).installed()
		dead.exportsItsOwnManifest(t)
		dead.ws.refuse["auth.test"] = "token_revoked"
		if got := observe(t, dead, state); !got.Exists || got.Enabled || got.DisabledBy != want {
			t.Errorf("ledger %s: %+v", state, got)
		}
	}

	// AN APP SLACK NO LONGER HAS is no account.
	gone := newSlackWorld(t).installed()
	gone.ws.refuse["apps.manifest.export"] = "app_not_found"
	if got := observe(t, gone, seataccess.AccountActive); got.Exists {
		t.Errorf("a deleted app observed %+v", got)
	}

	// A FAILED READ IS NOT A FINDING: the pass is told Slack could not say.
	down := newSlackWorld(t).installed()
	down.ws.refuse["apps.manifest.export"] = "fatal_error"
	if _, err := down.open(t).Observe(t.Context(), seataccess.ObserveInput{
		Seats: []seataccess.SeatRef{swe}, Records: map[string]seataccess.Record{"swe": *appRecord(seataccess.AccountActive)},
	}); err == nil {
		t.Error("a failed read observed something")
	}
}

// DRIFT IS RESTORED BY PUSHING THE ENGINE'S MANIFEST over the app.
func TestDriftRestorePushesTheManifest(t *testing.T) {
	t.Parallel()
	w := newSlackWorld(t).installed()
	if _, err := slackApply(t, w.open(t), seataccess.OpLevel, 0, appRecord(seataccess.AccountActive)); err != nil {
		t.Fatal(err)
	}
	if body := w.ws.lastBody("apps.manifest.update"); body["app_id"] != "A0SWE" || body["manifest"] == nil {
		t.Errorf("restored %v", body)
	}
}

// THE INSTALL'S CODE MUST BELONG TO THIS SEAT'S APP, and a token that cannot
// be sealed is revoked rather than left live.
func TestCompleteSeatInstall(t *testing.T) {
	t.Parallel()
	seat := slack.SeatApp{Handle: "swe", AppID: "A0SWE", ClientID: "1.2", ClientSecret: "cs"}

	ws := newWorkspace(t)
	ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-new","bot_user_id":"U0SWE","app_id":"A0SWE","team":{"id":"T0ACME"}}`
	sealed := newSink()
	installed, err := slack.CompleteSeatInstall(t.Context(), slack.NewAdmin(rewriting(t, ws)), sealed,
		rewriting(t, ws), seat, "code-1", engineBase)
	if err != nil || installed.BotUserID != "U0SWE" || sealed.value("SLACK_BOT_TOKEN_SWE") != "xoxb-new" {
		t.Fatalf("installed %+v, sealed %q, %v", installed, sealed.value("SLACK_BOT_TOKEN_SWE"), err)
	}

	other := newWorkspace(t)
	other.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-other","app_id":"A0PM"}`
	other.replies["auth.revoke"] = `{"ok":true,"revoked":true}`
	sealed = newSink()
	_, err = slack.CompleteSeatInstall(t.Context(), slack.NewAdmin(rewriting(t, other)), sealed,
		rewriting(t, other), seat, "code-2", engineBase)
	if err == nil || sealed.value("SLACK_BOT_TOKEN_SWE") != "" || other.called("auth.revoke") != 1 {
		t.Errorf("another app's code: %v, sealed %q, revokes %d", err,
			sealed.value("SLACK_BOT_TOKEN_SWE"), other.called("auth.revoke"))
	}

	unsealable := newWorkspace(t)
	unsealable.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-new","app_id":"A0SWE"}`
	unsealable.replies["auth.revoke"] = `{"ok":true,"revoked":true}`
	_, err = slack.CompleteSeatInstall(t.Context(), slack.NewAdmin(rewriting(t, unsealable)), refusingSink{newSink()},
		rewriting(t, unsealable), seat, "code-3", engineBase)
	if err == nil || unsealable.called("auth.revoke") != 1 {
		t.Errorf("an unsealable token: %v, revokes %d", err, unsealable.called("auth.revoke"))
	}
	if token := lastToken(unsealable); token != "xoxb-new" {
		t.Errorf("revoked %q, not the token that could not be sealed", token)
	}
}

// refusingSink refuses every seal.
type refusingSink struct{ *sink }

func (refusingSink) Record(context.Context, string, string) error {
	return errors.New("the store is unreachable")
}

// lastToken is the credential the workspace's last call carried.
func lastToken(ws *workspace) string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if len(ws.tokens) == 0 {
		return ""
	}
	return ws.tokens[len(ws.tokens)-1]
}
