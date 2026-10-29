package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// anyFleet is coord.Fleet under a name a wrapper can embed: the interface
// has a method called Fleet, which an embedded field of that name would hide.
type anyFleet = coord.Fleet

// failingCodeWrite is a fleet whose writes of a pending session fail: the
// store going away just as the CLI printed its code.
type failingCodeWrite struct{ anyFleet }

func (f failingCodeWrite) UpdateDeviceSignIn(ctx context.Context, s coord.DeviceSignIn) (coord.DeviceSignIn, bool, error) {
	if s.Status == coord.DevicePending {
		return coord.DeviceSignIn{}, false, errors.New("store unavailable")
	}
	return f.anyFleet.UpdateDeviceSignIn(ctx, s)
}

// A CODE NOBODY CAN READ IS NO SIGN-IN. Where the code cannot be recorded, a
// start answers that the sign-in is unavailable, saying why, and the session
// ends: never a zero session with no error, which the page polled for ever.
//
// Mutation: answer the code's record whatever its write returned, and this
// fails.
func TestAStartWhoseCodeCannotBeRecordedSaysSo(t *testing.T) {
	t.Parallel()
	// THE FAILING STORE FROM THE START, which the node's own watch and
	// sweep read as well.
	fleet, leases := failingCodeWrite{coordmemory.NewFleet()}, coordmemory.New()
	login := &fakeDeviceLogin{finish: make(chan error)}
	e := signInNode(t, fleet, leases, "node-1", login)
	got, err := e.StartDeviceSignIn(t.Context(), "codex-review", person)
	if !errors.Is(err, ErrDeviceSignInUnavailable) || !strings.Contains(err.Error(), "could not be recorded") {
		t.Fatalf("StartDeviceSignIn = %+v, %v; want unavailable, saying the code could not be recorded", got, err)
	}
}

// stubbornLogin is a device login that prints its code and then ends only when
// finish is closed, whatever its context says: a CLI slow to exit.
type stubbornLogin struct{ finish chan struct{} }

func (f *stubbornLogin) login(_ context.Context, _ *cliagent.Provider, out func(cliagent.DeviceCode)) error {
	out(cliagent.DeviceCode{URL: "https://auth.example.com/codex/device", Code: "QXJM-7KPD",
		ExpiresAt: time.Now().Add(15 * time.Minute)})
	<-f.finish
	return errors.New("cancelled")
}

// A START WHOSE CODE CANNOT BE RECORDED ANSWERS ITS CALLER IN TIME. The
// session is cancelled, and the start waits for its end only as long as the
// person's request lasts: a CLI slow to exit holds the session, not the
// request.
//
// Mutation: wait for the session's end whatever the caller's context says,
// and this start never answers.
func TestAFailedCodeWriteHonoursItsCaller(t *testing.T) {
	t.Parallel()
	fleet, leases := failingCodeWrite{coordmemory.NewFleet()}, coordmemory.New()
	e := signInNode(t, fleet, leases, "node-1", &fakeDeviceLogin{finish: make(chan error)})
	stubborn := &stubbornLogin{finish: make(chan struct{})}
	t.Cleanup(func() { close(stubborn.finish) })
	e.signIns.login = stubborn.login
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		_, err := e.StartDeviceSignIn(ctx, "codex-review", person)
		answered <- err
	}()
	select {
	case err := <-answered:
		if !errors.Is(err, ErrDeviceSignInUnavailable) || !strings.Contains(err.Error(), "could not be recorded") {
			t.Fatalf("StartDeviceSignIn = %v; want unavailable, saying the code could not be recorded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a start whose code could not be recorded did not answer once its caller's context ended")
	}
}

// slowWriter is a config writer whose re-activation takes delay.
type slowWriter struct {
	delay   time.Duration
	reloads atomic.Int32
}

func (w *slowWriter) Apply(context.Context, []byte, string, iam.Actor) error           { return nil }
func (w *slowWriter) Seat(context.Context, string) ([]byte, error)                     { return nil, nil }
func (w *slowWriter) SetSeat(context.Context, string, []byte, string, iam.Actor) error { return nil }
func (w *slowWriter) Reload(ctx context.Context, _ string, _ iam.Actor) error {
	w.reloads.Add(1)
	select {
	case <-time.After(w.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// adoptLogin gives p a login of its CLI's own files, as an approved device
// sign-in leaves one.
func adoptLogin(ctx context.Context, t *testing.T, p *cliagent.Provider, file, content string) error {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	_, err := p.AdoptHostLogin(ctx, home)
	return err
}

// AN APPROVAL IS STORED, CARRIED TO THE FLEET, AND SAID, EACH ON ITS OWN
// BUDGET. The login is exported, sealed under the entry's bundle and the
// revision re-activated within the approval's budget, and the session's last
// write gets a fresh budget of its own: a re-activation that takes longer
// than that write is allowed still leaves the session saying it was approved,
// with nothing left to do.
//
// Mutation: run the approval on the last write's budget, and the slow
// re-activation is cut off and the session says the login was not carried.
func TestAnApprovalIsStoredReactivatedAndSaid(t *testing.T) {
	t.Parallel()
	fleet, leases := coordmemory.NewFleet(), coordmemory.New()
	approve := make(chan struct{})
	login := &fakeDeviceLogin{finish: make(chan error)}
	e := signInNode(t, fleet, leases, "node-1", login)
	_, cipher := testKeyring(t)
	e.cipher = cipher
	writer := &slowWriter{delay: 300 * time.Millisecond}
	e.UseConfigWriter(writer)
	e.signIns.finalBudget = 50 * time.Millisecond
	e.signIns.login = func(ctx context.Context, p *cliagent.Provider, out func(cliagent.DeviceCode)) error {
		out(cliagent.DeviceCode{URL: "https://auth.example.com/codex/device", Code: "QXJM-7KPD",
			ExpiresAt: time.Now().Add(15 * time.Minute)})
		<-approve
		return adoptLogin(ctx, t, p, ".codex/auth.json", `{"tokens":{"access_token":"approved"}}`)
	}

	started, err := e.StartDeviceSignIn(t.Context(), "codex-review", person)
	if err != nil {
		t.Fatalf("StartDeviceSignIn: %v", err)
	}
	close(approve)
	ended := statusOn(t, e, started.ID, coord.DeviceApproved)
	if ended.Detail != "" {
		t.Errorf("the approved session says %q, want nothing left to do", ended.Detail)
	}
	if n := writer.reloads.Load(); n != 1 {
		t.Errorf("the revision was re-activated %d times, want once", n)
	}
	rows, err := fleet.SecretValues(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	name := cliprofile.BundleVarName("codex-review")
	found := false
	for _, r := range rows {
		found = found || r.Name == name
	}
	if !found {
		t.Errorf("the login was not sealed as %s", name)
	}
}

// SAME-NODE STARTS ARE ONE SESSION. A model's session is reserved on this node
// before its record is written, so a second start on the same node is told
// one runs rather than replacing it while its first is still being written.
//
// Mutation: register the session only after its record is claimed, and two of
// the starts each read the other's record as nobody's and replace it.
func TestSameNodeStartsAreOneSession(t *testing.T) {
	t.Parallel()
	fleet, leases := coordmemory.NewFleet(), coordmemory.New()
	login := &fakeDeviceLogin{finish: make(chan error)}
	e := signInNode(t, fleet, leases, "node-1", login)
	// EVERY SESSION'S CLI ENDS before the node's stop waits for them, so a
	// session two starts both believed theirs fails this case rather than
	// hanging it.
	t.Cleanup(func() { close(login.finish) })
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := e.StartDeviceSignIn(t.Context(), "codex-review", person)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	started := 0
	for err := range errs {
		var running *SignInInProgressError
		switch {
		case err == nil:
			started++
		case !errors.As(err, &running):
			t.Errorf("a concurrent start = %v, want the running session named", err)
		}
	}
	if started != 1 {
		t.Fatalf("%d concurrent starts on one node each began a session, want one", started)
	}
}

// THIS NODE'S SWEEP LEAVES A SESSION BEING STARTED, AND A STOPPING NODE STARTS
// NONE. A session's record is written after it is reserved, and the sweep that
// reads it in between must not fail it as one this node does not run; and a
// node that has begun stopping refuses a start rather than adding a session to
// a wait that has begun.
//
// Mutation: judge this node's sessions by those already running rather than
// those reserved, or let a stopping node start one, and this fails.
func TestASessionBeingStartedIsNotSweptAndAStoppingNodeStartsNone(t *testing.T) {
	t.Parallel()
	fleet, leases := coordmemory.NewFleet(), coordmemory.New()
	login := &fakeDeviceLogin{finish: make(chan error)}
	e := signInNode(t, fleet, leases, "node-1", login)
	record := coord.DeviceSignIn{ID: uuid.New(), Entry: "codex-review", Node: "node-1",
		Status: coord.DeviceStarting, StartedAt: time.Now(), ExpiresAt: time.Now().Add(15 * time.Minute),
		By: person.Name, ByKind: person.Kind}
	session, _, err := e.reserveDeviceSession(record)
	if err != nil {
		t.Fatalf("reserveDeviceSession: %v", err)
	}
	if _, _, err := fleet.CreateDeviceSignIn(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	e.sweepDeviceSignIns(t.Context())
	if got, _, _ := fleet.DeviceSignIn(t.Context(), "codex-review"); got.Status != coord.DeviceStarting {
		t.Errorf("the session being started reads %s after this node's sweep, want starting", got.Status)
	}
	e.unreserveDeviceSession(session.deviceSession, false)

	e.stopDeviceSignIns()
	if _, err := e.StartDeviceSignIn(t.Context(), "codex-review", person); !errors.Is(err, ErrDeviceSignInUnavailable) {
		t.Fatalf("a start on a stopping node = %v, want unavailable", err)
	}
}

// A CANCEL HONOURS ITS CALLER. It waits for the session's own end, and no
// longer than its caller does: a CLI slow to stop is the session's to wait
// out, not the request's.
//
// Mutation: poll the record on a clock of its own rather than the caller's
// context, and the cancel outlives its caller.
func TestACancelHonoursItsCaller(t *testing.T) {
	t.Parallel()
	fleet, leases := coordmemory.NewFleet(), coordmemory.New()
	login := &fakeDeviceLogin{finish: make(chan error)}
	e := signInNode(t, fleet, leases, "node-1", login)
	release := make(chan struct{})
	e.signIns.login = func(ctx context.Context, _ *cliagent.Provider, out func(cliagent.DeviceCode)) error {
		out(cliagent.DeviceCode{URL: "https://auth.example.com/codex/device", Code: "QXJM-7KPD",
			ExpiresAt: time.Now().Add(15 * time.Minute)})
		<-release
		return ctx.Err()
	}
	started, err := e.StartDeviceSignIn(t.Context(), "codex-review", person)
	if err != nil {
		t.Fatalf("StartDeviceSignIn: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		_, err := e.CancelDeviceSignIn(ctx, started.ID, person)
		answered <- err
	}()
	select {
	case err := <-answered:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a cancel whose caller gave up = %v, want its context's error", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("the cancel outlived its caller by seconds")
	}
	close(release)
	statusOn(t, e, started.ID, coord.DeviceCancelled)
}

// snapshot is every regular file under dir with its content and mode.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			out[path] = info.Mode().String()
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = info.Mode().String() + ":" + string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A DRAFT WITH A BUNDLE OF ITS OWN LEAVES THE LIVE ENTRY'S DISK ALONE. A draft
// on the live entry's state directory, naming a bundle with a newer login, is
// checked in a home of its own holding that bundle and nothing of the live
// login: a smoke call whose CLI refreshes its token writes that home, the live
// directory is byte-identical after, and the throwaway home is gone.
//
// Mutation: build the draft on its own state directory, and its bundle
// replaces the login every seat on the entry signs in with, or its refresh is
// carried over it.
func TestADraftCheckLeavesTheLiveEntrysDiskAlone(t *testing.T) {
	t.Parallel()
	missing := map[string]any{"binary": filepath.Join(t.TempDir(), "no-cli-here")}
	root := t.TempDir()
	stateDir, homes := filepath.Join(root, "state"), filepath.Join(root, "homes")
	for _, dir := range []string{stateDir, homes} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	spec := config.LLMProvider{Agent: "claude-code", Model: "opus", StateDir: stateDir,
		Overrides: map[string]any{"binary": refreshingCLI(t, root)}}
	live, err := BuildCLIAgent("plan", spec, config.NewResolver(config.MapSource{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := adoptLogin(t.Context(), t, live, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"live"}}`); err != nil {
		t.Fatalf("the live login: %v", err)
	}
	newer, err := BuildCLIAgent("plan", config.LLMProvider{Agent: "claude-code", Model: "opus",
		StateDir: t.TempDir(), Overrides: missing}, config.NewResolver(config.MapSource{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := adoptLogin(t.Context(), t, newer, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"newer"}}`); err != nil {
		t.Fatalf("the newer login: %v", err)
	}
	blob, err := newer.ExportBundle(t.Context())
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "plan", Provider: live}})
	if err != nil {
		t.Fatal(err)
	}
	e := epochEngine(&Company{Models: models, Config: &config.Company{Providers: config.Providers{
		LLM: map[string]config.LLMProvider{"plan": spec}, Sandbox: &config.SandboxProvider{Fake: true}}}})
	e.environ = config.MapSource{"PLAN_NEWER_LOGIN": blob}
	e.checkHomes = homes

	before := snapshot(t, stateDir)
	draft := spec
	draft.Auth.CredentialBundle = "${PLAN_NEWER_LOGIN}"
	if _, err := e.CheckModel(t.Context(), "plan", &draft, nil, true); err != nil {
		t.Fatalf("CheckModel(draft): %v", err)
	}
	after := snapshot(t, stateDir)
	if len(before) != len(after) {
		t.Fatalf("the live state directory holds %d paths after the draft's check, %d before", len(after), len(before))
	}
	for path, was := range before {
		if after[path] != was {
			t.Errorf("the draft's check changed the live entry's %s", path)
		}
	}
	if left, _ := os.ReadDir(e.checkHomes); len(left) != 0 {
		t.Errorf("the draft's check left its home behind: %v", left)
	}
}

// refreshingCLI is a claude-code CLI that answers --version and, on any other
// run, refreshes its login (writes "refreshed" over its credentials file) and
// fails: a token refresh during a call. It writes only under root, never the
// credentials of the host it runs on.
func refreshingCLI(t *testing.T, root string) string {
	t.Helper()
	cli := filepath.Join(t.TempDir(), "refreshing-claude")
	script := "#!/bin/sh\n" +
		"case \"$1\" in --version) echo '2.1.310 (Claude Code)'; exit 0;; esac\n" +
		"case \"$CLAUDE_CONFIG_DIR\" in " + root + "/*)\n" +
		"  mkdir -p \"$CLAUDE_CONFIG_DIR\"\n" +
		"  printf '%s' '{\"claudeAiOauth\":{\"accessToken\":\"refreshed\"}}' > \"$CLAUDE_CONFIG_DIR/.credentials.json\";;\n" +
		"esac\n" +
		"exit 1\n"
	if err := os.WriteFile(cli, []byte(script), 0o700); err != nil { //nolint:gosec // the test's own stub
		t.Fatal(err)
	}
	return cli
}

// A DRAFT CHECK ON THE LIVE LOGIN CARRIES A REFRESH BACK. A draft that names no
// bundle of its own signs in with the login the active entry keeps, and its
// smoke call checks that login out as a seat does: a CLI that refreshes its
// token during the check writes the shared directory, so the vendor's rotated
// refresh token is the one every seat signs in with next.
//
// Mutation: run the draft in a throwaway home seeded with a copy of the live
// login, and the refresh is removed with the home while the live directory
// keeps the token the vendor has just retired.
func TestADraftCheckCarriesARefreshedLoginBack(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir, homes := filepath.Join(root, "state"), filepath.Join(root, "homes")
	for _, dir := range []string{stateDir, homes} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cli := refreshingCLI(t, root)
	spec := config.LLMProvider{Agent: "claude-code", Model: "opus", StateDir: stateDir,
		Overrides: map[string]any{"binary": cli}}
	live, err := BuildCLIAgent("plan", spec, config.NewResolver(config.MapSource{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := adoptLogin(t.Context(), t, live, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"live"}}`); err != nil {
		t.Fatalf("the live login: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "plan", Provider: live}})
	if err != nil {
		t.Fatal(err)
	}
	e := epochEngine(&Company{Models: models, Config: &config.Company{Providers: config.Providers{
		LLM: map[string]config.LLMProvider{"plan": spec}, Sandbox: &config.SandboxProvider{Fake: true}}}})
	e.checkHomes = homes

	draft := spec
	draft.Model = "sonnet"
	if _, err := e.CheckModel(t.Context(), "plan", &draft, nil, true); err != nil {
		t.Fatalf("CheckModel(draft): %v", err)
	}
	got, err := os.ReadFile(filepath.Join(live.Workspace().CredentialsDir(), ".credentials.json"))
	if err != nil {
		t.Fatalf("the live login after the draft's check: %v", err)
	}
	if !strings.Contains(string(got), `"refreshed"`) {
		t.Errorf("the login the CLI refreshed during the draft's check never reached the live "+
			"directory, which still holds %s", got)
	}
	if left, _ := os.ReadDir(homes); len(left) != 0 {
		t.Errorf("the draft's check left its home behind: %v", left)
	}
}

// A DRAFT WHOSE STATE DIRECTORY CANNOT BE NAMED IS SAID. With no state_dir, no
// CLI home and no home directory, the draft names no directory to sign in
// from, and its check says so rather than answering for a home it would never
// run in.
//
// Mutation: drop the error naming the draft's state directory, and the check
// answers as if the draft were fine.
func TestADraftWithNoStateDirectoryIsSaid(t *testing.T) {
	// Not parallel: it clears the process's home.
	t.Setenv("HOME", "")
	t.Setenv(config.CLIHomeEnv, "")
	e := epochEngine(&Company{Config: &config.Company{Providers: config.Providers{
		LLM: map[string]config.LLMProvider{}, Sandbox: &config.SandboxProvider{Fake: true}}}})
	e.checkHomes = t.TempDir()
	draft := config.LLMProvider{Agent: "claude-code", Model: "opus",
		Overrides: map[string]any{"binary": filepath.Join(t.TempDir(), "no-cli-here")}}
	if _, err := e.CheckModel(t.Context(), "plan", &draft, nil, false); err == nil ||
		!strings.Contains(err.Error(), "state_dir") {
		t.Fatalf("CheckModel(a draft with no state directory) = %v; want its state_dir named", err)
	}
}

// A "RETRY NOW" THAT DOES NOT ANSWER LEAVES THIS NODE'S SIT-OUT AS IT WAS. The
// probe is made through a sibling with a record of its own, so a probe that
// fails for a reason that is not a limit writes no record and clears nothing,
// here or anywhere.
//
// Mutation: clear this node's window before probing, and a probe that failed
// leaves this node alone sending seats into a limit every other node sits out.
func TestAFailedRetryNowLeavesThisNodesSitOut(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	e, c := retryNode(t, fleet)
	spend(t, c, time.Now().Add(-time.Minute))
	res, err := e.RetryModel(t.Context(), "plan", coord.RetryProbe, person)
	if err != nil || res.Cleared || res.Failed == "" {
		t.Fatalf("RetryModel(probe) on a CLI that cannot run = %+v, %v; want a failure, nothing cleared", res, err)
	}
	if planOf(t, c).SittingOut() == nil {
		t.Error("a probe that did not answer cleared this node's sit-out")
	}
}

// A WAIT OUTLIVES AN APPLY'S RELEASE. An apply that brings new models releases
// every park and the seat parks again at once on the same limit: the same
// wait, still dated from when it began, and reported between the release and
// the re-park too, so `llm_limit_waiting` does not go out and come back with
// every config change. A wait that ended (the chain answered), or a park long
// after a release, begins a new one, and a release no park follows stops
// being reported once a park would no longer continue it.
//
// Mutation: date the wait by the probe the seat is on, end it on every
// release, or report only the seats parked now, and this fails.
func TestAWaitOutlivesAnApplysRelease(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	clock := began
	var mu sync.Mutex
	e := &Engine{clock: func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }}
	at := func(d time.Duration) { mu.Lock(); clock = began.Add(d); mu.Unlock() }
	before, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	after, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	seat := uuid.MustParse("6f1c2a3b-4d5e-5f60-8172-839405a6b7c8")
	old := providerBasis{models: before.Models, keys: []string{"plan"}}
	next := providerBasis{models: after.Models, keys: []string{"plan"}}
	since := func() time.Time {
		t.Helper()
		rows := e.parkedByEntry()["plan"]
		if len(rows) != 1 {
			t.Fatalf("parked = %+v, want the one seat", rows)
		}
		return rows[0].Since
	}
	retry := began.Add(time.Hour)
	e.joinLimitProbe(t.Context(), old, seat, "swe", retry)
	t.Cleanup(func() { e.leaveLimitProbe(seat, old); e.leaveLimitProbe(seat, next) })

	at(10 * time.Second)
	e.leaveLimitProbe(seat, old)
	e.parkReleased(seat)
	at(15 * time.Second)
	if got := since(); !got.Equal(began) {
		t.Errorf("a wait between a release and its re-park reads from %v, want %v", got, began)
	}
	at(20 * time.Second)
	e.joinLimitProbe(t.Context(), next, seat, "swe", retry)
	if got := since(); !got.Equal(began) {
		t.Errorf("the wait across an apply reads from %v, want %v", got, began)
	}

	at(time.Minute)
	e.leaveLimitProbe(seat, next)
	e.endParkWait(seat)
	at(2 * time.Minute)
	e.joinLimitProbe(t.Context(), next, seat, "swe", retry)
	if got := since(); !got.Equal(began.Add(2 * time.Minute)) {
		t.Errorf("a wait after the chain answered reads from %v, want a new one at %v", got, began.Add(2*time.Minute))
	}

	e.leaveLimitProbe(seat, next)
	e.parkReleased(seat)
	at(2*time.Minute + parkCarryWindow + time.Second)
	e.joinLimitProbe(t.Context(), next, seat, "swe", retry)
	if got := since(); !got.Equal(began.Add(2*time.Minute + parkCarryWindow + time.Second)) {
		t.Errorf("a park long after a release reads from %v, want a new wait", got)
	}

	e.leaveLimitProbe(seat, next)
	e.parkReleased(seat)
	at(3*time.Minute + 2*parkCarryWindow)
	if rows := e.parkedByEntry()["plan"]; len(rows) != 0 {
		t.Errorf("a seat released longer ago than a park would continue still reads as waiting: %+v", rows)
	}
}

// flakySecrets is a fleet whose secret listing fails while fail is set.
type flakySecrets struct {
	anyFleet
	fail atomic.Bool
}

func (f *flakySecrets) SecretValues(ctx context.Context) ([]coord.SecretRecord, error) {
	if f.fail.Load() {
		return nil, errors.New("store unavailable")
	}
	return f.anyFleet.SecretValues(ctx)
}

// A FAILED READ CARRIES THE LAST GOOD ONE. A blip in the secret store must not
// read as "no token expires": that cleared a standing expiry alarm and raised
// it again a tick later. The expiry read on the last good listing stands.
//
// Mutation: answer an unreadable store as an empty one, and the expiry is lost
// on the blip.
func TestAFailedSecretReadKeepsTheExpiry(t *testing.T) {
	t.Parallel()
	c, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus", subscription: true})
	_, cipher := testKeyring(t)
	fleet := &flakySecrets{anyFleet: coordmemory.NewFleet()}
	if err := fleetsecrets.New(fleet, cipher).Set(t.Context(), "CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-stored",
		secrets.Author{Name: "jane-doe", Kind: "human"}, secrets.SourceAPI, time.Now()); err != nil {
		t.Fatal(err)
	}
	e := epochEngine(c)
	e.backends, e.cipher = &Backends{Fleet: fleet}, cipher
	var first, second statelog.Reading
	e.modelsReading(t.Context(), time.Now(), &first)
	expiry := conditionOf(t, first.Models, "plan").ExpiresAt
	if expiry.IsZero() {
		t.Fatal("the premise: the stored token has an expiry")
	}
	fleet.fail.Store(true)
	e.modelsReading(t.Context(), time.Now(), &second)
	if got := conditionOf(t, second.Models, "plan").ExpiresAt; !got.Equal(expiry) {
		t.Errorf("after a failed read the expiry is %v, want the last good %v", got, expiry)
	}
}
