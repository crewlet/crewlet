package googlecloud_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// lane is the one lane a Google Cloud seat has.
const lane = seataccess.LaneGoogleCloud

// withMonitoring binds Google's Monitoring server for every seat, so the
// seat's tool reads as a server's does rather than as no server at all.
func (w *syncWorld) withMonitoring() *syncWorld {
	w.company.MCPServers = []config.MCPServer{{Name: "google-cloud-monitoring", Transport: config.TransportHTTP,
		URL: "https://monitoring.googleapis.com/mcp", Shared: org.Off()}}
	return w
}

// tool is the seat's tool as the Agents tab reads it now, and as it reads
// with no settling at all.
func (w *syncWorld) tool() (seataccess.ToolState, string, seataccess.ToolState) {
	w.t.Helper()
	lr := w.record().Lanes[lane]
	want, _ := seataccess.DesiredOf(&w.company.Roles[0], lane)
	state, reason := seataccess.ToolOf(w.company, "sre", lane, want, lr.Observed, lr.Settling, w.clock.now())
	return state, reason, seataccess.ToolStateOf(w.company, "sre", lane, want, lr.Observed)
}

// wantWaiting fails the test unless the seat's tool waits on reason, the lane
// recording when it ends no later than window from now.
func (w *syncWorld) wantWaiting(reason string, window time.Duration) {
	w.t.Helper()
	if state, said, _ := w.tool(); state != seataccess.ToolWaiting || said != reason {
		w.t.Fatalf("the tool reads %s %q, want waiting on %q", state, said, reason)
	}
	s := w.record().Lanes[lane].Settling
	if left := s.Until.Sub(w.clock.now()); left <= 0 || left > window {
		w.t.Errorf("the lane waits on %q for %s more, want at most %s", s.Reason, left, window)
	}
}

// wantSettled fails the test unless the seat's tool reads as it would with
// nothing settling.
func (w *syncWorld) wantSettled(label string) {
	w.t.Helper()
	if state, said, plain := w.tool(); state != plain || said != "" {
		w.t.Fatalf("%s: the tool reads %s %q, want %s with no reason", label, state, said, plain)
	}
}

// THE TOOL WAITS WHILE GOOGLE APPLIES CREWLET'S WRITE, AND THE SYNC DOES NOT:
// a seat turned on ends OK with no finding, and its tool reads waiting on the
// grant for up to ten minutes, which its first token waits on; dropping a
// project reads waiting on the change for the read-back bound. Each ends by
// itself, and the next pass clears it from the ledger.
func TestTheToolWaitsWhileGoogleAppliesAWrite(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb, projectData).withMonitoring()
	noFindings(t, "turning on", w.sync())
	if got := w.record().Lanes[lane].Sync; got.Outcome != seataccess.OutcomeOK {
		t.Fatalf("turning on ended %s, want OK: Google's delay is nobody's step", got.Outcome)
	}
	w.wantWaiting(googlecloud.ReasonGranting, googlecloud.GrantSettle)

	// A QUIET PASS INSIDE THE WINDOW keeps it: the ledger holds it fleet-wide.
	w.clock.advance(time.Minute)
	noFindings(t, "a quiet pass", w.sync())
	w.wantWaiting(googlecloud.ReasonGranting, googlecloud.GrantSettle)

	w.clock.advance(googlecloud.GrantSettle)
	w.wantSettled("ten minutes after the grant")
	noFindings(t, "the pass after", w.sync())
	if s := w.record().Lanes[lane].Settling; s != nil {
		t.Fatalf("the ledger still holds %+v once it ended", s)
	}

	w.seat().Projects = []string{projectWeb}
	noFindings(t, "dropping a project", w.sync())
	w.wantRoles(projectData, "")
	w.wantWaiting(googlecloud.ReasonApplying, googlecloud.ObserveBudget)
	w.clock.advance(googlecloud.ObserveBudget)
	w.wantSettled("past the read-back bound")
}

// A NEW ACCOUNT IS GOOGLE STILL CREATING IT, for the read-back bound after it
// is made: the create step says so, and a later step that writes a grant
// replaces it with the grant's wait.
func TestANewAccountIsGoogleStillCreatingIt(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	session, err := w.vendor.Open(t.Context(), w.open())
	if err != nil {
		t.Fatal(err)
	}
	want := seataccess.Desired{Enabled: true, Level: config.GoogleCloudLevelViewer, Scope: []string{projectWeb}}
	res, err := session.Apply(t.Context(), seataccess.ApplyInput{
		Seat: seataccess.SeatRef{Handle: "sre"}, Lane: lane,
		Step: seataccess.Step{Op: seataccess.OpCreate, Index: 0, Verb: seataccess.VerbCreate}, Want: want,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := res.Settling; s == nil || s.Reason != googlecloud.ReasonCreating ||
		s.Until.Sub(w.clock.now()) > googlecloud.ObserveBudget || !s.Until.After(w.clock.now()) {
		t.Fatalf("the create step answers %+v, want Google still creating it for the read-back bound", s)
	}
	res, err = session.Apply(t.Context(), seataccess.ApplyInput{
		Seat: seataccess.SeatRef{Handle: "sre"}, Lane: lane,
		Step: seataccess.Step{Op: seataccess.OpCreate, Index: 1, Verb: seataccess.VerbGrant}, Want: want,
		Prior: []seataccess.StepResult{res},
	})
	if err != nil || res.Settling == nil || res.Settling.Reason != googlecloud.ReasonGranting {
		t.Fatalf("the token grant answers %+v, %v; want Google still applying the grant", res.Settling, err)
	}
}

// THE RECORDED SETTLING IS THE MARK OF THE LAST WRITE: after a quiet pass has
// replaced the sync that wrote, a stale read inside the bound is still read
// again rather than judged drift.
//
// Mutation: drop the settling from lastWrite, and the stale read is drift.
func TestAStaleReadAfterAQuietPassIsStillInsideTheBound(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb, projectData)
	noFindings(t, "turning on", w.sync())
	w.settle()
	w.seat().Projects = []string{projectWeb}
	noFindings(t, "dropping a project", w.sync())
	w.clock.advance(10 * time.Second)
	noFindings(t, "a quiet pass", w.sync())
	if steps := w.record().Lanes[lane].Sync.Steps; len(steps) != 0 {
		t.Fatalf("the quiet pass took %+v", steps)
	}

	// THE POLICY AS IT STOOD BEFORE THE DROP, which a stale read answers.
	w.f.edit(func(f *fakeGoogle) {
		past := clonePolicy(f.projects[projectData].Policy)
		roles, _ := googlecloud.LevelRoles(config.GoogleCloudLevelViewer, projectData)
		googlecloud.Grant(&past, "serviceAccount:"+w.email(), roles)
		f.policyPast[projectData], f.policyLag[projectData] = past, 2
	})
	w.clock.advance(10 * time.Second)
	writes := len(w.f.wrote())
	noFindings(t, "a stale read inside the bound", w.sync())
	if len(w.drift) != 0 || len(w.f.wrote()) != writes {
		t.Fatalf("a stale read after a quiet pass was drift %+v, writes %q", w.drift, w.f.wrote()[writes:])
	}
}
