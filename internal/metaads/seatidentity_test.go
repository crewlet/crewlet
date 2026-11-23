package metaads_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/metaads"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// ownToken is the seat's own system user's token on the fake.
const ownToken = "EAAGownSEATtokenEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE"

// seats is the access ledger as the product reads one seat's record.
type seats struct{ identity *seataccess.IdentityRecord }

func (s *seats) SeatRecord(_ context.Context, kind integration.Kind, handle string) (seataccess.Record, bool, error) {
	if s.identity == nil {
		return seataccess.Record{}, false, nil
	}
	return seataccess.Record{Kind: kind, Handle: handle, Lanes: map[seataccess.Lane]seataccess.LaneRecord{
		seataccess.LaneMetaAds: {Identity: s.identity}}}, true, nil
}

// ownFixture is a product whose seat at level is set to its own identity,
// its record as s holds it, and its own token sealed where the sync seals it.
func ownFixture(t *testing.T, level string, s *seats) *fixture {
	t.Helper()
	f := newMeta(t)
	f.scopes[ownToken] = []string{"ads_management", "ads_read"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	fx := &fixture{f: f, audit: &audit{t: t}, ren: &renewals{}, doc: company(level, false)}
	fx.doc.Roles[0].Integrations.MetaAds.Identity = "own"
	sealed := seataccess.SinkName(integration.KindMetaAds, metaads.SeatTokenField, "growth")
	values := map[string]string{"${S}": appSecret, "${R}": readToken, "${O}": operateToken, "${A}": adminToken,
		"${" + sealed + "}": ownToken}
	fx.p = metaads.New(metaads.Deps{Company: func() *config.Company { return fx.doc },
		Resolve: resolver(values), HTTP: srv.Client(), Base: srv.URL, Audit: fx.audit,
		Leases: &leases{}, Renewals: fx.ren, Now: func() time.Time { return now },
		Identity: func(ctx context.Context, handle string) (seataccess.Signer, error) {
			return seataccess.ResolveSeat(ctx, s, fx.doc, seataccess.LaneMetaAds, handle)
		}})
	return fx
}

// kept is a kept own identity whose last check read status.
func kept(status seataccess.IdentityStatus) *seataccess.IdentityRecord {
	return &seataccess.IdentityRecord{Way: seataccess.WayMade, ID: "42", Name: "crewlet-growth",
		Credential: &seataccess.Credential{Variable: seataccess.SinkName(integration.KindMetaAds,
			metaads.SeatTokenField, "growth")},
		Check: &seataccess.IdentityCheck{Status: status, Said: "Meta no longer accepts this token."}}
}

// bearers are the tokens the fake was called with, past the first n requests,
// debug_token's app token left out.
func (f *meta) bearers(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests[n:] {
		if b := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); !strings.Contains(b, "|") {
			out = append(out, b)
		}
	}
	return out
}

func (f *meta) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// AN AGENT'S OWN SYSTEM USER SIGNS ITS CALLS once it keeps one that works:
// its reads and its changes go as its own token, which the access log names,
// while the engine's own policy reads stay the company's.
func TestAnAgentsCallsGoAsItsOwnSystemUser(t *testing.T) {
	t.Parallel()
	fx := ownFixture(t, "operate", &seats{identity: kept(seataccess.IdentityReady)})
	before := fx.f.asked()
	answer, err := fx.p.Run(context.Background(), fx.call(metaads.ToolRead, map[string]any{
		"operation": "campaign.get", "path": map[string]any{"campaign_id": "501"}}))
	if err != nil || answer.(metaads.Answer).Token != "own" {
		t.Fatalf("a read went through %+v (%v)", answer, err)
	}
	if got := fx.f.bearers(before); len(got) == 0 || got[len(got)-1] != ownToken {
		t.Fatalf("the read was sent as %v, want the agent's own token last", got)
	}
	before = fx.f.asked()
	if _, err := fx.p.Run(context.Background(), fx.call(metaads.ToolCampaigns, map[string]any{})); err != nil {
		t.Fatal(err)
	}
	if got := fx.f.bearers(before); got[len(got)-1] != ownToken {
		t.Fatalf("the campaign list was sent as %v", got)
	}
	before = fx.f.asked()
	if _, _, err := fx.operate(t, metaads.ToolPause, map[string]any{"level": "ad", "ids": []any{"702"}}); err != nil {
		t.Fatal(err)
	}
	if rec := fx.audit.last().rec; rec.Signer != growth.SignerOwn || rec.Via != "" {
		t.Errorf("the pause is logged as signed by %q through %q, want the agent's own and no company slot",
			rec.Signer, rec.Via)
	}
	for _, b := range fx.f.bearers(before) {
		if b == operateToken {
			t.Fatal("the pause went as the company's operate token")
		}
	}
}

// A REFUSED OWN IDENTITY STOPS THE AGENT'S CALLS: nothing reaches Meta, and
// the company's token is never put in its place.
func TestARefusedOwnIdentityStopsTheAgentsCalls(t *testing.T) {
	t.Parallel()
	fx := ownFixture(t, "operate", &seats{identity: kept(seataccess.IdentityRefused)})
	before := fx.f.asked()
	_, err := fx.p.Run(context.Background(), fx.call(metaads.ToolRead, map[string]any{
		"operation": "campaign.get", "path": map[string]any{"campaign_id": "501"}}))
	refusedAs(t, err, growth.RefusedForbidden, "own Meta system user is refused")
	_, err = fx.p.Run(context.Background(), fx.call(metaads.ToolCampaigns, map[string]any{}))
	refusedAs(t, err, growth.RefusedForbidden, "No call was made")
	if _, _, err := fx.operate(t, metaads.ToolPause, map[string]any{"level": "ad", "ids": []any{"702"}}); err == nil {
		t.Fatal("a refused agent paused an ad")
	}
	if n := fx.f.asked(); n != before {
		t.Fatalf("%d requests reached Meta for an agent whose own identity is refused", n-before)
	}
}

// UNTIL ONE IS KEPT, OR PAST META'S CAP, the agent calls as the company's
// system user; an own token whose permissions no longer match its level is
// refused at the call rather than used beyond it.
func TestAnAgentWithNoOwnIdentityKeptCallsAsTheCompany(t *testing.T) {
	t.Parallel()
	capped := &seataccess.IdentityRecord{Attempt: &seataccess.IdentityCheck{Status: seataccess.IdentityNone,
		Blocked: "Meta allows this business 10 system users and all 10 are used."}}
	for name, s := range map[string]*seats{"nothing kept": {}, "past the cap": {identity: capped}} {
		fx := ownFixture(t, "operate", s)
		answer, err := fx.p.Run(context.Background(), fx.call(metaads.ToolRead, map[string]any{
			"operation": "campaign.get", "path": map[string]any{"campaign_id": "501"}}))
		if err != nil || answer.(metaads.Answer).Token != "read" {
			t.Fatalf("%s: a read went through %+v (%v)", name, answer, err)
		}
		if rec := fx.audit.last().rec; rec.Signer != growth.SignerCompany || rec.Via != "read" {
			t.Fatalf("%s: the read is logged as signed by %q through %q", name, rec.Signer, rec.Via)
		}
	}
	fx := ownFixture(t, "read", &seats{identity: kept(seataccess.IdentityReady)})
	_, err := fx.p.Run(context.Background(), fx.call(metaads.ToolRead, map[string]any{
		"operation": "campaign.get", "path": map[string]any{"campaign_id": "501"}}))
	refusedAs(t, err, growth.RefusedForbidden, "this agent only reads")
}
