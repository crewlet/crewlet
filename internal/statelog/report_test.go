package statelog_test

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// reportAt is the instant every case in this file is assembled at. Fixed
// rather than time.Now(), because half of what the report says is a
// comparison against an instant and a moving one makes a boundary case a
// flake.
var reportAt = time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)

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
// A compacted domain has no wake feed. Rendering that as `0` claims the feed
// has scanned nothing, and rendering it as unreadable puts a block on the
// screen of a fleet where nothing is wrong.
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
		t.Errorf("a compacted domain's wake feed reports %q; a domain that has no "+
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
// Five of the six name something to fix. The sixth binds on a healthy fleet,
// and an operator reading five remedies and one blank concludes the surface
// is incomplete rather than that the fleet is fine.
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
// `retention.backup_owner` reaches an operator in exactly one place, and this
// is it. A remedy that never named it would make the field decorative.
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
// The headroom alarm is the one an operator cannot ignore. Deriving it from a
// ceiling nobody could read pages a whole fleet for a broker that was briefly
// slow.
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
// Two logs fill at different rates. An alarm that said only "10% left" would
// send an operator to look at both.
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
	if !strings.HasPrefix(fired[0].Detail, "vectors: ") {
		t.Errorf("the headroom alarm reads %q and does not name the log that is "+
			"filling", fired[0].Detail)
	}
}

// THE REPORT'S COUNTED SET IS THE GATE'S OWN, TO THE INSTANT.
//
// An eviction takes effect one fence window after the gesture. A screen that
// called it effective a moment before the trim did would show a node dropped
// from the set the gate is still waiting for — which is the operator
// concluding the gesture worked and walking away.
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
			rep := statelog.NewReport(statelog.ReportInputs{
				NodeID:           "node-1",
				At:               tc.at,
				RegisterReadable: true,
				Register: []coord.NodePositions{
					{NodeID: "node-9", At: evicted},
				},
				Tombstones: []statelog.Tombstone{{NodeID: "node-9", At: evicted, By: "ops-3"}},
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
// It is a node between boot and its first heartbeat — a node adopting a
// snapshot — and it blocks every term derived from the counted set. A row it
// did not appear in would be a block with no visible cause.
func TestALiveNodeWithNoPositionIsCountedAndRendered(t *testing.T) {
	t.Parallel()
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Live:             []statelog.Presence{{NodeID: "node-7"}},
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

	blocked := healthyDomain("tracker", true)
	in := statelog.TrimInputs{Now: reportAt, HoldsReadable: true, BackupMaxAge: 24 * time.Hour}
	blocked.Decision = statelog.Trim(in.Terms())
	blocked.BlockedSince = reportAt.Add(-90 * time.Minute)
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Domains:          []statelog.DomainInputs{blocked},
	})
	if !rep.ExitNonZero() {
		t.Error("a fleet whose trim has been blocked for 90 minutes exits zero")
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
// `GET /work/retention` and `crewlet retention status --json` hand this
// document to a script and to a dashboard, and both address fields by name.
// [Alarm] shipped untagged, so the alarms array alone came back as
// `Kind`/`Detail`/`Remedy` beside siblings spelling themselves `node_id` and
// `first_seq`. Nothing reported it, and the reason is worth writing down: the
// only consumer at the time was `crewlet retention status`, and Go's decoder
// matches field names case-INSENSITIVELY, so the one reader that could have
// noticed was the one reader that could not. Every case-sensitive reader —
// the dashboard, `jq`, anything not written in Go — reads the field as absent
// rather than as an error.
//
// A REFLECTION WALK rather than a golden document: a golden one is edited to
// match whatever the code emits the day a field is added, which is precisely
// how the untagged struct survived. This fails on the field.
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
		statelog.NodeDomainReport{}, statelog.EvictionReport{}, statelog.SnapshotReport{}, statelog.ReplicaReport{}} {
		if !seen[reflect.TypeOf(want)] {
			t.Errorf("the walk never reached %T, so its fields are unchecked", want)
		}
	}
}
