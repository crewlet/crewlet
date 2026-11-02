package slack_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
	"github.com/crewlet/crewlet/internal/slack"
)

// throughVendor is the Slack vendor reaching the fake workspace, for a sync
// run exactly as the loop runs it.
type throughVendor struct {
	seataccess.Vendor
	client *http.Client
}

func (v throughVendor) Open(ctx context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return slack.OpenThrough(ctx, in, v.client)
}

// fullSync runs one pass of the shared sync over the world.
func (w *slackWorld) fullSync(t *testing.T, ledger *seataccesstest.Ledger, sealed *seataccesstest.Sink,
	asks map[string]seataccess.Ask) seataccess.Record {
	t.Helper()
	_, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: throughVendor{Vendor: accessVendor(t), client: rewriting(t, w.ws)},
		Open: seataccess.OpenInput{
			Config: w.cfg, Env: config.NewResolver(w.sealed), Sink: w.sink, WebhookBase: w.base,
		},
		Ledger: ledger, Sink: sealed, Pass: "test", By: "test", Asks: asks,
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	rec, ok := ledger.Record(integration.KindSlack, "swe")
	if !ok {
		t.Fatal("the ledger holds no record for swe")
	}
	return rec
}

// A CREATE WAITS FOR ITS INSTALL, through the whole sync. The resolver is a
// snapshot taken when the pass began, so the install step used to find the
// OAuth client its own create had just sealed missing, and refuse with "the
// app's OAuth client is not sealed".
func TestACreateWaitsForItsInstallThroughTheSync(t *testing.T) {
	t.Parallel()
	w := newSlackWorld(t)
	w.ws.replies["apps.manifest.create"] = `{"ok":true,"app_id":"A0SWE",
		"credentials":{"client_id":"1.2","client_secret":"cs","signing_secret":"ss"}}`
	sealed := seataccesstest.NewSink()
	rec := w.fullSync(t, seataccesstest.NewLedger(), sealed, nil)
	lane := rec.Lanes[seataccess.LaneSlack]
	if !slack.AwaitsInstall(rec) || lane.Sync.Error.Actor != seataccess.ByYou {
		t.Errorf("after the create: %+v", lane.Sync)
	}
	if id, ok := sealed.Held("SLACK_CLIENT_ID_SWE"); !ok || id != "1.2" {
		t.Errorf("the OAuth client was not sealed: %q", id)
	}
}

// A ROTATE WAITS FOR THE REINSTALL. Its first step revokes the bot token, and
// the install step after it used to read the token from the pass-start
// snapshot, find the cached observation saying it worked, and mark the
// reinstall done with the agent holding no token at all.
func TestARotateWaitsForTheReinstallThroughTheSync(t *testing.T) {
	t.Parallel()
	w := newSlackWorld(t).installed()
	w.exportsItsOwnManifest(t)
	w.ws.replies["auth.test"] = `{"ok":true,"user_id":"U0SWE","team_id":"T0ACME","user":"swe","app_id":"A0SWE"}`
	w.ws.replies["auth.revoke"] = `{"ok":true,"revoked":true}`
	ledger := seataccesstest.NewLedger()
	if err := ledger.Put(t.Context(), seataccess.Record{
		Kind: integration.KindSlack, Handle: "swe", Name: "Engineer",
		Account: &seataccess.Account{ID: "A0SWE", State: seataccess.AccountActive},
		Credential: &seataccess.Credential{Variable: "SLACK_BOT_TOKEN_SWE",
			Source: seataccess.CredentialMinted},
	}); err != nil {
		t.Fatal(err)
	}
	rec := w.fullSync(t, ledger, seataccesstest.NewSink(),
		map[string]seataccess.Ask{"swe": {Rotate: true}})
	if !slack.AwaitsInstall(rec) {
		t.Errorf("a rotate reported done before anybody reinstalled: %+v",
			rec.Lanes[seataccess.LaneSlack].Sync)
	}
	if n := w.ws.called("auth.revoke"); n != 1 {
		t.Errorf("the token was revoked %d times", n)
	}
}

// A CONFIGURATION TOKEN SLACK RETIRED EARLY IS RENEWED, ONCE: the call is made
// again with the renewal, and a token still refused after it is reported
// rather than renewed in a loop that spends refresh tokens.
func TestAnExpiredConfigurationTokenIsRenewedOnce(t *testing.T) {
	t.Parallel()
	for name, always := range map[string]bool{"renewed": false, "still refused": true} {
		w := newSlackWorld(t)
		w.exportsItsOwnManifest(t)
		w.ws.replies["tooling.tokens.rotate"] = `{"ok":true,"token":"xoxe.xoxp-new","refresh_token":"xoxe-1-next","exp":4102444800}`
		if always {
			w.ws.refuse["apps.manifest.export"] = "token_expired"
		} else {
			w.ws.refuseOnce["apps.manifest.export"] = "token_expired"
		}
		_, err := w.open(t).Observe(t.Context(), seataccess.ObserveInput{
			Seats:   []seataccess.SeatRef{swe},
			Records: map[string]seataccess.Record{"swe": *appRecord(seataccess.AccountActive)},
		})
		if (err != nil) != always {
			t.Errorf("%s: observe answered %v", name, err)
		}
		if n := w.ws.called("tooling.tokens.rotate"); n != 1 {
			t.Errorf("%s: %d renewals", name, n)
		}
		if sealed, _ := slack.DecodeConfigToken(w.sink.value(slack.ConfigTokenVar)); sealed.Token != "xoxe.xoxp-new" {
			t.Errorf("%s: the renewal was not kept: %+v", name, sealed)
		}
	}
}

// A TOKEN IS USED ONLY WITH A PASS'S WORTH OF LIFE LEFT: creating apps is
// rate limited to about one a minute, so a pass opened on a token with minutes
// left fails half way through.
func TestATokenWithLittleLifeLeftIsRotated(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	held := func(left time.Duration) slack.ConfigToken {
		return slack.ConfigToken{Token: "xoxe.xoxp-held", ExpiresAt: now.Add(left)}
	}
	if held(10 * time.Minute).Usable(now) {
		t.Error("a token with ten minutes left was used for a pass")
	}
	if !held(2 * time.Hour).Usable(now) {
		t.Error("a token with two hours left was rotated")
	}
}

// flakyStore refuses its first saves, and remembers whether the context a save
// ran under was already cancelled.
type flakyStore struct {
	fails     int
	saved     slack.ConfigToken
	cancelled bool
}

func (s *flakyStore) LoadConfigToken(context.Context) (slack.ConfigToken, error) {
	return slack.ConfigToken{}, nil
}

func (s *flakyStore) SaveConfigToken(ctx context.Context, token slack.ConfigToken) error {
	s.cancelled = s.cancelled || ctx.Err() != nil
	if s.fails > 0 {
		s.fails--
		return errors.New("the store blinked")
	}
	s.saved = token
	return nil
}

// A ROTATED PAIR IS KEPT WHATEVER HAPPENS TO THE CALLER: the rotate retires the
// refresh token it was given, so a cancelled pass or a store that blinked must
// not lose the only way back into the company's apps.
func TestARotatedPairIsKeptWhateverHappensToTheCaller(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["tooling.tokens.rotate"] = `{"ok":true,"token":"xoxe.xoxp-new","refresh_token":"xoxe-1-next","exp":4102444800}`
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	store := &flakyStore{fails: 1}
	token, err := slack.NewAdmin(rewriting(t, ws)).ConfigToken(ctx, store, "xoxe-1-refresh", time.Now(), true)
	if err != nil || token != "xoxe.xoxp-new" || store.saved.RefreshToken != "xoxe-1-next" {
		t.Fatalf("token %q, saved %+v, %v", token, store.saved, err)
	}
	if store.cancelled {
		t.Error("the save ran under the caller's cancelled context")
	}
}

// A BLOCK THAT NAMES NO CONFIGURATION TOKEN HANDS ITS APPS IN FROM OUTSIDE
// CREWLET (D4b): the sync leaves them alone, whatever the store holds.
func TestABlockWithNoConfigurationTokenIsExternal(t *testing.T) {
	t.Parallel()
	w := newSlackWorld(t)
	w.cfg.Integrations.Slack.AppConfigToken = ""
	_, err := slack.OpenThrough(t.Context(), seataccess.OpenInput{
		Config: w.cfg, Env: config.NewResolver(w.sealed), Sink: w.sink, WebhookBase: w.base,
	}, rewriting(t, w.ws))
	if !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("open answered %v", err)
	}
}

// cancellingSink refuses every seal, cancelling the request's context first,
// which is a person closing the tab while the landing seals.
type cancellingSink struct {
	*sink
	cancel context.CancelFunc
}

func (s cancellingSink) Record(context.Context, string, string) error {
	s.cancel()
	return errors.New("the store refused the write")
}

// A TOKEN THE LANDING COULD NOT SEAL IS REVOKED EVEN AFTER THE REQUEST ENDS,
// so no bot token is live that nothing holds.
func TestALandingRevokeOutlivesTheRequest(t *testing.T) {
	t.Parallel()
	ws := newWorkspace(t)
	ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-new","bot_user_id":"U0SWE","app_id":"A0SWE","team":{"id":"T0"}}`
	ws.replies["auth.revoke"] = `{"ok":true,"revoked":true}`
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := slack.CompleteSeatInstall(ctx, slack.NewAdmin(rewriting(t, ws)),
		cancellingSink{sink: newSink(), cancel: cancel}, rewriting(t, ws),
		slack.SeatApp{Handle: "swe", AppID: "A0SWE", ClientID: "1.2", ClientSecret: "cs"}, "code-1", engineBase)
	if err == nil {
		t.Fatal("a seal that failed reported success")
	}
	if n := ws.called("auth.revoke"); n != 1 {
		t.Errorf("the unsealed token was revoked %d times", n)
	}
}
