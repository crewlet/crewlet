package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/issuereport"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/store"
)

// reportingEngine is an engine literal whose reporter sends to a counting relay.
func reportingEngine(t *testing.T) (*Engine, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"status":"filed","issue":"https://github.com/crewlet/crewlet/issues/1"}`)
	}))
	t.Cleanup(relay.Close)
	e := &Engine{issueReports: issuereport.New(issuereport.Config{Store: memory.NewFleet(),
		Client: relay.Client(), URL: relay.URL, Origin: issuereport.Origin{Build: issuereport.Build{Version: "v1.2.3",
			Commit: issuereport.CommitHash(strings.Repeat("a", 40)), Go: "go1.27.2", OS: "linux", Arch: "amd64"}}})}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.issueReports.Run(ctx)
	return e, &requests
}

func companyReporting(enabled bool) *Company {
	cfg := &config.Company{}
	cfg.Diagnostics.IssueReports.Enabled = enabled
	return &Company{Config: cfg}
}

func soon(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// THE REVISION IS THE SWITCH, on every apply: a default company reports nothing,
// a revision turns it on, and the revision after it, or no company, turns it off.
func TestARevisionTurnsIssueReportsOnAndOff(t *testing.T) {
	t.Parallel()
	e, requests := reportingEngine(t)
	breach := events.New(types.TurnGuardBreach{Kind: types.GuardUnhandledException,
		PanicFunction: "github.com/crewlet/crewlet/internal/engine.first", PanicFile: "first.go"}, events.TraceContext{})

	e.applyIssueReports(companyReporting(false))
	e.issueReports.OnPublish(t.Context(), "t", breach)
	time.Sleep(20 * time.Millisecond)
	if got := requests.Load(); got != 0 {
		t.Fatalf("a default company sent %d", got)
	}

	e.applyIssueReports(companyReporting(true))
	e.issueReports.OnPublish(t.Context(), "t", breach)
	soon(t, "the report", func() bool { return requests.Load() == 1 })

	for name, c := range map[string]*Company{"a revision off": companyReporting(false), "no company": nil} {
		e.applyIssueReports(companyReporting(true))
		e.applyIssueReports(c)
		e.issueReports.OnPublish(t.Context(), "t", events.New(types.TurnGuardBreach{
			Kind:          types.GuardUnhandledException,
			PanicFunction: "github.com/crewlet/crewlet/internal/engine." + name[:1], PanicFile: "some.go",
		}, events.TraceContext{}))
		time.Sleep(20 * time.Millisecond)
		if got := requests.Load(); got != 1 {
			t.Fatalf("after %s, %d sent", name, got)
		}
	}
}

// ONLY THE ENGINE'S OWN CAUSE IS A DEFECT TO REPORT: a host that cannot be
// reached, a spent plan, a refused key and an outage of the engine's own
// environment dead-letter mail too, and no maintainer can mend them. A message
// whose deliveries all went on hand-backs has no outside cause to name. A lost
// message is reported whether or not its error carried a stamp.
func TestADeadLetterIsReportedOnlyForTheEnginesOwnCause(t *testing.T) {
	t.Parallel()
	unreachable := sandbox.Because(sandbox.WhyProviderUnavailable, errors.New("the box host did not answer"))
	for name, c := range map[string]struct {
		result queue.Result
		want   int32
	}{
		"an unattributed failure": {queue.Nak(errors.New("a nil map somewhere")), 1},
		"a stamped failure":       {queue.Nak(fmt.Errorf("engine: turn: %w", stampedFailure(errors.New("x")))), 1},
		"a busy store":            {queue.Nak(fmt.Errorf("engine: turn: %w", store.ErrBusy)), 0},
		"a full disk": {queue.Nak(fmt.Errorf("engine: turn: %w",
			&os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC})), 0},
		"hand-backs":          {queue.Defer(queue.DeferSeatMoved, "the seat moved"), 1},
		"an unreachable host": {queue.Nak(fmt.Errorf("engine: turn: %w", unreachable)), 0},
		"a spent plan":        {queue.Nak(&llm.Error{Kind: llm.KindRateLimit, Err: errors.New("limit")}), 0},
		"a refused key":       {queue.Nak(&llm.Error{Kind: llm.KindAuth, Err: errors.New("401")}), 0},
		"a provider outage":   {queue.Nak(&llm.Error{Kind: llm.KindServer, Err: errors.New("overloaded")}), 0},
		"a budget":            {queue.Nak(fmt.Errorf("x: %w", toolloop.ErrBudgetExhausted)), 0},
		"a cancelled context": {queue.Nak(context.Canceled), 0},
		"a deadline":          {queue.Nak(context.DeadlineExceeded), 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, requests := reportingEngine(t)
			e.applyIssueReports(companyReporting(true))
			e.reportDeadLetter(t.Context(), "a-seat", c.result)
			if c.want > 0 {
				soon(t, "the report", func() bool { return requests.Load() == c.want })
				return
			}
			time.Sleep(20 * time.Millisecond)
			if got := requests.Load(); got != 0 {
				t.Errorf("%d sent for an outside cause", got)
			}
		})
	}
}

// A DELIVERY'S HEADROOM DECIDES WHETHER ITS HAND-BACK RETIRES A MESSAGE, on what
// the queue states of each message, a spent sibling and a deferral included.
func TestAHandBackRetiresOnlyASpentMessage(t *testing.T) {
	t.Parallel()
	spent, healthy := uuid.New(), uuid.New()
	failing := queue.Nak(errors.New("failed"))
	ev := func(id uuid.UUID) *events.Event { return &events.Event{ID: id} }
	stated := queue.WithHeadroom(context.Background(), []queue.Headroom{{ID: spent, Left: 0}, {ID: healthy, Left: 7}})
	for name, c := range map[string]struct {
		result queue.Result
		evs    []*events.Event
		ctx    context.Context
		want   bool
	}{
		"a spent message handed back":   {failing, []*events.Event{ev(spent)}, stated, true},
		"a spent sibling":               {failing, []*events.Event{ev(healthy), ev(spent)}, stated, true},
		"a deferral of a spent message": {queue.Defer(queue.DeferSeatMoved, "moved"), []*events.Event{ev(spent)}, stated, true},
		"headroom in hand":              {failing, []*events.Event{ev(healthy)}, stated, false},
		"acknowledged":                  {queue.Ack(), []*events.Event{ev(spent)}, stated, false},
		"no headroom stated":            {failing, []*events.Event{ev(spent)}, context.Background(), false},
		"no message":                    {failing, []*events.Event{nil}, stated, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := deadLetters(c.ctx, c.result, c.evs); got != c.want {
				t.Errorf("deadLetters = %v, want %v", got, c.want)
			}
		})
	}
}

// reportTopology names the placement a company's cell resolves to, and a company
// with no sandbox as none.
func TestAReportsTopologyIsTheClosedSets(t *testing.T) {
	t.Parallel()
	got := issuereport.NewTopology(3, string(config.StreamEmbedded), string(config.CoordinationEmbeddedKV), string(config.PlacementE2B))
	want := issuereport.Topology{Nodes: "2-3", Stream: "embedded", Coordination: "embedded-kv", Sandbox: "e2b"}
	if got != want {
		t.Errorf("topology = %+v, want %+v", got, want)
	}
}

// A NODE WITHOUT `data` SENDS WHAT IT DETECTS. It builds a reporter and listens to
// its own publishes like any other, and runs no core, so a sender tied to the core
// left it counting and queueing for nobody.
func TestANodeThatBuildsAReporterRunsItsSender(t *testing.T) {
	t.Parallel()
	boot := config.DefaultBootstrap()
	boot.Node.Roles = []string{"seats"}
	if HoldsEstate(&boot) {
		t.Fatal("the case wants a node without data")
	}
	queue := queuememory.NewBroker().Client()
	e := &Engine{}
	e.startIssueReports(&boot, &Backends{Queue: queue, Fleet: memory.NewFleet()})
	if e.issueReports == nil {
		t.Fatal("a node with a broker and a fleet store built no reporter")
	}
	e.startIssueSender(t.Context())
	soon(t, "the sender to run", e.issueReports.Sending)
	e.stopIssueSender()
	if e.issueReports.Sending() {
		t.Error("the sender outlived its stop")
	}
}

// A NODE WITH NO REPORTER STARTS NOTHING, and stopping what never started is safe:
// a failed boot's teardown comes through here.
func TestNoReporterNoSender(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.startIssueSender(t.Context())
	e.stopIssueSender()
	if e.endIssueSender != nil {
		t.Error("a node with no reporter started a sender")
	}
}

// A RESUME THAT KEPT FAILING IS REPORTED ONLY ON THE ENGINE'S OWN KIND, so a run's
// announcement must carry the classifier's word: unwired, it reads as no kind at all
// and that report is never sent, with nothing to say so.
func TestARunsAnnouncementCarriesTheClassifiersWord(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("sandbox.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`FailureKind:\s+runner\.ErrorKind,`).Match(source) {
		t.Error("the coordinator is built without the classifier")
	}
}

// THE FLEET MOVES SEATS WHEN A NODE DRAINS OR BUILDS DIFFER, as the reporter's
// shadow reads it off every node's beat: one build and no drain is settled; a
// draining node, a second build, or a node that does not say its build (an
// older one, or one whose beat carries no status) is moving. Every node's
// alarms are read, whatever it says of its build.
//
// Mutation: drop the build count and the two-build row reads settled; read an
// unsaid build as matching and the silent rows do; skip a draining beat and
// its row does.
func TestTheReportedFleetMovesOnADrainOrASecondBuild(t *testing.T) {
	t.Parallel()
	beat := func(status coord.NodeStatus) map[string]any {
		return map[string]any{"roles": []string{"seats"}, coord.StatusKey: status.Meta()}
	}
	alarmed := coord.NodeStatus{Build: "v1.2.3",
		Alarms:          []coord.AlarmMark{{Kind: "volume_low", Since: time.Now().UTC().Truncate(time.Second)}},
		AlarmsEvaluated: true}
	for name, tc := range map[string]struct {
		beats  map[string]map[string]any
		moving bool
	}{
		"one build":  {map[string]map[string]any{"a": beat(alarmed), "b": beat(coord.NodeStatus{Build: "v1.2.3"})}, false},
		"two builds": {map[string]map[string]any{"a": beat(alarmed), "b": beat(coord.NodeStatus{Build: "v1.2.4"})}, true},
		"a drain":    {map[string]map[string]any{"a": beat(alarmed), "b": beat(coord.NodeStatus{Build: "v1.2.3", Draining: true})}, true},
		"no build": {map[string]map[string]any{"a": beat(coord.NodeStatus{Build: "v1.2.3"}),
			"b": beat(coord.NodeStatus{Alarms: alarmed.Alarms, AlarmsEvaluated: true})}, true},
		"no status": {map[string]map[string]any{"a": beat(alarmed), "b": {"roles": []string{"seats"}}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			backend := memory.New()
			for id, meta := range tc.beats {
				if _, _, err := backend.TryAcquire(t.Context(), coord.NodeResource(id), coord.AcquireOptions{
					Owner: id + ":1", TTL: time.Hour, Meta: meta}); err != nil {
					t.Fatal(err)
				}
			}
			fleet, err := (&Engine{backends: &Backends{Coord: backend}}).reportFleet(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if fleet.Moving != tc.moving || !slices.Contains(fleet.Alarms, "volume_low") {
				t.Errorf("fleet = %+v, want moving %v with volume_low standing", fleet, tc.moving)
			}
		})
	}
}
