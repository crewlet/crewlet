package slack_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/slack"
)

// OFF IS auth.revoke ON THE SEAT'S OWN BOT TOKEN, and a token Slack already
// refuses is the outcome asked for rather than a fault.
func TestRevokeWithdrawsTheBotToken(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reply, refuse string
		revoked       bool
		fails         bool
	}{
		"revoked now":       {reply: `{"ok":true,"revoked":true}`, revoked: true},
		"already revoked":   {refuse: "token_revoked"},
		"never valid":       {refuse: "invalid_auth"},
		"bot deactivated":   {refuse: "account_inactive"},
		"slack is degraded": {refuse: "fatal_error", fails: true},
	} {
		ws := newWorkspace(t)
		if tc.reply != "" {
			ws.replies["auth.revoke"] = tc.reply
		}
		if tc.refuse != "" {
			ws.refuse["auth.revoke"] = tc.refuse
		}
		client, err := slack.NewClient("xoxb-swe", rewriting(t, ws))
		if err != nil {
			t.Fatal(err)
		}
		revoked, err := client.Revoke(t.Context())
		if (err != nil) != tc.fails || revoked != tc.revoked {
			t.Errorf("%s: revoked %v, err %v", name, revoked, err)
		}
		if token := lastToken(ws); token != "xoxb-swe" {
			t.Errorf("%s: revoked with %q, not the seat's own token", name, token)
		}
	}
}

// THE MANIFEST IS READ BACK with the app-configuration token, and an empty
// answer is an error rather than an app with no scopes.
func TestExportManifestReadsTheScopesSlackHolds(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["apps.manifest.export"] = `{"ok":true,"manifest":{"oauth_config":
		{"scopes":{"bot":["chat:write","users:read","chat:write"]}}}}`
	admin := slack.NewAdmin(rewriting(t, ws))
	manifest, err := admin.ExportManifest(t.Context(), "xoxe.xoxp-config", "A0SWE")
	if err != nil {
		t.Fatal(err)
	}
	if got := slack.ManifestScopes(manifest); !slices.Equal(got, []string{"chat:write", "users:read"}) {
		t.Errorf("scopes = %v", got)
	}
	if body := ws.lastBody("apps.manifest.export"); body["app_id"] != "A0SWE" {
		t.Errorf("exported %v", body)
	}

	empty := newWorkspace(t)
	if _, err := slack.NewAdmin(rewriting(t, empty)).ExportManifest(t.Context(), "x", "A0SWE"); err == nil {
		t.Error("an answer with no manifest read as success")
	}
}

// DRIFT IS BOTH DIRECTIONS: what the manifest asks for and the app lacks,
// and what the app holds that nobody asked for.
func TestScopeDriftNamesBothDirections(t *testing.T) {
	t.Parallel()
	held := append(slices.Clone(slack.BotScopes[1:]), "admin")
	slices.Sort(held)
	missing, extra := slack.ScopeDrift(held)
	if !slices.Equal(missing, []string{slack.BotScopes[0]}) || !slices.Equal(extra, []string{"admin"}) {
		t.Errorf("missing %v, extra %v", missing, extra)
	}
	if missing, extra := slack.ScopeDrift(slices.Sorted(slices.Values(slack.BotScopes))); missing != nil || extra != nil {
		t.Errorf("an app holding exactly the manifest drifted: %v %v", missing, extra)
	}
}

// AN APP ALREADY GONE IS THE REMOVAL ASKED FOR; a refusal is not.
func TestDeleteAppTreatsGoneAsDone(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	admin := slack.NewAdmin(rewriting(t, ws))
	if err := admin.DeleteApp(t.Context(), "xoxe.xoxp-config", "A0SWE"); err != nil {
		t.Fatal(err)
	}
	ws.refuse["apps.manifest.delete"] = "app_not_found"
	if err := admin.DeleteApp(t.Context(), "xoxe.xoxp-config", "A0SWE"); err != nil {
		t.Errorf("an app already gone failed the removal: %v", err)
	}
	ws.refuse["apps.manifest.delete"] = "not_allowed_token_type"
	if err := admin.DeleteApp(t.Context(), "xoxe.xoxp-config", "A0SWE"); err == nil {
		t.Error("a refused delete read as done")
	}
}

// THE LOOP'S TOKEN STORE: a usable token is reused, an expiring one is
// rotated and SAVED BEFORE USE as one sealed value, and a save that fails
// says the refresh token is spent.
func TestTheLoopRotatesAndSealsThePairAsOneValue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	ws := newWorkspace(t)
	ws.replies["tooling.tokens.rotate"] = `{"ok":true,"token":"xoxe.xoxp-new",
		"refresh_token":"xoxe-1-next","exp":` + strconv.FormatInt(now.Add(12*time.Hour).Unix(), 10) + `}`
	admin := slack.NewAdmin(rewriting(t, ws))
	sink := newSink()
	store := slack.SinkConfigTokenStore{Sink: sink, Var: slack.ConfigTokenVar}

	// A PERSON PASTED A BARE REFRESH TOKEN into the connect form.
	_ = sink.Record(t.Context(), slack.ConfigTokenVar, "xoxe-1-pasted")
	token, err := admin.ConfigToken(t.Context(), store, "", now, false)
	if err != nil || token != "xoxe.xoxp-new" {
		t.Fatalf("token %q, err %v", token, err)
	}
	if body := ws.lastBody("tooling.tokens.rotate"); body["refresh_token"] != "xoxe-1-pasted" {
		t.Errorf("rotated with %v", body)
	}
	sealed, err := slack.DecodeConfigToken(sink.value(slack.ConfigTokenVar))
	if err != nil || sealed.Token != "xoxe.xoxp-new" || sealed.RefreshToken != "xoxe-1-next" || sealed.ExpiresAt.IsZero() {
		t.Fatalf("sealed %+v, err %v", sealed, err)
	}

	// REUSED WHILE GOOD: no second rotate within its life.
	if _, err := admin.ConfigToken(t.Context(), store, "xoxe-1-stale-bootstrap", now.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if n := ws.called("tooling.tokens.rotate"); n != 1 {
		t.Errorf("a usable token was rotated again: %d rotations", n)
	}

	// FORCED after a refusal, with the HELD refresh token and never the
	// stale bootstrap.
	if _, err := admin.ConfigToken(t.Context(), store, "xoxe-1-stale-bootstrap", now.Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if body := ws.lastBody("tooling.tokens.rotate"); body["refresh_token"] != "xoxe-1-next" {
		t.Errorf("a forced rotation used %v, not the held pair", body)
	}
}

func TestASaveThatFailsSaysTheRefreshTokenIsSpent(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["tooling.tokens.rotate"] = `{"ok":true,"token":"t","refresh_token":"r","exp":4102444800}`
	_, err := slack.NewAdmin(rewriting(t, ws)).ConfigToken(t.Context(),
		failingStore{}, "xoxe-1-refresh", time.Now(), false)
	if err == nil || !strings.Contains(err.Error(), "invalidates the refresh token") {
		t.Errorf("a failed save answered %v", err)
	}
}

func TestNoConfigTokenIsAStepForAPerson(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	_, err := slack.NewAdmin(rewriting(t, ws)).ConfigToken(t.Context(),
		slack.SinkConfigTokenStore{Sink: newSink(), Var: slack.ConfigTokenVar}, "", time.Now(), false)
	if !errors.Is(err, slack.ErrNoConfigToken) {
		t.Errorf("no token answered %v", err)
	}
	if ws.called("tooling.tokens.rotate") != 0 {
		t.Error("a rotate was attempted with nothing to rotate")
	}
}

// A SEALED VALUE THAT IS NOT OURS IS REFUSED WITHOUT QUOTING IT.
func TestAMalformedSealedPairIsRefusedWithoutQuotingIt(t *testing.T) {
	t.Parallel()
	_, err := slack.DecodeConfigToken(`{"token": xoxe-secret`)
	if err == nil || strings.Contains(err.Error(), "xoxe-secret") {
		t.Errorf("decode answered %v", err)
	}
}

// THE ENGINE NAMES EVERY SEAT VARIABLE, vendor first.
func TestSeatVarsAreEngineNamed(t *testing.T) {
	t.Parallel()
	if got := slack.SeatVar("senior-engineer", slack.FieldBotToken); got != "SLACK_BOT_TOKEN_SENIOR_ENGINEER" {
		t.Errorf("bot token var = %q", got)
	}
	if got := slack.SeatVars("sre"); !slices.Equal(got, []string{
		"SLACK_BOT_TOKEN_SRE", "SLACK_SIGNING_SECRET_SRE", "SLACK_CLIENT_SECRET_SRE",
	}) {
		t.Errorf("seat vars = %v", got)
	}
}

type failingStore struct{}

func (failingStore) LoadConfigToken(context.Context) (slack.ConfigToken, error) {
	return slack.ConfigToken{}, nil
}

func (failingStore) SaveConfigToken(context.Context, slack.ConfigToken) error {
	return errors.New("the store is unreachable")
}
