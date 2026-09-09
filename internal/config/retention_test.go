package config_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// retentionBoot is a valid Tier A with a retention block to vary.
func retentionBoot(t *testing.T, r config.TrackerRetention) config.Bootstrap {
	t.Helper()
	b := config.DefaultBootstrap()
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
	shipped := config.DefaultBootstrap()
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
	// records on the stream, so it can answer "where did this ceiling come
	// from" a year later.
	var s config.Stream
	if got, derived := s.LogMaxBytes(200 * gib); got != 50*gib || !derived {
		t.Errorf("unset = (%d, %v), want the derived value", got, derived)
	}
	s.TrackerLogMaxBytes = 32 * gib
	if got, derived := s.LogMaxBytes(200 * gib); got != 32*gib || derived {
		t.Errorf("set = (%d, %v), want the configured value", got, derived)
	}
}

// THE VECTOR CHANGELOG IS SIZED FOR THE PEAK, and the peak is a model change
// republishing every source at once — 93× the steady state. A default sized
// from the steady state would refuse the one operation it exists to survive.
//
// AND THE PEAK IS CAPPED BY THE DISK, which is the other half: the default is
// a number nobody chose for this volume, and a broker refuses a reservation it
// cannot back — so a node with a small disk gets a ceiling that fits and boots,
// rather than a ceiling that is right in principle and a refusal in fact.
func TestTheVectorCeilingIsSizedForAModelChange(t *testing.T) {
	t.Parallel()
	// The modelled year-five peak: every source's current message inside
	// the window at once.
	const yearFivePeak = int64(8_460_000_000)
	// A volume with room for it.
	const roomy = int64(512) << 30
	var s config.Stream
	got, capped := s.VectorsMaxBytes(roomy)
	if float64(got) < 1.5*float64(yearFivePeak) {
		t.Errorf("the default vector ceiling is %d on a roomy volume, under "+
			"1.5× the modelled year-five peak of %d — a width change would be "+
			"refused partway through", got, yearFivePeak)
	}
	if capped {
		t.Error("the default was reported as capped on a volume with room for it")
	}

	// AND ON A SMALL DISK IT IS CAPPED AND SAYS SO.
	small, capped := config.Stream{}.VectorsMaxBytes(8 << 30)
	if !capped {
		t.Error("a ceiling the disk cannot back was not reported as capped")
	}
	if small >= config.DefaultTrackerVectorsMaxBytes {
		t.Errorf("the capped ceiling is %d, which is not below the default %d",
			small, config.DefaultTrackerVectorsMaxBytes)
	}
	if small < config.TrackerVectorsMaxBytesFloor {
		t.Errorf("the capped ceiling is %d, under the floor %d — below it a log "+
			"is a window that refuses appends within a week",
			small, config.TrackerVectorsMaxBytesFloor)
	}

	// AN OPERATOR'S OWN NUMBER IS NOT CAPPED. They named a limit for a
	// broker they can see, and silently lowering it would be the engine
	// deciding a limit an emergency grant had just raised.
	s.TrackerVectorsMaxBytes = 2 << 30
	if got, capped := s.VectorsMaxBytes(1 << 30); got != 2<<30 || capped {
		t.Errorf("a configured ceiling read back as %d (capped %v)", got, capped)
	}
}

// THE BYTE CEILINGS ARE BOUNDED, and the bounds are what the schema
// publishes.
func TestTheByteCeilingsAreBounded(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
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
		"vectors at 256 GiB":       {func(b *config.Bootstrap) { b.Stream.TrackerVectorsMaxBytes = 256 * gib }, true, ""},
		"vectors past 256 GiB":     {func(b *config.Bootstrap) { b.Stream.TrackerVectorsMaxBytes = 257 * gib }, false, "tracker_vectors_max_bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			b := config.DefaultBootstrap()
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
