package digitalocean

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

const (
	sealedSRE  = "DIGITALOCEAN_TOKEN_SRE"
	retiredSRE = "DIGITALOCEAN_RETIRED_TOKEN_SRE"
)

// testKey is the fleet key a test's fingerprints are made with.
func testKey(string) []byte { return []byte("0123456789abcdef0123456789abcdef") }

// fakeVendor is the registered vendor, opened against the fake.
type fakeVendor struct {
	seataccess.Vendor
	hc *http.Client
}

func (v fakeVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return accessVendor{}.open(in, v.hc)
}

// sinkSource resolves what the test sink holds, as the sealed store does.
type sinkSource struct{ s *seataccesstest.Sink }

func (s sinkSource) Lookup(name string) (string, bool) { return s.s.Held(name) }

// syncWorld is one company, its DigitalOcean team, the ledger and the sealed
// store, run through the shared sync as the loop runs it.
type syncWorld struct {
	t       *testing.T
	do      *fakeDO
	vendor  seataccess.Vendor
	company *config.Company
	ledger  *seataccesstest.Ledger
	sink    *seataccesstest.Sink
}

func newSyncWorld(t *testing.T, level string) *syncWorld {
	t.Helper()
	f := newFakeDO()
	registered, ok := seataccess.Lookup(integration.KindDigitalOcean)
	if !ok {
		t.Fatal("the DigitalOcean vendor is not registered")
	}
	on := true
	company := &config.Company{Name: "Acme"}
	company.Integrations.DigitalOcean = &config.DigitalOcean{Enabled: true,
		Provisioning: &config.DigitalOceanProvisioning{Team: "${DO_TEAM}"}}
	company.Roles = []config.Role{
		{Name: "SRE", Handle: "sre", Integrations: config.RoleIntegrations{
			DigitalOcean: &config.RoleDigitalOcean{Enabled: &on, Level: level}}},
		{Name: "QA", Handle: "qa", Integrations: config.RoleIntegrations{
			DigitalOcean: &config.RoleDigitalOcean{Enabled: &on, Level: config.DigitalOceanLevelRead}}},
	}
	return &syncWorld{t: t, do: f, company: company, vendor: fakeVendor{Vendor: registered, hc: f.server(t)},
		ledger: seataccesstest.NewLedger(), sink: seataccesstest.NewSink()}
}

func (w *syncWorld) seat() *config.RoleDigitalOcean {
	return w.company.Roles[0].Integrations.DigitalOcean
}

func (w *syncWorld) open() seataccess.OpenInput {
	return seataccess.OpenInput{Config: w.company, Derive: testKey, Env: config.NewResolver(
		config.MapSource{"DO_TEAM": "Example Team"}, sinkSource{w.sink})}
}

func (w *syncWorld) sync(input func(*seataccess.SyncInput)) seataccess.SyncReport {
	w.t.Helper()
	in := seataccess.SyncInput{Vendor: w.vendor, Ledger: w.ledger, Sink: w.sink, Pass: "test", By: "test",
		Open: w.open()}
	if input != nil {
		input(&in)
	}
	report, err := seataccess.Sync(context.Background(), in)
	if err != nil {
		w.t.Fatalf("sync: %v", err)
	}
	return report
}

// paste is the paste route's work for one seat: the check, then the seat's
// sync carrying the value.
func (w *syncWorld) paste(handle, value string) (seataccess.HandedIn, error) {
	w.t.Helper()
	i := slices.IndexFunc(w.company.Roles, func(r config.Role) bool { return r.Handle == handle })
	role := &w.company.Roles[i]
	sess, err := w.vendor.Open(context.Background(), w.open())
	if err != nil {
		w.t.Fatal(err)
	}
	records, err := w.ledger.Records(context.Background(), integration.KindDigitalOcean)
	if err != nil {
		w.t.Fatal(err)
	}
	var rec *seataccess.Record
	if r, ok := records[handle]; ok {
		rec = &r
	}
	lane := seataccess.LaneDigitalOcean
	want, _ := seataccess.DesiredOf(role, lane)
	handed, err := sess.(seataccess.HandsIn).CheckHandIn(context.Background(), seataccess.HandInInput{
		Seat: seataccess.SeatRef{Handle: handle, Name: role.Name}, Lane: lane, Want: want,
		Record: rec, Records: records, Value: value,
	})
	if err != nil {
		return handed, err
	}
	w.sync(func(in *seataccess.SyncInput) {
		in.Asks = map[string]seataccess.Ask{handle: {HandIn: value, Rotate: handed.Rotate, Lane: lane}}
	})
	return handed, nil
}

func (w *syncWorld) lane(handle string) seataccess.LaneRecord {
	w.t.Helper()
	rec, ok := w.ledger.Record(integration.KindDigitalOcean, handle)
	if !ok {
		w.t.Fatalf("the ledger has no record for %s", handle)
	}
	return rec.Lanes[seataccess.LaneDigitalOcean]
}

// waitingOn is the step sre's lane waits on a person for, with its sentence.
func (w *syncWorld) waitingOn(verb seataccess.StepVerb) string {
	w.t.Helper()
	sync := w.lane("sre").Sync
	if sync == nil || sync.Outcome != seataccess.OutcomeWaiting || sync.Error == nil || sync.Error.Step != verb ||
		sync.Error.Actor != seataccess.ByYou {
		w.t.Fatalf("sync = %+v, want waiting on a person to %s at DigitalOcean", sync, verb)
	}
	return sync.Error.Said
}

func (w *syncWorld) inSync(handle string) {
	w.t.Helper()
	if sync := w.lane(handle).Sync; sync == nil || sync.Outcome != seataccess.OutcomeOK {
		w.t.Fatalf("%s sync = %+v, want in sync", handle, sync)
	}
}

// A SEAT'S WHOLE LIFE: turned on, it waits for a token and makes no call; the
// pasted token is checked and sealed; a converged pass proves its level; a
// level change asks for a new token and Crewlet revokes the old one itself; off
// clears the token at once and revokes it, leaving nothing for a person.
func TestADigitalOceanSeatsWholeLife(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.DigitalOceanLevelOperate)

	w.sync(nil)
	said := w.waitingOn(seataccess.VerbCreate)
	for _, want := range []string{"crewlet-acme-sre-operate", "Operate", "Example Team"} {
		if !strings.Contains(said, want) {
			t.Errorf("%q does not say %q", said, want)
		}
	}
	if len(w.do.requests) != 0 {
		t.Fatalf("a seat with no token called DigitalOcean: %v", w.do.requests)
	}

	first := fakeToken(0x11)
	w.do.add(first, scopesOf(config.DigitalOceanLevelOperate))
	handed, err := w.paste("sre", first)
	if err != nil {
		t.Fatalf("paste: %v", err)
	}
	if want := "Works. Owner on team Example Team (Team ID 11111111-2222-4333-a444-555555555555), Operate. " +
		"Sealed as DIGITALOCEAN_TOKEN_SRE."; handed.Detail != want {
		t.Errorf("detail = %q", handed.Detail)
	}
	if strings.Contains(handed.AccountID, first) || !strings.HasPrefix(handed.AccountID, "11111111-") {
		t.Errorf("account ID = %q, want the team and an opaque fingerprint", handed.AccountID)
	}
	if sealed, ok := w.sink.Held(sealedSRE); !ok || sealed != first {
		t.Fatal("the pasted token was not sealed")
	}
	w.inSync("sre")

	for _, f := range w.sync(nil).Findings {
		if f.Subject == "sre" {
			t.Errorf("a converged pass reported %+v", f)
		}
	}
	w.inSync("sre")
	if got := w.lane("sre").Observed.Level; got != config.DigitalOceanLevelOperate {
		t.Errorf("observed level = %q, want operate", got)
	}

	w.seat().Level = config.DigitalOceanLevelRead
	w.sync(nil)
	said = w.waitingOn(seataccess.VerbMint)
	if !strings.Contains(said, "cannot change a token's scopes") || !strings.Contains(said, "crewlet-acme-sre-read") {
		t.Errorf("a level change says %q", said)
	}
	second := fakeToken(0x22)
	w.do.add(second, scopesOf(config.DigitalOceanLevelRead))
	if _, err := w.paste("sre", second); err != nil {
		t.Fatalf("paste for the new level: %v", err)
	}
	if sealed, _ := w.sink.Held(sealedSRE); sealed != second {
		t.Fatal("the new level's token was not sealed")
	}
	if !slices.Contains(w.do.revoked, first) {
		t.Error("the previous token was not revoked at DigitalOcean")
	}
	if _, kept := w.sink.Held(retiredSRE); kept {
		t.Error("the previous token is still sealed after its revoke")
	}
	w.inSync("sre")

	off := false
	w.seat().Enabled = &off
	w.sync(nil)
	if _, kept := w.sink.Held(sealedSRE); kept {
		t.Error("the token is still sealed after off")
	}
	if !slices.Contains(w.do.revoked, second) {
		t.Error("off did not revoke the token at DigitalOcean")
	}
	if rec, _ := w.ledger.Record(integration.KindDigitalOcean, "sre"); rec.Held() {
		t.Errorf("record = %+v, want it closed", rec)
	}
	w.do.onlyDigitalOcean(t)
}

// A TOKEN DIGITALOCEAN REFUSES IS CLEARED AND ASKED FOR AGAIN, and never sent
// again.
func TestARefusedDigitalOceanTokenIsAskedForAgain(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.DigitalOceanLevelRead)
	token := fakeToken(0x33)
	p := w.do.add(token, scopesOf(config.DigitalOceanLevelRead))
	if _, err := w.paste("sre", token); err != nil {
		t.Fatalf("paste: %v", err)
	}
	p.revoked = true
	w.sync(nil)
	w.sync(nil)
	said := w.waitingOn(seataccess.VerbMint)
	if !strings.Contains(said, "no longer accepts") {
		t.Errorf("said %q", said)
	}
	if _, kept := w.sink.Held(sealedSRE); kept {
		t.Error("the refused token is still sealed")
	}
}

// A ROTATE IS A NEW TOKEN AT THE SAME LEVEL, AND THE OLD ONE IS REVOKED BY
// CREWLET, unless Regenerate already killed it.
func TestADigitalOceanRotateRevokesTheOldToken(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.DigitalOceanLevelRead)
	old := fakeToken(0x44)
	w.do.add(old, scopesOf(config.DigitalOceanLevelRead))
	if _, err := w.paste("sre", old); err != nil {
		t.Fatalf("paste: %v", err)
	}
	fresh := fakeToken(0x55)
	w.do.add(fresh, scopesOf(config.DigitalOceanLevelRead))
	handed, err := w.paste("sre", fresh)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !handed.Rotate {
		t.Error("a paste for a seat holding a working token is not a rotate")
	}
	if !slices.Contains(w.do.revoked, old) {
		t.Error("the old token was not revoked")
	}
	if sealed, _ := w.sink.Held(sealedSRE); sealed != fresh {
		t.Error("the new token was not sealed")
	}
	w.inSync("sre")
}

// ONE TOKEN IS ONE SEAT'S: the same token pasted for a second seat is refused.
func TestADigitalOceanTokenIsOneSeats(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.DigitalOceanLevelRead)
	token := fakeToken(0x66)
	w.do.add(token, scopesOf(config.DigitalOceanLevelRead))
	if _, err := w.paste("sre", token); err != nil {
		t.Fatalf("paste: %v", err)
	}
	_, err := w.paste("qa", token)
	var r *seataccess.Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Said, "already sre's") {
		t.Fatalf("the same token for qa = %v, want refused as sre's", err)
	}
}

// A REVOKE THAT FAILS IS RETRIED, AND THE TOKEN NEVER REACHES THE BOX MEANWHILE:
// off clears it at once, keeps it where no box reads it, and a later pass
// revokes it.
func TestAFailedRevokeIsRetriedWithTheTokenOutOfTheBox(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.DigitalOceanLevelRead)
	token := fakeToken(0x77)
	w.do.add(token, scopesOf(config.DigitalOceanLevelRead))
	if _, err := w.paste("sre", token); err != nil {
		t.Fatalf("paste: %v", err)
	}
	w.do.revokeDown = true
	off := false
	w.seat().Enabled = &off
	w.sync(nil)
	if _, kept := w.sink.Held(sealedSRE); kept {
		t.Error("the token stays in the box while its revoke fails")
	}
	if _, kept := w.sink.Held(retiredSRE); !kept {
		t.Fatal("the token was dropped before DigitalOcean revoked it")
	}
	w.do.revokeDown = false
	w.sync(nil)
	if !slices.Contains(w.do.revoked, token) {
		t.Error("a later pass did not revoke the token")
	}
	if _, kept := w.sink.Held(retiredSRE); kept {
		t.Error("the revoked token is still sealed")
	}
}

// A REVOKE DIGITALOCEAN KEEPS REFUSING, while the token still works, waits for
// a person to delete it, naming it.
func TestARefusedRevokeWaitsForAPerson(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.DigitalOceanLevelRead)
	token := fakeToken(0x88)
	p := w.do.add(token, scopesOf(config.DigitalOceanLevelRead))
	if _, err := w.paste("sre", token); err != nil {
		t.Fatalf("paste: %v", err)
	}
	p.refuseRevoke = true
	off := false
	w.seat().Enabled = &off
	w.sync(nil)
	sync := w.lane("sre").Sync
	if sync == nil || sync.Outcome != seataccess.OutcomeWaiting || sync.Error == nil ||
		!strings.Contains(sync.Error.Said, "refused to revoke") || sync.Error.ActionURL != TokensURL {
		t.Fatalf("sync = %+v, want a wait on a person to delete the token", sync)
	}
	p.revoked = true
	w.sync(nil)
	if _, kept := w.sink.Held(retiredSRE); kept {
		t.Error("a token a person deleted is still sealed")
	}
}

// THE OFFER CARRIES WHAT THE SHEET DRAWS: each level's scopes, the scopes
// Crewlet refuses and those it accepts, the team, and the page to act on. No
// release: a kept token is live access nobody watches.
func TestTheDigitalOceanOfferCarriesItsLists(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.DigitalOceanLevelRead)
	sess, err := w.vendor.Open(context.Background(), w.open())
	if err != nil {
		t.Fatal(err)
	}
	o, err := sess.Options(context.Background(), seataccess.LaneDigitalOcean)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Levels) != 3 || !o.Levels[2].Administers || o.Levels[0].Administers ||
		!slices.Equal(o.Levels[1].Grants, Grants(config.DigitalOceanLevelOperate)) {
		t.Errorf("levels = %+v", o.Levels)
	}
	if !slices.Equal(o.Refused, refusedScopes) || !slices.Equal(o.Unneeded, unneededScopes) ||
		o.VendorAccount != "Example Team" || o.ActionURL != TokensURL || !o.Rotatable || o.Releasable {
		t.Errorf("options = %+v", o)
	}
	spec, _ := accessVendor{}.Catalog().Lane(seataccess.LaneDigitalOcean)
	if spec.Releasable || len(spec.BoxEnv) != 1 || spec.BoxEnv[0] != (seataccess.BoxVar{Field: TokenField, Env: ToolEnv}) {
		t.Errorf("lane spec = %+v: want doctl reading only the live token, and no release", spec)
	}
	name := seataccess.HandInOf(accessVendor{}, w.company, seataccess.SeatRef{Handle: "sre"},
		seataccess.LaneDigitalOcean, seataccess.Desired{Enabled: true, Level: config.DigitalOceanLevelAdminister})
	if name == nil || name.Name != "crewlet-acme-sre-administer" {
		t.Errorf("hand-in = %+v", name)
	}
}

// A DISCONNECT REVOKES EVERY TOKEN AND KEEPS NONE.
func TestADigitalOceanDisconnectKeepsNoToken(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.DigitalOceanLevelRead)
	token := fakeToken(0x99)
	w.do.add(token, scopesOf(config.DigitalOceanLevelRead))
	if _, err := w.paste("sre", token); err != nil {
		t.Fatalf("paste: %v", err)
	}
	w.sync(func(in *seataccess.SyncInput) { in.Disconnect = true })
	for _, name := range []string{sealedSRE, retiredSRE} {
		if _, kept := w.sink.Held(name); kept {
			t.Errorf("%s is still sealed after a disconnect", name)
		}
	}
	if !slices.Contains(w.do.revoked, token) {
		t.Error("a disconnect did not revoke the token")
	}
}
