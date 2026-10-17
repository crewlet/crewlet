package api_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
)

// retentionAlarmsGolden is the retention report's `alarms` in each state the
// backup alarm takes, rendered by the report the engine answers
// `/query/retention` with, committed, and read by the dashboard's own suites
// as their fixture.
//
// THE BACKUP ALARM BECAME PER LOG and three suites went on drawing the
// node-wide one they had typed: a headline no listed log carried, held logs
// whose lines omitted the backup's own measurement, and a sentence the engine
// no longer writes. Each passed, because a fixture typed beside the screen
// agrees with the screen whatever the engine sends.
const retentionAlarmsGolden = "testdata/retention_alarms.json"

// retentionAlarmsAt is the instant every scenario is evaluated at, so the
// golden is stable and a suite can render its ages against the same clock.
var retentionAlarmsAt = time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)

// retentionAlarmsWindow is the scenarios' replay window and backup policy:
// one day each, the deployment a reader of the alarm most often has.
const retentionAlarmsWindow = 24 * time.Hour

// retentionAlarmsHeld is how long a held trim has stood still: past the
// window and a tick, so a hold that keeps records past the window is one the
// report raises.
const retentionAlarmsHeld = 46 * time.Hour

// alarmLog is one registered log as the report reads it, holding `kept`
// records older than the replay window and trimmed by `in`, which the caller
// fills with what this log's backup and counted set are.
//
// THE COUNT IS THE LOG'S OWN, read the way the report reads it: the age term's
// first young-enough sequence against the log's first record.
func alarmLog(domain statelog.Domain, kept uint64, in statelog.TrimInputs) statelog.DomainInputs {
	in.Now, in.HoldsReadable, in.HoldStale = retentionAlarmsAt, true, statelog.TrimHoldStale
	in.BackupMaxAge, in.AgeFloor = retentionAlarmsWindow, kept+1
	spec := domain.Stream()
	return statelog.DomainInputs{
		Domain: domain.Name(), Stream: spec.Name, Replay: spec.Replay,
		FirstSeq: 1, LastSeq: kept + 400, Bytes: 64 << 20, MaxBytes: 16 << 30,
		StreamReadable: true, TrimFloor: 1,
		Decision: statelog.Trim(in.Terms()), FloorState: statelog.TrimFloorPublished,
		BackupAt: in.BackupAt,
	}
}

// counted is a counted set every node of which has applied the log.
func counted(seq uint64) []statelog.NodePosition {
	return []statelog.NodePosition{{NodeID: "node-a", Seq: seq, At: retentionAlarmsAt}}
}

// retentionAlarmsScenarios is one report per state the backup alarm takes,
// each over every log this build registers ([engine.Domains]), so a log the
// register gains is in every one of them.
//
//   - `young`: a deployment minutes old with no backup at all. Every log is
//     named, none is held past its window, so no entry carries a term.
//   - `held`: no backup at all, and every log's trim held by the backup term
//     for longer than the window while keeping a count of its own past it.
//     Each entry carries the term, the instant and its hold's measurement
//     after its backup's.
//   - `held_beside_a_blocked_trim`: as `held`, but the THIRD log is backed up
//     and held by a counted set nobody could read, so it is `trim_blocked`'s.
//   - `stale`: the first log's backup is older than the policy and its trim
//     has not yet been held past the window; every other log is covered. One
//     log, no term.
func retentionAlarmsScenarios() map[string]statelog.Report {
	report := func(domains []statelog.DomainInputs) statelog.Report {
		return statelog.NewReport(statelog.ReportInputs{
			NodeID: "node-a", At: retentionAlarmsAt, RegisterReadable: true,
			ReplayWindow: retentionAlarmsWindow, BackupMaxAge: retentionAlarmsWindow,
			Domains: domains,
		})
	}
	each := func(build func(i int, domain statelog.Domain) statelog.DomainInputs) statelog.Report {
		var domains []statelog.DomainInputs
		for i, domain := range engine.Domains() {
			domains = append(domains, build(i, domain))
		}
		return report(domains)
	}
	// A COUNT OF ITS OWN PER LOG, so a suite can tell one log's line from
	// another's.
	keeps := func(i int) uint64 { return 1200 >> i }
	unbacked := func(i int, domain statelog.Domain, since time.Time) statelog.DomainInputs {
		d := alarmLog(domain, keeps(i), statelog.TrimInputs{
			Counted: counted(keeps(i) + 400), CountedReadable: true})
		d.BlockedSince = since
		return d
	}
	backed := func(i int, domain statelog.Domain, age time.Duration) statelog.TrimInputs {
		return statelog.TrimInputs{Counted: counted(keeps(i) + 400), CountedReadable: true,
			HasBackupFloor: true, BackupFloor: keeps(i) + 400,
			BackupAt: retentionAlarmsAt.Add(-age)}
	}
	heldSince := retentionAlarmsAt.Add(-retentionAlarmsHeld)
	return map[string]statelog.Report{
		"young": each(func(_ int, domain statelog.Domain) statelog.DomainInputs {
			// NOTHING PAST THE WINDOW: every record is minutes old.
			d := alarmLog(domain, 0, statelog.TrimInputs{
				Counted: counted(400), CountedReadable: true})
			d.BlockedSince = retentionAlarmsAt.Add(-4 * time.Minute)
			return d
		}),
		"held": each(func(i int, domain statelog.Domain) statelog.DomainInputs {
			return unbacked(i, domain, heldSince)
		}),
		"held_beside_a_blocked_trim": each(func(i int, domain statelog.Domain) statelog.DomainInputs {
			if i != 2 {
				return unbacked(i, domain, heldSince)
			}
			in := backed(i, domain, time.Hour)
			in.Counted, in.CountedReadable = nil, false
			d := alarmLog(domain, keeps(i), in)
			d.BlockedSince = heldSince
			return d
		}),
		"stale": each(func(i int, domain statelog.Domain) statelog.DomainInputs {
			if i != 0 {
				return alarmLog(domain, keeps(i), backed(i, domain, time.Hour))
			}
			d := alarmLog(domain, keeps(i), backed(i, domain, 30*time.Hour))
			// HELD SINCE THE COPY WENT STALE, which is not yet past the
			// window: the trim it holds raises nothing of its own.
			d.BlockedSince = retentionAlarmsAt.Add(-6 * time.Hour)
			return d
		}),
	}
}

// renderRetentionAlarms is the golden's content: each scenario's `alarms`, as
// the report marshals them onto the wire, and the instant they were evaluated
// at.
func renderRetentionAlarms(t *testing.T) []byte {
	t.Helper()
	alarms := map[string][]statelog.Alarm{}
	for name, rep := range retentionAlarmsScenarios() {
		alarms[name] = rep.Alarms
	}
	out, err := json.MarshalIndent(map[string]any{
		"at": retentionAlarmsAt, "alarms": alarms}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// THE RETENTION ALARMS ARE WHAT THEIR GOLDEN FILE SAYS, for the gate answer's
// reason: a screen whose suite types its own fixtures agrees with itself
// whatever the engine sends. A change to the alarm's shape or wording is a
// failing test until the golden is regenerated (`make retention-alarms`) and a
// failing Vitest suite until the screens that draw it follow.
func TestTheRetentionAlarmsMatchTheirGoldenFile(t *testing.T) {
	t.Parallel()
	got := renderRetentionAlarms(t)
	if os.Getenv("CREWLET_REGENERATE_RETENTION_ALARMS") == "1" {
		if err := os.MkdirAll(filepath.Dir(retentionAlarmsGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(retentionAlarmsGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(retentionAlarmsGolden)
	if err != nil {
		t.Fatalf("read %s: %v — `make retention-alarms` writes it", retentionAlarmsGolden, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the retention alarms no longer match %s. Run `make retention-alarms`, "+
			"read the diff, and follow it in the dashboard's retention, backups and "+
			"engine-panel screens — their suites load this file.\n--- rendered now ---\n%s",
			retentionAlarmsGolden, got)
	}
}

// EACH SCENARIO IS THE STATE IT IS NAMED FOR. A golden regenerated after a
// change to the report would otherwise go on loading as a fixture while it
// described a different state — a backup alarm whose held logs had moved to
// `trim_blocked`, say — and every suite reading it would test that instead.
func TestEachRetentionAlarmScenarioIsTheStateItIsNamedFor(t *testing.T) {
	t.Parallel()
	logs := len(engine.Domains())
	scenarios := retentionAlarmsScenarios()
	// kinds is each alarm's logs, and how many of them carry `term`.
	type raised struct{ logs, termed int }
	kinds := func(name string) map[statelog.Kind]raised {
		out := map[statelog.Kind]raised{}
		for _, a := range scenarios[name].Alarms {
			r := raised{logs: len(a.Domains)}
			for _, d := range a.Domains {
				if d.Term != "" {
					r.termed++
				}
			}
			out[a.Kind] = r
		}
		return out
	}
	for name, want := range map[string]map[statelog.Kind]raised{
		"young": {statelog.KindBackupAge: {logs: logs}},
		"held":  {statelog.KindBackupAge: {logs: logs, termed: logs}},
		"held_beside_a_blocked_trim": {
			statelog.KindBackupAge:   {logs: logs - 1, termed: logs - 1},
			statelog.KindTrimBlocked: {logs: 1, termed: 1},
		},
		"stale": {statelog.KindBackupAge: {logs: 1}},
	} {
		got := kinds(name)
		if len(got) != len(want) {
			t.Errorf("%s raised %v; want exactly %v", name, got, want)
			continue
		}
		for kind, w := range want {
			if got[kind] != w {
				t.Errorf("%s raised %s over %+v; want %+v", name, kind, got[kind], w)
			}
		}
	}
}
