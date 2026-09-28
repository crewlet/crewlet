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
	b := config.DefaultBootstrap()
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
	for name, tc := range map[string]struct {
		mutate func(*config.Bootstrap)
		accept bool
		says   string
	}{
		"unset":                {func(*config.Bootstrap) {}, true, ""},
		"below four gibibytes": {func(b *config.Bootstrap) { b.Stream.StoreMaxBytes = 4*gib - 1 }, false, "store_max_bytes"},
		"at four gibibytes":    {func(b *config.Bootstrap) { b.Stream.StoreMaxBytes = 4 * gib }, true, ""},
		"at 64 TiB":            {func(b *config.Bootstrap) { b.Stream.StoreMaxBytes = 65536 * gib }, true, ""},
		"past 64 TiB":          {func(b *config.Bootstrap) { b.Stream.StoreMaxBytes = 65537 * gib }, false, "store_max_bytes"},
		// OTHERWISE A VALID EXTERNAL DOCUMENT, coordination included: a
		// case whose document has a second problem passes on whichever
		// of the two fires, which would leave this rule uncovered.
		"on an external cluster": {func(b *config.Bootstrap) {
			b.Stream.Type, b.Stream.URL, b.Stream.StoreDir = config.StreamNATS, "nats://broker:4222", ""
			b.Coordination.Type = config.CoordinationEmbeddedKV
			b.Stream.StoreMaxBytes = 16 * gib
		}, false, "store_max_bytes"},
		"an external cluster with none of its own": {func(b *config.Bootstrap) {
			b.Stream.Type, b.Stream.URL, b.Stream.StoreDir = config.StreamNATS, "nats://broker:4222", ""
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

// A STORE LIMIT ON A BROKER WITH NO STORE BOUNDS NOTHING, and says so.
//
// An embedded server with no `store_dir` holds its streams in MEMORY, where a
// ceiling is reserved against the memory allowance rather than against this
// number. The configuration is valid — a test, an ingress-only node — so it is
// a warning; what it must not be is silent, because the pair reads exactly
// like "I have bounded this node's broker".
func TestAStoreLimitWithNoStoreDirectoryIsCalledOut(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Stream.StoreDir, b.Stream.StoreMaxBytes = "", 16<<30
	if err := b.Validate(); err != nil {
		t.Fatalf("refused a valid document: %v", err)
	}

	var found bool
	for _, w := range b.Warnings() {
		if strings.Contains(w.Path, "store_max_bytes") {
			found = true
			if !strings.Contains(w.Message, "memory") {
				t.Errorf("the warning does not say what does bound those "+
					"streams: %q", w.Message)
			}
		}
	}
	if !found {
		t.Error("a store limit on an in-memory broker was not mentioned at all")
	}

	// AND IT IS NOT RAISED ON A BROKER THAT HAS A STORE, or it is noise on
	// every deployment that configured the field correctly.
	b.Stream.StoreDir = t.TempDir()
	for _, w := range b.Warnings() {
		if strings.Contains(w.Path, "store_max_bytes") {
			t.Errorf("a store limit beside a store directory was warned about: %q",
				w.Message)
		}
	}
}
