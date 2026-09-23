package config

import (
	"path/filepath"
	"strings"
	"time"
)

// The log trim's defaults and bounds.
//
// # Why every one of them is a range rather than a number
//
// Each is a statement about an operator's own estate — their backup cadence,
// their disks, their network — and the shipped value is what a first
// deployment should have rather than what every deployment must have. What
// the bounds do is refuse the values that make the trim meaningless in one
// direction or unrecoverable in the other, and each bound below says which.
const (
	// DefaultMinAge is the age floor no trim may cross.
	DefaultMinAge = 7 * 24 * time.Hour

	// MinAgeFloor is the shortest floor that is still a floor. Below a day
	// it cannot outlast a nightly backup cycle, it stops being the margin
	// that keeps a quiet object writable, and it shortens the window a
	// joining node's vector gap is refilled from.
	MinAgeFloor = 24 * time.Hour

	// MinAgeCeiling is where the honest lever stops being this field and
	// becomes a bigger byte ceiling: a quarter of a year of records is
	// most of a gibibyte of stream at the modelled rate.
	MinAgeCeiling = 90 * 24 * time.Hour

	// DefaultBackupMaxAge is how stale the newest backup may be before the
	// trim stops. A nightly cadence against a fifteen-minute trim.
	DefaultBackupMaxAge = 24 * time.Hour

	// BackupMaxAgeFloor and BackupMaxAgeCeiling bound it. An hour is the
	// shortest cadence any real backup keeps; past a month the field has
	// stopped being a gate.
	BackupMaxAgeFloor   = time.Hour
	BackupMaxAgeCeiling = 30 * 24 * time.Hour

	// DefaultSnapshotInterval is how stale a node's newest snapshot may be
	// before it takes another.
	DefaultSnapshotInterval = 24 * time.Hour

	// SnapshotIntervalFloor and SnapshotIntervalCeiling bound it. Below an
	// hour a full copy of the estate is running more often than it
	// finishes; past a week every snapshot is older than the trim floor.
	SnapshotIntervalFloor   = time.Hour
	SnapshotIntervalCeiling = 7 * 24 * time.Hour

	// DefaultRejoinWindow is the budget for a node to become a complete
	// replica.
	DefaultRejoinWindow = 30 * time.Minute

	// RejoinWindowFloor and RejoinWindowCeiling bound it. Below five
	// minutes no real join finishes; past a day the node is not rejoining,
	// it is being rebuilt.
	RejoinWindowFloor   = 5 * time.Minute
	RejoinWindowCeiling = 24 * time.Hour

	// SnapshotsKept is how many snapshots a node retains, and it appears
	// here because the cross-field rule below is stated in terms of it.
	SnapshotsKept = 1

	// DefaultTrackerVectorsMaxBytes is the vector changelog's ceiling:
	// twice the modelled year-five peak of a model change republishing
	// every source at once.
	DefaultTrackerVectorsMaxBytes int64 = 16 << 30

	// TrackerLogMaxBytesFloor and TrackerLogMaxBytesCeiling bound the
	// mutation log's ceiling.
	TrackerLogMaxBytesFloor   int64 = 1 << 30
	TrackerLogMaxBytesCeiling int64 = 1 << 40

	// TrackerVectorsMaxBytesFloor and TrackerVectorsMaxBytesCeiling bound
	// the vector changelog's.
	TrackerVectorsMaxBytesFloor   int64 = 1 << 30
	TrackerVectorsMaxBytesCeiling int64 = 256 << 30

	// PagesLogMaxBytesFloor and PagesLogMaxBytesCeiling bound the
	// knowledge base's log. The floor is every log's, and the ceiling is a
	// quarter of the mutation log's for the corpus ratio
	// DerivedPagesLogDivisor states.
	PagesLogMaxBytesFloor   int64 = 1 << 30
	PagesLogMaxBytesCeiling int64 = 256 << 30

	// ChartLogMaxBytesFloor and ChartLogMaxBytesCeiling bound the org
	// chart's log, and DefaultChartLogMaxBytes is what an unset value takes.
	//
	// THE DEFAULT IS THE FLOOR, and this is the one state log whose ceiling
	// is not derived from the disk. The other three grow with a corpus the
	// operator's volume has something to say about; a chart does not. It is
	// hundreds of objects — a company's units and its seats — and it changes
	// when somebody is hired, moved or promoted rather than on every comment
	// or every save.
	//
	// # Sixty-four mebibytes, and why it is not the gibibyte the others take
	//
	// At the reference company's rate — a few hundred structural records and
	// a few thousand content records a year, a few kilobytes each, so under
	// twenty mebibytes a year at the pessimistic end — this is FOUR YEARS of
	// a COMPLETELY BLOCKED trim. A blocked trim is not a quiet state: the
	// retention screen names its term from the first tick, a backup behind
	// it that is missing or past the policy raises `backup_age`, and a trim
	// blocked for longer than `min_age` and a tick while its log keeps
	// records older than `min_age` raises `trim_blocked` — so somebody has
	// been told within `min_age` (ninety days at the most) and a couple of
	// trim ticks of the log first keeping what a working trim would have
	// removed, and four years past that is not a window that refuses an
	// append before anybody could act.
	//
	// It took a gibibyte first, on the reasoning that a gibibyte is the
	// framework's own minimum domain ceiling ([engine.MinDomainCeiling]) and
	// there was no smaller number worth having. That reasoning was wrong in
	// a way a test then demonstrated. The floor is a property of the logs it
	// was written for — a mutation log below a gibibyte really is a window
	// that refuses appends within a week — and what it costs is RESERVED
	// BYTES: the broker grants a stream its whole ceiling at create time, so
	// a fourth domain at the same floor raised the free space a node needs
	// to boot at all by a gibibyte, for a log that will not fill one this
	// century. On a 3.4 GiB volume, where three domains fitted, the fourth
	// made the node refuse to start.
	//
	// So a floor is per-log rather than universal, and this one is the
	// chart's own. The engine's scaling already handles it without a change:
	// a log that asks for less than [engine.MinDomainCeiling] keeps what it
	// asked for, because scaling never raises.
	//
	// THE CEILING IS A TYPO GUARD rather than a policy, like the store
	// limit's: 16 GiB is five orders of magnitude past the modelled
	// year-five volume, so anything beyond it is a unit mistake and not a
	// deployment.
	DefaultChartLogMaxBytes int64 = 64 << 20
	ChartLogMaxBytesFloor   int64 = 64 << 20
	ChartLogMaxBytesCeiling int64 = 16 << 30

	// IamLogMaxBytesFloor and IamLogMaxBytesCeiling bound the identity
	// estate's log, and DefaultIamLogMaxBytes is what an unset value takes.
	//
	// THE DEFAULT IS NOT THE FLOOR HERE, which is the difference from the
	// org chart's and the whole of the arithmetic below. Like the chart's,
	// this ceiling is NOT derived from the disk: what it grows with is the
	// company's headcount and how often people sign in, and a volume has
	// nothing to say about either. Unlike the chart's, it genuinely grows.
	//
	// # Half a gibibyte, and where the number comes from
	//
	// SESSIONS DOMINATE. People, credentials and invitations are hundreds
	// of records a year at any company that fits on one broker — the
	// chart's order of magnitude. A session writes one record when it opens
	// and one when it closes and nothing in between, because a rotation id
	// is DERIVED from the lineage and the session's age rather than
	// recorded; so the rate is about (people × sign-ins a day × 2).
	//
	// At the reference company docs/guides/retention.md forecasts — whose
	// 3 000 turns a day and 50 projects put it in the low hundreds of
	// people — a pessimistic three new sessions per person per day is
	// roughly 220 000 session records a year, and at the ~700 bytes a
	// SIGNED record costs on the wire that is about 285 MB a year with
	// everything else folded in. So this default is around eighteen months
	// of a COMPLETELY BLOCKED trim at the pessimistic rate and about five
	// years at a realistic one — and a blocked trim is named on the
	// retention screen from its first tick and raises `trim_blocked` once it
	// has been blocked for longer than `min_age` and a tick while its log
	// keeps records older than `min_age` (a backup behind it that is missing
	// or past the policy raises `backup_age` sooner), so the question is how
	// long somebody has to act on a firing alarm, not how long until anyone
	// notices.
	//
	// IT IS NOT 64 MiB. The chart's number is four years because a chart
	// changes when somebody is hired, moved or promoted; this log moves
	// every morning, and at 64 MiB the pessimistic rate fills it in under
	// three months — a window that could refuse an append before somebody
	// got back from leave.
	//
	// IT IS NOT THE GIBIBYTE [engine.MinDomainCeiling] names either, and
	// the chart's own history is why: the broker grants a stream its whole
	// ceiling at create time, so this number is free space a node must have
	// BEFORE IT CAN BOOT AT ALL, and a fifth domain at the framework floor
	// raises that by a gibibyte for a log that will not fill one. Half is
	// the smallest power of two that clears a year at the pessimistic rate
	// with margin.
	//
	// THE FLOOR IS THE CHART'S so a small company can pay the small price:
	// ten people write a fiftieth of the reference rate, for which 64 MiB
	// is decades. THE CEILING IS A TYPO GUARD rather than a policy, like
	// the store limit's — 16 GiB is orders of magnitude past any modelled
	// volume, so anything beyond it is a unit mistake and not a deployment.
	DefaultIamLogMaxBytes int64 = 512 << 20
	IamLogMaxBytesFloor   int64 = 64 << 20
	IamLogMaxBytesCeiling int64 = 16 << 30

	// DerivedPagesLogDivisor is how much smaller an unset PagesLogMaxBytes
	// is than the mutation log's derived ceiling.
	//
	// FOUR, and the ratio is the corpus rather than a guess: the reference
	// company files 100 000 tasks and 300 000 comments a year against a
	// knowledge base of a few thousand pages, and a page's records are
	// dominated by saves rather than creates. So the knowledge base's log
	// grows at about a quarter of the tracker's rate, and at a quarter of
	// the tracker's ceiling a blocked trim fills both in the same time.
	DerivedPagesLogDivisor = 4

	// DerivedLogMaxBytesFraction is the share of a volume's free space an
	// unset TrackerLogMaxBytes takes, and DerivedLogMaxBytesFloor /
	// DerivedLogMaxBytesCeiling clamp the result.
	//
	// A QUARTER, because the same volume holds the node's own databases
	// and its snapshots — a log allowed to fill the disk takes the store
	// down with it, and the store is where the log is applied TO.
	DerivedLogMaxBytesFraction       = 4
	DerivedLogMaxBytesFloor    int64 = 4 << 30
	DerivedLogMaxBytesCeiling  int64 = 64 << 30

	// StoreMaxBytesFloor and StoreMaxBytesCeiling bound the embedded
	// broker's own declared store limit.
	//
	// THE FLOOR IS FIVE GIBIBYTES, which is the smallest limit the engine's
	// own logs fit inside: FOUR state-log domains, none of which may be
	// sized below TrackerLogMaxBytesFloor, plus the mailboxes, the event
	// stream and every coordination bucket, which reserve nothing and grow
	// against the same number. Below it a node provisions its way to a
	// refusal on whichever stream happens to be last.
	//
	// IT MOVED WITH THE FOURTH DOMAIN. At four gibibytes the org chart's
	// log was the one that did not fit: four explicit floors exactly fill
	// the old limit, so the cross-field check above passes — it refuses
	// only a sum GREATER than the limit — and the node then fails at boot
	// on whichever stream the broker reached last, which is the failure
	// that check exists to move forward to `crewlet validate`.
	//
	// THE CEILING IS A TYPO GUARD rather than a policy: 64 TiB is two
	// orders of magnitude above the largest estate the domain ceilings can
	// describe (1 TiB of mutation log, 256 GiB of vectors), so anything
	// past it is a unit mistake rather than a deployment.
	StoreMaxBytesFloor   int64 = 5 << 30
	StoreMaxBytesCeiling int64 = 64 << 40
)

// MinAge is the trim's age floor as a duration, with the default applied.
func (r TrackerRetention) MinAge() time.Duration {
	return durationOr(r.MinAgeRaw, DefaultMinAge)
}

// BackupMaxAge is how stale the newest backup may be, with the default
// applied.
func (r TrackerRetention) BackupMaxAge() time.Duration {
	return durationOr(r.BackupMaxAgeRaw, DefaultBackupMaxAge)
}

// SnapshotInterval is how stale a node's newest snapshot may be, with the
// default applied.
func (r TrackerRetention) SnapshotInterval() time.Duration {
	return durationOr(r.SnapshotIntervalRaw, DefaultSnapshotInterval)
}

// RejoinWindow is the budget for a node to become a complete replica, with
// the default applied.
func (r TrackerRetention) RejoinWindow() time.Duration {
	return durationOr(r.RejoinWindowRaw, DefaultRejoinWindow)
}

// Floor is whose word the trim takes, with the default applied.
func (r TrackerRetention) Floor() BackupFloor {
	if r.BackupFloor == "" {
		return BackupFloorEngine
	}
	return r.BackupFloor
}

// IsZero lets an unset block drop out of a JSON round trip.
func (r TrackerRetention) IsZero() bool {
	return r.MinAgeRaw == "" && r.BackupMaxAgeRaw == "" && r.BackupFloor == "" &&
		r.SnapshotIntervalRaw == "" && r.RejoinWindowRaw == ""
}

// durationOr parses a configured duration, falling back to the default for an
// empty value. Validation has already established that a non-empty one parses.
func durationOr(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
}

func (r TrackerRetention) validate(path Path) error {
	var p problems
	// The parameter is `name` rather than `field`, which would shadow the
	// package's own path constructor for the whole closure.
	check := func(name, raw string, lo, hi time.Duration, why string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			p.add(at(path, name), ErrShape,
				"%q is not a duration, write it as 24h, 7d is 168h, 30m", raw)
			return
		}
		if d < lo || d > hi {
			p.add(at(path, name), ErrOutOfRange,
				"%s is outside %s..%s: %s", raw, lo, hi, why)
		}
	}

	check("min_age", r.MinAgeRaw, MinAgeFloor, MinAgeCeiling,
		"below a day the floor cannot outlast a nightly backup cycle, it stops "+
			"being the margin that keeps a quiet object writable, and it shortens "+
			"the window a joining node's vectors are refilled from; past a quarter "+
			"of a year the honest lever is stream.tracker_log_max_bytes")
	check("backup_max_age", r.BackupMaxAgeRaw, BackupMaxAgeFloor, BackupMaxAgeCeiling,
		"an hour is the shortest cadence a real backup keeps, and past a month "+
			"this has stopped being a gate on anything")
	check("snapshot_interval", r.SnapshotIntervalRaw, SnapshotIntervalFloor, SnapshotIntervalCeiling,
		"below an hour a full copy of the replicated estate runs more often than "+
			"it finishes; past a week every snapshot is older than the trim floor")
	check("rejoin_window", r.RejoinWindowRaw, RejoinWindowFloor, RejoinWindowCeiling,
		"below five minutes no real join finishes, and past a day a node is not "+
			"rejoining, it is being rebuilt")

	if r.BackupFloor != "" && !containsFloor(r.BackupFloor) {
		p.add(at(path, "backup_floor"), ErrUnknownValue,
			"%q (want %s or %s)", r.BackupFloor, BackupFloorEngine, BackupFloorOperator)
	}

	// THE CROSS-FIELD RULE, and it is what stops the per-field ranges
	// producing an unrecoverable fleet.
	//
	// Both `snapshot_interval: 7d` and `min_age: 24h` are inside their own
	// ranges, and together they make EVERY snapshot older than the trim
	// floor — so a node whose store is lost finds no snapshot it can
	// resume from and has no recovery path at all. The trim would then
	// block for ever on the snapshot term, which is safe and useless.
	if interval, floor := r.SnapshotInterval(), r.MinAge(); interval*(SnapshotsKept+1) >= floor {
		p.add(at(path, "snapshot_interval"), ErrConflict,
			"%s × (SnapshotsKept + 1 = %d) is not less than min_age %s, so every "+
				"snapshot this node keeps would be older than the trim floor and a "+
				"node that lost its store would have nothing to resume from. Lower "+
				"snapshot_interval or raise min_age",
			interval, SnapshotsKept+1, floor)
	}
	return p.err()
}

func containsFloor(f BackupFloor) bool {
	for _, known := range BackupFloors {
		if f == known {
			return true
		}
	}
	return false
}

// bytesInRange refuses a byte ceiling outside its bounds, naming both.
func bytesInRange(p *problems, path Path, name string, v, lo, hi int64) {
	if v == 0 {
		return
	}
	if v < lo || v > hi {
		p.add(at(path, name), ErrOutOfRange,
			"%d is outside %d..%d bytes", v, lo, hi)
	}
}

// DerivedLogMaxBytes is the ceiling an unset TrackerLogMaxBytes takes on a
// volume with free bytes of headroom.
//
// A QUARTER OF FREE SPACE, clamped. The same volume holds this node's own
// databases and its snapshots, so a log allowed to fill the disk takes down
// the store it is applied to; and the clamp is what keeps the derived value
// meaningful on both a laptop and a storage array.
func DerivedLogMaxBytes(free int64) int64 {
	share := free / DerivedLogMaxBytesFraction
	return min(max(share, DerivedLogMaxBytesFloor), DerivedLogMaxBytesCeiling)
}

// LogMaxBytes is the configured ceiling, or the value derived from free, and
// whether it was derived. The second return is what the engine logs when it
// sizes the stream, so a node can say where its ceiling came from.
func (s Stream) LogMaxBytes(free int64) (int64, bool) {
	if s.TrackerLogMaxBytes > 0 {
		return s.TrackerLogMaxBytes, false
	}
	return DerivedLogMaxBytes(free), true
}

// PagesMaxBytes is the knowledge base's log ceiling, and whether it was
// derived.
//
// DERIVED FROM THE MUTATION LOG'S DERIVED VALUE rather than from the disk
// directly, so the two stay in the ratio their corpora grow at on every volume:
// 1 GiB beside the tracker's 4 GiB floor, 16 GiB beside its 64 GiB clamp.
func (s Stream) PagesMaxBytes(free int64) (int64, bool) {
	if s.PagesLogMaxBytes > 0 {
		return s.PagesLogMaxBytes, false
	}
	return DerivedLogMaxBytes(free) / DerivedPagesLogDivisor, true
}

// ChartMaxBytes is the org chart's log ceiling, and whether it was derived.
//
// IT TAKES NO FREE-SPACE ARGUMENT, unlike every other ceiling here, and the
// absence is the statement: this default is a property of the CORPUS and not of
// the disk — see [DefaultChartLogMaxBytes] — so a parameter it ignored would be
// a signature claiming a relationship that does not exist.
//
// The second return is still what the engine logs when it sizes the stream, and
// it still reports `true` for the default: a value nobody wrote is one the
// shared-budget scaling may lower, and one an operator wrote is not.
func (s Stream) ChartMaxBytes() (int64, bool) {
	if s.ChartLogMaxBytes > 0 {
		return s.ChartLogMaxBytes, false
	}
	return DefaultChartLogMaxBytes, true
}

// IamMaxBytes is the identity estate's log ceiling, and whether it was derived.
//
// IT TAKES NO FREE-SPACE ARGUMENT, like the org chart's and unlike every other
// ceiling here, and the absence is the statement: this default is a property of
// the company's HEADCOUNT and its sign-in rate, not of the disk — see
// [DefaultIamLogMaxBytes] — so a parameter it ignored would be a signature
// claiming a relationship that does not exist.
//
// The second return is still what the engine logs when it sizes the stream, and
// it still reports `true` for the default: a value nobody wrote is one the
// shared-budget scaling may lower, and one an operator wrote is not.
func (s Stream) IamMaxBytes() (int64, bool) {
	if s.IamLogMaxBytes > 0 {
		return s.IamLogMaxBytes, false
	}
	return DefaultIamLogMaxBytes, true
}

// VectorsMaxBytes is the vector changelog's ceiling, and whether it was
// derived.
//
// # Why an UNSET value is capped by the disk and a SET one is not
//
// The default is sized for the PEAK rather than the steady state, and the two
// differ by 93x: the stream keeps one message per source and bounds their age,
// so a week's minting is small — but changing the embedding model rewrites
// every source in a few hours, and for the following week every source's
// current message is inside the window. Sizing the default from the steady
// state would refuse the one operation it exists to survive.
//
// That default is a number nobody chose for THIS disk, though, and a broker
// refuses a reservation it cannot back — so an unset value is capped by the
// same share of free space the mutation log derives from, and the node boots.
// An operator who WROTE a number gets it: they named a ceiling for a disk they
// can see, and silently lowering it would be the engine deciding a limit an
// emergency grant had just raised.
func (s Stream) VectorsMaxBytes(free int64) (int64, bool) {
	if s.TrackerVectorsMaxBytes > 0 {
		return s.TrackerVectorsMaxBytes, false
	}
	capped := min(DefaultTrackerVectorsMaxBytes, DerivedLogMaxBytes(free))
	return max(capped, TrackerVectorsMaxBytesFloor), capped != DefaultTrackerVectorsMaxBytes
}

// SnapshotDirFor is where a node keeps its snapshots, with the default
// applied and a relative value resolved against the store's own directory.
//
// RELATIVE TO THE STORE rather than to the process's working directory,
// because an engine's working directory is not something the person who wrote
// the config can see — the same file is applied to a container and run from a
// shell, and a path resolved against the caller's cwd would land somewhere
// different in each.
func (s Store) SnapshotDirFor() string {
	dir := strings.TrimSpace(s.SnapshotDir)
	base := filepath.Dir(s.Path)
	if dir == "" {
		return filepath.Join(base, defaultSnapshotDirName)
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Join(base, dir)
}

// defaultSnapshotDirName is the snapshot directory beside the store.
const defaultSnapshotDirName = "snapshots"
