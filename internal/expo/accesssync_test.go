package expo_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

const (
	sealedQA  = "EXPO_TOKEN_QA"
	retiredQA = "EXPO_RETIRED_TOKEN_QA"
)

// fakeVendor is the registered Expo vendor, opened against a fake Expo.
type fakeVendor struct {
	seataccess.Vendor
	hc *http.Client
}

func (v fakeVendor) Open(ctx context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return expo.OpenAccess(ctx, v.Vendor, in, v.hc)
}

// sinkSource resolves what the test sink holds, as the sealed store does.
type sinkSource struct{ s *seataccesstest.Sink }

func (s sinkSource) Lookup(name string) (string, bool) { return s.s.Held(name) }

// syncWorld is one company, its Expo account, the ledger and the sealed
// store, run through the shared sync as the loop runs it.
type syncWorld struct {
	t       *testing.T
	expo    *fakeExpo
	vendor  seataccess.Vendor
	company *config.Company
	ledger  *seataccesstest.Ledger
	sink    *seataccesstest.Sink
	drift   []seataccess.DriftRestored
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
	company.Integrations.Expo = &config.Expo{Enabled: true,
		Provisioning: &config.ExpoProvisioning{AccountName: "${EXPO_ACCOUNT}"}}
	company.Roles = []config.Role{{Name: "QA", Handle: "qa",
		Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: &on, Level: level}}}}
	return &syncWorld{t: t, expo: f, company: company,
		vendor: fakeVendor{Vendor: registered, hc: f.server(t)},
		ledger: seataccesstest.NewLedger(), sink: seataccesstest.NewSink()}
}

func (w *syncWorld) seat() *config.RoleExpo { return w.company.Roles[0].Integrations.Expo }

func (w *syncWorld) open() seataccess.OpenInput {
	return seataccess.OpenInput{Config: w.company, Env: config.NewResolver(
		config.MapSource{"EXPO_ACCOUNT": testAccountName}, sinkSource{w.sink})}
}

func (w *syncWorld) syncAsking(asks map[string]seataccess.Ask) seataccess.SyncReport {
	w.t.Helper()
	report, err := seataccess.Sync(context.Background(), seataccess.SyncInput{
		Vendor: w.vendor, Ledger: w.ledger, Sink: w.sink, Pass: "test", By: "test", Asks: asks,
		Open:    w.open(),
		OnDrift: func(_ context.Context, d seataccess.DriftRestored) { w.drift = append(w.drift, d) },
	})
	if err != nil {
		w.t.Fatalf("sync: %v", err)
	}
	return report
}

func (w *syncWorld) sync() seataccess.SyncReport { return w.syncAsking(nil) }

// paste is the paste route's work for qa.
func (w *syncWorld) paste(value string) (seataccess.HandedIn, error) {
	w.t.Helper()
	return w.pasteFor("qa", value)
}

// pasteFor is the paste route's work for one seat: the check, then the seat's
// sync carrying the value.
func (w *syncWorld) pasteFor(handle, value string) (seataccess.HandedIn, error) {
	w.t.Helper()
	i := slices.IndexFunc(w.company.Roles, func(r config.Role) bool { return r.Handle == handle })
	if i < 0 {
		w.t.Fatalf("no seat %s", handle)
	}
	role := &w.company.Roles[i]
	sess, err := w.vendor.Open(context.Background(), w.open())
	if err != nil {
		w.t.Fatal(err)
	}
	records, err := w.ledger.Records(context.Background(), integration.KindExpo)
	if err != nil {
		w.t.Fatal(err)
	}
	var rec *seataccess.Record
	if r, ok := records[handle]; ok {
		rec = &r
	}
	want, _ := seataccess.DesiredOf(role, seataccess.LaneExpo)
	handed, err := sess.(seataccess.HandsIn).CheckHandIn(context.Background(), seataccess.HandInInput{
		Seat: seataccess.SeatRef{Handle: handle, Name: role.Name}, Lane: seataccess.LaneExpo, Want: want,
		Record: rec, Records: records, Value: value,
	})
	if err != nil {
		return handed, err
	}
	w.syncAsking(map[string]seataccess.Ask{handle: {HandIn: value, Rotate: handed.Rotate, Lane: seataccess.LaneExpo}})
	return handed, nil
}

func (w *syncWorld) lane() seataccess.LaneRecord {
	w.t.Helper()
	rec, ok := w.ledger.Record(integration.KindExpo, "qa")
	if !ok {
		w.t.Fatal("the ledger has no record for qa")
	}
	return rec.Lanes[seataccess.LaneExpo]
}

// waitingOn is the step the seat's lane waits on a person for, with its
// sentence, or fails.
func (w *syncWorld) waitingOn(verb seataccess.StepVerb) string {
	w.t.Helper()
	sync := w.lane().Sync
	if sync == nil || sync.Outcome != seataccess.OutcomeWaiting || sync.Error == nil || sync.Error.Step != verb ||
		sync.Error.Actor != seataccess.ByYou || sync.Error.ActionURL != expo.AccessTokensURL(testAccountName) {
		w.t.Fatalf("sync = %+v, want waiting on a person to %s at Expo", sync, verb)
	}
	return sync.Error.Said
}

func (w *syncWorld) inSync() {
	w.t.Helper()
	if sync := w.lane().Sync; sync == nil || sync.Outcome != seataccess.OutcomeOK {
		w.t.Fatalf("sync = %+v, want in sync", sync)
	}
}

// A SEAT'S WHOLE LIFE, SET UP BY A PERSON: turned on, it waits for a robot
// and makes no call to Expo; the pasted token is checked and sealed; a
// converged pass only reads; a level change and drift wait for the person to
// change the role at Expo; off clears the token at once and waits for the
// robot to be deleted.
func TestAnExpoSeatSetUpByAPerson(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ExpoLevelRunner)

	w.sync()
	said := w.waitingOn(seataccess.VerbCreate)
	for _, want := range []string{"crewlet-acme-qa", "Runner", testAccountName} {
		if !strings.Contains(said, want) {
			t.Errorf("%q does not say %q", said, want)
		}
	}
	if w.lane().Observed.Account != "crewlet-acme-qa" {
		t.Errorf("observed account = %q, want the name to give the robot", w.lane().Observed.Account)
	}
	if ops := w.expo.operations(); len(ops) != 0 {
		t.Fatalf("a seat with no token called Expo: %v", ops)
	}

	robot, tok := w.expo.robot("crewlet-acme-qa", expo.RoleRunner)
	handed, err := w.paste(tok.value)
	if err != nil {
		t.Fatalf("paste: %v", err)
	}
	if handed.Detail != "Works. Robot crewlet-acme-qa, Runner on example-org. Sealed as EXPO_TOKEN_QA." {
		t.Errorf("detail = %q", handed.Detail)
	}
	if sealed, ok := w.sink.Held(sealedQA); !ok || sealed != tok.value {
		t.Fatal("the pasted token was not sealed as EXPO_TOKEN_QA")
	}
	w.inSync()
	if rec, _ := w.ledger.Record(integration.KindExpo, "qa"); !rec.Held() || rec.Account.ID != robot.id {
		t.Fatalf("record = %+v, want the robot recorded", rec)
	}
	w.expo.onlyExpo(t)

	if report := w.sync(); len(report.Findings) != 0 {
		t.Errorf("a converged pass reported %+v", report.Findings)
	}
	w.inSync()

	// A LEVEL CHANGE HERE is the person's to make at Expo.
	w.seat().Level = config.ExpoLevelDeveloper
	w.sync()
	if said := w.waitingOn(seataccess.VerbRerole); !strings.Contains(said, "Runner") ||
		!strings.Contains(said, "Developer") {
		t.Errorf("%q does not name both roles", said)
	}
	w.expo.setRole(robot.id, expo.RoleDeveloper)
	w.sync()
	w.inSync()

	// DRIFT AT EXPO waits for the person too, naming both roles.
	w.expo.setRole(robot.id, expo.RoleReleaseManager)
	w.sync()
	if said := w.waitingOn(seataccess.VerbRerole); !strings.Contains(said, "Release Manager") {
		t.Errorf("%q does not name the role at Expo", said)
	}
	w.expo.setRole(robot.id, expo.RoleDeveloper)
	w.sync()
	w.inSync()

	// OFF: the token leaves the box at once, and the robot is watched until
	// it is deleted at Expo.
	off := false
	w.seat().Enabled = &off
	w.sync()
	if _, ok := w.sink.Held(sealedQA); ok {
		t.Error("the token outlived the seat being turned off")
	}
	if retired, ok := w.sink.Held(retiredQA); !ok || retired != tok.value {
		t.Fatal("the turned-off token was not kept where no box reads it")
	}
	w.waitingOn(seataccess.VerbRemove)
	w.sync()
	w.waitingOn(seataccess.VerbRemove)
	w.expo.deleteActor(robot.id)
	w.sync()
	if _, ok := w.sink.Held(retiredQA); ok {
		t.Error("the retired token outlived its robot's deletion")
	}
	if rec, _ := w.ledger.Record(integration.KindExpo, "qa"); rec.Held() {
		t.Errorf("record = %+v, want the deleted robot released", rec)
	}
}

// A TOKEN EXPO STOPS ACCEPTING IS CLEARED AND ASKED FOR AGAIN, and Expo is not
// asked with it a second time.
func TestARevokedExpoTokenIsAskedForAgain(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	_, tok := w.expo.robot("crewlet-acme-qa", expo.RoleViewOnly)
	if _, err := w.paste(tok.value); err != nil {
		t.Fatal(err)
	}
	w.expo.revoke(tok)
	w.sync()
	if said := w.waitingOn(seataccess.VerbMint); !strings.Contains(said, "no longer accepts") {
		t.Errorf("%q does not say the token was refused", said)
	}
	if _, ok := w.sink.Held(sealedQA); ok {
		t.Error("the refused token is still sealed")
	}
	calls := len(w.expo.operations())
	w.sync()
	if got := w.expo.operations(); len(got) != calls {
		t.Errorf("a pass after the refusal called Expo again: %v", got[calls:])
	}
	w.waitingOn(seataccess.VerbMint)

	// A NEW ROBOT IS ALLOWED after a refusal: the old one may be gone.
	_, fresh := w.expo.robot("crewlet-acme-qa-2", expo.RoleViewOnly)
	if _, err := w.paste(fresh.value); err != nil {
		t.Fatalf("a new robot's token after a refusal: %v", err)
	}
	w.inSync()
}

// A ROTATE IS A NEW TOKEN FOR THE SAME ROBOT, and the old one is the person's
// to delete at Expo.
func TestAnExpoRotateIsANewTokenForTheSameRobot(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	robot, first := w.expo.robot("crewlet-acme-qa", expo.RoleViewOnly)
	if _, err := w.paste(first.value); err != nil {
		t.Fatal(err)
	}
	w.sync()

	_, stranger := w.expo.robot("someone-else", expo.RoleViewOnly)
	if _, err := w.paste(stranger.value); !refused(err, "not to QA's robot") {
		t.Fatalf("another robot's token for a rotate = %v", err)
	}
	second := w.expo.mint(robot.id)
	handed, err := w.paste(second.value)
	if err != nil || !handed.Rotate {
		t.Fatalf("rotate = %+v, %v", handed, err)
	}
	if sealed, _ := w.sink.Held(sealedQA); sealed != second.value {
		t.Fatal("the new token was not sealed")
	}
	if said := w.waitingOn(seataccess.VerbRevoke); !strings.Contains(said, first.prefix) {
		t.Errorf("%q does not name the old token", said)
	}
	w.expo.revoke(first)
	w.syncAsking(map[string]seataccess.Ask{"qa": {Rotate: true, HandIn: second.value, Lane: seataccess.LaneExpo}})
	w.inSync()
}

// EVERY REFUSAL IS THE PERSON'S, beside the field, and seals nothing.
func TestAnExpoPasteIsRefusedForEachCheck(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ExpoLevelRunner)
	_, onAnother := w.expo.robot("crewlet-acme-qa", expo.RoleRunner)
	w.expo.mu.Lock()
	for _, a := range w.expo.actors {
		a.roles = map[string]expo.Role{"another-org": expo.RoleRunner}
	}
	w.expo.mu.Unlock()
	_, developer := w.expo.robot("crewlet-acme-qa", expo.RoleDeveloper)
	_, admin := w.expo.robot("crewlet-acme-qa", expo.RoleAdmin)
	_, custom := w.expo.robot("crewlet-acme-qa", expo.RoleCustom)
	_, unsaid := w.expo.robot("crewlet-acme-qa", "")
	_, revoked := w.expo.robot("crewlet-acme-qa", expo.RoleRunner)
	w.expo.revoke(revoked)
	person := w.expo.person("Avery", expo.RoleRunner)
	for _, tc := range []struct {
		name, value, says string
	}{
		{"a refused token", revoked.value, "Expo did not accept this token"},
		{"a person's token", person.value, "belongs to the person Avery"},
		{"a robot of another account", onAnother.value, "holds no role on example-org"},
		{"another role", developer.value, "is a Developer on example-org, and you chose Runner"},
		{"Admin", admin.value, "Agents never hold Admin"},
		{"a custom role", custom.value, "custom role"},
		{"a role Expo does not say", unsaid.value, "Expo did not say this robot's role"},
	} {
		if _, err := w.paste(tc.value); !refused(err, tc.says) {
			t.Errorf("%s: %v, want a refusal saying %q", tc.name, err, tc.says)
		}
		if _, ok := w.sink.Held(sealedQA); ok {
			t.Fatalf("%s: a refused token was sealed", tc.name)
		}
	}
}

// A ROBOT IS ONE SEAT'S: another seat's robot is refused, naming that seat.
func TestAnExpoRobotIsOneSeats(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	on := true
	w.company.Roles = append(w.company.Roles, config.Role{Name: "SRE", Handle: "sre",
		Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: &on}}})
	robot, tok := w.expo.robot("crewlet-acme-sre", expo.RoleViewOnly)
	w.ledger.Put(context.Background(), seataccess.Record{Kind: integration.KindExpo, Handle: "sre", Name: "SRE",
		Account: &seataccess.Account{ID: robot.id, Name: "crewlet-acme-sre", State: seataccess.AccountActive}})
	if _, err := w.paste(tok.value); !refused(err, "already sre's") {
		t.Fatalf("another seat's robot = %v", err)
	}
}

// refused reports a check refusal saying says.
func refused(err error, says string) bool {
	r, ok := errors.AsType[*seataccess.Refusal](err)
	return ok && strings.Contains(r.Said, says) && r.Remedy != "" && r.Actor == seataccess.ByYou
}

// A DISCONNECT KEEPS NOTHING: no pass reads the seat again, so its token is
// cleared outright rather than kept to watch its robot.
func TestAnExpoDisconnectKeepsNoToken(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	_, tok := w.expo.robot("crewlet-acme-qa", expo.RoleViewOnly)
	if _, err := w.paste(tok.value); err != nil {
		t.Fatal(err)
	}
	if _, err := seataccess.Sync(context.Background(), seataccess.SyncInput{
		Vendor: w.vendor, Ledger: w.ledger, Sink: w.sink, Pass: "test", By: "test",
		Open: w.open(), Disconnect: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{sealedQA, retiredQA} {
		if _, ok := w.sink.Held(name); ok {
			t.Errorf("%s outlived the disconnect", name)
		}
	}
	if rec, _ := w.ledger.Record(integration.KindExpo, "qa"); rec.Held() {
		t.Errorf("record = %+v, want the robot let go", rec)
	}
}

// A ROBOT'S OTHER LIVE TOKENS ARE ASKED FOR ON EVERY PASS until Expo shows them
// gone, after a first paste of a robot that already had one as after a rotate.
func TestAnExpoRobotsOtherTokensAreAskedForUntilDeleted(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	robot, old := w.expo.robot("crewlet-acme-qa", expo.RoleViewOnly)
	pasted := w.expo.mint(robot.id)
	if _, err := w.paste(pasted.value); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		w.sync()
		if said := w.waitingOn(seataccess.VerbRevoke); !strings.Contains(said, old.prefix+"…") {
			t.Errorf("%q does not name the old token", said)
		}
		retired := w.lane().Observed.Retired
		if len(retired) != 1 || retired[0].Label != old.prefix+"…" || retired[0].Since.IsZero() {
			t.Fatalf("observed retired = %+v, want the old token with its creation time", retired)
		}
		if plan := w.lane().Sync.Steps; len(plan) != 1 || plan[0].Step.Op != seataccess.OpRetire {
			t.Errorf("steps = %+v, want the retire op alone", plan)
		}
	}
	w.expo.revoke(old)
	w.sync()
	w.inSync()
	if retired := w.lane().Observed.Retired; len(retired) != 0 {
		t.Errorf("observed retired = %+v after the old token was revoked", retired)
	}
}

// TURNED BACK ON, a seat forgets its old robot's token: the robot comes back
// only through a paste, never by reusing what was kept to watch it.
func TestAnExpoSeatTurnedBackOnForgetsTheRetiredToken(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	robot, tok := w.expo.robot("crewlet-acme-qa", expo.RoleViewOnly)
	if _, err := w.paste(tok.value); err != nil {
		t.Fatal(err)
	}
	off, on := false, true
	w.seat().Enabled = &off
	w.sync()
	if _, ok := w.sink.Held(retiredQA); !ok {
		t.Fatal("the turned-off token was not kept to watch its robot")
	}
	w.seat().Enabled = &on
	w.sync()
	for _, name := range []string{sealedQA, retiredQA} {
		if _, ok := w.sink.Held(name); ok {
			t.Errorf("%s is sealed after the seat was turned back on", name)
		}
	}
	w.waitingOn(seataccess.VerbMint)
	again := w.expo.mint(robot.id)
	if _, err := w.paste(again.value); err != nil {
		t.Fatalf("the old robot's new token: %v", err)
	}
	if sealed, _ := w.sink.Held(sealedQA); sealed != again.value {
		t.Error("the new token was not sealed")
	}
}

// KEEPING THE ROBOT AT EXPO closes the delete step, forgets the retired token
// and says it was the person's choice.
func TestKeepingAnExpoRobotClosesItsDeleteStep(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "")
	if spec, _ := w.vendor.Catalog().Lane(seataccess.LaneExpo); !spec.Releasable {
		t.Fatal("Expo's lane does not offer keeping its robot")
	}
	_, tok := w.expo.robot("crewlet-acme-qa", expo.RoleViewOnly)
	if _, err := w.paste(tok.value); err != nil {
		t.Fatal(err)
	}
	off := false
	w.seat().Enabled = &off
	w.sync()
	w.waitingOn(seataccess.VerbRemove)
	w.syncAsking(map[string]seataccess.Ask{"qa": {Release: true, Lane: seataccess.LaneExpo}})
	if _, ok := w.sink.Held(retiredQA); ok {
		t.Error("the retired token outlived the person keeping the robot")
	}
	steps := w.lane().Sync.Steps
	if last := steps[len(steps)-1]; !last.Done || !strings.Contains(last.Text, "as you chose") {
		t.Errorf("steps = %+v, want the delete step closed as the person's choice", steps)
	}
	if rec, _ := w.ledger.Record(integration.KindExpo, "qa"); rec.Held() {
		t.Errorf("record = %+v, want the kept robot no longer tracked", rec)
	}
}

// THE BOX READS THE LIVE TOKEN ALONE: Expo's lane binds EXPO_TOKEN to the
// seat's sealed token and nothing to the retired one, a person makes and
// re-enables its robot, and config restates the catalog's sealed fields.
func TestTheExpoCatalogBindsOnlyTheLiveTokenToTheBox(t *testing.T) {
	t.Parallel()
	vendor, ok := seataccess.Lookup(integration.KindExpo)
	if !ok {
		t.Fatal("the Expo vendor is not registered")
	}
	cat := vendor.Catalog()
	spec, ok := cat.Lane(seataccess.LaneExpo)
	if !ok || spec.CreatedBy != seataccess.ByYou || spec.EnabledBy != seataccess.ByYou ||
		!spec.Rotatable || !spec.Releasable {
		t.Fatalf("lane = %+v, want a robot a person makes, rotatable and releasable", spec)
	}
	want := []seataccess.BoxLine{{Name: "EXPO_TOKEN", Value: "${" + sealedQA + "}", By: seataccess.ByYou}}
	if got := seataccess.BoxLines(integration.KindExpo, seataccess.LaneExpo, "qa"); !slices.Equal(got, want) {
		t.Fatalf("box lines = %v, want %v", got, want)
	}
	if !slices.Equal(config.SeatSealedFields("expo"), cat.Credentials) {
		t.Fatalf("config restates %v, the catalog %v", config.SeatSealedFields("expo"), cat.Credentials)
	}
}
