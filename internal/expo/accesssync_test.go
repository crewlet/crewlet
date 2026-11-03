package expo_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// fakeVendor is the registered Expo vendor, opened against a fake Expo, with
// a clock a case can move on.
type fakeVendor struct {
	seataccess.Vendor
	hc    *http.Client
	clock *atomic.Pointer[time.Time]
}

func (v fakeVendor) Open(ctx context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return expo.OpenAccess(ctx, v.Vendor, in, v.hc, func() time.Time {
		if at := v.clock.Load(); at != nil {
			return *at
		}
		return time.Now().UTC()
	})
}

const sealedQA = "EXPO_TOKEN_QA"

// syncWorld is one company, its Expo account, the ledger and the sealed
// store, run through the shared sync exactly as the loop runs it.
type syncWorld struct {
	t       *testing.T
	expo    *fakeExpo
	vendor  seataccess.Vendor
	company *config.Company
	ledger  *seataccesstest.Ledger
	sink    *seataccesstest.Sink
	drift   []seataccess.DriftRestored
	clock   *atomic.Pointer[time.Time]
}

func newSyncWorld(t *testing.T, level string) *syncWorld {
	t.Helper()
	f := newFakeExpo()
	registered, ok := seataccess.Lookup(integration.KindExpo)
	if !ok {
		t.Fatal("the Expo vendor is not registered")
	}
	on := true
	company := &config.Company{Name: "Acme"}
	company.Integrations.Expo = &config.Expo{
		Enabled: true,
		Provisioning: &config.ExpoProvisioning{
			AccountName: "${EXPO_ACCOUNT}", AccessToken: "${EXPO_ACCESS_TOKEN}",
		},
	}
	company.Roles = []config.Role{{
		Name: "QA", Handle: "qa",
		Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: &on, Level: level}},
	}}
	clock := &atomic.Pointer[time.Time]{}
	return &syncWorld{
		t: t, expo: f, company: company, clock: clock,
		vendor: fakeVendor{Vendor: registered, hc: f.server(t), clock: clock},
		ledger: seataccesstest.NewLedger(), sink: seataccesstest.NewSink(),
	}
}

// later moves the vendor's clock past the window a new robot may go unlisted.
func (w *syncWorld) later() {
	at := time.Now().UTC().Add(time.Hour)
	w.clock.Store(&at)
}

func (w *syncWorld) seat() *config.RoleExpo { return w.company.Roles[0].Integrations.Expo }

func (w *syncWorld) syncAsking(asks map[string]seataccess.Ask) seataccess.SyncReport {
	w.t.Helper()
	report, err := seataccess.Sync(context.Background(), seataccess.SyncInput{
		Vendor: w.vendor, Ledger: w.ledger, Sink: w.sink, Pass: "test", By: "test", Asks: asks,
		Open: seataccess.OpenInput{
			Config: w.company,
			Env: config.NewResolver(config.MapSource{
				"EXPO_ACCOUNT": testAccountName, "EXPO_ACCESS_TOKEN": provisioner,
			}),
		},
		OnDrift: func(_ context.Context, d seataccess.DriftRestored) { w.drift = append(w.drift, d) },
	})
	if err != nil {
		w.t.Fatalf("sync: %v", err)
	}
	return report
}

func (w *syncWorld) sync() seataccess.SyncReport { return w.syncAsking(nil) }

func (w *syncWorld) record() seataccess.Record {
	w.t.Helper()
	rec, ok := w.ledger.Record(integration.KindExpo, "qa")
	if !ok {
		w.t.Fatal("the ledger has no record for qa")
	}
	return rec
}

// sealed is the token sealed for qa and the robot's live tokens, which must
// be that one alone.
func (w *syncWorld) sealed(robotID string) string {
	w.t.Helper()
	value, ok := w.sink.Held(sealedQA)
	live := w.expo.liveTokens(robotID)
	if !ok || len(live) != 1 || w.expo.value(live[0]) != value {
		w.t.Fatalf("sealed=%v live=%v, want the robot's one live token sealed as %s", ok, live, sealedQA)
	}
	return value
}

// A SEAT'S WHOLE LIFE THROUGH THE SHARED SYNC: create and seal, a converged
// pass that writes nothing, drift taken back, level changes, a rotation, off
// deleting the robot and its tokens, and back on creating a new robot.
func TestAnExpoSeatThroughTheSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ExpoLevelRunner)

	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("turning on reported %+v", report.Findings)
	}
	rec := w.record()
	if !rec.Held() || rec.Credential == nil || rec.Credential.Variable != sealedQA {
		t.Fatalf("record = %+v", rec)
	}
	robot, ok := w.expo.robot(rec.Account.ID)
	if !ok || robot.name != "crewlet-acme-qa" ||
		!slices.Equal(robot.perms, []expo.Permission{expo.PermissionRun, expo.PermissionView}) {
		t.Fatalf("robot = %+v", robot)
	}
	first := w.sealed(robot.id)
	w.expo.onlyExpo(t)

	writes := len(w.expo.writes())
	if report := w.sync(); len(report.Findings) != 0 {
		t.Errorf("a converged pass reported %+v", report.Findings)
	}
	if got := w.expo.writes(); len(got) != writes {
		t.Errorf("a converged pass wrote %q", got[writes:])
	}

	// DRIFT: somebody makes the robot an admin at Expo.
	w.expo.mu.Lock()
	w.expo.robots[robot.id].perms = append(w.expo.robots[robot.id].perms, expo.PermissionAdmin)
	w.expo.mu.Unlock()
	w.sync()
	if got, _ := w.expo.robot(robot.id); !slices.Equal(sortPerms(got.perms), []expo.Permission{expo.PermissionRun, expo.PermissionView}) {
		t.Errorf("permissions after drift = %v, want the level's restored", got.perms)
	}
	if len(w.drift) != 1 || w.drift[0].After.Level != config.ExpoLevelRunner {
		t.Errorf("drift events = %+v, want one restoring runner", w.drift)
	}

	// A LEVEL CHANGE.
	w.seat().Level = config.ExpoLevelReleaseManager
	w.sync()
	want := []expo.Permission{expo.PermissionPublish, expo.PermissionPublishProtected, expo.PermissionRun, expo.PermissionView}
	if got, _ := w.expo.robot(robot.id); !slices.Equal(sortPerms(got.perms), want) {
		t.Errorf("permissions after the level change = %v, want %v", got.perms, want)
	}
	w.seat().Level = config.ExpoLevelViewer
	w.sync()
	if got, _ := w.expo.robot(robot.id); !slices.Equal(got.perms, []expo.Permission{expo.PermissionView}) {
		t.Errorf("permissions after lowering = %v, want VIEW", got.perms)
	}

	// ROTATE.
	w.syncAsking(map[string]seataccess.Ask{"qa": {Rotate: true}})
	if rotated := w.sealed(robot.id); rotated == first {
		t.Error("a rotation sealed the same token")
	}

	// OFF.
	off := false
	w.seat().Enabled = &off
	w.sync()
	if got, _ := w.expo.robot(robot.id); !got.deleting {
		t.Error("the robot's deletion was not scheduled after off")
	}
	if live := w.expo.liveTokens(robot.id); len(live) != 0 {
		t.Errorf("live tokens after off = %v", live)
	}
	if _, ok := w.sink.Held(sealedQA); ok {
		t.Error("the sealed token outlived the seat being turned off")
	}

	// BACK ON: a new robot, since Expo keeps no switched-off one.
	on := true
	w.seat().Enabled = &on
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("back on reported %+v", report.Findings)
	}
	again := w.record()
	if !again.Held() || again.Account.ID == robot.id {
		t.Fatalf("record after back on = %+v, want a new robot", again)
	}
	w.sealed(again.Account.ID)
}

// A TOKEN THE SYNC CANNOT SEAL IS REVOKED AT EXPO, and the seat reports the
// failure rather than holding a live token nobody has.
func TestAnExpoTokenThatCannotBeSealedIsRevoked(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	w.sink.FailRecord = map[string]error{sealedQA: errors.New("the store refused")}
	report := w.sync()
	rec := w.record()
	if !rec.Held() {
		t.Fatalf("record = %+v, want the created robot recorded", rec)
	}
	if live := w.expo.liveTokens(rec.Account.ID); len(live) != 0 {
		t.Errorf("live tokens = %v, want the unsealed token revoked", live)
	}
	if len(report.Findings) != 1 || report.Findings[0].Kind != integration.FindingIdentityFailed {
		t.Errorf("findings = %+v, want the seat's failure", report.Findings)
	}
}

// A TOKEN EXPO ANSWERED WITH NO VALUE is taken back at once, never left live
// and held by nobody.
func TestAnExpoTokenWithNoValueIsTakenBack(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	w.expo.emptyValue = true
	w.sync()
	rec := w.record()
	if live := w.expo.liveTokens(rec.Account.ID); len(live) != 0 {
		t.Errorf("live tokens = %v, want none", live)
	}
	if _, ok := w.sink.Held(sealedQA); ok {
		t.Error("an empty token was sealed")
	}
}

// A TOKEN REVOKED AT EXPO IS REPLACED. The ledger still records the sealed
// token, and the robot holds no live one, so the next sync mints and seals a
// fresh token and retires nothing a person added.
func TestAnExpoTokenRevokedAtExpoIsReMinted(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	w.sync()
	rec := w.record()
	before := w.sealed(rec.Account.ID)
	w.expo.mu.Lock()
	for _, tok := range w.expo.tokens {
		tok.revoked = true
	}
	w.expo.mu.Unlock()
	someone := w.expo.addToken(rec.Account.ID, "a person's token")

	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("findings = %+v", report.Findings)
	}
	after, ok := w.sink.Held(sealedQA)
	if !ok || after == before {
		t.Fatalf("sealed=%v changed=%v, want a fresh token sealed", ok, after != before)
	}
	if live := w.expo.liveTokens(rec.Account.ID); !slices.Contains(live, someone.id) || len(live) != 2 {
		t.Errorf("live tokens = %v, want the new one and the person's own", live)
	}
}

// A ROBOT DELETED AT EXPO IS NOT THE SEAT'S ANY MORE: the next sync creates a
// new one and seals its token.
func TestAnExpoRobotDeletedAtExpoIsReplaced(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	w.sync()
	old := w.record().Account.ID
	w.expo.mu.Lock()
	delete(w.expo.robots, old)
	w.expo.mu.Unlock()
	w.later()
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("findings = %+v", report.Findings)
	}
	rec := w.record()
	if !rec.Held() || rec.Account.ID == old {
		t.Fatalf("record = %+v, want a new robot", rec)
	}
	w.sealed(rec.Account.ID)
}

// A ROBOT UNDER THE ENGINE'S NAME THAT THE LEDGER DOES NOT RECORD is never the
// seat's: it is not adopted, changed or deleted, and the seat gets its own.
func TestAnExpoRobotIsNeverAdoptedByName(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	stranger := w.expo.addRobot("crewlet-acme-qa", expo.PermissionAdmin)
	strangerToken := w.expo.addToken(stranger.id, expo.TokenNote("qa"))
	w.sync()
	rec := w.record()
	if rec.Account.ID == stranger.id {
		t.Fatal("the seat adopted a robot by its name")
	}
	got, _ := w.expo.robot(stranger.id)
	if got.deleting || !slices.Equal(got.perms, []expo.Permission{expo.PermissionAdmin}) ||
		!slices.Contains(w.expo.liveTokens(stranger.id), strangerToken.id) {
		t.Errorf("the stranger robot was touched: %+v", got)
	}
}

// A REFUSAL IS A PERSON'S, with the remedy and where to act; a rate limit or
// a 5xx is a fault the sync retries.
func TestAnExpoRefusalNamesItsRemedy(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	w.expo.refuse["CrewletCreateRobot"] = fakeRefusal{code: "UNAUTHORIZED_ERROR", message: "not permitted"}
	report := w.sync()
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %+v, want one", report.Findings)
	}
	f := report.Findings[0]
	if f.Kind != integration.FindingIdentityFailed || f.ActionURL != expo.AccessTokensURL(testAccountName) {
		t.Errorf("finding = %+v", f)
	}
	sync := w.record().Lanes[seataccess.LaneExpo].Sync
	if sync == nil || sync.Outcome != seataccess.OutcomeFailed || sync.Error == nil ||
		sync.Error.Actor != seataccess.ByYou || sync.Error.Remedy == "" {
		t.Errorf("sync = %+v, want a person's refusal with a remedy", sync)
	}
}

// A ROBOT EMPTIED BY SOMEBODY ELSE WAITS FOR A PERSON: the engine never strips
// a robot's permissions itself, so one holding none was changed at Expo, and a
// pass does not hand them back unasked.
func TestAnExpoRobotEmptiedByHandWaits(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	w.sync()
	rec := w.record()
	w.expo.mu.Lock()
	w.expo.robots[rec.Account.ID].perms = nil
	w.expo.mu.Unlock()
	writes := len(w.expo.writes())
	w.sync()
	if got := w.expo.writes(); len(got) != writes {
		t.Errorf("writes = %q, want none", got[writes:])
	}
	w.syncAsking(map[string]seataccess.Ask{"qa": {Reenable: true}})
	if got, _ := w.expo.robot(rec.Account.ID); !slices.Equal(got.perms, []expo.Permission{expo.PermissionView}) {
		t.Errorf("permissions after turning back on = %v, want VIEW", got.perms)
	}
}

func sortPerms(perms []expo.Permission) []expo.Permission {
	out := slices.Clone(perms)
	slices.Sort(out)
	return out
}

// A ROBOT THE LISTING DOES NOT SHOW YET IS STILL THE ONE JUST CREATED: its
// token is minted on it and sealed, and the next pass finds it and writes
// nothing, rather than closing the record and creating a second robot.
func TestAnExpoRobotNotListedYetIsStillTheSeats(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	w.expo.lag = 1
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("findings = %+v", report.Findings)
	}
	rec := w.record()
	w.sealed(rec.Account.ID)
	// The next listing still misses it: a fault to retry, and nothing written.
	w.expo.lag = 0
	w.expo.mu.Lock()
	w.expo.lagged[rec.Account.ID] = 1
	w.expo.mu.Unlock()
	writes := len(w.expo.writes())
	w.sync()
	if got := w.expo.writes(); len(got) != writes {
		t.Errorf("a pass that could not list the robot wrote %q", got[writes:])
	}
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("the pass that lists it again = %+v", report.Findings)
	}
	if got := w.expo.writes(); len(got) != writes {
		t.Errorf("the robot listed again was written %q", got[writes:])
	}
	if again := w.record(); again.Account.ID != rec.Account.ID {
		t.Errorf("the seat moved to robot %s from %s", again.Account.ID, rec.Account.ID)
	}
	w.expo.mu.Lock()
	robots := len(w.expo.robots)
	w.expo.mu.Unlock()
	if robots != 1 {
		t.Errorf("the account holds %d robots, want one", robots)
	}
}
