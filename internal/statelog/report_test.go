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

// A LOG TOO SMALL FOR ITS OWN REPLAY WINDOW IS NAMED, and only that log.
//
// Each domain has its own ceiling and its own rate, so the comparison is per
// domain exactly as headroom is — an alarm that said only "a ceiling is too
// small" would send an operator to resize every log they have.
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
	if !strings.HasPrefix(fired[0].Detail, "iam: ") {
		t.Errorf("the alarm reads %q and does not name the log that is short",
			fired[0].Detail)
	}

	// AND THE WINDOW IS WHAT IT IS COMPARED AGAINST: the same log under a
	// three-day min_age fits, so a report that dropped the window would
	// fire on every log with any rate at all or on none.
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
// The two are opposite facts — a log that took in nothing, and a node that
// cannot say — and every surface downstream renders from the JSON. Collapsed
// into one representation, a dashboard would show an idle log on every node
// that has not ticked yet.
func TestAMeasuredZeroRateSurvivesTheWireAndAnUnmeasuredOneIsAbsent(t *testing.T) {
	t.Parallel()
	idle := healthyDomain("chart", true)
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

// THE REPORT NAMES THE LEVEL IT WAS SERVED AT, and it is DERIVED from whether
// this node could measure its own distance from the log.
//
// This is the one answer that has to keep working during the outage it
// describes, so it never takes a barrier — the barrier is the instrument and
// its health is the subject. But `stale` is a claim about AGE, and a document
// assembled while the lag could not be read cannot make one: the broker was
// unreachable, or coordination was, which is the ordinary signature of what
// somebody opened this page to diagnose. Weakening one step and NAMING it is
// what keeps this answer inside the read-level contract instead of exempt from
// it — an answer that quietly stopped carrying a level would be the silent
// downgrade wearing a different hat.
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

	// AND A NODE WITH NO ROW OF ITS OWN IS NOT A MEASURED ONE. That is
	// the state during exactly the coordination outage this document is
	// opened for, and reading "no domains reported" as "nothing unknown"
	// would claim an age with no evidence at all.
	absent := here(healthyDomain("tracker", true))
	absent.Register = nil
	if got := statelog.NewReport(absent).ReadLevel; got != statelog.ReadConsistentPrefix {
		t.Errorf("a node absent from its own register served %q — an empty "+
			"node block is not a measurement", got)
	}

	// NOR IS A NODE THAT MEASURED SOME OF ITS DOMAINS. A document that
	// could state an age for the tracker and not for the pages log is one
	// whose `stale` claim is true of half its rows.
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
// [statelog.SeqUnbounded] is the identity for the minimum the trim takes
// across the terms — chosen to lose a comparison, never to describe a
// position. It reached the retention screen as a sequence anyway: a solo
// fleet's snapshot floor and an unpinned log's min_hold both rendered
// `18446744073709552000` beside their own prose saying nothing was pinning
// anything. Not even the right digits, because a JSON number is a float64 and
// 2^64-1 is two thousand short of representable in one — so a reader could not
// have recovered the sentinel even knowing to look for it.
//
// Both halves are asserted: the STATE the screen switches on, and the number
// on the wire, because the state alone would let the sentinel keep riding
// along in a field the type documents as meaningless.
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

	// AND THE TRIM ITSELF IS UNAFFECTED. The sentinel's whole job is to lose
	// the minimum, so a fleet with nothing pinning it trims to the lowest
	// term that IS a position — never to 2^64-1, and never blocked.
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
// `seq` carried `omitempty`, which drops exactly the value a young fleet's
// binding term has. A term permitting removal up to 0 — the state that holds
// the trim, and therefore the one an operator opened this screen to read —
// went onto the wire looking identical to an unreadable term, and a reader
// could only recover it by assuming an absent field meant zero, which for the
// unreadable one it does not.
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
