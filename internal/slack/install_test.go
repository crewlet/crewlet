package slack_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/slack"
)

// claims is an in-memory first-claim-wins registry.
type claims struct {
	mu   sync.Mutex
	held map[string]bool
	err  error
}

func newClaims() *claims { return &claims{held: map[string]bool{}} }

func (c *claims) Claim(_ context.Context, key string, _ time.Duration, _ time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	if c.held[key] {
		return false, nil
	}
	c.held[key] = true
	return true, nil
}

func (c *claims) Release(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held, key)
	return c.err
}

// installRig is one fleet's install half: a fake workspace, the seat's
// sealed OAuth client, the keyring, the claim registry and the sink.
type installRig struct {
	ws       *workspace
	claims   *claims
	sealed   config.MapSource
	sink     *sink
	now      time.Time
	installs *slack.Installs
}

func newInstallRig(t *testing.T) *installRig {
	t.Helper()
	r := &installRig{
		ws: newWorkspace(t), claims: newClaims(), sink: newSink(),
		now:    time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
		sealed: config.MapSource{"SLACK_CLIENT_ID_SWE": "1.2", "SLACK_CLIENT_SECRET_SWE": "cs"},
	}
	r.installs = r.build(t, r.claims)
	return r
}

func (r *installRig) build(t *testing.T, registry slack.Claims) *slack.Installs {
	t.Helper()
	resolver := config.NewResolver(r.sealed)
	installs, err := slack.NewInstalls(slack.InstallOptions{
		Material: runtoken.OneKey("k1", "fleet-key-material"),
		Claims:   registry,
		Base:     func() string { return engineBase },
		Resolve:  resolver.Value,
		Sink:     func(context.Context) (provision.TokenSink, error) { return r.sink, nil },
		Admin:    slack.NewAdmin(rewriting(t, r.ws)),
		HTTP:     rewriting(t, r.ws),
		Now:      func() time.Time { return r.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return installs
}

func (r *installRig) link(t *testing.T) string {
	t.Helper()
	link, err := r.installs.Link(t.Context(), "swe", "A0SWE")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("client_id") != "1.2" || q.Get("redirect_uri") != slack.OAuthRedirectURL(engineBase) {
		t.Fatalf("the link carries %v", q)
	}
	return q.Get("state")
}

func refusal(t *testing.T, err error) *slack.InstallRefusal {
	t.Helper()
	var r *slack.InstallRefusal
	if !errors.As(err, &r) {
		t.Fatalf("answered %v, want a refusal that names the reason and the remedy", err)
	}
	if r.Reason == "" || r.Remedy == "" {
		t.Errorf("refusal = %+v", r)
	}
	return r
}

// A LINK COMPLETES ONCE: the code is exchanged for this seat's app, the token
// sealed under the seat's own name, and the same state presented again is
// refused, even inside its fifteen minutes.
func TestAnInstallLinkCompletesOnce(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	r.ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-new","bot_user_id":"U0SWE","app_id":"A0SWE","team":{"id":"T0"}}`
	state := r.link(t)
	if !slack.IsInstallState(state) {
		t.Fatalf("a minted state is not recognised as one: %q", state)
	}
	handle, err := r.installs.Complete(t.Context(), state, "code-1")
	if err != nil || handle != "swe" || r.sink.value("SLACK_BOT_TOKEN_SWE") != "xoxb-new" {
		t.Fatalf("complete: %q, %v, sealed %q", handle, err, r.sink.value("SLACK_BOT_TOKEN_SWE"))
	}
	_, err = r.installs.Complete(t.Context(), state, "code-1")
	if got := refusal(t, err); got.Status != http.StatusBadRequest {
		t.Errorf("a replayed link answered %d", got.Status)
	}
	if n := r.ws.called("oauth.v2.access"); n != 1 {
		t.Errorf("a replay reached Slack: %d exchanges", n)
	}
}

// EVERY BAD STATE IS ONE ANSWER: edited, lapsed, minted for another purpose,
// or signed by this keyring and never handed out by this fleet.
func TestABadStateIsRefusedBeforeSlackIsAsked(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	state := r.link(t)

	edited := strings.Replace(state, "swe~", "pm~", 1)
	other, _ := runtoken.New(runtoken.Options{Material: runtoken.OneKey("k1", "fleet-key-material"), Domain: "otel"})
	stranger := newInstallRig(t) // same keyring, its own registry
	unissued, err := stranger.installs.Link(t.Context(), "swe", "A0SWE")
	if err != nil {
		t.Fatal(err)
	}
	unissuedState, _ := url.Parse(unissued)

	for name, bad := range map[string]string{
		"edited":           edited,
		"another purpose":  other.Mint("swe~A0SWE~nonce", time.Hour),
		"never handed out": unissuedState.Query().Get("state"),
		"garbage":          "not-a-state.at.all",
	} {
		if _, err := r.installs.Complete(t.Context(), bad, "code"); refusal(t, err).Status != http.StatusBadRequest {
			t.Errorf("%s: answered %v", name, err)
		}
	}
	r.now = r.now.Add(slack.InstallTTL + time.Second)
	if _, err := r.installs.Complete(t.Context(), state, "code"); refusal(t, err).Status != http.StatusBadRequest {
		t.Errorf("a lapsed link answered %v", err)
	}
	if n := r.ws.called("oauth.v2.access"); n != 0 {
		t.Errorf("a bad state reached Slack %d times", n)
	}
}

// A REGISTRY THAT CANNOT ANSWER REFUSES: this is an authorization check.
func TestAnUnreachableRegistryRefusesTheInstall(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	state := r.link(t)
	r.claims.err = errors.New("the store is unreachable")
	if _, err := r.installs.Complete(t.Context(), state, "code"); refusal(t, err).Status != http.StatusServiceUnavailable {
		t.Errorf("answered %v", err)
	}
	if _, err := r.installs.Link(t.Context(), "swe", "A0SWE"); err == nil {
		t.Error("a link was minted that could not be recorded")
	}
}

// A CODE FROM ANOTHER AGENT'S APP IS REFUSED, its token revoked, nothing
// sealed, and the refusal quotes neither the code nor the token.
func TestAnotherAppsInstallIsRefusedAndRevoked(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	r.ws.replies["oauth.v2.access"] = `{"ok":true,"access_token":"xoxb-colleague","app_id":"A0PM"}`
	r.ws.replies["auth.revoke"] = `{"ok":true,"revoked":true}`
	_, err := r.installs.Complete(t.Context(), r.link(t), "code-secret-1")
	got := refusal(t, err)
	if got.Status != http.StatusConflict || r.sink.value("SLACK_BOT_TOKEN_SWE") != "" || r.ws.called("auth.revoke") != 1 {
		t.Errorf("refusal %+v, sealed %q, revokes %d", got, r.sink.value("SLACK_BOT_TOKEN_SWE"), r.ws.called("auth.revoke"))
	}
	for _, leaked := range []string{"code-secret-1", "xoxb-colleague"} {
		if strings.Contains(got.Reason+got.Remedy, leaked) {
			t.Errorf("the refusal quotes %q", leaked)
		}
	}
}

// NO APP, NO LINK: an install link is minted only for an app whose OAuth
// client is sealed.
func TestNoLinkWithoutAnApp(t *testing.T) {
	t.Parallel()
	r := newInstallRig(t)
	delete(r.sealed, "SLACK_CLIENT_SECRET_SWE")
	installs := r.build(t, r.claims)
	if _, err := installs.Link(t.Context(), "swe", "A0SWE"); !errors.Is(err, slack.ErrNoApp) {
		t.Errorf("answered %v", err)
	}
	if _, err := r.installs.Link(t.Context(), "swe", ""); !errors.Is(err, slack.ErrNoApp) {
		t.Errorf("a link with no app id answered %v", err)
	}
}

// A SEAT AWAITS ITS INSTALL only while its last sync stopped at the approve
// step.
func TestAwaitsInstall(t *testing.T) {
	t.Parallel()
	waiting := seataccess.Record{
		Kind: integration.KindSlack, Handle: "swe",
		Account: &seataccess.Account{ID: "A0SWE", State: seataccess.AccountActive},
		Lanes: map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneSlack: {Sync: &seataccess.SyncResult{
			Outcome: seataccess.OutcomeWaiting,
			Error:   &seataccess.Failure{Step: seataccess.VerbApprove},
		}}},
	}
	if !slack.AwaitsInstall(waiting) {
		t.Error("a seat waiting at the approve step does not await its install")
	}
	converged := waiting
	converged.Lanes = map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneSlack: {Sync: &seataccess.SyncResult{Outcome: seataccess.OutcomeOK}}}
	if slack.AwaitsInstall(converged) {
		t.Error("a converged seat awaits an install")
	}
	closed := waiting
	closed.Account = &seataccess.Account{ID: "A0SWE", State: seataccess.AccountClosed}
	if slack.AwaitsInstall(closed) {
		t.Error("a closed app awaits an install")
	}
}
