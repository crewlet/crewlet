package cloudflare_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// fakeVendor is the registered Cloudflare vendor, opened against the fake with
// the world's clock.
type fakeVendor struct {
	seataccess.Vendor
	hc  *http.Client
	now func() time.Time
}

func (v fakeVendor) Open(ctx context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return cloudflare.OpenAccess(ctx, v.Vendor, in, v.hc, v.now)
}

// syncWorld is one company, its Cloudflare account, the ledger and the sealed
// store, run through the shared sync exactly as the loop runs it.
type syncWorld struct {
	t       *testing.T
	f       *fakeCloudflare
	vendor  seataccess.Vendor
	company *config.Company
	ledger  *seataccesstest.Ledger
	sink    *seataccesstest.Sink
	clock   time.Time
	drift   []seataccess.DriftRestored
}

const sealed = "CLOUDFLARE_API_TOKEN_SRE"

func newSyncWorld(t *testing.T, level string) *syncWorld {
	t.Helper()
	f := newFakeCloudflare()
	registered, ok := seataccess.Lookup(integration.KindCloudflare)
	if !ok {
		t.Fatal("no Cloudflare vendor is registered")
	}
	on := true
	company := &config.Company{Name: "Acme"}
	company.Integrations.Cloudflare = &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
		AccountID: testAccount, APIToken: "${CF_TOKEN}", Zones: []string{zoneExampleName},
	}}
	company.Roles = []config.Role{{
		Name: "SRE", Handle: "sre",
		Integrations: config.RoleIntegrations{Cloudflare: &config.RoleCloudflare{Enabled: &on, Level: level}},
	}}
	w := &syncWorld{
		t: t, f: f, company: company, clock: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
		ledger: seataccesstest.NewLedger(), sink: seataccesstest.NewSink(),
	}
	w.vendor = fakeVendor{Vendor: registered, hc: f.server(t), now: func() time.Time { return w.clock }}
	return w
}

func (w *syncWorld) seat() *config.RoleCloudflare { return w.company.Roles[0].Integrations.Cloudflare }

func (w *syncWorld) syncWith(asks map[string]seataccess.Ask, disconnect bool) seataccess.SyncReport {
	w.t.Helper()
	report, err := seataccess.Sync(w.t.Context(), seataccess.SyncInput{
		Vendor: w.vendor, Ledger: w.ledger, Sink: w.sink, Pass: "test", By: "test",
		Asks: asks, Disconnect: disconnect, Now: func() time.Time { return w.clock },
		Open: seataccess.OpenInput{
			Config: w.company,
			Env:    config.NewResolver(config.MapSource{"CF_TOKEN": provisioner}),
		},
		OnDrift: func(_ context.Context, d seataccess.DriftRestored) { w.drift = append(w.drift, d) },
	})
	if err != nil {
		w.t.Fatalf("sync: %v", err)
	}
	return report
}

func (w *syncWorld) sync() seataccess.SyncReport { return w.syncWith(nil, false) }

func (w *syncWorld) record() seataccess.Record {
	w.t.Helper()
	rec, ok := w.ledger.Record(integration.KindCloudflare, "sre")
	if !ok {
		w.t.Fatal("the ledger has no record for sre")
	}
	return rec
}

// levelOf is the level the fake token holds, as the engine reads it.
func (w *syncWorld) levelOf(tok fakeToken) string {
	return cloudflare.LevelOfPolicies(testAccount, catalogueOf(w.f), w.f.zones, []string{zoneExampleName}, tok.Policies)
}

// A SEAT'S WHOLE LIFE THROUGH THE SHARED SYNC: on mints its token at its level
// on the company's zones and seals the value under the engine's name; a
// converged pass writes nothing; an edit at Cloudflare is narrowed back and
// reported; a level change narrows it; a token near its expiry is rolled and
// renewed; off deletes it and clears the sealed value; on again mints anew.
func TestACloudflareSeatThroughTheSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")

	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("turning on reported %+v", report.Findings)
	}
	rec := w.record()
	if !rec.Held() || rec.Credential == nil || rec.Credential.Variable != sealed {
		t.Fatalf("record = %+v", rec)
	}
	tok, ok := w.f.token(rec.Account.ID)
	if !ok || tok.Name != "crewlet-acme-sre" || w.levelOf(tok) != config.CloudflareLevelRead {
		t.Fatalf("token = %+v, want crewlet-acme-sre at read on example.com", tok)
	}
	if want := rfc3339(w.clock.Add(cloudflare.TokenLifetime)); tok.ExpiresOn != want {
		t.Errorf("expires %s, want %s", tok.ExpiresOn, want)
	}
	if value, _ := w.sink.Held(sealed); value != tok.Value || value == "" {
		t.Fatalf("sealed %q, want the token's value", value)
	}

	writes := len(w.f.wrote())
	if report := w.sync(); len(report.Findings) != 0 {
		t.Errorf("a converged pass reported %+v", report.Findings)
	}
	if got := w.f.wrote(); len(got) != writes {
		t.Fatalf("a converged pass wrote %q", got[writes:])
	}

	// DRIFT: somebody widens the token at Cloudflare.
	wide, _ := cloudflare.PoliciesFor(testAccount, catalogueOf(w.f), w.f.zones, nil, config.CloudflareLevelOperate)
	w.f.edit(tok.ID, func(t *fakeToken) { t.Policies = wide })
	w.sync()
	if got, _ := w.f.token(tok.ID); w.levelOf(got) != config.CloudflareLevelRead {
		t.Errorf("after drift the token holds %q, want read restored", w.levelOf(got))
	}
	if len(w.drift) != 1 || w.drift[0].After.Level != config.CloudflareLevelRead {
		t.Errorf("drift events = %+v, want one restoring read", w.drift)
	}

	// AN EDIT: the document moves the seat to operate, which is no drift.
	w.seat().Level = config.CloudflareLevelOperate
	w.sync()
	if got, _ := w.f.token(tok.ID); w.levelOf(got) != config.CloudflareLevelOperate {
		t.Errorf("after the edit the token holds %q, want operate", w.levelOf(got))
	}
	if len(w.drift) != 1 {
		t.Errorf("an edit was reported as drift: %+v", w.drift)
	}

	// RENEWAL: 61 days on, fewer than 30 remain.
	before, _ := w.sink.Held(sealed)
	w.clock = w.clock.Add(61 * 24 * time.Hour)
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("the renewal reported %+v", report.Findings)
	}
	renewed, _ := w.f.token(tok.ID)
	if after, _ := w.sink.Held(sealed); after == before || after != renewed.Value {
		t.Errorf("sealed after renewal = %q, want the rolled value %q", after, renewed.Value)
	}
	if want := rfc3339(w.clock.Add(cloudflare.TokenLifetime)); renewed.ExpiresOn != want {
		t.Errorf("renewed expiry %s, want %s", renewed.ExpiresOn, want)
	}
	if w.levelOf(renewed) != config.CloudflareLevelOperate {
		t.Errorf("the renewal changed the token's level to %q", w.levelOf(renewed))
	}
	if w.record().Account.ID != tok.ID {
		t.Error("a renewal replaced the token instead of rolling it")
	}

	// A ROTATION A PERSON ASKED FOR rolls the value.
	before, _ = w.sink.Held(sealed)
	w.syncWith(map[string]seataccess.Ask{"sre": {Rotate: true}}, false)
	if after, _ := w.sink.Held(sealed); after == before {
		t.Error("a rotation left the sealed value as it was")
	}

	// OFF disables the token, so the agent stops at once, then deletes it
	// and clears its sealed value.
	off := false
	w.seat().Enabled = &off
	writes = len(w.f.wrote())
	w.sync()
	if got := w.f.wrote()[writes:]; !slices.Equal(got, []string{"update " + tok.ID, "delete " + tok.ID}) {
		t.Errorf("turning off wrote %q, want the token disabled then deleted", got)
	}
	if _, ok := w.f.token(tok.ID); ok {
		t.Error("turning the agent off left its token at Cloudflare")
	}
	if _, ok := w.sink.Held(sealed); ok {
		t.Error("turning the agent off left its value sealed")
	}
	if w.record().Held() {
		t.Error("the ledger still holds a deleted token")
	}

	// BACK ON mints a new token.
	on := true
	w.seat().Enabled = &on
	w.sync()
	again := w.record()
	if !again.Held() || again.Account.ID == tok.ID {
		t.Fatalf("back on = %+v, want a new token", again.Account)
	}
	if _, ok := w.sink.Held(sealed); !ok {
		t.Error("back on sealed nothing")
	}
	w.f.onlyCloudflare(t)
}

// AN EXPIRED TOKEN IS REPLACED, and the expired one deleted: a seat holds one
// token, and a dead one is not it.
func TestAnExpiredTokenIsReplaced(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	w.sync()
	first := w.record().Account.ID
	w.f.edit(first, func(t *fakeToken) { t.Status = cloudflare.StatusExpired })
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("replacing an expired token reported %+v", report.Findings)
	}
	if _, ok := w.f.token(first); ok {
		t.Error("the expired token was left at Cloudflare")
	}
	rec := w.record()
	tok, ok := w.f.token(rec.Account.ID)
	if !ok || rec.Account.ID == first || tok.Status != cloudflare.StatusActive {
		t.Fatalf("record %+v, token %+v, want a new active token", rec.Account, tok)
	}
	if value, _ := w.sink.Held(sealed); value != tok.Value {
		t.Error("the new token's value is not the sealed one")
	}
}

// A VALUE THAT CANNOT BE SEALED IS DELETED WITH ITS TOKEN, so no live token is
// held by nobody, and the next pass mints a fresh one.
func TestAnUnsealableTokenIsDeleted(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	w.sink.FailRecord = map[string]error{sealed: errors.New("the store is read-only")}
	report := w.sync()
	if len(report.Findings) != 1 || report.Findings[0].Kind != integration.FindingIdentityFailed {
		t.Fatalf("findings = %+v, want the seat's failure", report.Findings)
	}
	w.f.mu.Lock()
	live := len(w.f.tokens)
	w.f.mu.Unlock()
	if live != 0 {
		t.Errorf("%d tokens are live with their values sealed nowhere", live)
	}
	w.sink.FailRecord = nil
	w.sync()
	if _, ok := w.sink.Held(sealed); !ok {
		t.Error("the next pass did not mint a fresh token")
	}
}

// A REFUSAL IS THE SEAT'S FINDING, with Cloudflare's words, the remedy and
// where to act; nothing is sealed.
func TestARefusedCreateIsTheSeatsFinding(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	w.f.refuse["create"] = http.StatusForbidden
	report := w.sync()
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %+v", report.Findings)
	}
	f := report.Findings[0]
	if f.Subject != "sre" || !strings.Contains(f.Detail, "Cloudflare:") ||
		!strings.Contains(f.Remedy, "Account API Tokens Write") || f.ActionURL != cloudflare.TokensURL(testAccount) {
		t.Errorf("finding = %+v", f)
	}
	if _, ok := w.sink.Held(sealed); ok {
		t.Error("a refused create sealed a value")
	}
}

// A LEVEL CLOUDFLARE CANNOT RESOLVE IS REFUSED BY NAME, never minted narrower.
func TestAnUnresolvableLevelIsRefusedForTheSeat(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelOperate)
	w.f.set(func(f *fakeCloudflare) {
		f.groups = slices.DeleteFunc(f.groups, func(g fakeGroup) bool { return g.Name == "Cache Purge" && g.Selectable == nil })
	})
	report := w.sync()
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, "Cache Purge") {
		t.Fatalf("findings = %+v, want the seat refused naming Cache Purge", report.Findings)
	}
	if len(w.f.wrote()) != 0 {
		t.Errorf("an unresolvable level wrote %v", w.f.wrote())
	}
}

// A LEVEL THE PROVISIONER CANNOT GRANT is refused for the seat on it with what
// to add, and nothing is asked of Cloudflare; the offer carries the same
// reason on that level alone. Mutation: drop the check from the grant, and
// the seat's token is minted.
func TestALevelTheProvisionerCannotGrantIsRefusedAndOfferedAsUnavailable(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelConfigure)
	w.f.set(withoutHeld("Zone Settings Write"))
	const reason = "The Configure level is unavailable until the provisioner token holds Zone Settings: Edit"
	report := w.sync()
	if len(report.Findings) != 1 || report.Findings[0].Kind != integration.FindingIdentityFailed ||
		!strings.Contains(report.Findings[0].Detail, reason) ||
		!strings.Contains(report.Findings[0].Remedy, "add it over") {
		t.Fatalf("findings = %+v, want the seat refused with what to add", report.Findings)
	}
	if len(w.f.wrote()) != 0 {
		t.Errorf("a level the provisioner cannot grant wrote %v", w.f.wrote())
	}

	session, err := w.vendor.Open(t.Context(), seataccess.OpenInput{Config: w.company,
		Env: config.NewResolver(config.MapSource{"CF_TOKEN": provisioner})})
	if err != nil {
		t.Fatal(err)
	}
	offer, err := session.Options(t.Context(), seataccess.LaneCloudflare)
	if err != nil {
		t.Fatal(err)
	}
	// SECURE GRANTS EVERYTHING CONFIGURE DOES, so it waits on the same group.
	unavailable := map[string]string{
		config.CloudflareLevelConfigure: reason,
		config.CloudflareLevelSecure:    strings.Replace(reason, "Configure", "Secure", 1),
	}
	for _, l := range offer.Levels {
		if want := unavailable[l.ID]; l.Unavailable != want {
			t.Errorf("level %s is offered as unavailable %q, want %q", l.ID, l.Unavailable, want)
		}
	}
}

// EACH LEVEL MINTS EXACTLY ITS GROUPS: the create Cloudflare receives holds
// one account statement and one statement on the company's zone, each naming
// exactly the groups spelled out here. Read reaches WAF custom rules and the
// IP lists they match on; only Secure changes them.
func TestEachLevelMintsExactlyItsGroups(t *testing.T) {
	t.Parallel()
	readAccount := []string{"Account Analytics Read", "Account Rule Lists Read", "Account Settings Read"}
	readZone := []string{
		"Zone Read", "Zone Settings Read", "Analytics Read", "DNS Read", "Cache Settings Read",
		"Zone Transform Rules Read", "Firewall Services Read", "Zone WAF Read", "SSL and Certificates Read",
	}
	operateZone := append(slices.Clone(readZone), "Cache Purge", "Cache Settings Write")
	configureZone := append(slices.Clone(operateZone),
		"Zone Settings Write", "Response Compression Write", "Zone Transform Rules Write")
	for _, tc := range []struct {
		level         string
		account, zone []string
	}{
		{config.CloudflareLevelRead, readAccount, readZone},
		{config.CloudflareLevelOperate, readAccount, operateZone},
		{config.CloudflareLevelConfigure, readAccount, configureZone},
		{
			config.CloudflareLevelSecure, append(slices.Clone(readAccount), "Account Rule Lists Write"),
			append(slices.Clone(configureZone), "Zone WAF Write"),
		},
	} {
		t.Run(tc.level, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, tc.level)
			if report := w.sync(); len(report.Findings) != 0 {
				t.Fatalf("minting %s reported %+v", tc.level, report.Findings)
			}
			tok, _ := w.f.token(w.record().Account.ID)
			got := map[string][]string{}
			for _, p := range tok.Policies {
				resources, _ := json.Marshal(p.Resources)
				if p.Effect != "allow" {
					t.Errorf("a statement on %s has effect %q", resources, p.Effect)
				}
				for _, g := range p.PermissionGroups {
					got[string(resources)] = append(got[string(resources)], g.ID)
				}
			}
			want := map[string][]string{
				`{"com.cloudflare.api.account.` + testAccount + `":"*"}`:        groupIDs(tc.account),
				`{"com.cloudflare.api.account.zone.` + zoneExampleID + `":"*"}`: groupIDs(tc.zone),
			}
			if len(got) != len(want) {
				t.Fatalf("the %s token's statements are %v, want %v", tc.level, got, want)
			}
			for resources, ids := range want {
				held := slices.Sorted(slices.Values(got[resources]))
				if !slices.Equal(held, ids) {
					t.Errorf("the %s token grants %v on %s, want %v", tc.level, held, resources, ids)
				}
			}
		})
	}
}

// groupIDs is the fake's IDs for names, sorted.
func groupIDs(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, groupID(n))
	}
	slices.Sort(out)
	return out
}

// SECURE IS AN ADMINISTRATION GRANT, so moving an agent to it asks the person
// saving for a fresh step-up, and no other level does: a WAF rule or a list
// entry it writes outlives its token. Mutation: drop the mark from the table
// and the step-up is not asked for.
func TestOnlySecureNeedsAStepUp(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	session, err := w.vendor.Open(t.Context(), seataccess.OpenInput{Config: w.company,
		Env: config.NewResolver(config.MapSource{"CF_TOKEN": provisioner})})
	if err != nil {
		t.Fatal(err)
	}
	offer, err := session.Options(t.Context(), seataccess.LaneCloudflare)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, l := range offer.Levels {
		ids = append(ids, l.ID)
		if l.Administers != (l.ID == config.CloudflareLevelSecure) || l.Note == "" || l.Unavailable != "" {
			t.Errorf("level %+v, want secure alone administering, each with a note and available", l)
		}
	}
	if !slices.Equal(ids, config.CloudflareLevels()) {
		t.Errorf("the offer is %v, want %v", ids, config.CloudflareLevels())
	}
	on := func(level string) seataccess.Desired { return seataccess.Desired{Enabled: true, Level: level} }
	if !seataccess.NeedsStepUp(offer, on(config.CloudflareLevelConfigure), on(config.CloudflareLevelSecure)) {
		t.Error("raising an agent to secure asks for no step-up")
	}
	if seataccess.NeedsStepUp(offer, on(config.CloudflareLevelRead), on(config.CloudflareLevelConfigure)) {
		t.Error("raising an agent to configure asks for a step-up")
	}
}

// A LEVEL THAT GAINS GROUPS REACHES EVERY TOKEN ALREADY MINTED AT IT on the
// next pass, with nobody acting: the token matches no level any more, so the
// pass rewrites its policy in place and keeps its value. While the provisioner
// lacks the new groups the token is left as it is, still working, and the
// seat is refused naming what to add. Mutation: read a token's level by a
// subset of its groups, and the old token is never brought up.
func TestATokenMintedBeforeItsLevelGrewIsBroughtUpToIt(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	w.sync()
	id := w.record().Account.ID
	value, _ := w.sink.Held(sealed)
	added := []string{groupID("Zone WAF Read"), groupID("Account Rule Lists Read")}
	w.f.edit(id, func(tok *fakeToken) {
		for i := range tok.Policies {
			tok.Policies[i].PermissionGroups = slices.DeleteFunc(slices.Clone(tok.Policies[i].PermissionGroups),
				func(g cloudflare.GroupRef) bool { return slices.Contains(added, g.ID) })
		}
	})
	if tok, _ := w.f.token(id); w.levelOf(tok) != "" {
		t.Fatalf("a token without Read's new groups reads as %q", w.levelOf(tok))
	}

	// THE PROVISIONER WITHOUT THEM: nothing is written and nothing revoked.
	held := newFakeCloudflare().provisionerOwn
	w.f.set(withoutHeld("Zone WAF Read", "Account Rule Lists Read"))
	writes := len(w.f.wrote())
	report := w.sync()
	const reason = "The Read level is unavailable until the provisioner token holds " +
		"Zone WAF: Read and Account Filter Lists: Read"
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, reason) {
		t.Fatalf("findings = %+v, want the seat refused naming what to add", report.Findings)
	}
	if got := w.f.wrote()[writes:]; len(got) != 0 {
		t.Errorf("a provisioner without the groups wrote %q", got)
	}
	if after, ok := w.sink.Held(sealed); !ok || after != value {
		t.Error("the seat's working token was unsealed while its level waited")
	}

	// THE PROVISIONER GIVEN THEM: the same token is brought up to Read.
	w.f.set(func(f *fakeCloudflare) { f.provisionerOwn = held })
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("bringing the token up reported %+v", report.Findings)
	}
	if got := w.f.wrote()[writes:]; !slices.Equal(got, []string{"update " + id}) {
		t.Errorf("bringing the token up wrote %q, want its policy rewritten in place", got)
	}
	if tok, _ := w.f.token(id); w.levelOf(tok) != config.CloudflareLevelRead {
		t.Errorf("the token reads as %q, want read", w.levelOf(tok))
	}
	if after, _ := w.sink.Held(sealed); after != value || w.record().Account.ID != id {
		t.Error("bringing the token up replaced it or its value")
	}
}

// A CONFIGURE TOKEN MINTED BEFORE THE LEVEL COVERED TRANSFORM RULES GAINS THEM
// on the next pass with nobody acting: one update of the same token, which
// keeps its name, expiry and value. Mutation: drop Zone Transform Rules Write
// from Configure, and the rewritten token never gains it.
func TestAConfigureTokenFromBeforeTransformRulesIsRewrittenInPlace(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelConfigure)
	w.sync()
	id := w.record().Account.ID
	value, _ := w.sink.Held(sealed)
	transform := []string{groupID("Zone Transform Rules Read"), groupID("Zone Transform Rules Write")}
	holds := func(tok fakeToken, group string) bool {
		return slices.ContainsFunc(tok.Policies, func(p cloudflare.Policy) bool {
			return slices.ContainsFunc(p.PermissionGroups, func(g cloudflare.GroupRef) bool { return g.ID == group })
		})
	}
	w.f.edit(id, func(tok *fakeToken) {
		for i := range tok.Policies {
			tok.Policies[i].PermissionGroups = slices.DeleteFunc(slices.Clone(tok.Policies[i].PermissionGroups),
				func(g cloudflare.GroupRef) bool { return slices.Contains(transform, g.ID) })
		}
	})
	before, _ := w.f.token(id)
	if got := w.levelOf(before); got != "" {
		t.Fatalf("a Configure token without the transform rule groups reads as %q", got)
	}

	writes := len(w.f.wrote())
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("bringing the token up reported %+v", report.Findings)
	}
	if got := w.f.wrote()[writes:]; !slices.Equal(got, []string{"update " + id}) {
		t.Fatalf("bringing the token up wrote %q, want its policy rewritten in place", got)
	}
	after, _ := w.f.token(id)
	if got := w.levelOf(after); got != config.CloudflareLevelConfigure {
		t.Errorf("the token reads as %q, want configure", got)
	}
	for _, group := range transform {
		if !holds(after, group) {
			t.Errorf("the rewritten token does not hold %s", group)
		}
	}
	if after.Name != before.Name || after.ExpiresOn != before.ExpiresOn || after.Value != before.Value {
		t.Errorf("the rewrite changed the token: name %q to %q, expiry %q to %q, value kept %t",
			before.Name, after.Name, before.ExpiresOn, after.ExpiresOn, after.Value == before.Value)
	}
	if held, _ := w.sink.Held(sealed); held != value || w.record().Account.ID != id {
		t.Error("bringing the token up replaced it or its sealed value")
	}

	// CONVERGED: the pass after writes nothing.
	writes = len(w.f.wrote())
	w.sync()
	if got := w.f.wrote()[writes:]; len(got) != 0 {
		t.Errorf("a converged pass wrote %q", got)
	}
}

// A TOKEN SOMEBODY DISABLED AT CLOUDFLARE STAYS OFF until a person asks (D3),
// and turning it back on keeps the value it holds.
func TestAHandDisabledTokenWaitsForAPerson(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	w.sync()
	id := w.record().Account.ID
	w.f.edit(id, func(t *fakeToken) { t.Status = cloudflare.StatusDisabled })
	report := w.sync()
	if len(report.Findings) != 1 || report.Findings[0].Kind != integration.FindingIdentityFailed {
		t.Fatalf("findings = %+v, want the seat waiting on a person", report.Findings)
	}
	if tok, _ := w.f.token(id); tok.Status != cloudflare.StatusDisabled {
		t.Fatal("a token disabled by hand was turned back on unasked")
	}
	value, _ := w.sink.Held(sealed)
	w.syncWith(map[string]seataccess.Ask{"sre": {Reenable: true}}, false)
	if tok, _ := w.f.token(id); tok.Status != cloudflare.StatusActive {
		t.Errorf("after Turn back on the token is %q", tok.Status)
	}
	if after, _ := w.sink.Held(sealed); after != value {
		t.Error("turning a token back on replaced a value it still held")
	}
}

// ONE SEAT'S READ IS ONE SEAT'S FAULT: a token Cloudflare cannot answer for
// leaves the other seats synced.
func TestOneSeatsUnreadableTokenIsItsOwnFault(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	on := true
	w.company.Roles = append(w.company.Roles, config.Role{Name: "Ops", Handle: "ops",
		Integrations: config.RoleIntegrations{Cloudflare: &config.RoleCloudflare{Enabled: &on}}})
	w.sync()
	sre := w.record().Account.ID
	w.f.set(func(f *fakeCloudflare) { f.refuseID[sre] = http.StatusInternalServerError })
	w.seat().Level = config.CloudflareLevelOperate
	opsRole := &w.company.Roles[1]
	opsRole.Integrations.Cloudflare.Level = config.CloudflareLevelOperate
	report := w.sync()
	if len(report.Findings) != 1 || report.Findings[0].Subject != "sre" {
		t.Fatalf("findings = %+v, want sre's read alone", report.Findings)
	}
	ops, _ := w.ledger.Record(integration.KindCloudflare, "ops")
	if tok, _ := w.f.token(ops.Account.ID); w.levelOf(tok) != config.CloudflareLevelOperate {
		t.Error("the readable seat was not synced beside the unreadable one")
	}
}

// A DELETE THAT FAILS LEAVES THE TOKEN DISABLED, and the next pass deletes it.
func TestAFailedDeleteLeavesTheTokenOffAndIsRetried(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	w.sync()
	id := w.record().Account.ID
	off := false
	w.seat().Enabled = &off
	w.f.set(func(f *fakeCloudflare) { f.refuse["delete"] = http.StatusBadGateway })
	if report := w.sync(); len(report.Findings) != 1 {
		t.Fatalf("a failed delete reported %+v, want the seat's failure", report.Findings)
	}
	if tok, ok := w.f.token(id); !ok || tok.Status != cloudflare.StatusDisabled {
		t.Fatalf("after a failed delete the token is %+v, want it disabled", tok)
	}
	w.f.set(func(f *fakeCloudflare) { delete(f.refuse, "delete") })
	w.sync()
	if _, ok := w.f.token(id); ok {
		t.Error("the next pass left the disabled token at Cloudflare")
	}
	if _, ok := w.sink.Held(sealed); ok {
		t.Error("the next pass left the value sealed")
	}
}

// A DISCONNECT DELETES EVERY TOKEN, and a seat that left the document has its
// token deleted and its record dropped.
func TestADisconnectAndARemovedSeatDeleteTheirTokens(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	w.sync()
	id := w.record().Account.ID
	w.syncWith(nil, true)
	if _, ok := w.f.token(id); ok {
		t.Error("a disconnect left the token")
	}
	if _, ok := w.sink.Held(sealed); ok {
		t.Error("a disconnect left the value sealed")
	}

	w = newSyncWorld(t, config.CloudflareLevelRead)
	w.sync()
	id = w.record().Account.ID
	w.company.Roles = nil
	w.sync()
	if _, ok := w.f.token(id); ok {
		t.Error("a seat that left the document kept its token")
	}
	if _, ok := w.ledger.Record(integration.KindCloudflare, "sre"); ok {
		t.Error("the record of a seat that left the document was kept")
	}
}

// A PREVIEW AND A SYNC AGREE, and the preview draws Cloudflare's own steps.
func TestThePlanDrawsCloudflaresSteps(t *testing.T) {
	t.Parallel()
	vendor, _ := seataccess.Lookup(integration.KindCloudflare)
	seat := seataccess.SeatRef{Handle: "sre", Name: "SRE"}
	want := seataccess.Desired{Enabled: true, Level: config.CloudflareLevelRead}
	plan, err := seataccess.Plan(vendor, seataccess.SeatInput{
		Seat: seat, Want: map[seataccess.Lane]seataccess.Desired{seataccess.LaneCloudflare: want},
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := plan[seataccess.LaneCloudflare]
	if len(steps) != 1 || steps[0].Verb != seataccess.VerbCreate || !strings.Contains(steps[0].Text, sealed) {
		t.Fatalf("a seat turned on plans %+v", steps)
	}
	renew := vendor.Steps(seat, seataccess.LaneCloudflare, seataccess.OpMint, want, seataccess.Observed{})
	if len(renew) != 2 || renew[0].Verb != seataccess.VerbMint || renew[1].Index != 1 {
		t.Errorf("a mint plans %+v, want the roll then the renewal", renew)
	}
	if got := vendor.Steps(seat, seataccess.LaneCloudflare, seataccess.OpScope, want, seataccess.Observed{}); got != nil {
		t.Errorf("a scope change plans %+v on a lane with no scope", got)
	}
}
