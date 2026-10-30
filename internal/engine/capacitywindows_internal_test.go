package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// judgeWindows is the alarm's half of one window read at now: what the watch
// judges open, evaluated through the table.
func judgeWindows(ctx context.Context, w *windowWatch, fleet coord.Fleet,
	now time.Time) (windowsRead, []statelog.Alarm) {

	read := readWindows(ctx, fleet)
	var reading statelog.Reading
	oldestWindow(now, w.observe(ctx, now, read), &reading)
	return read, statelog.Evaluate(reading)
}

// maintenanceDetail is the `maintenance_open` alarm's detail among alarms, and
// false where it isn't raised.
func maintenanceDetail(alarms []statelog.Alarm) (string, bool) {
	for _, a := range alarms {
		if a.Kind == statelog.KindMaintenanceOpen {
			return a.Detail, true
		}
	}
	return "", false
}

// A WINDOW THIS NODE CAN'T READ IS NEVER READ AS NO WINDOW.
//
// The report and the alarm skipped a log whose window they couldn't read, so a
// coordination blip cleared a `maintenance_open` that was firing, and a node
// that couldn't read the register said no window was open while its own boot
// refused to start because one might be. The read names the log as unread, and
// the alarm keeps what this node last read there: a window it last read open
// stays up, as old as it was then and saying when that was, and a log it never
// read a window on raises nothing.
//
// Mutation: drop the unread logs in [windowWatch.observe] and the blip clears
// the alarm; judge an unread log as open whatever was last read and the
// restarted node raises one; forget a window that closed and the last step
// raises one too.
func TestAnUnreadableWindowIsNeverReadAsNone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := coordmem.NewFleet()
	now := time.Now().UTC()
	op, _, err := fleet.OpenMaintenance(ctx, coord.MaintenanceOperation{
		Stream: worklet.Domain{}.Stream().Name, OperationID: "capacity:tracker",
		TargetMaxBytes: 1 << 33, Phase: coord.PhaseSealing, Attempt: 1,
		Participants: []string{"node-1"}, EnteredAt: now.Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	blind := &blindFleet{fleetStore: fleet}
	w := newWindowWatch()

	read, alarms := judgeWindows(ctx, w, blind, now)
	if len(read.open) != 1 || len(read.unread) != 0 {
		t.Fatalf("a readable register read %+v", read)
	}
	if detail, up := maintenanceDetail(alarms); !up ||
		!strings.Contains(detail, op.Stream) || strings.Contains(detail, "last read") {
		t.Fatalf("a window open two hours raised %q (up %v), want the alarm naming %s "+
			"as read now", detail, up, op.Stream)
	}

	// THE BLIP: the tracker's window can't be read.
	blind.readErr, blind.readErrOn = errors.New("store unreachable"), op.Stream
	read, _ = judgeWindows(ctx, w, blind, now.Add(30*time.Second))
	if len(read.open) != 0 || !slices.Equal(read.unread, []string{op.Stream}) {
		t.Fatalf("a register the read couldn't answer read %+v, want the tracker's "+
			"log named as unread and no window open", read)
	}
	_, alarms = judgeWindows(ctx, w, blind, now.Add(90*time.Second))
	detail, up := maintenanceDetail(alarms)
	if !up {
		t.Fatal("a blip cleared the alarm about a window this node last read open")
	}
	for _, says := range []string{"had been open for 2h ", "when this node last read it, " +
		"1m 30s ago", "can't read the window now"} {
		if !strings.Contains(detail, says) {
			t.Errorf("the alarm held through a blip says %q, want it to say %q", detail, says)
		}
	}

	// A NODE THAT NEVER READ THE WINDOW raises nothing about it.
	if _, alarms := judgeWindows(ctx, newWindowWatch(), blind, now); fired(alarms,
		statelog.KindMaintenanceOpen) {
		t.Error("a node that never read the window raised the alarm about it")
	}
	// AND NEITHER DOES A NODE WITH NO MEMORY AT ALL.
	if _, alarms := judgeWindows(ctx, nil, blind, now); fired(alarms,
		statelog.KindMaintenanceOpen) {
		t.Error("a watch that remembers nothing raised the alarm about an unread window")
	}

	// THE WINDOW CLOSES AND IS READ CLOSED: the alarm clears, and a blip
	// after that raises nothing, because the last read found no window.
	blind.readErr = nil
	if err := fleet.CloseMaintenance(ctx, op.Stream, op.OperationID, op.Revision); err != nil {
		t.Fatal(err)
	}
	if _, alarms := judgeWindows(ctx, w, blind, now.Add(2*time.Minute)); fired(alarms,
		statelog.KindMaintenanceOpen) {
		t.Fatal("a window read closed still raised the alarm")
	}
	blind.readErr = errors.New("store unreachable")
	if _, alarms := judgeWindows(ctx, w, blind, now.Add(3*time.Minute)); fired(alarms,
		statelog.KindMaintenanceOpen) {
		t.Error("a blip after the window was read closed raised the alarm again")
	}
}

// A BLIP NEVER RAISES THE ALARM. A window this node last read open is held at
// the age it had when that read answered, so one read open at 59 minutes stays
// under the hour however long the blip lasts, and one read past the hour keeps
// the alarm that read raised.
//
// Mutation: age a held window on the observing clock in [oldestWindow] and the
// 59-minute window raises the alarm during the blip.
func TestABlipNeverRaisesTheMaintenanceAlarm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()
	stream := worklet.Domain{}.Stream().Name
	for name, tc := range map[string]struct {
		openFor time.Duration
		up      bool
	}{
		"read at 59 minutes": {59 * time.Minute, false},
		"read at 61 minutes": {61 * time.Minute, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fleet := coordmem.NewFleet()
			if _, _, err := fleet.OpenMaintenance(ctx, coord.MaintenanceOperation{
				Stream: stream, OperationID: "capacity:tracker", Phase: coord.PhaseSealing,
				Attempt: 1, TargetMaxBytes: 1 << 33, Participants: []string{"node-1"},
				EnteredAt: now.Add(-tc.openFor),
			}); err != nil {
				t.Fatal(err)
			}
			blind := &blindFleet{fleetStore: fleet}
			w := newWindowWatch()
			if _, alarms := judgeWindows(ctx, w, blind, now); fired(alarms,
				statelog.KindMaintenanceOpen) != tc.up {
				t.Fatalf("a window read at %s raised the alarm: %v, want %v",
					tc.openFor, !tc.up, tc.up)
			}
			blind.readErr, blind.readErrOn = errors.New("store unreachable"), stream
			for _, later := range []time.Duration{time.Minute, 2 * time.Minute, time.Hour} {
				_, alarms := judgeWindows(ctx, w, blind, now.Add(later))
				if fired(alarms, statelog.KindMaintenanceOpen) != tc.up {
					t.Errorf("%s into a blip, a window last read at %s raised the "+
						"alarm: %v, want %v", later, tc.openFor, !tc.up, tc.up)
				}
			}
		})
	}
}

// THE NEWEST READ WINS, WHATEVER ORDER READS REACH THE WATCH IN. Every report
// feeds the watch, and the alarm heartbeat's and an API caller's are assembled
// at once, so a read taken before a window closed can reach the watch after one
// that found it closed.
//
// Mutation: apply reads in the order they are observed, and the older read puts
// the closed window back, so the next blip raises the alarm about it.
func TestAnOlderReadObservedLaterChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()
	fleet := coordmem.NewFleet()
	stream := worklet.Domain{}.Stream().Name
	op, _, err := fleet.OpenMaintenance(ctx, coord.MaintenanceOperation{
		Stream: stream, OperationID: "capacity:tracker", Phase: coord.PhaseSealing,
		Attempt: 1, TargetMaxBytes: 1 << 33, Participants: []string{"node-1"},
		EnteredAt: now.Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	blind := &blindFleet{fleetStore: fleet}
	early := readWindows(ctx, blind)
	if err := fleet.CloseMaintenance(ctx, stream, op.OperationID, op.Revision); err != nil {
		t.Fatal(err)
	}
	late := readWindows(ctx, blind)
	if len(early.open) != 1 || len(late.open) != 0 {
		t.Fatalf("the two reads found %d and %d windows, want 1 and 0",
			len(early.open), len(late.open))
	}
	w := newWindowWatch()
	w.observe(ctx, now, late)
	if judged := w.observe(ctx, now, early); len(judged) != 0 {
		t.Errorf("a read taken before the window closed, observed after the read "+
			"that found it closed, judged %d window(s) open", len(judged))
	}
	blind.readErr, blind.readErrOn = errors.New("store unreachable"), stream
	if _, alarms := judgeWindows(ctx, w, blind, now.Add(time.Minute)); fired(alarms,
		statelog.KindMaintenanceOpen) {
		t.Error("a blip raised the alarm about a window this node's newest read " +
			"found closed")
	}
}

// A READ THIS NODE'S OWN CONTEXT ENDED CHANGES NOTHING. A stop says nothing
// about coordination, so a held window is never marked unreadable by it.
//
// Mutation: take the unread logs whatever the context, and the window reads as
// one this node can't read.
func TestAStoppedReadChangesNothing(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	now := time.Now().UTC()
	stream := worklet.Domain{}.Stream().Name
	if _, _, err := fleet.OpenMaintenance(context.Background(), coord.MaintenanceOperation{
		Stream: stream, OperationID: "capacity:tracker", Phase: coord.PhaseSealing,
		Attempt: 1, TargetMaxBytes: 1 << 33, Participants: []string{"node-1"},
		EnteredAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	w := newWindowWatch()
	judgeWindows(context.Background(), w, fleet, now)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	blind := &blindFleet{fleetStore: fleet, readErr: context.Canceled, readErrOn: stream}
	judgeWindows(stopped, w, blind, now.Add(time.Minute))
	_, alarms := judgeWindows(stopped, w, blind, now.Add(5*time.Minute))
	detail, up := maintenanceDetail(alarms)
	if !up {
		t.Fatal("a stopped read cleared the alarm")
	}
	if strings.Contains(detail, "can't read the window now") {
		t.Errorf("the stopped reads marked the window unreadable: %q", detail)
	}
}

// WHY A WINDOW COULDN'T BE READ IS LOGGED, once when the reads start failing
// and once when they stop. The report and the alarm say they couldn't read it
// and the log is where the reason is, but only a gesture wrote it, so a node
// whose banner said "couldn't read" had nothing in its log saying why. Once per
// change, because the alarm heartbeat assembles a report every ten seconds.
//
// NOT PARALLEL: it swaps the process-wide logger.
//
// Mutation: drop either line, or write the first on every failing read.
func TestAnUnreadableWindowIsLoggedOnceWithItsReason(t *testing.T) {
	logs := &stopLineBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	count := func(msg string) (n int, errs []any) {
		for _, record := range logs.records(t) {
			if record["msg"] == msg {
				n++
				errs = append(errs, record["err"])
			}
		}
		return n, errs
	}
	ctx := context.Background()
	stream := worklet.Domain{}.Stream().Name
	blind := &blindFleet{fleetStore: coordmem.NewFleet(),
		readErr: errors.New("store unreachable"), readErrOn: stream}
	w := newWindowWatch()
	now := time.Now().UTC()
	for i := range 3 {
		judgeWindows(ctx, w, blind, now.Add(time.Duration(i)*10*time.Second))
	}
	if n, errs := count("capacity_window_unreadable"); n != 1 ||
		!strings.Contains(fmt.Sprint(errs[0]), "store unreachable") {
		t.Errorf("three failing reads wrote %d unreadable line(s) carrying %v, want "+
			"one carrying the reason", n, errs)
	}
	blind.readErr = nil
	judgeWindows(ctx, w, blind, now.Add(time.Minute))
	judgeWindows(ctx, w, blind, now.Add(2*time.Minute))
	if n, _ := count("capacity_window_readable"); n != 1 {
		t.Errorf("two reads after the blip wrote %d readable line(s), want one", n)
	}
}

// liveFleet is a running node's coordination store, under a name an embedding
// can take: embedded as coord.Fleet, its field would hide the store's own Fleet
// method.
type liveFleet = coord.Fleet

// windowsBlindTo is a fleet whose capacity windows are read from a register of
// the test's own, and fail on the logs it names; everything else is the
// running node's own coordination store.
type windowsBlindTo struct {
	liveFleet
	windows *coordmem.Fleet
	blind   map[string]bool
}

func (f *windowsBlindTo) Maintenance(ctx context.Context, stream string) (
	coord.MaintenanceOperation, bool, error) {

	if f.blind[stream] {
		return coord.MaintenanceOperation{}, false, errors.New("store unreachable")
	}
	return f.windows.Maintenance(ctx, stream)
}

// THE REPORT SAYS WHICH LOGS' WINDOWS IT COULDN'T READ, and its alarm and its
// banner are one read.
//
// `crewlet retention status` and the dashboard's banner render the report's
// `maintenance` alone, so a log left out of it read as a log with no window
// open. The report names every log it couldn't read in `maintenance_unread`,
// and the alarm it evaluates holds what this node last read rather than
// clearing.
//
// Mutation: leave `maintenance_unread` unset, or evaluate the alarm from the
// read alone, and this fails.
func TestTheReportSaysWhichLogsWindowsItCouldNotRead(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	ctx := t.Context()
	trackerLog, pagesLog := worklet.Domain{}.Stream().Name, doclet.Domain{}.Stream().Name
	windows := coordmem.NewFleet()
	if _, _, err := windows.OpenMaintenance(ctx, coord.MaintenanceOperation{
		Stream: trackerLog, OperationID: "capacity:tracker", Phase: coord.PhaseSealing,
		Attempt: 1, TargetMaxBytes: 1 << 33, Participants: []string{"node-a"},
		EnteredAt: time.Now().UTC().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	fleet := &windowsBlindTo{liveFleet: e.backends.Fleet, windows: windows,
		blind: map[string]bool{pagesLog: true}}
	r := &retention{fleet: fleet, state: e.core.Load().log, db: e.backends.Store,
		nodeID: "node-a", windows: newWindowWatch()}

	report := r.Report(ctx)
	if len(report.Maintenance) != 1 || report.Maintenance[0].Stream != trackerLog {
		t.Fatalf("the report's open windows are %+v, want the tracker's", report.Maintenance)
	}
	if !slices.Equal(report.MaintenanceUnread, []string{pagesLog}) {
		t.Errorf("the report names %v as unread, want the pages log", report.MaintenanceUnread)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"maintenance_unread":["`+pagesLog+`"]`) {
		t.Errorf("the report encodes no maintenance_unread: %s", encoded)
	}
	if !fired(report.Alarms, statelog.KindMaintenanceOpen) {
		t.Fatal("a window open two hours raised no alarm on the report")
	}

	fleet.blind[trackerLog] = true
	report = r.Report(ctx)
	if len(report.Maintenance) != 0 {
		t.Errorf("a window nobody could read is listed as open: %+v", report.Maintenance)
	}
	if !slices.Contains(report.MaintenanceUnread, trackerLog) {
		t.Errorf("the report names %v as unread, want the tracker's log among them",
			report.MaintenanceUnread)
	}
	detail, up := maintenanceDetail(report.Alarms)
	if !up || !strings.Contains(detail, "when this node last read it") {
		t.Errorf("the report's alarm through the blip is %q (up %v), want it held "+
			"on the last read and saying so", detail, up)
	}
}

// WHAT HOLDS A WINDOW THE REPORT COULDN'T READ IS SAID AS THAT, never as a
// list. The acknowledgements and the admissions are read beside each open
// window, and a failed read of either was taken as an empty register: every
// participant named as owing a restart, or nobody's admission holding the
// window, both printed by the CLI and the dashboard as facts.
//
// Mutation: answer the whole participant set, or nobody, for an unreadable
// register, or leave either flag unset.
func TestAWindowsUnreadPartsAreSaidNotListed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmem.NewFleet()
	stream := worklet.Domain{}.Stream().Name
	if _, _, err := fleet.OpenMaintenance(ctx, coord.MaintenanceOperation{
		Stream: stream, OperationID: "capacity:tracker", Phase: coord.PhaseOpened,
		Attempt: 1, TargetMaxBytes: 1 << 33,
		Participants: []string{"node-1", "node-2", "node-3"},
		EnteredAt:    time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	blind := &blindFleet{fleetStore: fleet}
	r := &retention{fleet: blind, windows: newWindowWatch()}

	row := onlyWindow(t, r.openMaintenance(ctx, readWindows(ctx, blind)))
	if row.AcksUnread || row.AdmissionsUnread || len(row.ParticipantsMissing) != 3 {
		t.Fatalf("a readable window reads %+v, want every participant missing and "+
			"nothing unread", row)
	}

	blind.acksErr = errors.New("store unreachable")
	row = onlyWindow(t, r.openMaintenance(ctx, readWindows(ctx, blind)))
	if !row.AcksUnread || row.ParticipantsMissing != nil {
		t.Errorf("unreadable acknowledgements read as %v missing (unread %v), want "+
			"them said unread and no list", row.ParticipantsMissing, row.AcksUnread)
	}
	blind.admissionsErr = errors.New("store unreachable")
	row = onlyWindow(t, r.openMaintenance(ctx, readWindows(ctx, blind)))
	if !row.AdmissionsUnread || row.AdmissionsBlocking != nil {
		t.Errorf("unreadable admissions read as %v holding (unread %v), want them "+
			"said unread and no list", row.AdmissionsBlocking, row.AdmissionsUnread)
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"acks_unread":true`, `"admissions_unread":true`} {
		if !strings.Contains(string(encoded), field) {
			t.Errorf("the row encodes no %s: %s", field, encoded)
		}
	}
}
