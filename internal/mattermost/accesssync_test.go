package mattermost_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// accessWorld is one company, its Mattermost instance, the ledger and the sealed
// store, run through the shared sync exactly as the loop runs it.
type accessWorld struct {
	t       *testing.T
	chat    *accessChat
	company *config.Company
	ledger  *seataccesstest.Ledger
	sink    *seataccesstest.Sink
	drift   []seataccess.DriftRestored
}

func newAccessWorld(t *testing.T, roles []string, channels ...string) *accessWorld {
	t.Helper()
	chat := newAccessChat(t)
	on := true
	company := &config.Company{}
	company.Integrations.Mattermost = &config.Mattermost{
		Enabled: true, URL: chat.url, Team: "nimbus",
		Provisioning: &config.MattermostProvisioning{
			UsernamePrefix: "agent-", AdminToken: "${MM_ADMIN}",
			Channels: []string{"general", "incidents"},
		},
	}
	company.Roles = []config.Role{{
		Name: "SRE", Handle: "sre",
		Integrations: config.RoleIntegrations{Mattermost: &config.RoleMattermost{
			Enabled: &on, Roles: roles, Channels: channels,
		}},
	}}
	return &accessWorld{
		t: t, chat: chat, company: company,
		ledger: seataccesstest.NewLedger(), sink: seataccesstest.NewSink(),
	}
}

func (w *accessWorld) seat() *config.RoleMattermost {
	return w.company.Roles[0].Integrations.Mattermost
}

func (w *accessWorld) sync() seataccess.SyncReport {
	w.t.Helper()
	vendor, _ := seataccess.Lookup(integration.KindMattermost)
	report, err := seataccess.Sync(context.Background(), seataccess.SyncInput{
		Vendor: vendor, Ledger: w.ledger, Sink: w.sink, Pass: "test", By: "test",
		Open: seataccess.OpenInput{
			Config: w.company,
			Env:    config.NewResolver(config.MapSource{"MM_ADMIN": "fake-admin"}),
		},
		OnDrift: func(_ context.Context, d seataccess.DriftRestored) { w.drift = append(w.drift, d) },
	})
	if err != nil {
		w.t.Fatalf("sync: %v", err)
	}
	return report
}

func (w *accessWorld) record() seataccess.Record {
	w.t.Helper()
	rec, ok := w.ledger.Record(integration.KindMattermost, "sre")
	if !ok {
		w.t.Fatal("the ledger has no record for sre")
	}
	return rec
}

// A SEAT'S WHOLE LIFE THROUGH THE SHARED SYNC: on creates the bot in the team
// with its roles and channels and seals its token under the engine's name; a
// converged pass writes nothing; a channel and a role changed by hand are
// restored and reported; off deactivates the bot, revokes its token and
// clears the sealed value; back on reactivates it and seals a fresh token.
func TestAMattermostSeatThroughTheSync(t *testing.T) {
	t.Parallel()
	w := newAccessWorld(t, []string{"system_user", "system_post_all_public"}, "general")

	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("turning on reported %+v", report.Findings)
	}
	rec := w.record()
	if !rec.Held() || rec.Credential == nil || rec.Credential.Variable != "MATTERMOST_BOT_TOKEN_SRE" {
		t.Fatalf("record = %+v", rec)
	}
	bot := w.chat.user(rec.Account.ID)
	if bot.username != "agent-sre" || !mattermost.SameRoles(strings.Fields(bot.roles),
		[]string{"system_user", "system_post_all_public"}) {
		t.Errorf("bot = %+v", bot)
	}
	if got := w.chat.channelsOf(rec.Account.ID); !slices.Equal(got, []string{"general"}) {
		t.Errorf("channels = %q", got)
	}
	if _, ok := w.sink.Held("MATTERMOST_BOT_TOKEN_SRE"); !ok {
		t.Fatal("no token sealed under the engine's name")
	}

	writes := len(w.chat.wrote())
	if report := w.sync(); len(report.Findings) != 0 {
		t.Errorf("a converged pass reported %+v", report.Findings)
	}
	if got := w.chat.wrote(); len(got) != writes {
		t.Errorf("a converged pass wrote %q", got[writes:])
	}

	// DRIFT: somebody makes the bot a System Admin and adds it to a
	// managed channel the seat does not name.
	w.chat.mu.Lock()
	w.chat.users[rec.Account.ID].roles = "system_admin system_user"
	w.chat.join("c-incidents", rec.Account.ID)
	w.chat.mu.Unlock()
	w.sync()
	if got := w.chat.user(rec.Account.ID).roles; !mattermost.SameRoles(strings.Fields(got),
		[]string{"system_user", "system_post_all_public"}) {
		t.Errorf("roles after drift = %q", got)
	}
	if got := w.chat.channelsOf(rec.Account.ID); !slices.Equal(got, []string{"general"}) {
		t.Errorf("channels after drift = %q", got)
	}
	if len(w.drift) == 0 {
		t.Error("no drift_restored was reported")
	}

	// OFF.
	off := false
	w.seat().Enabled = &off
	w.sync()
	bot = w.chat.user(rec.Account.ID)
	if bot.deleteAt == 0 || bot.description != mattermost.DisconnectedDescription {
		t.Errorf("bot after off = %+v, want deactivated and marked", bot)
	}
	if tokens := w.chat.tokenIDs(rec.Account.ID); len(tokens) != 0 {
		t.Errorf("tokens after off = %q", tokens)
	}
	if _, ok := w.sink.Held("MATTERMOST_BOT_TOKEN_SRE"); ok {
		t.Error("the sealed token outlived the seat being turned off")
	}

	// BACK ON.
	on := true
	w.seat().Enabled = &on
	w.sync()
	bot = w.chat.user(rec.Account.ID)
	if bot.deleteAt != 0 || bot.description != "" || len(w.chat.tokenIDs(rec.Account.ID)) != 1 {
		t.Errorf("bot after back on = %+v", bot)
	}
	if _, ok := w.sink.Held("MATTERMOST_BOT_TOKEN_SRE"); !ok {
		t.Error("no token sealed after turning the seat back on")
	}
}

// A TOKEN THE SYNC CANNOT SEAL IS REVOKED AT MATTERMOST (design 5.2), and the
// seat reports the failure rather than holding a live token nobody has.
func TestAMattermostTokenThatCannotBeSealedIsRevoked(t *testing.T) {
	t.Parallel()
	w := newAccessWorld(t, []string{"system_user"})
	w.sink.FailRecord = map[string]error{"MATTERMOST_BOT_TOKEN_SRE": errors.New("the store refused")}
	report := w.sync()
	rec := w.record()
	if !rec.Held() {
		t.Fatalf("record = %+v, want the created bot recorded", rec)
	}
	if tokens := w.chat.tokenIDs(rec.Account.ID); len(tokens) != 0 {
		t.Errorf("tokens = %q, want the unsealed token revoked", tokens)
	}
	if len(report.Findings) != 1 || report.Findings[0].Kind != integration.FindingIdentityFailed {
		t.Errorf("findings = %+v", report.Findings)
	}
}

// A CHANNEL OUTSIDE THE COMPANY'S LIST IS REFUSED BEFORE ANYTHING IS WRITTEN,
// so a seat can never reach further than the company does.
func TestAMattermostSeatCannotWidenTheCompanysReach(t *testing.T) {
	t.Parallel()
	w := newAccessWorld(t, []string{"system_user"}, "random")
	report := w.sync()
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, "random") {
		t.Errorf("findings = %+v, want the channel outside the company's list", report.Findings)
	}
	if writes := w.chat.wrote(); len(writes) != 0 {
		t.Errorf("writes = %q, want none", writes)
	}
}

// A TOKEN REVOKED AT MATTERMOST IS REPLACED. The ledger still records the
// sealed token, and the bot holds none of this engine's, so every socket the
// sealed value opens is refused: the next sync mints and seals a fresh one.
func TestAMattermostTokenRevokedAtMattermostIsReMinted(t *testing.T) {
	t.Parallel()
	w := newAccessWorld(t, []string{"system_user"}, "general")
	w.sync()
	rec := w.record()
	before, _ := w.sink.Held("MATTERMOST_BOT_TOKEN_SRE")
	w.chat.mu.Lock()
	w.chat.tokens[rec.Account.ID] = nil
	w.chat.mu.Unlock()

	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("findings = %+v", report.Findings)
	}
	after, ok := w.sink.Held("MATTERMOST_BOT_TOKEN_SRE")
	if !ok || after == before || len(w.chat.tokenIDs(rec.Account.ID)) != 1 {
		t.Errorf("sealed=%v changed=%v tokens=%q, want a fresh token sealed",
			ok, after != before, w.chat.tokenIDs(rec.Account.ID))
	}
}
