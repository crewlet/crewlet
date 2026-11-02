package config_test

import (
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// retentionBoot is a valid Tier A with a retention block to vary.
func retentionBoot(t *testing.T, r config.TrackerRetention) config.Bootstrap {
	t.Helper()
	b := config.RunnableBootstrap()
	b.Stream.TrackerRetention = r
	return b
}

// EVERY TERM HAS A RANGE, AND THE RANGE IS WHAT THE FIELD MEANS.
//
// These are not arbitrary bounds. Below its floor each field stops doing the
// thing it exists for — a trim floor that cannot outlast a nightly backup, a
// snapshot loop that runs more often than it finishes, a rejoin budget no
// real join fits in — and above its ceiling it has stopped being a gate on
// anything.
func TestTheTrimsTermsAreBounded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		block  config.TrackerRetention
		accept bool
		says   string
	}{
		"min_age below the floor": {config.TrackerRetention{MinAgeRaw: "1h"}, false, "min_age"},
		// AT THE FLOOR THE SNAPSHOT INTERVAL HAS TO COME DOWN WITH IT,
		// which is the cross-field rule doing its job: a day-old floor
		// cannot hold two days of snapshots.
		"min_age at the floor":       {config.TrackerRetention{MinAgeRaw: "24h", SnapshotIntervalRaw: "6h"}, true, ""},
		"min_age at the ceiling":     {config.TrackerRetention{MinAgeRaw: "2160h", SnapshotIntervalRaw: "24h"}, true, ""},
		"min_age past the ceiling":   {config.TrackerRetention{MinAgeRaw: "2184h", SnapshotIntervalRaw: "24h"}, false, "min_age"},
		"backup_max_age at zero":     {config.TrackerRetention{BackupMaxAgeRaw: "0s"}, false, "backup_max_age"},
		"a term at its default":      {config.TrackerRetention{MinAgeRaw: "168h"}, true, ""},
		"backup_max_age at an hour":  {config.TrackerRetention{BackupMaxAgeRaw: "1h"}, true, ""},
		"backup_max_age at a month":  {config.TrackerRetention{BackupMaxAgeRaw: "720h"}, true, ""},
		"backup_max_age past it":     {config.TrackerRetention{BackupMaxAgeRaw: "744h"}, false, "backup_max_age"},
		"snapshot_interval too fast": {config.TrackerRetention{SnapshotIntervalRaw: "15m"}, false, "snapshot_interval"},
		"snapshot_interval at 6h":    {config.TrackerRetention{SnapshotIntervalRaw: "6h"}, true, ""},
		"snapshot_interval at a day": {config.TrackerRetention{SnapshotIntervalRaw: "24h"}, true, ""},
		"snapshot_interval past it":  {config.TrackerRetention{SnapshotIntervalRaw: "192h", MinAgeRaw: "2160h"}, false, "snapshot_interval"},
		"rejoin_window too short":    {config.TrackerRetention{RejoinWindowRaw: "4m"}, false, "rejoin_window"},
		"rejoin_window at 5m":        {config.TrackerRetention{RejoinWindowRaw: "5m"}, true, ""},
		"rejoin_window at a day":     {config.TrackerRetention{RejoinWindowRaw: "24h"}, true, ""},
		"rejoin_window past a day":   {config.TrackerRetention{RejoinWindowRaw: "25h"}, false, "rejoin_window"},
		"backup_floor engine":        {config.TrackerRetention{BackupFloor: config.BackupFloorEngine}, true, ""},
		"backup_floor operator":      {config.TrackerRetention{BackupFloor: config.BackupFloorOperator}, true, ""},
		"backup_floor none":          {config.TrackerRetention{BackupFloor: "none"}, false, "backup_floor"},
		"a duration that is not one": {config.TrackerRetention{MinAgeRaw: "7 days"}, false, "not a duration"},
	} {
		t.Run(name, func(t *testing.T) {
			b := retentionBoot(t, tc.block)
			err := b.Validate()
			if tc.accept {
				if err != nil {
					t.Fatalf("%+v was refused: %v", tc.block, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%+v was accepted", tc.block)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("refusal = %q, want it to say %q", err, tc.says)
			}
		})
	}
}

// THE CROSS-FIELD RULE IS WHAT STOPS THE RANGES PRODUCING AN UNRECOVERABLE
// FLEET.
//
// `snapshot_interval: 7d` and `min_age: 24h` are each inside their own range,
// and together they make EVERY snapshot older than the trim floor — so a node
// that loses its store finds nothing it can resume from and has no recovery
// path at all. The trim would then block for ever on the snapshot term, which
// is safe and useless.
func TestASnapshotOlderThanTheTrimFloorIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		interval, floor string
		accept          bool
	}{
		"a daily snapshot under a weekly floor": {"24h", "168h", true},
		"a weekly snapshot under a daily floor": {"168h", "24h", false},
		"exactly at the boundary":               {"12h", "24h", false},
		"just inside it":                        {"11h", "24h", true},
	} {
		t.Run(name, func(t *testing.T) {
			b := retentionBoot(t, config.TrackerRetention{
				SnapshotIntervalRaw: tc.interval, MinAgeRaw: tc.floor,
			})
			err := b.Validate()
			if tc.accept {
				if err != nil {
					t.Fatalf("interval %s under floor %s was refused: %v",
						tc.interval, tc.floor, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("interval %s under floor %s was accepted — every snapshot "+
					"this node keeps would be older than the trim floor",
					tc.interval, tc.floor)
			}
			for _, want := range []string{"snapshot_interval", "min_age", "SnapshotsKept"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to name %q", err, want)
				}
			}
		})
	}
}

// AN ABSENT BLOCK READS BACK AS THE DEFAULTS, which is what makes "the
// operator has not thought about this yet" a working deployment rather than a
// refusal.
func TestAnAbsentRetentionBlockIsTheDefaults(t *testing.T) {
	t.Parallel()
	var r config.TrackerRetention
	if !r.IsZero() {
		t.Error("an unset block does not report zero, so it would be written into " +
			"every stored config an operator never touched")
	}
	for name, got := range map[string]struct{ have, want time.Duration }{
		"min_age":           {r.MinAge(), 7 * 24 * time.Hour},
		"backup_max_age":    {r.BackupMaxAge(), 24 * time.Hour},
		"snapshot_interval": {r.SnapshotInterval(), 24 * time.Hour},
		"rejoin_window":     {r.RejoinWindow(), 30 * time.Minute},
	} {
		if got.have != got.want {
			t.Errorf("%s = %v, want %v", name, got.have, got.want)
		}
	}
	if r.Floor() != config.BackupFloorEngine {
		t.Errorf("backup_floor = %q, want %q", r.Floor(), config.BackupFloorEngine)
	}
	// AND THE DEFAULTS THEMSELVES SATISFY THE CROSS-FIELD RULE. A shipped
	// default set that its own validator refuses is a deployment nobody
	// can start.
	shipped := config.RunnableBootstrap()
	if err := shipped.Validate(); err != nil {
		t.Fatalf("the shipped defaults do not validate: %v", err)
	}
}

// THE LOG'S CEILING IS DERIVED FROM THE VOLUME when it is unset, because one
// number is wrong in both directions: the same value is five years of history
// on the modelled write rate and one boot on a small disk.
func TestTheLogCeilingIsDerivedFromTheVolume(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	for name, tc := range map[string]struct {
		free int64
		want int64
	}{
		"a small disk clamps up":           {8 * gib, 4 * gib},
		"an ordinary disk takes a quarter": {200 * gib, 50 * gib},
		"a large array clamps down":        {4000 * gib, 64 * gib},
		"an empty answer still clamps up":  {0, 4 * gib},
	} {
		t.Run(name, func(t *testing.T) {
			if got := config.DerivedLogMaxBytes(tc.free); got != tc.want {
				t.Errorf("DerivedLogMaxBytes(%d) = %d, want %d", tc.free, got, tc.want)
			}
		})
	}

	// A CONFIGURED VALUE WINS AND SAYS SO. The second return is what a node
	// logs when it sizes the stream, so it can answer "where did this
	// ceiling come from" a year later.
	var s config.Stream
	if got, derived := s.LogMaxBytes(200 * gib); got != 50*gib || !derived {
		t.Errorf("unset = (%d, %v), want the derived value", got, derived)
	}
	s.TrackerLogMaxBytes = 32 * gib
	if got, derived := s.LogMaxBytes(200 * gib); got != 32*gib || derived {
		t.Errorf("set = (%d, %v), want the configured value", got, derived)
	}
}

// THE KNOWLEDGE BASE'S LOG IS DERIVED FROM THE MUTATION LOG'S CEILING — the one
// an operator wrote, or the one the volume derives — at a quarter of it, the
// rate its records grow at beside the tracker's. Both logs hold a trailing
// window of records, so a blocked trim fills both in the same time.
//
// It was once a fixed 4 GiB, reserved on top of a budget the other two logs had
// already been scaled to fill, and later a quarter of the DERIVED value alone —
// so an operator who sized the mutation log for their company left it sized
// for their disk.
func TestThePagesCeilingIsAQuarterOfTheMutationLogs(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	for name, tc := range map[string]struct {
		stream config.Stream
		free   int64
		want   int64
	}{
		"an unmeasured volume, at the floor": {free: 0, want: 1 * gib},
		"a small disk":                       {free: 8 * gib, want: 1 * gib},
		"an ordinary disk":                   {free: 200 * gib, want: 12*gib + gib/2},
		"a large array, at the clamp":        {free: 4000 * gib, want: 16 * gib},
		// THE OPERATOR'S FIGURE FOR THE COMPANY, on a disk that would
		// have derived far less.
		"a mutation log an operator sized": {stream: config.Stream{TrackerLogMaxBytes: 512 * gib},
			free: 8 * gib, want: 128 * gib},
		"the largest mutation log Tier A accepts": {stream: config.Stream{TrackerLogMaxBytes: config.TrackerLogMaxBytesCeiling},
			free: 8 * gib, want: config.PagesLogMaxBytesCeiling},
		// AND NEVER BELOW WHAT TIER A WOULD ACCEPT AS A VALUE, or a
		// node could derive a ceiling its own validation refuses to be
		// told.
		"the smallest mutation log Tier A accepts": {stream: config.Stream{TrackerLogMaxBytes: config.TrackerLogMaxBytesFloor},
			free: 4000 * gib, want: config.PagesLogMaxBytesFloor},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, derived := tc.stream.PagesMaxBytes(tc.free); got != tc.want || !derived {
				t.Errorf("the knowledge base's log derives (%d, derived %v), want (%d, true)",
					got, derived, tc.want)
			}
		})
	}

	// AN OPERATOR'S OWN NUMBER IS NEITHER DERIVED NOR CAPPED. They named a
	// limit for a broker they can see, and silently lowering it would be
	// the engine deciding a limit an emergency grant had just raised.
	s := config.Stream{PagesLogMaxBytes: 3 * gib, TrackerVectorsMaxBytes: 2 * gib}
	if got, derived := s.PagesMaxBytes(200 * gib); got != 3*gib || derived {
		t.Errorf("a set knowledge-base ceiling reads back (%d, derived %v)", got, derived)
	}
	if got, derived := s.VectorsMaxBytes(1 << 30); got != 2*gib || derived {
		t.Errorf("a set vector ceiling reads back (%d, derived %v)", got, derived)
	}
}

// THE CODE LOG IS DERIVED FROM THE MUTATION LOG'S CEILING — set or derived —
// at two thirds of it, the bytes its push records and its git clients' barriers
// take a day beside the tracker's (DerivedCodeLogNumerator), on the knowledge
// base's rule: both logs hold a trailing window of records, so a blocked trim
// fills both in the same time. Each expectation is
// the mutation log's own ceiling for the row, times two over three, held
// inside the code log's own bounds.
func TestCodeLogCeilingIsTwoThirdsOfTheTrackers(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	for name, tc := range map[string]struct {
		stream config.Stream
		free   int64
		want   int64
	}{
		// The mutation log derives its 4 GiB floor on these two.
		"an unmeasured volume": {free: 0, want: 4 * gib * 2 / 3},
		"a 16 GiB disk":        {free: 16 * gib, want: 4 * gib * 2 / 3},
		// A quarter of 200 GiB is 50 GiB.
		"an ordinary disk": {free: 200 * gib, want: 50 * gib * 2 / 3},
		// A quarter of a tebibyte is past the 64 GiB clamp.
		"a large array, at the mutation log's clamp": {free: 1024 * gib, want: 64 * gib * 2 / 3},
		// THE OPERATOR'S FIGURE FOR THE COMPANY, on a disk that would have
		// derived far less.
		"a mutation log an operator sized": {stream: config.Stream{TrackerLogMaxBytes: 900 * gib},
			free: 8 * gib, want: 600 * gib},
		"the largest mutation log Tier A accepts": {stream: config.Stream{TrackerLogMaxBytes: config.TrackerLogMaxBytesCeiling},
			free: 8 * gib, want: config.TrackerLogMaxBytesCeiling * 2 / 3},
		// AND NEVER BELOW WHAT TIER A WOULD ACCEPT AS A VALUE: two thirds
		// of the smallest mutation log is under a gibibyte.
		"the smallest mutation log Tier A accepts": {stream: config.Stream{TrackerLogMaxBytes: config.TrackerLogMaxBytesFloor},
			free: 4000 * gib, want: config.CodeLogMaxBytesFloor},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, derived := tc.stream.CodeMaxBytes(tc.free)
			if got != tc.want || !derived {
				t.Errorf("the code log derives (%d, derived %v), want (%d, true)",
					got, derived, tc.want)
			}
			if got < config.CodeLogMaxBytesFloor || got > config.CodeLogMaxBytesCeiling {
				t.Errorf("the code log derives %d, outside the %d..%d Tier A accepts",
					got, config.CodeLogMaxBytesFloor, config.CodeLogMaxBytesCeiling)
			}
		})
	}

	// AN OPERATOR'S OWN NUMBER IS NEITHER DERIVED NOR SCALED.
	s := config.Stream{CodeLogMaxBytes: 3 * gib, TrackerLogMaxBytes: 900 * gib}
	if got, derived := s.CodeMaxBytes(200 * gib); got != 3*gib || derived {
		t.Errorf("a set code-log ceiling reads back (%d, derived %v)", got, derived)
	}
}

// THE SCHEMA SPELLS EACH LOG'S BOUNDS A SECOND TIME, as the literals of a
// struct tag, and the generated schema is what an editor and `crewlet validate
// --schema` admit a value by — so a tag left behind when its constant moved
// admits a value validation then refuses. The store floor's tag did exactly
// that when the floor moved from five gibibytes to six; nothing tied the two.
// This does, for every field whose bounds are constants.
func TestTheStoreFloorIsTheSchemasMinimum(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field    string
		min, max int64
	}{
		{"StoreMaxBytes", config.StoreMaxBytesFloor, config.StoreMaxBytesCeiling},
		{"TrackerLogMaxBytes", config.TrackerLogMaxBytesFloor, config.TrackerLogMaxBytesCeiling},
		{"TrackerVectorsMaxBytes", config.TrackerVectorsMaxBytesFloor, config.TrackerVectorsMaxBytesCeiling},
		{"PagesLogMaxBytes", config.PagesLogMaxBytesFloor, config.PagesLogMaxBytesCeiling},
		{"IamLogMaxBytes", config.IamLogMaxBytesFloor, config.IamLogMaxBytesCeiling},
		{"UsageLogMaxBytes", config.UsageLogMaxBytesFloor, config.UsageLogMaxBytesCeiling},
		{"CodeLogMaxBytes", config.CodeLogMaxBytesFloor, config.CodeLogMaxBytesCeiling},
		{"ArtifactsLogMaxBytes", config.ArtifactsLogMaxBytesFloor, config.ArtifactsLogMaxBytesCeiling},
		{"ArtifactDataLogMaxBytes", config.ArtifactDataLogMaxBytesFloor, config.ArtifactDataLogMaxBytesCeiling},
	} {
		field, ok := reflect.TypeFor[config.Stream]().FieldByName(tc.field)
		if !ok {
			t.Errorf("config.Stream has no field %s", tc.field)
			continue
		}
		bounds := map[string]int64{}
		for _, part := range strings.Split(field.Tag.Get("js"), ";") {
			key, value, found := strings.Cut(part, "=")
			if !found {
				continue
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Errorf("%s's js tag %q: %v", tc.field, part, err)
				continue
			}
			bounds[key] = n
		}
		if bounds["min"] != tc.min || bounds["max"] != tc.max {
			t.Errorf("%s's schema admits %d..%d and validation %d..%d — the tag "+
				"was left behind when a constant moved", tc.field,
				bounds["min"], bounds["max"], tc.min, tc.max)
		}
	}
}

// A LOG THAT GROWS WITH NO CORPUS TAKES A FLOOR WELL UNDER ONE THAT DOES, and
// that is a decision rather than an accident.
//
// # What a ceiling costs before a byte is written
//
// The broker grants a stream its WHOLE ceiling when it creates it, so a
// domain's floor is free space a node must have before it can boot at all. The
// identity log grows with headcount and sign-ins, the usage log with a census
// of seats and schedules, and the Dashboard Assistant's log with the questions
// people ask it: none of them with a corpus the operator's volume has anything
// to say about. A ten-person company writes a few megabytes to the first two,
// and a month of its questions fits in the assistant's 64 MiB. Held to the
// gibibyte the corpus-sized logs take, each would raise the disk the smallest
// deployment needs by a gibibyte for a log it will never fill.
//
// The floor the corpus-sized logs take belongs to them: a mutation log under a
// gibibyte really is a window that refuses appends within a week of a company
// starting work.
//
// SO THE ASSERTION IS THE RELATION, not the number: these floors must stay well
// under the corpus floor, because that is the property that keeps adding a
// domain from raising what a deployment needs. The margin is deliberately
// loose — what would be wrong is either creeping back up to the others' floor,
// not its exact value.
func TestALogThatGrowsWithNoCorpusTakesAFloorFarUnderOneThatDoes(t *testing.T) {
	t.Parallel()

	// THE SEVEN CORPUS-SIZED FLOORS, read through a map rather than
	// compared as constants: they are equal today, and `go vet` reads a
	// constant comparison that cannot be false as a mistake. The chat log
	// is one of them — it grows with what the company says, as the
	// mutation log grows with what it files. So are the artifacts log and
	// the artifact datastore's, for the knowledge base's reasons, and the
	// code log, which grows with the repositories the company keeps here.
	corpus := map[string]int64{
		"tracker_log_max_bytes":       config.TrackerLogMaxBytesFloor,
		"pages_log_max_bytes":         config.PagesLogMaxBytesFloor,
		"tracker_vectors_max_bytes":   config.TrackerVectorsMaxBytesFloor,
		"artifacts_log_max_bytes":     config.ArtifactsLogMaxBytesFloor,
		"artifact_data_log_max_bytes": config.ArtifactDataLogMaxBytesFloor,
		"chat_log_max_bytes":          config.ChatLogMaxBytesFloor,
		"code_log_max_bytes":          config.CodeLogMaxBytesFloor,
	}
	noCorpus := map[string]int64{
		"iam_log_max_bytes":       config.IamLogMaxBytesFloor,
		"usage_log_max_bytes":     config.UsageLogMaxBytesFloor,
		"assistant_log_max_bytes": config.AssistantLogMaxBytesFloor,
	}
	// EVERY LOG IS IN EXACTLY ONE OF THE TWO, read off config.Stream rather
	// than listed: a log that joins the broker with a ceiling and is in
	// neither would otherwise pass here with its floor never weighed.
	for name := range logCeilings(t) {
		_, big := corpus[name]
		_, small := noCorpus[name]
		if big == small {
			t.Errorf("%s must be in exactly one of the two tables, by whether "+
				"its log grows with a corpus", name)
		}
	}
	corpusFloor := config.TrackerLogMaxBytesFloor
	for name, floor := range corpus {
		if floor != corpusFloor {
			t.Fatalf("%s's floor is %d and the others take %d, so the "+
				"corpus-sized logs no longer share one floor and this "+
				"comparison has nothing to hold the others against",
				name, floor, corpusFloor)
		}
	}
	for name, floor := range noCorpus {
		if floor*4 > corpusFloor {
			t.Errorf("%s's floor is %d bytes and a corpus-sized log's is %d.\n"+
				"A ceiling is granted in full at create time, so a floor is "+
				"free space every node must have before it boots — and this "+
				"log grows with no corpus. Keep it well under the floor the "+
				"corpus logs take, or the smallest deployment pays a "+
				"gibibyte for a log it never fills.", name, floor, corpusFloor)
		}
	}
}

// logCeilings is the yaml name of every state log's byte ceiling, read off
// config.Stream: each `*_max_bytes` field but `store_max_bytes`, which is the
// broker's own limit rather than a log's.
func logCeilings(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	stream := reflect.TypeFor[config.Stream]()
	for i := range stream.NumField() {
		name, _, _ := strings.Cut(stream.Field(i).Tag.Get("yaml"), ",")
		if strings.HasSuffix(name, "_max_bytes") && name != "store_max_bytes" {
			out[name] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("config.Stream declares no log ceiling, so this case weighs nothing")
	}
	return out
}

// THE VECTOR CHANGELOG IS SIZED FOR THE PEAK, FROM THE VOLUME.
//
// The peak is a model change republishing every source at once — the whole
// corpus inside the window, 93× the steady state — and the corpus is the
// company's whole history, while the mutation log's ceiling bounds a trailing
// window. So an unset changelog asks for what the mutation log derives from the
// volume, whatever the mutation log is set to:
//
//   - the reference company's fifth-year model change (8.46 GB) fits twice
//     over on a 64 GiB volume — half the mutation log's ceiling, the rule
//     this replaced, asked for 8 GiB there and the pool scaled it under the
//     peak;
//   - an operator who sized a small mutation log for a fleet that trims does
//     not shrink the changelog with it — beside a 4 GiB mutation log that
//     rule asked for 2 GiB, which the reference company outgrows in its
//     second year;
//   - on no volume does it ask for less than the fixed 16 GiB default, capped
//     by the same share of free space, that the first rule replaced.
//
// And the largest company the engine is sized for — 10 000 seats in its fifth
// year — must be able to WRITE a ceiling that holds its peak twice.
func TestTheVectorCeilingIsSizedForAModelChange(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	// The vector corpus per 100 seats per year: 8.46 GB at the reference
	// company's fifth year.
	const corpusPerYear = 8_460_000_000 / 5
	const referenceYearFive = 5 * corpusPerYear

	if got, _ := (config.Stream{}).VectorsMaxBytes(64 * gib); got < 2*referenceYearFive {
		t.Errorf("on 64 GiB free the vector changelog derives %d, under twice "+
			"the reference company's fifth-year model change (%d)", got, 2*referenceYearFive)
	}
	sized := config.Stream{TrackerLogMaxBytes: 4 * gib}
	got, derived := sized.VectorsMaxBytes(64 * gib)
	if want, _ := (config.Stream{}).VectorsMaxBytes(64 * gib); got != want || !derived {
		t.Errorf("beside a 4 GiB mutation log the vector changelog derives (%d, "+
			"derived %v), want the volume's (%d, true): the mutation log's "+
			"ceiling is a trailing window and says nothing about the corpus", got, derived, want)
	}
	for _, free := range []int64{0, 8 * gib, 16 * gib, 64 * gib, 200 * gib, 4000 * gib} {
		got, derived := config.Stream{}.VectorsMaxBytes(free)
		if want := config.DerivedLogMaxBytes(free); got != want || !derived {
			t.Errorf("unset on %d free derives (%d, derived %v), want the "+
				"mutation log's own derivation (%d, true)", free, got, derived, want)
		}
		if replaced := min(int64(16)*gib, config.DerivedLogMaxBytes(free)); got < replaced {
			t.Errorf("unset on %d free derives %d, under the %d the 16 GiB "+
				"default asked for there", free, got, replaced)
		}
		if got < config.TrackerVectorsMaxBytesFloor || got > config.TrackerVectorsMaxBytesCeiling {
			t.Errorf("unset on %d free derives %d, outside the %d..%d Tier A "+
				"accepts", free, got, config.TrackerVectorsMaxBytesFloor,
				config.TrackerVectorsMaxBytesCeiling)
		}
	}

	const largest = 10_000 / 100 * 5 * corpusPerYear
	if config.TrackerVectorsMaxBytesCeiling < 2*largest {
		t.Errorf("Tier A accepts at most %d bytes of vector changelog, under twice "+
			"the peak of 10 000 seats in their fifth year (%d)",
			config.TrackerVectorsMaxBytesCeiling, int64(2*largest))
	}
	b := config.RunnableBootstrap()
	b.Stream.TrackerVectorsMaxBytes = 2 * largest
	if err := b.Validate(); err != nil {
		t.Errorf("a vector changelog sized for 10 000 seats in their fifth year "+
			"was refused: %v", err)
	}
}

// THE BYTE CEILINGS ARE BOUNDED, and the bounds are what the schema
// publishes.
func TestTheByteCeilingsAreBounded(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	const mib = int64(1) << 20
	for name, tc := range map[string]struct {
		mutate func(*config.Bootstrap)
		accept bool
		says   string
	}{
		"a log below a gibibyte":   {func(b *config.Bootstrap) { b.Stream.TrackerLogMaxBytes = gib - 1 }, false, "tracker_log_max_bytes"},
		"a log at a gibibyte":      {func(b *config.Bootstrap) { b.Stream.TrackerLogMaxBytes = gib }, true, ""},
		"a log at a tebibyte":      {func(b *config.Bootstrap) { b.Stream.TrackerLogMaxBytes = 1024 * gib }, true, ""},
		"a log past a tebibyte":    {func(b *config.Bootstrap) { b.Stream.TrackerLogMaxBytes = 1024*gib + 1 }, false, "tracker_log_max_bytes"},
		"vectors below a gibibyte": {func(b *config.Bootstrap) { b.Stream.TrackerVectorsMaxBytes = gib - 1 }, false, "tracker_vectors_max_bytes"},
		"vectors at 2 TiB":         {func(b *config.Bootstrap) { b.Stream.TrackerVectorsMaxBytes = 2048 * gib }, true, ""},
		"vectors past 2 TiB":       {func(b *config.Bootstrap) { b.Stream.TrackerVectorsMaxBytes = 2048*gib + 1 }, false, "tracker_vectors_max_bytes"},
		"pages below a gibibyte":   {func(b *config.Bootstrap) { b.Stream.PagesLogMaxBytes = gib - 1 }, false, "pages_log_max_bytes"},
		"pages at a gibibyte":      {func(b *config.Bootstrap) { b.Stream.PagesLogMaxBytes = gib }, true, ""},
		"pages at 256 GiB":         {func(b *config.Bootstrap) { b.Stream.PagesLogMaxBytes = 256 * gib }, true, ""},
		"pages past 256 GiB":       {func(b *config.Bootstrap) { b.Stream.PagesLogMaxBytes = 256*gib + 1 }, false, "pages_log_max_bytes"},
		"code below a gibibyte":    {func(b *config.Bootstrap) { b.Stream.CodeLogMaxBytes = gib - 1 }, false, "code_log_max_bytes"},
		"code at a gibibyte":       {func(b *config.Bootstrap) { b.Stream.CodeLogMaxBytes = gib }, true, ""},
		"code at a tebibyte":       {func(b *config.Bootstrap) { b.Stream.CodeLogMaxBytes = 1024 * gib }, true, ""},
		"code past a tebibyte":     {func(b *config.Bootstrap) { b.Stream.CodeLogMaxBytes = 1024*gib + 1 }, false, "code_log_max_bytes"},
		// THE IDENTITY LOG AND THE USAGE LOG TAKE A FLOOR OF THEIR OWN,
		// well under the gibibyte the corpus-sized logs take: neither
		// grows with a corpus, and a floor is free space every node must
		// have before it boots. See
		// TestALogThatGrowsWithNoCorpusTakesAFloorFarUnderOneThatDoes.
		"identity below its floor": {func(b *config.Bootstrap) { b.Stream.IamLogMaxBytes = mib*64 - 1 }, false, "iam_log_max_bytes"},
		"identity at its floor":    {func(b *config.Bootstrap) { b.Stream.IamLogMaxBytes = mib * 64 }, true, ""},
		"identity at 16 GiB":       {func(b *config.Bootstrap) { b.Stream.IamLogMaxBytes = 16 * gib }, true, ""},
		"identity past 16 GiB":     {func(b *config.Bootstrap) { b.Stream.IamLogMaxBytes = 16*gib + 1 }, false, "iam_log_max_bytes"},
		"usage below its floor":    {func(b *config.Bootstrap) { b.Stream.UsageLogMaxBytes = mib*64 - 1 }, false, "usage_log_max_bytes"},
		"usage at its floor":       {func(b *config.Bootstrap) { b.Stream.UsageLogMaxBytes = mib * 64 }, true, ""},
		"usage at 64 GiB":          {func(b *config.Bootstrap) { b.Stream.UsageLogMaxBytes = 64 * gib }, true, ""},
		"usage past 64 GiB":        {func(b *config.Bootstrap) { b.Stream.UsageLogMaxBytes = 64*gib + 1 }, false, "usage_log_max_bytes"},
		// THE DASHBOARD ASSISTANT'S LOG TAKES THE SAME 64 MiB: it grows with
		// the questions people ask it, not with a corpus.
		"assistant below its floor": {func(b *config.Bootstrap) { b.Stream.AssistantLogMaxBytes = mib*64 - 1 }, false, "assistant_log_max_bytes"},
		"assistant at its floor":    {func(b *config.Bootstrap) { b.Stream.AssistantLogMaxBytes = mib * 64 }, true, ""},
		"assistant at 64 GiB":       {func(b *config.Bootstrap) { b.Stream.AssistantLogMaxBytes = 64 * gib }, true, ""},
		"assistant past 64 GiB":     {func(b *config.Bootstrap) { b.Stream.AssistantLogMaxBytes = 64*gib + 1 }, false, "assistant_log_max_bytes"},
		// THE ARTIFACTS LOG AND THE ARTIFACT DATASTORE'S TAKE THE CORPUS-SIZED
		// LOGS' GIBIBYTE, each under a ceiling of its own.
		"artifacts below a gibibyte":     {func(b *config.Bootstrap) { b.Stream.ArtifactsLogMaxBytes = gib - 1 }, false, "artifacts_log_max_bytes"},
		"artifacts at a gibibyte":        {func(b *config.Bootstrap) { b.Stream.ArtifactsLogMaxBytes = gib }, true, ""},
		"artifacts at 256 GiB":           {func(b *config.Bootstrap) { b.Stream.ArtifactsLogMaxBytes = 256 * gib }, true, ""},
		"artifacts past 256 GiB":         {func(b *config.Bootstrap) { b.Stream.ArtifactsLogMaxBytes = 256*gib + 1 }, false, "artifacts_log_max_bytes"},
		"artifact data below a gibibyte": {func(b *config.Bootstrap) { b.Stream.ArtifactDataLogMaxBytes = gib - 1 }, false, "artifact_data_log_max_bytes"},
		"artifact data at a gibibyte":    {func(b *config.Bootstrap) { b.Stream.ArtifactDataLogMaxBytes = gib }, true, ""},
		"artifact data at 512 GiB":       {func(b *config.Bootstrap) { b.Stream.ArtifactDataLogMaxBytes = 512 * gib }, true, ""},
		"artifact data past 512 GiB":     {func(b *config.Bootstrap) { b.Stream.ArtifactDataLogMaxBytes = 512*gib + 1 }, false, "artifact_data_log_max_bytes"},
		// THE CHAT LOG TAKES THE CORPUS-SIZED LOGS' GIBIBYTE: it grows with
		// what the company says.
		"chat below a gibibyte": {func(b *config.Bootstrap) { b.Stream.ChatLogMaxBytes = gib - 1 }, false, "chat_log_max_bytes"},
		"chat at a gibibyte":    {func(b *config.Bootstrap) { b.Stream.ChatLogMaxBytes = gib }, true, ""},
		"chat at 256 GiB":       {func(b *config.Bootstrap) { b.Stream.ChatLogMaxBytes = 256 * gib }, true, ""},
		"chat past 256 GiB":     {func(b *config.Bootstrap) { b.Stream.ChatLogMaxBytes = 256*gib + 1 }, false, "chat_log_max_bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			b := config.RunnableBootstrap()
			tc.mutate(&b)
			err := b.Validate()
			if tc.accept {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("refusal = %q, want it to name %q", err, tc.says)
			}
		})
	}
}

// THE SNAPSHOT DIRECTORY DEFAULTS BESIDE THE STORE and resolves a relative
// value against the STORE's directory rather than the process's — the same
// file is applied to a container and run from a shell, and a path resolved
// against the caller's working directory would land somewhere different in
// each.
func TestTheSnapshotDirectoryResolvesAgainstTheStore(t *testing.T) {
	t.Parallel()
	s := config.Store{Path: "/var/lib/crewlet/company.db"}
	if got, want := s.SnapshotDirFor(), filepath.Join("/var/lib/crewlet", "snapshots"); got != want {
		t.Errorf("unset = %q, want %q", got, want)
	}
	s.SnapshotDir = "snaps"
	if got, want := s.SnapshotDirFor(), filepath.Join("/var/lib/crewlet", "snaps"); got != want {
		t.Errorf("relative = %q, want %q", got, want)
	}
	s.SnapshotDir = "/mnt/backups/crewlet"
	if got := s.SnapshotDirFor(); got != "/mnt/backups/crewlet" {
		t.Errorf("absolute = %q", got)
	}
}

// THE BROKER'S OWN STORAGE LIMIT IS BOUNDED, IS EMBEDDED-ONLY, AND CANNOT BE
// SMALLER THAN THE CEILINGS DECLARED INSIDE IT.
//
// # Why each of those is a refusal rather than a warning
//
// This field is the number every stream ceiling on this node's broker is
// compared against, and the failure it prevents is the one that has no
// symptom of its own: a create refused with `insufficient storage resources
// available` — or, on a clustered member, `no suitable peers for placement,
// insufficient storage` — on whichever stream a bring-up happened to reach
// last rather than on the one that is too big.
//
// So a limit smaller than the operator's OWN ceilings cannot be honoured by
// anybody, and both numbers are on the same page of the same file — the
// honest moment to say so is while they are still being written. And on an
// external cluster this reaches nothing at all, which is the rule `url`,
// `store_dir` and `debug` already keep.
func TestTheBrokerStorageLimitIsBoundedEmbeddedOnlyAndFitsItsOwnCeilings(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	const mib = int64(1) << 20
	for name, tc := range map[string]struct {
		mutate func(*config.Bootstrap)
		accept bool
		says   string
	}{
		"unset": {func(*config.Bootstrap) {}, true, ""},
		// SIX: the smallest limit the logs' floors sat inside, with room
		// beside them, while the register held fewer — what a fresh node
		// needs is computed (engine.BrokerFloorFor) and is past it now; see
		// config.StoreMaxBytesFloor.
		"below six gibibytes": {func(b *config.Bootstrap) { b.Stream.StoreMaxBytes = 6*gib - 1 }, false, "store_max_bytes"},
		"at six gibibytes":    {func(b *config.Bootstrap) { b.Stream.StoreMaxBytes = 6 * gib }, true, ""},
		"at 64 TiB":           {func(b *config.Bootstrap) { b.Stream.StoreMaxBytes = 65536 * gib }, true, ""},
		"past 64 TiB":         {func(b *config.Bootstrap) { b.Stream.StoreMaxBytes = 65537 * gib }, false, "store_max_bytes"},
		// THE FLOOR IS WHY THE FLOOR MOVED. The five state-log domains the
		// register held then, at the ceilings they take unset, come to
		// 4.5 GiB, which fits the old five-gibibyte limit — and the
		// cross-field rule below refuses only a sum GREATER than the
		// limit, so at five this document validated and the node was left
		// half a gibibyte for every mailbox, the event stream and every
		// coordination bucket. The floor is what moves that refusal forward
		// to `crewlet validate`. (The eight there are now reserve 7.5 GiB
		// at their smallest, past this floor, which is why the room a fresh
		// node needs is computed and the boot refuses a limit under it.)
		"the five first domains at their unset sizes inside the floor": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 6 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = gib, gib
			b.Stream.PagesLogMaxBytes, b.Stream.UsageLogMaxBytes = gib, gib
			b.Stream.IamLogMaxBytes = 512 * mib
		}, true, ""},
		"the five first domains at their unset sizes inside the old limit": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 5 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = gib, gib
			b.Stream.PagesLogMaxBytes, b.Stream.UsageLogMaxBytes = gib, gib
			b.Stream.IamLogMaxBytes = 512 * mib
		}, false, "store_max_bytes"},
		// EVERY STATE LOG IS IN THE SUM: ceilings that fit exactly, and
		// the one log that tips them past the limit.
		"tipped past it by the knowledge base's log": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 16 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = 8*gib, 4*gib
			b.Stream.PagesLogMaxBytes = 4*gib + 1
		}, false, "pages_log_max_bytes"},
		"tipped past it by the identity log": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 48 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = 32*gib, 16*gib
			b.Stream.IamLogMaxBytes = 64 * mib
		}, false, "iam_log_max_bytes"},
		// OTHERWISE A VALID EXTERNAL DOCUMENT, coordination included: a
		// case whose document has a second problem passes on whichever
		// of the two fires, which would leave this rule uncovered.
		"on an external cluster": {func(b *config.Bootstrap) {
			b.Stream.Type, b.Stream.URL, b.Stream.StoreDir = config.StreamNATS, "nats://broker:4222", ""
			b.Stream.Credentials = "/etc/crewlet/engine.creds"
			b.Coordination.Type = config.CoordinationEmbeddedKV
			b.Stream.StoreMaxBytes = 16 * gib
		}, false, "store_max_bytes"},
		"an external cluster with none of its own": {func(b *config.Bootstrap) {
			b.Stream.Type, b.Stream.URL, b.Stream.StoreDir = config.StreamNATS, "nats://broker:4222", ""
			b.Stream.Credentials = "/etc/crewlet/engine.creds"
			b.Coordination.Type = config.CoordinationEmbeddedKV
		}, true, ""},
		"holding ceilings that fit": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 64 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = 32*gib, 16*gib
		}, true, ""},
		"smaller than its own ceilings": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 16 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = 32*gib, 16*gib
		}, false, "store_max_bytes"},
		"tipped past it by the usage log": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 48 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = 32*gib, 16*gib
			b.Stream.UsageLogMaxBytes = 1 * gib
		}, false, "usage_log_max_bytes"},
		// AND THE CODE LOG: four ceilings that fit exactly, and the fifth
		// tips them past it — the message names it beside the others.
		"tipped past it by the code log": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 49 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = 32*gib, 16*gib
			b.Stream.UsageLogMaxBytes = 1 * gib
			b.Stream.CodeLogMaxBytes = 1 * gib
		}, false, "code_log_max_bytes"},
		"tipped past it by the artifacts log": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 48 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = 32*gib, 16*gib
			b.Stream.ArtifactsLogMaxBytes = 1 * gib
		}, false, "artifacts_log_max_bytes"},
		"tipped past it by the artifact datastore's log": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 48 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = 32*gib, 16*gib
			b.Stream.ArtifactDataLogMaxBytes = 1 * gib
		}, false, "artifact_data_log_max_bytes"},
		"tipped past it by the Dashboard Assistant's log": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 48 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = 32*gib, 16*gib
			b.Stream.AssistantLogMaxBytes = 64 * mib
		}, false, "assistant_log_max_bytes"},
		"tipped past it by the chat log": {func(b *config.Bootstrap) {
			b.Stream.StoreMaxBytes = 48 * gib
			b.Stream.TrackerLogMaxBytes, b.Stream.TrackerVectorsMaxBytes = 32*gib, 16*gib
			b.Stream.ChatLogMaxBytes = 1 * gib
		}, false, "chat_log_max_bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			b := config.RunnableBootstrap()
			tc.mutate(&b)
			err := b.Validate()
			if tc.accept {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("refusal = %q, want it to name %q", err, tc.says)
			}
		})
	}
}

// A STORE LIMIT ON A BROKER WITH NO STORE IS REFUSED WITH THE BROKER.
//
// An embedded server with no `store_dir` holds its streams in MEMORY, where a
// ceiling is reserved against the memory allowance rather than against this
// number, so the pair reads exactly like "I have bounded this node's broker"
// and bounds nothing. That was a warning while such a node was valid; every
// node keeps its state logs on the stream from boot now, so the missing
// directory is refused ([config.Stream.Durable]) and the limit goes with it —
// no deployment is left standing for a warning to describe.
func TestAStoreLimitWithNoStoreDirectoryIsRefusedWithIt(t *testing.T) {
	t.Parallel()
	b := config.RunnableBootstrap()
	b.Stream.StoreDir, b.Stream.StoreMaxBytes = "", 16<<30
	err := b.Validate()
	if err == nil || !strings.Contains(err.Error(), "stream.store_dir") {
		t.Fatalf("Validate = %v, want the missing store directory refused", err)
	}

	// AND A LIMIT BESIDE A STORE DIRECTORY IS NOT WARNED ABOUT, or it is
	// noise on every deployment that configured the field correctly.
	b.Stream.StoreDir = t.TempDir()
	for _, w := range b.Warnings() {
		if strings.Contains(w.Path, "store_max_bytes") {
			t.Errorf("a store limit beside a store directory was warned about: %q",
				w.Message)
		}
	}
}
