package statelog_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// reportAt is the instant every case is assembled at: fixed, because the
// report compares against instants and a moving one makes a boundary flake.
var reportAt = time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)

// replayWindow is the shipped `min_age`, which the trim-blocked cases hold a
// block against.
const replayWindow = 7 * 24 * time.Hour

// healthyDomain is a domain where nothing is wrong: every term readable, the
// age floor binding, plenty of headroom.
func healthyDomain(name string, feed bool) statelog.DomainInputs {
	in := statelog.TrimInputs{
		Now:             reportAt,
		Counted:         []statelog.NodePosition{{NodeID: "node-1", Seq: 900, At: reportAt}},
		CountedReadable: true,
		HoldsReadable:   true,
		BackupFloor:     880,
		BackupAt:        reportAt.Add(-time.Hour),
		HasBackupFloor:  true,
		BackupMaxAge:    24 * time.Hour,
		FeedAckFloor:    890,
		HasFeed:         feed,
		FeedReadable:    feed,
		AgeFloor:        800,
		HoldStale:       statelog.TrimHoldStale,
	}
	return statelog.DomainInputs{
		Domain:         name,
		Stream:         "CREWLET_" + strings.ToUpper(name),
		Replay:         statelog.ReplayStrict,
		FirstSeq:       700,
		LastSeq:        900,
		Bytes:          64 << 20,
		MaxBytes:       16 << 30,
		StreamReadable: true,
		TrimFloor:      700,
		Decision:       statelog.Trim(in.Terms()),
		FloorState:     statelog.TrimFloorPublished,
		// THE BACKUP THE TRIM ABOVE COUNTS, an hour old, so a case that
		// sets a backup policy is quiet on it.
		BackupAt: in.BackupAt,
	}
}

// term finds one term's row.
func term(t *testing.T, d statelog.DomainReport, name statelog.TermName) statelog.TermReport {
	t.Helper()
	for _, r := range d.Terms {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("domain %s has no %s term at all", d.Domain, name)
	return statelog.TermReport{}
}

// A TERM A DOMAIN DOES NOT HAVE IS `n/a`, NEVER ZERO AND NEVER UNKNOWN.
//
// A domain with no wake feed has none: `0` claims the feed scanned nothing,
// and unreadable puts a block on a healthy fleet's screen.
func TestATermADomainDoesNotHaveIsAbsentRatherThanUnknown(t *testing.T) {
	t.Parallel()
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Domains:          []statelog.DomainInputs{healthyDomain("vectors", false)},
	})
	got := term(t, rep.Domains[0], statelog.TermFeedAckFloor)
	if got.State != statelog.TermAbsent {
		t.Errorf("a domain with no declared wake feed reports %q; a domain that has no "+
			"feed at all is n/a, and both of the other two states put a fault "+
			"on the screen", got.State)
	}
	if rep.Domains[0].BlockedBy != "" {
		t.Errorf("an absent term blocked the trim (blocked_by %q)",
			rep.Domains[0].BlockedBy)
	}
}

// EVERY TERM CARRIES A REMEDY, INCLUDING THE ONE THAT MEANS NOTHING IS WRONG.
//
// Five of the six name something to fix; the sixth binds on a healthy fleet,
// and a blank there reads as an incomplete surface, not a fine fleet.
func TestEveryTermCarriesARemedyIncludingTheHealthyOne(t *testing.T) {
	t.Parallel()
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Domains:          []statelog.DomainInputs{healthyDomain("tracker", true)},
	})
	for _, name := range statelog.TermNames {
		if r := term(t, rep.Domains[0], name); strings.TrimSpace(r.Remedy) == "" {
			t.Errorf("term %s carries no remedy: an operator watching a term "+
				"approach is exactly who this surface is for", name)
		}
	}
	age := term(t, rep.Domains[0], statelog.TermAgeFloor)
	if !strings.Contains(age.Remedy, "Nothing is wrong") {
		t.Errorf("the age floor's remedy is %q; it is the term that binds when "+
			"the fleet is healthy and it has to say so", age.Remedy)
	}
}

// THE BACKUP REMEDY NAMES THE OWNER, OR SAYS THERE IS NONE.
//
// `retention.backup_owner` reaches an operator only here; a remedy that never
// named it would make the field decorative.
func TestTheBackupRemedyNamesTheOwnerOrSaysThereIsNone(t *testing.T) {
	t.Parallel()
	named := statelog.NewReport(statelog.ReportInputs{
		NodeID:      "node-1",
		At:          reportAt,
		BackupOwner: "platform-oncall",
		Domains:     []statelog.DomainInputs{healthyDomain("tracker", true)},
	})
	if got := term(t, named.Domains[0], statelog.TermBackupFloor).Remedy; !strings.Contains(got, "platform-oncall") {
		t.Errorf("the backup remedy is %q and does not name the configured "+
			"owner: this is the one surface that value reaches", got)
	}
	anon := statelog.NewReport(statelog.ReportInputs{
		NodeID:  "node-1",
		At:      reportAt,
		Domains: []statelog.DomainInputs{healthyDomain("tracker", true)},
	})
	if got := term(t, anon.Domains[0], statelog.TermBackupFloor).Remedy; !strings.Contains(got, "retention.backup_owner") {
		t.Errorf("with no owner configured the remedy is %q; it has to name the "+
			"field, because the next reader's real problem is that nobody owns "+
			"the backup", got)
	}
}

// AN UNREADABLE CEILING IS NOT ZERO HEADROOM.
//
// The headroom alarm is the one an operator cannot ignore; derived from an
// unread ceiling it pages a whole fleet for a briefly slow broker.
func TestAnUnreadableCeilingIsNotZeroHeadroom(t *testing.T) {
	t.Parallel()
	d := healthyDomain("tracker", true)
	d.StreamReadable, d.MaxBytes, d.Bytes = false, 0, 0
	rep := statelog.NewReport(statelog.ReportInputs{NodeID: "node-1", At: reportAt, Domains: []statelog.DomainInputs{d}})
	if rep.Domains[0].HeadroomFraction != nil {
		t.Errorf("an unreadable stream reported a headroom fraction of %v: a "+
			"fraction of an unknown ceiling is not a number",
			*rep.Domains[0].HeadroomFraction)
	}
	for _, a := range rep.Alarms {
		if a.Kind == statelog.KindLogHeadroom {
			t.Errorf("an unreadable stream fired %s: %s", a.Kind, a.Detail)
		}
	}
}

// THE HEADROOM ALARM NAMES ITS DOMAIN.
//
// Two logs fill at different rates; "10% left" alone sends an operator to both.
func TestTheHeadroomAlarmNamesItsDomain(t *testing.T) {
	t.Parallel()
	full := healthyDomain("vectors", false)
	full.Bytes, full.MaxBytes = 97, 100
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:  "node-1",
		At:      reportAt,
		Domains: []statelog.DomainInputs{healthyDomain("tracker", true), full},
	})
	var fired []statelog.Alarm
	for _, a := range rep.Alarms {
		if a.Kind == statelog.KindLogHeadroom {
			fired = append(fired, a)
		}
	}
	if len(fired) != 1 {
		t.Fatalf("one domain is at 3%% headroom and %d headroom alarm(s) fired", len(fired))
	}
	if d := fired[0].Domains; len(d) != 1 || !slices.Equal(d[0].Domains, []string{"vectors"}) {
		t.Errorf("the headroom alarm names %+v, not the log that is filling", d)
	}
	if !strings.Contains(fired[0].Detail, "3%") {
		t.Errorf("the headroom alarm reads %q, which is not the filling log's measurement",
			fired[0].Detail)
	}
}

// A LOG TOO SMALL FOR ITS OWN REPLAY WINDOW IS NAMED, and only that log.
//
// Each domain has its own ceiling and rate, so the comparison is per domain,
// as headroom is: "a ceiling is too small" would send an operator to resize
// every log.
func TestTheCeilingShortAlarmNamesItsDomain(t *testing.T) {
	t.Parallel()
	busy := healthyDomain("iam", true)
	// A GIBIBYTE CEILING taking in 200 MiB a day holds five days of a
	// seven-day window.
	busy.MaxBytes, busy.BytesPerDay = 1<<30, statelog.PerDay(200<<20)
	quiet := healthyDomain("tracker", true)
	quiet.BytesPerDay = statelog.PerDay(10 << 20)

	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-1", At: reportAt, ReplayWindow: 7 * 24 * time.Hour,
		Domains: []statelog.DomainInputs{quiet, busy},
	})
	var fired []statelog.Alarm
	for _, a := range rep.Alarms {
		if a.Kind == statelog.KindLogCeilingShort {
			fired = append(fired, a)
		}
	}
	if len(fired) != 1 {
		t.Fatalf("one log is short of its window and %d alarm(s) fired: %v",
			len(fired), rep.Alarms)
	}
	if d := fired[0].Domains; len(d) != 1 || !slices.Equal(d[0].Domains, []string{"iam"}) {
		t.Fatalf("the alarm stands on %+v, not the log that is short", d)
	}
	if fired[0].Detail != fired[0].Domains[0].Detail ||
		!strings.Contains(fired[0].Detail, "200.0 MiB a day") {
		t.Errorf("the alarm reads %q, which is not the short log's measurement",
			fired[0].Detail)
	}

	// AND THE WINDOW IS WHAT IT IS COMPARED AGAINST: under a three-day
	// min_age the same log fits; a report ignoring the window fires on
	// every log or none.
	rep = statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-1", At: reportAt, ReplayWindow: 3 * 24 * time.Hour,
		Domains: []statelog.DomainInputs{quiet, busy},
	})
	for _, a := range rep.Alarms {
		if a.Kind == statelog.KindLogCeilingShort {
			t.Errorf("a window the ceiling holds fired: %s", a.Detail)
		}
	}
}

// A MEASURED ZERO GOES ON THE WIRE AS ZERO, AND AN UNMEASURED RATE AS
// NOTHING.
//
// Opposite facts (a log that took in nothing, a node that cannot say), and
// every surface renders from the JSON: collapsed, a dashboard shows an idle
// log on every node that has not ticked yet.
func TestAMeasuredZeroRateSurvivesTheWireAndAnUnmeasuredOneIsAbsent(t *testing.T) {
	t.Parallel()
	idle := healthyDomain("pages", true)
	idle.BytesPerDay = statelog.PerDay(0)
	unmeasured := healthyDomain("iam", true)

	raw, err := json.Marshal(statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-1", At: reportAt,
		Domains: []statelog.DomainInputs{idle, unmeasured},
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc struct {
		Domains []map[string]any `json:"domains"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, present := doc.Domains[0]["bytes_per_day"]; !present || got != float64(0) {
		t.Errorf("a measured zero went out as %v (present %v), want 0", got, present)
	}
	if got, present := doc.Domains[1]["bytes_per_day"]; present {
		t.Errorf("an unmeasured rate went out as %v, want the field absent", got)
	}
}

// THE BACKUP-AGE ALARM AGES EACH LOG'S OWN BACKUP, AND NAMES THE LOG.
//
// Under `backup_floor: operator` each log is acknowledged on its own, so each
// has its own backup age. One age per node (the newest point's) reads green
// beside logs whose acknowledgement is days old or missing, their trims
// blocked and unsaid.
func TestTheBackupAlarmAgesEachLogsOwnBackup(t *testing.T) {
	t.Parallel()
	fresh := healthyDomain("tracker", true)
	fresh.BackupAt = reportAt.Add(-time.Hour)
	stale := healthyDomain("pages", true)
	stale.BackupAt = reportAt.Add(-30 * time.Hour)
	uncovered := healthyDomain("usage", false)
	uncovered.BackupAt = time.Time{}
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:       "node-1",
		At:           reportAt,
		BackupMaxAge: 24 * time.Hour,
		Domains:      []statelog.DomainInputs{fresh, stale, uncovered},
	})
	fired := map[string]string{}
	for _, a := range rep.Alarms {
		if a.Kind != statelog.KindBackupAge {
			continue
		}
		if len(a.Domains) == 0 {
			t.Fatalf("the backup alarm reads %q and names no log", a.Detail)
		}
		for _, d := range a.Domains {
			for _, domain := range d.Domains {
				fired[domain] = d.Detail
			}
		}
		// THE LOG NOTHING COVERS LEADS, ahead of one whose copy is merely
		// old: it is the one whose trim cannot advance at all.
		if !strings.Contains(a.Detail, "no verified backup") {
			t.Errorf("the backup alarm leads with %q, want usage's missing backup", a.Detail)
		}
	}
	if _, wrong := fired["tracker"]; wrong {
		t.Errorf("the tracker's own backup is an hour old and it fired: %q", fired["tracker"])
	}
	if got := fired["pages"]; !strings.Contains(got, "30h old") {
		t.Errorf("pages fired %q, want its own backup's 30h age — not the "+
			"tracker's hour", got)
	}
	if got := fired["usage"]; !strings.Contains(got, "no verified backup") {
		t.Errorf("usage fired %q, want the absence of a backup covering it", got)
	}
	// AND IT LEADS IN EITHER ORDER: first in the report, it is not displaced
	// by the log whose copy is merely old.
	reversed := statelog.NewReport(statelog.ReportInputs{
		NodeID:       "node-1",
		At:           reportAt,
		BackupMaxAge: 24 * time.Hour,
		Domains:      []statelog.DomainInputs{uncovered, stale, fresh},
	})
	for _, a := range reversed.Alarms {
		if a.Kind == statelog.KindBackupAge &&
			!strings.Contains(a.Detail, "no verified backup") {
			t.Errorf("with the logs reversed the backup alarm leads with %q, want "+
				"usage's missing backup", a.Detail)
		}
	}

	// AND THE INSTANT IS ON THE ROW, absent where nothing covers the log —
	// the one place the report says which acknowledgement each log waits on.
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Domains []map[string]any `json:"domains"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	for _, d := range wire.Domains {
		_, has := d["backup_at"]
		if want := d["domain"] != "usage"; has != want {
			t.Errorf("%v carries backup_at = %v, want present %v", d["domain"], has, want)
		}
	}

	// AND A DEPLOYMENT WITH NO POLICY IS NOT ALARMED AT, per log either.
	quiet := statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-1", At: reportAt,
		Domains: []statelog.DomainInputs{stale, uncovered},
	})
	for _, a := range quiet.Alarms {
		if a.Kind == statelog.KindBackupAge {
			t.Errorf("no backup policy, and %s fired: %s", a.Kind, a.Detail)
		}
	}
}

// A RESERVED LOG'S HEADROOM IS OF THE CEILING ITS ORDINARY WRITES MEET.
//
// On a log keeping a gate reserve, ordinary writes are refused at the ceiling
// less the reserve, so that is "full" for the headroom alarm: against the
// broker's ceiling it fires a sixteenth late. A log keeping none is measured
// against its whole ceiling; the reserve is reported beside the ceiling.
func TestAReservedLogsHeadroomIsOfItsOrdinaryCeiling(t *testing.T) {
	t.Parallel()
	// 12% of the broker's ceiling is left, 6% of the 1500 MiB ordinary
	// one: here the sixteenth is the reserve's larger term (the fleet term
	// for a 4 KiB largest append is about a mebibyte).
	reserved := healthyDomain("tracker", true)
	reserved.Bytes, reserved.MaxBytes = 1408<<20, 1600<<20
	reserved.Reservation = statelog.Reservation{Kept: true, MaxAppend: 4 << 10}
	plain := healthyDomain("vectors", false)
	plain.Bytes, plain.MaxBytes = 1408<<20, 1600<<20
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-1", At: reportAt,
		Domains: []statelog.DomainInputs{reserved, plain},
	})
	byName := map[string]statelog.DomainReport{}
	for _, d := range rep.Domains {
		byName[d.Domain] = d
	}
	for name, want := range map[string]struct {
		headroom float64
		reserve  uint64
	}{
		"tracker": {headroom: 92.0 / 1500, reserve: 100 << 20},
		"vectors": {headroom: 192.0 / 1600, reserve: 0},
	} {
		d := byName[name]
		if d.HeadroomFraction == nil || *d.HeadroomFraction != want.headroom {
			t.Errorf("%s reports a headroom of %v, want %v", name,
				d.HeadroomFraction, want.headroom)
		}
		if d.ReserveBytes != want.reserve {
			t.Errorf("%s reports a reserve of %d bytes, want %d", name,
				d.ReserveBytes, want.reserve)
		}
	}
	var fired []string
	for _, a := range rep.Alarms {
		if a.Kind == statelog.KindLogHeadroom {
			for _, d := range a.Domains {
				fired = append(fired, d.Domains...)
			}
		}
	}
	if len(fired) != 1 || fired[0] != "tracker" {
		t.Fatalf("the headroom alarm stands on %q, want tracker alone: the one "+
			"within a tenth of the ceiling its writes are refused at", fired)
	}
}

// THE REPORT'S COUNTED SET IS THE GATE'S OWN, TO THE INSTANT.
//
// An eviction takes effect one fence window after the gesture. A screen
// calling it effective before the trim does shows a node dropped from the set
// the gate still waits on, and the operator walks away.
func TestAnEvictionIsEffectiveExactlyWhenTheGateSaysSo(t *testing.T) {
	t.Parallel()
	evicted := reportAt.Add(-statelog.EvictionFenceWindow)
	for _, tc := range []struct {
		name      string
		at        time.Time
		effective bool
		counted   bool
	}{
		{"at the window's edge", reportAt, false, true},
		{"one instant past it", reportAt.Add(time.Nanosecond), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tomb := statelog.Tombstone{NodeID: "node-9", At: evicted, By: "ops-3"}
			d := healthyDomain("tracker", true)
			d.Tombstones = []statelog.Tombstone{tomb}
			rep := statelog.NewReport(statelog.ReportInputs{
				NodeID:           "node-1",
				At:               tc.at,
				RegisterReadable: true,
				Domains:          []statelog.DomainInputs{d},
				Register: []coord.NodePositions{{NodeID: "node-9", At: evicted,
					Domains: map[string]coord.DomainPosition{"tracker": {Seq: 3}}}},
				Tombstones: []statelog.Tombstone{tomb},
			})
			row := rep.Nodes[0]
			if row.Evicted == nil {
				t.Fatal("an evicted node's row carries no tombstone")
			}
			if row.Evicted.Effective != tc.effective {
				t.Errorf("effective=%v, want %v", row.Evicted.Effective, tc.effective)
			}
			if row.Counted != tc.counted {
				t.Errorf("counted=%v, want %v — the report and the trim gate "+
					"must name the same fleet at the same instant",
					row.Counted, tc.counted)
			}
		})
	}
}

// A LIVE NODE WITH NO POSITION YET IS COUNTED, AND VISIBLE.
//
// A live data node whose row has not landed (its heartbeat's write refused)
// blocks every term derived from the counted set; left out, the block has no
// visible cause.
func TestALiveNodeWithNoPositionIsCountedAndRendered(t *testing.T) {
	t.Parallel()
	d := healthyDomain("tracker", true)
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Live:             []statelog.Presence{{NodeID: "node-7"}},
		Domains:          []statelog.DomainInputs{d},
	})
	if len(rep.Nodes) != 1 || rep.Nodes[0].NodeID != "node-7" {
		t.Fatalf("a live node that has never reported is missing from the node "+
			"block: %+v", rep.Nodes)
	}
	row := rep.Nodes[0]
	if !row.Counted || !row.Live {
		t.Errorf("node-7 counted=%v live=%v, want both", row.Counted, row.Live)
	}
	if !row.At.IsZero() || len(row.Domains) != 0 {
		t.Errorf("a node that has never reported shows a position (%v, %d domains); "+
			"that is a node at zero rather than a node with nothing to say",
			row.At, len(row.Domains))
	}
}

// THE COUNTED MARK IS THE TRIM'S SET, LOG BY LOG.
//
// A node is counted where some log's counted set holds it, each over that
// log's own rows, tombstones and the live data nodes: one evicted past the
// window on the only log its row names is counted nowhere; one evicted on one
// log and reporting another is counted on the other. One set over every row
// would mark the first counted while no log's trim waits for it.
func TestTheCountedMarkIsTheTrimsSetLogByLog(t *testing.T) {
	t.Parallel()
	tracker := healthyDomain("tracker", true)
	tracker.Tombstones = []statelog.Tombstone{
		{NodeID: "evicted", At: reportAt.Add(-time.Hour)},
		{NodeID: "half", At: reportAt.Add(-time.Hour)},
	}
	pages := healthyDomain("pages", true)
	row := func(node string, positions map[string]coord.DomainPosition) coord.NodePositions {
		return coord.NodePositions{NodeID: node, At: reportAt, Domains: positions}
	}
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Domains:          []statelog.DomainInputs{tracker, pages},
		Register: []coord.NodePositions{
			row("server", map[string]coord.DomainPosition{"tracker": {Seq: 9}, "pages": {Seq: 9}}),
			row("evicted", map[string]coord.DomainPosition{"tracker": {Seq: 2}}),
			row("half", map[string]coord.DomainPosition{"tracker": {Seq: 2}, "pages": {Seq: 2}}),
		},
	})
	marks := map[string]bool{}
	for _, n := range rep.Nodes {
		marks[n.NodeID] = n.Counted
	}
	want := map[string]bool{"server": true, "evicted": false, "half": true}
	if !maps.Equal(marks, want) {
		t.Errorf("the node block marks counted %v, want %v", marks, want)
	}
}

// LAG IS UNKNOWN, NOT ZERO, WHEN THE STREAM COULD NOT BE READ.
//
// The same pointer rule Health uses: an unknown lag rendered as zero is every
// node reported caught up during exactly the outage that stopped the reads.
func TestLagIsUnknownWhenTheStreamCouldNotBeRead(t *testing.T) {
	t.Parallel()
	d := healthyDomain("tracker", true)
	d.StreamReadable = false
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Domains:          []statelog.DomainInputs{d},
		Register: []coord.NodePositions{{
			NodeID:  "node-2",
			At:      reportAt,
			Domains: map[string]coord.DomainPosition{"tracker": {Seq: 10, AppliedThrough: 10}},
		}},
	})
	if got := rep.Nodes[0].Domains["tracker"].Lag; got != nil {
		t.Errorf("lag reported as %d against a stream nobody could read", *got)
	}
}

// A NODE WITH NO ARTEFACT KEEPS ITS ROW, AND ITS REASON.
//
// "Why can this node not donate" is the question a failed join raises. A row
// dropped for holding nothing renders as a node that was never asked.
func TestASnapshotlessNodeKeepsItsRowAndItsReason(t *testing.T) {
	t.Parallel()
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Register: []coord.NodePositions{{
			NodeID:       "node-4",
			At:           reportAt,
			SnapshotSkip: string(statelog.SkipLagging),
			Domains:      map[string]coord.DomainPosition{"tracker": {Seq: 5}},
		}},
	})
	if len(rep.Snapshots) != 1 {
		t.Fatalf("a node holding no artefact has no snapshot row: %+v", rep.Snapshots)
	}
	row := rep.Snapshots[0]
	if len(row.Domains) != 0 {
		t.Errorf("a node holding no artefact reports positions %v", row.Domains)
	}
	if row.Skip != statelog.SkipLagging {
		t.Errorf("skip reason %q, want %q — the reason is the whole answer",
			row.Skip, statelog.SkipLagging)
	}
}

// THE EXIT CODE IS THE ALARMS, AND NOTHING ELSE.
//
// A second rule in the CLI would be a second definition of "is something
// wrong", running where it cannot see the register.
func TestTheExitCodeIsTheAlarms(t *testing.T) {
	t.Parallel()
	healthy := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Domains:          []statelog.DomainInputs{healthyDomain("tracker", true)},
	})
	if len(healthy.Alarms) != 0 {
		t.Fatalf("a healthy fleet raised %d alarm(s): %+v", len(healthy.Alarms), healthy.Alarms)
	}
	if healthy.ExitNonZero() {
		t.Error("a healthy fleet exits non-zero")
	}
	if got := healthy.Blocked(); len(got) != 0 {
		t.Errorf("a healthy fleet reports %v blocked", got)
	}

	// BLOCKED PAST THE WINDOW, with a hundred records older than it: the
	// trim has kept what a working one would have removed, which is the
	// condition `trim_blocked` fires on.
	blocked := healthyDomain("tracker", true)
	in := statelog.TrimInputs{Now: reportAt, HoldsReadable: true,
		BackupMaxAge: 24 * time.Hour, AgeFloor: 800}
	blocked.Decision = statelog.Trim(in.Terms())
	blocked.BlockedSince = reportAt.Add(-(replayWindow + statelog.TrimInterval + time.Hour))
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		ReplayWindow:     replayWindow,
		Domains:          []statelog.DomainInputs{blocked},
	})
	if !rep.ExitNonZero() {
		t.Error("a fleet whose trim has been blocked past its replay window exits zero")
	}
	if got := rep.Blocked(); len(got) != 1 || got[0] != "tracker" {
		t.Errorf("Blocked() is %v, want [tracker]", got)
	}
	if !strings.HasPrefix(rep.Domains[0].Prose, "Nothing is being trimmed on tracker:") {
		t.Errorf("the blocked domain's prose is %q; it is the answer to the only "+
			"question this command is run for and it leads", rep.Domains[0].Prose)
	}
	if rep.Domains[0].BlockedSince.IsZero() {
		t.Error("a blocked domain carries no instant, so its alarm reports a " +
			"duration measured from the zero time")
	}
}

// WHAT A BLOCKED TRIM IS KEEPING PAST THE WINDOW IS READ OFF THE LOG, not off
// the clock.
//
// The report has both halves of the count: the age term's sequence (the
// first record young enough to keep) and the log's first. A block old by the
// clock stays silent while they say nothing is past the window: a young
// fleet, an unwritten log, an unreadable stream (no bounds). A count, when
// there is one, is what the alarm prints.
func TestABlockedTrimsAlarmReadsWhatTheLogHoldsPastTheWindow(t *testing.T) {
	t.Parallel()
	old := reportAt.Add(-(replayWindow + statelog.TrimInterval + time.Hour))
	blockedAt := func(ageFloor uint64, mutate func(*statelog.DomainInputs)) statelog.Report {
		d := healthyDomain("tracker", true)
		// THE BACKUP TERM BLOCKS, as it does on every fresh deployment.
		in := statelog.TrimInputs{
			Now: reportAt, HoldsReadable: true, CountedReadable: true,
			Counted:      []statelog.NodePosition{{NodeID: "node-1", Seq: 900, At: reportAt}},
			BackupMaxAge: 24 * time.Hour, HasFeed: true, FeedReadable: true,
			FeedAckFloor: 890, AgeFloor: ageFloor, HoldStale: statelog.TrimHoldStale,
		}
		d.Decision = statelog.Trim(in.Terms())
		d.BlockedSince = old
		if mutate != nil {
			mutate(&d)
		}
		return statelog.NewReport(statelog.ReportInputs{
			NodeID: "node-1", At: reportAt, RegisterReadable: true,
			ReplayWindow: replayWindow, Domains: []statelog.DomainInputs{d},
		})
	}

	// THE CONTROL: the age term keeps from 800, the log starts at 700, so a
	// hundred records are past the window — and the alarm says so.
	fired, found := find(blockedAt(800, nil).Alarms, statelog.KindTrimBlocked)
	if !found {
		t.Fatal("a trim blocked past its window with a hundred records past it " +
			"raised no trim_blocked, so every silence below proves nothing")
	}
	if !strings.Contains(fired.Detail, "keeping up to 100 record(s)") {
		t.Errorf("the alarm reads %q; it says what the log is keeping", fired.Detail)
	}
	// THE LOG AND THE TERM HOLDING IT ride on the log the alarm stands on.
	if d := fired.Domains; len(d) != 1 || !slices.Equal(d[0].Domains, []string{"tracker"}) ||
		d[0].Term != statelog.TermBackupFloor || !d[0].Since.Equal(old) {
		t.Errorf("the alarm stands on %+v; want the tracker's log, held by the "+
			"backup term since %v", d, old)
	}

	for name, rep := range map[string]statelog.Report{
		// Everything the log holds is younger than the window: the age
		// term keeps from the log's first record.
		"a young log": blockedAt(700, nil),
		// Nothing written at all.
		"an empty log": blockedAt(1, func(d *statelog.DomainInputs) {
			d.FirstSeq, d.LastSeq = 0, 0
		}),
		// A stream this node could not read carries no bounds.
		"an unreadable stream": blockedAt(800, func(d *statelog.DomainInputs) {
			d.StreamReadable, d.FirstSeq, d.LastSeq = false, 0, 0
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if a, found := find(rep.Alarms, statelog.KindTrimBlocked); found {
				t.Errorf("raised %q while the log holds nothing past the window", a.Detail)
			}
			// AND IT IS STILL REPORTED AS BLOCKED: the alarm is quiet,
			// the screen is not.
			if got := rep.Blocked(); len(got) != 1 {
				t.Errorf("Blocked() = %v; a quiet alarm must not hide the block", got)
			}
		})
	}
}

// A HELD RECORD AND AN UNREADABLE FLOOR ARE EACH LOG'S OWN MEASUREMENT, and
// each says what is true of that log.
//
// Each log's copy is judged on its own (ADR-0079), so past the deferral grace
// a record held on one log takes this node's copy of THAT log out of service
// and no other's; reduced to the node's oldest held record, the alarm would
// name one log while another's copy had gone too. A floor likewise: a read
// refuses on its own log's floor, and two logs re-anchored are two re-anchors.
func TestAHeldRecordAndAnUnreadableFloorAreEachLogsOwnAlarm(t *testing.T) {
	t.Parallel()
	identity := healthyDomain("iam", true)
	identity.DeferredAge = 2 * time.Hour
	identity.DeferredRecord = "at CREWLET_IAM@1:88, written at record version 2 against the 1 this build reads"
	work := healthyDomain("tracker", true)
	work.DeferredAge = 40 * time.Minute
	work.DeferredRecord = "at CREWLET_TRACKER@1:640, written at record version 3 against the 2 this build reads"
	// AND ONE LOG'S FLOOR UNREADABLE, the other's fine.
	work.FloorUnknownFor = 3 * time.Minute
	work.FloorUnknownCause = "the published floor for tracker is at generation 2 and this node is on 1"
	pages := healthyDomain("pages", true)

	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-1", At: reportAt, RegisterReadable: true,
		ReplayWindow: replayWindow,
		Domains:      []statelog.DomainInputs{identity, work, pages},
	})
	// ONE ALARM PER KIND, listing each log with that log's own measurement,
	// so the tracker raises and clears a line pair per log while the count
	// and the gauge say one.
	byLog := func(kind statelog.Kind) (statelog.Alarm, map[string]string) {
		var alarm statelog.Alarm
		out := map[string]string{}
		for _, a := range rep.Alarms {
			if a.Kind != kind {
				continue
			}
			if len(alarm.Kind) > 0 {
				t.Errorf("%s was raised twice: %v", kind, rep.Alarms)
			}
			alarm = a
			for _, d := range a.Domains {
				for _, domain := range d.Domains {
					out[domain] = d.Detail
				}
			}
		}
		return alarm, out
	}

	deferred, held := byLog(statelog.KindDeferredOld)
	if len(held) != 2 {
		t.Fatalf("deferred_old was raised for %v; want each of the two logs "+
			"holding a record past the grace, on its own", held)
	}
	for log, want := range map[string][]string{
		"tracker": {"CREWLET_TRACKER@1:640", "held for 40m,",
			"at which this node's copy of this log goes out of service"},
		"iam": {"CREWLET_IAM@1:88", "held for 2h,",
			"at which this node's copy of this log goes out of service"},
	} {
		for _, says := range want {
			if !strings.Contains(held[log], says) {
				t.Errorf("the %s log's deferred_old reads %q; want it to say %q",
					log, held[log], says)
			}
		}
	}

	// THE ALARM LEADS WITH THE RECORD HELD LONGEST, which is the identity
	// log's two hours.
	if deferred.Detail != held["iam"] {
		t.Errorf("deferred_old leads with %q; want the identity log's, held longest",
			deferred.Detail)
	}

	_, floors := byLog(statelog.KindFloorUnknown)
	if len(floors) != 1 || !strings.Contains(floors["tracker"], "generation 2") {
		t.Errorf("floor_unknown was raised for %v; want the tracker's log alone, "+
			"naming its floor", floors)
	}
}

// THE ALARMS THE REPORT RAISES PER LOG ARE EXACTLY THE ONES THE TABLE FLAGS,
// in both directions.
//
// The flag is what the published reference names them from and what the
// tracker writes a line pair per log for. One log sits at the firing value of
// every per-log condition beside a node reading firing its own: every alarm
// listing a log must be flagged, every flagged kind must list it, and nothing
// node-wide may list a log.
func TestTheAlarmsRaisedPerLogAreTheOnesTheTableFlags(t *testing.T) {
	t.Parallel()
	d := healthyDomain("tracker", true)
	// NEARLY FULL, and taking in a gibibyte a day against a gibibyte ceiling.
	d.MaxBytes, d.Bytes = 1<<30, 1<<30-1
	d.BytesPerDay = statelog.PerDay(1 << 30)
	// BLOCKED ON A COUNTED SET NOBODY COULD READ, past the window with a
	// hundred records past it, and covered by no backup. The applied term
	// comes first so the hold is trim_blocked's own: one on the backup term
	// folds under backup_age, and trim_blocked would list nothing.
	in := statelog.TrimInputs{
		Now: reportAt, HoldsReadable: true,
		BackupMaxAge: 24 * time.Hour, HasFeed: true, FeedReadable: true,
		FeedAckFloor: 890, AgeFloor: 800, HoldStale: statelog.TrimHoldStale,
	}
	d.Decision = statelog.Trim(in.Terms())
	d.BlockedSince = reportAt.Add(-(replayWindow + statelog.TrimInterval + time.Hour))
	d.BackupAt = time.Time{}
	// HOLDING A RECORD PAST THE GRACE, and unable to read its floor.
	d.DeferredAge = statelog.DeferralGrace + time.Minute
	d.FloorUnknownFor = statelog.FloorCacheStale + time.Second
	// ITS WAKE FEED HOLDING A DEAD LETTER.
	d.FeedDeadLetters = []statelog.FeedDeadLetter{{Age: 2 * time.Hour, ExpiresIn: 29 * 24 * time.Hour}}
	// AND READ LINEARIZABLY PAST ITS CENSUS, by more than the drift factor.
	d.LinearizableReadsExpected = 1_000
	d.LinearizableReads = int(statelog.CensusDriftFactor*1_000) + 1

	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-1", At: reportAt, RegisterReadable: true,
		ReplayWindow: replayWindow,
		BackupMaxAge: 24 * time.Hour,
		Reading:      statelog.Reading{ApplyLag: 2 * time.Minute},
		Domains:      []statelog.DomainInputs{d},
	})
	flagged := map[statelog.Kind]bool{}
	for _, kind := range statelog.PerLogKinds() {
		flagged[kind] = true
	}
	stamped := map[statelog.Kind]bool{}
	for _, a := range rep.Alarms {
		switch {
		case len(a.Domains) == 0 && flagged[a.Kind]:
			t.Errorf("%s is flagged per log and was raised with no log: %q", a.Kind, a.Detail)
		case len(a.Domains) > 0 && !flagged[a.Kind]:
			t.Errorf("%s was raised for the logs %+v and is not flagged per log, so "+
				"the reference says nothing of it", a.Kind, a.Domains)
		case len(a.Domains) == 0:
			// A node-wide alarm, unflagged and listing no log.
		case len(a.Domains) == 1 && slices.Equal(a.Domains[0].Domains, []string{"tracker"}):
			stamped[a.Kind] = true
		default:
			t.Errorf("%s lists %+v; want the one log this case put at its firing value",
				a.Kind, a.Domains)
		}
	}
	for kind := range flagged {
		if !stamped[kind] {
			t.Errorf("%s is flagged per log and the report never raised it for a log "+
				"whose inputs are all at the firing value", kind)
		}
	}
	if !alarmedKind(rep.Alarms, statelog.KindApplyLag) {
		t.Error("the node's own alarm was not raised, so this case proves nothing " +
			"about what a node-wide alarm carries")
	}
}

func alarmedKind(alarms []statelog.Alarm, kind statelog.Kind) bool {
	for _, a := range alarms {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

// A BLOCKED DOMAIN WITH NO KNOWN INSTANT STILL CARRIES ONE.
//
// The first tick to see a block has nothing to date it from. The zero time
// would render as blocked since year one and alarm on a fleet that has been
// blocked for a millisecond.
func TestABlockedDomainWithNoInstantDatesItselfFromThisTick(t *testing.T) {
	t.Parallel()
	d := healthyDomain("tracker", true)
	d.Decision = statelog.Trim(statelog.TrimInputs{Now: reportAt, HoldsReadable: true}.Terms())
	rep := statelog.NewReport(statelog.ReportInputs{NodeID: "node-1", At: reportAt, Domains: []statelog.DomainInputs{d}})
	if got := rep.Domains[0].BlockedSince; !got.Equal(reportAt) {
		t.Errorf("blocked_since is %v, want this tick (%v)", got, reportAt)
	}
	for _, a := range rep.Alarms {
		if a.Kind == statelog.KindTrimBlocked && strings.Contains(a.Detail, "years") {
			t.Errorf("the trim-blocked alarm reads %q", a.Detail)
		}
	}
}

// AN UNREADABLE REGISTER IS NOT AN EMPTY FLEET.
//
// The two produce the same empty node block, and they happen in opposite
// conditions: one cannot happen at all, the other happens during exactly the
// outage somebody is running this command in.
func TestAnUnreadableRegisterIsNotAnEmptyFleet(t *testing.T) {
	t.Parallel()
	rep := statelog.NewReport(statelog.ReportInputs{NodeID: "node-1", At: reportAt, RegisterReadable: false})
	if rep.RegisterReadable {
		t.Fatal("the report claims the register was readable when it was not")
	}
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(body), `"register_readable":false`) {
		t.Errorf("the flag does not survive encoding, so the one surface that "+
			"renders it cannot see it: %s", body)
	}
}

// THE WHOLE DOCUMENT IS ONE WIRE CONTRACT, so every field in it is spelled
// the same way.
//
// `GET /worklet/retention` and `crewlet retention status --json` hand this
// document to scripts and the dashboard, which address fields by name. Go's
// decoder matches names case-INSENSITIVELY, so a Go reader misses an untagged
// `Kind` beside `node_id` that every case-sensitive reader (the dashboard,
// `jq`) reads as absent rather than as an error.
//
// A REFLECTION WALK, not a golden document: a golden one is edited to match
// whatever the code emits the day a field is added. This fails on the field.
func TestEveryFieldOfTheRetentionDocumentIsSnakeCase(t *testing.T) {
	t.Parallel()

	snake := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	seen := map[reflect.Type]bool{}

	var walk func(t reflect.Type, path string)
	walk = func(rt reflect.Type, path string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := range rt.NumField() {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			switch {
			case name == "-":
				continue
			case name == "":
				t.Errorf("%s.%s carries no json tag, so it goes on the wire as %q "+
					"while its siblings are snake_case", path, f.Name, f.Name)
			case !snake.MatchString(name):
				t.Errorf("%s.%s is tagged %q, which is not snake_case", path, f.Name, name)
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(statelog.Report{}), "Report")

	// The counterfactual: the walk reaches the nested types rather than
	// stopping at Report's own fields. Without this a tagless field on
	// any child would pass.
	for _, want := range []any{statelog.Alarm{}, statelog.DomainReport{}, statelog.TermReport{}, statelog.NodeReport{},
		statelog.NodeDomainReport{}, statelog.EvictionReport{}, statelog.SnapshotReport{}, statelog.ReplicaReport{},
		statelog.DomainRefusal{}} {
		if !seen[reflect.TypeOf(want)] {
			t.Errorf("the walk never reached %T, so its fields are unchecked", want)
		}
	}
}

// THE REPORT NAMES THE LEVEL IT WAS SERVED AT, and it is DERIVED from whether
// this node could measure its own distance from the log.
//
// This answer must keep working during the outage it describes, so it never
// takes a barrier (the barrier's health is its subject). But `stale` claims an
// AGE, which a document assembled while the lag could not be read cannot.
// Weakening one step and NAMING it keeps the answer inside the read-level
// contract; one that silently dropped its level would be the silent downgrade.
func TestTheReportNamesTheLevelItCouldAnswerAt(t *testing.T) {
	t.Parallel()
	here := func(d statelog.DomainInputs) statelog.ReportInputs {
		return statelog.ReportInputs{
			NodeID:           "node-1",
			At:               reportAt,
			RegisterReadable: true,
			Domains:          []statelog.DomainInputs{d},
			Register: []coord.NodePositions{{
				NodeID:  "node-1",
				At:      reportAt,
				Domains: map[string]coord.DomainPosition{"tracker": {Seq: 890, AppliedThrough: 890}},
			}},
		}
	}
	measured := statelog.NewReport(here(healthyDomain("tracker", true)))
	if measured.ReadLevel != statelog.ReadStale {
		t.Errorf("a node that measured its own lag served %q, want stale — "+
			"the lag is on the answer, so the age is a claim it can make",
			measured.ReadLevel)
	}

	blind := healthyDomain("tracker", true)
	blind.StreamReadable = false
	unmeasured := statelog.NewReport(here(blind))
	if unmeasured.ReadLevel != statelog.ReadConsistentPrefix {
		t.Errorf("a node that could not read the stream served %q — with no "+
			"lag there is no age to claim, and `stale` claims one",
			unmeasured.ReadLevel)
	}

	// AND A NODE WITH NO ROW OF ITS OWN IS NOT A MEASURED ONE: that is the
	// coordination outage this document is opened for, and "no domains
	// reported" read as "nothing unknown" claims an age with no evidence.
	absent := here(healthyDomain("tracker", true))
	absent.Register = nil
	if got := statelog.NewReport(absent).ReadLevel; got != statelog.ReadConsistentPrefix {
		t.Errorf("a node absent from its own register served %q — an empty "+
			"node block is not a measurement", got)
	}

	// NOR IS A NODE THAT MEASURED SOME OF ITS DOMAINS: its `stale` claim
	// would be true of half its rows.
	partial := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Domains: []statelog.DomainInputs{
			healthyDomain("tracker", true), healthyDomain("pages", true),
		},
		Register: []coord.NodePositions{{
			NodeID:  "node-1",
			At:      reportAt,
			Domains: map[string]coord.DomainPosition{"tracker": {Seq: 890, AppliedThrough: 890}},
		}},
	})
	if partial.ReadLevel != statelog.ReadConsistentPrefix {
		t.Errorf("a node that measured one domain of two served %q — the age "+
			"claim would be true of half the document", partial.ReadLevel)
	}
}

// A TERM THAT BINDS NOTHING SAYS SO, RATHER THAN PRINTING 2^64-1.
//
// [statelog.SeqUnbounded] is the identity for the trim's minimum across the
// terms, chosen to lose a comparison, never to describe a position; and a JSON
// float64 cannot even hold 2^64-1, so a reader could not recover it.
//
// Both halves are asserted: the STATE the screen switches on, and the number
// on the wire, since the state alone would let the sentinel ride along in a
// field the type documents as meaningless.
func TestATermThatBindsNothingReportsUnboundedRatherThanTheSentinel(t *testing.T) {
	t.Parallel()
	in := statelog.TrimInputs{
		Now:             reportAt,
		Counted:         []statelog.NodePosition{{NodeID: "node-1", Seq: 900, At: reportAt}},
		CountedReadable: true,
		// NO HOLDS AND ONE NODE: the first makes min_hold unbounded, the
		// second makes the snapshot floor unbounded. They are the only
		// two terms that can be, and both are ordinary healthy states.
		HoldsReadable:  true,
		BackupFloor:    880,
		BackupAt:       reportAt.Add(-time.Hour),
		HasBackupFloor: true,
		BackupMaxAge:   24 * time.Hour,
		FeedAckFloor:   890,
		HasFeed:        true,
		FeedReadable:   true,
		AgeFloor:       800,
		HoldStale:      statelog.TrimHoldStale,
	}
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Domains: []statelog.DomainInputs{{
			Domain:         "tracker",
			Stream:         "CREWLET_TRACKER",
			Replay:         statelog.ReplayStrict,
			FirstSeq:       700,
			LastSeq:        900,
			StreamReadable: true,
			TrimFloor:      700,
			Decision:       statelog.Trim(in.Terms()),
		}},
	})
	for _, name := range []statelog.TermName{statelog.TermMinHold, statelog.TermSnapshotFloor} {
		got := term(t, rep.Domains[0], name)
		if got.State != statelog.TermUnbounded {
			t.Errorf("%s binds nothing and reports state %q; a term permitting "+
				"everything is not an ordinary sequence, and reporting it as one "+
				"is what put 2^64-1 on the screen", name, got.State)
		}
		if got.Seq != 0 {
			t.Errorf("%s carries seq %d; a sequence goes on the wire only where "+
				"it is one, and %d is the trim's own identity for a minimum",
				name, got.Seq, statelog.SeqUnbounded)
		}
	}

	// AND THE TRIM ITSELF IS UNAFFECTED: the sentinel loses the minimum, so
	// an unpinned fleet trims to the lowest term that IS a position.
	if got := rep.Domains[0].BlockedBy; got != "" {
		t.Errorf("a healthy unpinned fleet reports blocked_by %q", got)
	}
	if got := rep.Domains[0].TrimTo; got != 800 {
		t.Errorf("trim_to is %d, want the age floor at 800: an unbounded term "+
			"must lose the minimum rather than win it", got)
	}
}

// AND A TERM PERMITTING NOTHING YET IS DISTINGUISHABLE FROM ONE WITH NO
// SEQUENCE AT ALL.
//
// A term permitting removal up to 0 is a young fleet's binding term, the one
// an operator opens this screen to read; with `seq` omitted at zero it is
// identical on the wire to an unreadable term, whose absent seq is not zero.
func TestASequenceOfZeroSurvivesTheWire(t *testing.T) {
	t.Parallel()
	in := statelog.TrimInputs{
		Now: reportAt,
		// A NODE THAT HAS APPLIED NOTHING: `applied` is 0, readable, and
		// binding — a fleet on its first minute.
		Counted:         []statelog.NodePosition{{NodeID: "node-1", Seq: 0, At: reportAt}},
		CountedReadable: true,
		HoldsReadable:   true,
		BackupFloor:     880,
		BackupAt:        reportAt.Add(-time.Hour),
		HasBackupFloor:  true,
		BackupMaxAge:    24 * time.Hour,
		FeedAckFloor:    890,
		HasFeed:         true,
		FeedReadable:    true,
		AgeFloor:        800,
		HoldStale:       statelog.TrimHoldStale,
	}
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Domains: []statelog.DomainInputs{{
			Domain:         "tracker",
			Stream:         "CREWLET_TRACKER",
			Replay:         statelog.ReplayStrict,
			FirstSeq:       700,
			LastSeq:        900,
			StreamReadable: true,
			TrimFloor:      700,
			Decision:       statelog.Trim(in.Terms()),
		}},
	})
	got := term(t, rep.Domains[0], statelog.TermApplied)
	if got.State != statelog.TermKnown {
		t.Fatalf("a node at position 0 reports state %q; it was read, so it is ok "+
			"and its sequence is the answer", got.State)
	}
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(blob), `"seq":0`) {
		t.Errorf("a term permitting nothing yet serialises as %s, with no `seq` "+
			"in it: absent and zero are different answers and the reader cannot "+
			"tell them apart", blob)
	}
}

// A DOMAIN THE TRIM HAS CONCLUDED NOTHING ABOUT SAYS SO, AND ITS TERMS ARE AN
// EMPTY LIST.
//
// After every reanchor the floor row is the old generation's, so the report
// fills nothing from it until the trim's first tick on the adopted stream.
// `"terms": null` crashes the Fleet screen and the domain page, where an
// operator watches a reanchor recover, and no blocking term reads "advancing".
func TestADomainWithNoConclusionSaysSoAndCarriesNoNullTerms(t *testing.T) {
	t.Parallel()
	d := healthyDomain("pages", false)
	d.TrimFloor, d.Decision = 0, statelog.TrimDecision{}
	d.FloorState = statelog.TrimFloorNoneAtGeneration
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-1", At: reportAt, RegisterReadable: true,
		Domains: []statelog.DomainInputs{d},
	})
	row := rep.Domains[0]
	if row.TrimFloorState != statelog.TrimFloorNoneAtGeneration {
		t.Errorf("trim_floor_state = %q, want none_at_generation", row.TrimFloorState)
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"terms":[]`, `"trim_floor_state":"none_at_generation"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("the row encodes %s, want %s in it", encoded, want)
		}
	}

	// A CALLER THAT SAID NOTHING HAS NOTHING TO SHOW: its zeros are not
	// a published floor of zero.
	d.FloorState = ""
	rep = statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-1", At: reportAt, Domains: []statelog.DomainInputs{d},
	})
	if got := rep.Domains[0].TrimFloorState; got != statelog.TrimFloorNoneAtGeneration {
		t.Errorf("an unset floor state reported %q, want none_at_generation", got)
	}
	for _, s := range statelog.TrimFloorStates() {
		if !s.Valid() {
			t.Errorf("%q is not Valid()", s)
		}
	}
}

// A REPORT WITH NOTHING STANDING CARRIES NO NULL: every list the wire requires
// encodes as a list, and every optional field is absent rather than null.
func TestAReportWithNothingStandingCarriesNoNull(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		in   statelog.ReportInputs
	}{
		{"a healthy fleet", statelog.ReportInputs{
			NodeID: "node-1", At: reportAt, RegisterReadable: true,
			Domains: []statelog.DomainInputs{healthyDomain("tracker", true)},
			Register: []coord.NodePositions{{NodeID: "node-1", At: reportAt,
				Domains: map[string]coord.DomainPosition{
					"tracker": {Seq: 900, AppliedThrough: 900}}}},
		}},
		{"nothing read at all", statelog.ReportInputs{NodeID: "node-1", At: reportAt}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rep := statelog.NewReport(c.in)
			if len(rep.Alarms) != 0 {
				t.Fatalf("the case raises %v, so it is not one with nothing standing",
					rep.Alarms)
			}
			body, err := json.Marshal(rep)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"alarms":[]`) {
				t.Errorf("the report encodes %s, want \"alarms\":[] in it", body)
			}
			var doc map[string]any
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"domains", "nodes", "snapshots", "alarms"} {
				if _, list := doc[key].([]any); !list {
					t.Errorf("%s encodes as %v, want a list", key, doc[key])
				}
			}
			if found := nulls("report", doc); len(found) > 0 {
				t.Errorf("the report carries null at %v: %s", found, body)
			}
		})
	}
}

// nulls is the path of every null in a decoded JSON value.
func nulls(path string, v any) []string {
	var out []string
	switch v := v.(type) {
	case nil:
		out = append(out, path)
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			out = append(out, nulls(path+"."+k, v[k])...)
		}
	case []any:
		for i, e := range v {
			out = append(out, nulls(fmt.Sprintf("%s[%d]", path, i), e)...)
		}
	}
	return out
}

// A POSITION FROM ANOTHER GENERATION IS LABELLED, NOT COMPARED.
//
// Its sequence is in a number space the log has left: subtracted from the
// adopted stream's last, it reads lag 0 whenever the old number is larger,
// showing caught up a node that readmission then refuses.
func TestAPositionFromAnotherGenerationIsLabelledRatherThanCompared(t *testing.T) {
	t.Parallel()
	d := healthyDomain("tracker", true)
	d.Generation, d.LastSeq = 1, 40
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-1", At: reportAt, RegisterReadable: true,
		Domains: []statelog.DomainInputs{d},
		Register: []coord.NodePositions{
			{NodeID: "node-1", At: reportAt, Domains: map[string]coord.DomainPosition{
				"tracker": {Generation: 1, Seq: 30, AppliedThrough: 30}}},
			{NodeID: "node-2", At: reportAt, Domains: map[string]coord.DomainPosition{
				"tracker": {Generation: 0, Seq: 918_000_000, AppliedThrough: 918_000_000,
					LogDiverged: true}}},
			{NodeID: "node-3", At: reportAt, Domains: map[string]coord.DomainPosition{
				"tracker": {Generation: 2, Seq: 5, AppliedThrough: 5},
				"vectors": {Generation: 0, Seq: 9, AppliedThrough: 9}}},
		},
	})
	nodes := map[string]statelog.NodeReport{}
	for _, n := range rep.Nodes {
		nodes[n.NodeID] = n
	}
	current := nodes["node-1"].Domains["tracker"]
	if current.GenerationState != statelog.GenerationCurrent || current.Lag == nil ||
		*current.Lag != 10 {
		t.Errorf("the current node reads %+v, want current and lag 10", current)
	}
	left := nodes["node-2"].Domains["tracker"]
	if left.GenerationState != statelog.GenerationLeft {
		t.Errorf("a generation-0 position under a generation-1 log reads %q, want left",
			left.GenerationState)
	}
	if left.Lag != nil {
		t.Errorf("a position from a generation the log left carries lag %d — a "+
			"difference between two number spaces", *left.Lag)
	}
	if !left.LogDiverged {
		t.Error("the node's own log_diverged report was dropped")
	}
	ahead := nodes["node-3"].Domains["tracker"]
	if ahead.GenerationState != statelog.GenerationAhead || ahead.Lag != nil {
		t.Errorf("a generation-2 position under a generation-1 log reads %+v, want "+
			"ahead with no lag", ahead)
	}
	if got := nodes["node-3"].Domains["vectors"].GenerationState; got != statelog.GenerationUnknown {
		t.Errorf("a domain this node does not run reads %q, want unknown", got)
	}
	for _, s := range statelog.GenerationStates() {
		if !s.Valid() {
			t.Errorf("%q is not Valid()", s)
		}
	}
}

// A REFUSED DOMAIN NAMES THE REFUSAL AND WHICH FINDING IS BEHIND IT.
//
// `wrong_stream` is one word for four facts, each with its own remedy, and the
// guides send an operator to the status to find out which one this is.
func TestANotReadyDomainNamesTheFindingBehindIt(t *testing.T) {
	t.Parallel()
	now := reportAt
	fresh := statelog.Floor{State: statelog.FloorOK, ReadAt: now}
	last := uint64(40)
	ahead := statelog.Health{
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 90},
		LastSeq:  &last, Floor: fresh,
	}
	got := ahead.NotReady(now, "S", nil)
	if got == nil || got.Code != string(statelog.RefuseWrongStream) ||
		len(got.Causes) != 1 || got.Causes[0] != statelog.CauseAheadOfLog {
		t.Fatalf("a checkpoint past the end reads %+v, want wrong_stream from ahead_of_log", got)
	}
	if !strings.Contains(got.Detail, "sequence 90") || !strings.Contains(got.Detail, "ends at 40") {
		t.Errorf("the sentence does not name both numbers: %s", got.Detail)
	}

	both := statelog.Health{Position: statelog.Position{Seq: 30}, LastSeq: &last,
		Floor: fresh, StreamRecreated: true, LogDiverged: true}
	identity := errors.New("the stream was created at a later instant")
	got = both.NotReady(now, "S", identity)
	if got == nil || len(got.Causes) != 2 || got.Causes[0] != statelog.CauseRecreated ||
		got.Causes[1] != statelog.CauseLogDiverged || got.Detail != identity.Error() {
		t.Errorf("two findings read %+v, want both causes and the applier's own sentence", got)
	}

	if ready := (statelog.Health{Position: statelog.Position{Seq: 30}, LastSeq: &last,
		Floor: fresh}).NotReady(now, "S", nil); ready != nil {
		t.Errorf("a healthy domain reads not ready: %+v", ready)
	}
	for _, c := range statelog.IdentityCauses() {
		if !c.Valid() {
			t.Errorf("%q is not Valid()", c)
		}
	}
}
