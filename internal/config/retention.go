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

	// TrackerLogMaxBytesFloor and TrackerLogMaxBytesCeiling bound the
	// mutation log's ceiling.
	TrackerLogMaxBytesFloor   int64 = 1 << 30
	TrackerLogMaxBytesCeiling int64 = 1 << 40

	// TrackerVectorsMaxBytesFloor and TrackerVectorsMaxBytesCeiling bound
	// the vector changelog's.
	//
	// THE CEILING IS TWO TEBIBYTES because the changelog's peak is a
	// DESIGNED operation rather than a failure: a model change puts every
	// source's message inside the window at once, 1.69 GB per 100 seats per
	// year of corpus (8.46 GB at the reference company's fifth year). At
	// the largest company this engine is sized for — 10 000 seats, in its
	// fifth year — that is 846 GB, and twice it is 1.69 TB, which the old
	// 256 GiB bound refused: a model change there overflowed any value an
	// operator was allowed to write from about year one and a half. Two
	// tebibytes is the next power of two above it, a typo guard at that
	// scale, and above the half of the mutation log's own ceiling an
	// unset value can derive ([Stream.VectorsMaxBytes]).
	TrackerVectorsMaxBytesFloor   int64 = 1 << 30
	TrackerVectorsMaxBytesCeiling int64 = 2 << 40

	// PagesLogMaxBytesFloor and PagesLogMaxBytesCeiling bound the
	// knowledge base's log. The floor is every log's, and the ceiling is a
	// quarter of the mutation log's for the corpus ratio
	// DerivedPagesLogDivisor states.
	PagesLogMaxBytesFloor   int64 = 1 << 30
	PagesLogMaxBytesCeiling int64 = 256 << 30

	// DerivedPagesLogDivisor is how much smaller an unset PagesLogMaxBytes
	// is than the mutation log's ceiling.
	//
	// FOUR, and the ratio is the corpus rather than a guess: the reference
	// company files 100 000 tasks and 300 000 comments a year against a
	// knowledge base of a few thousand pages, and a page's records are
	// dominated by saves rather than creates. So the knowledge base's log
	// grows at about a quarter of the tracker's rate, and at a quarter of
	// the tracker's ceiling a blocked trim fills both in the same time.
	DerivedPagesLogDivisor = 4

	// DerivedVectorsLogDivisor is how much smaller an unset
	// TrackerVectorsMaxBytes is than the mutation log's ceiling.
	//
	// TWO, and like the knowledge base's divisor the ratio is the corpus:
	// both logs grow per seat-year, so one number relates them at every
	// company size and every horizon. The mutation log takes 8.13 GB per 100
	// seats a year (the reference company's 156 MB `min_age` window, a week
	// of records, over the year), and that is what its ceiling holds after a
	// trim blocked for as long as the ceiling lasts. The changelog's peak is
	// a model change republishing every source at once, the whole corpus
	// inside the window: 1.69 GB per 100 seats a year of it (8.46 GB at the
	// reference company's fifth year). So whatever horizon a mutation-log
	// ceiling covers, the changelog's peak over that horizon is 0.21 of it,
	// twice the peak — the margin the changelog has always been sized with,
	// because a width change refused partway leaves the corpus in two spaces
	// — is 0.42, and HALF is the smallest whole divisor that covers it.
	//
	// It replaces a FIXED 16 GiB, which was twice the peak of one company
	// (100 seats, fifth year) and so wrong for every other: a 1 000-seat
	// company's model change overflowed it within its first year. The
	// mutation log's ceiling is the one figure Tier A has for how big the
	// company is — derived from the volume the streams live on, or written
	// by an operator who sized it — so deriving from it sizes the changelog
	// for the same company. And under a partitioned layout the default is the
	// DOMAIN's budget, divided evenly across its logs like every other
	// (statelog's Layout.LogShare), so a fixed default shrank every
	// partition's share as the partition count grew while the corpus each
	// partition holds did not.
	DerivedVectorsLogDivisor = 2

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
	// THE FLOOR IS FOUR GIBIBYTES, which is the smallest limit the engine's
	// own logs fit inside: three state-log domains, none of which may be
	// sized below TrackerLogMaxBytesFloor, plus the mailboxes, the event
	// stream and every coordination bucket, which reserve nothing and grow
	// against the same number. Below it a node provisions its way to a
	// refusal on whichever stream happens to be last.
	//
	// THE CEILING IS A TYPO GUARD rather than a policy: 64 TiB is more than
	// an order of magnitude above the largest estate the domain ceilings can
	// describe (1 TiB of mutation log, 2 TiB of vectors, 256 GiB of
	// knowledge base), so anything past it is a unit mistake rather than a
	// deployment.
	StoreMaxBytesFloor   int64 = 4 << 30
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
// DERIVED FROM THE MUTATION LOG'S CEILING — the one an operator wrote, or the
// one the volume derives — rather than from the disk directly, so the two stay
// in the ratio their corpora grow at: 1 GiB beside the mutation log's 4 GiB
// derived floor, 16 GiB beside its 64 GiB clamp, 256 GiB beside the largest
// value an operator may write. It used to take the DERIVED value alone, which
// kept the ratio on every volume and on no configured deployment: an operator
// who sized the mutation log for their company left the knowledge base's log
// sized for their disk.
func (s Stream) PagesMaxBytes(free int64) (int64, bool) {
	if s.PagesLogMaxBytes > 0 {
		return s.PagesLogMaxBytes, false
	}
	return s.corpusShare(free, DerivedPagesLogDivisor,
		PagesLogMaxBytesFloor, PagesLogMaxBytesCeiling), true
}

// VectorsMaxBytes is the vector changelog's ceiling, and whether it was
// derived.
//
// # Sized for the PEAK, from the mutation log's ceiling
//
// The peak and the steady state differ by 93x: the stream keeps one message per
// source and bounds their age, so a week's minting is small — but changing the
// embedding model rewrites every source in a few hours, and for the following
// week every source's current message is inside the window. Sizing an unset
// value from the steady state would refuse the one operation it exists to
// survive.
//
// That peak is the corpus, which grows with the company exactly as the mutation
// log does, so an unset value is HALF THE MUTATION LOG'S CEILING
// ([DerivedVectorsLogDivisor] says why half): the one an operator wrote, or the
// share of the stream volume's free space the mutation log derives. That also
// bounds it by the disk, which a broker's reservation needs — a default nobody
// chose for this volume would be refused on it.
//
// An operator who WROTE a number gets it: they named a ceiling for a broker they
// can see, and silently lowering it would be the engine deciding a limit an
// emergency grant had just raised.
func (s Stream) VectorsMaxBytes(free int64) (int64, bool) {
	if s.TrackerVectorsMaxBytes > 0 {
		return s.TrackerVectorsMaxBytes, false
	}
	return s.corpusShare(free, DerivedVectorsLogDivisor,
		TrackerVectorsMaxBytesFloor, TrackerVectorsMaxBytesCeiling), true
}

// corpusShare is an unset corpus log's ceiling: the mutation log's ceiling —
// the one an operator wrote, or the one the volume derives — over the ratio
// the two corpora grow at, held inside the bounds Tier A accepts for the
// field, so a node never derives a ceiling its own validation would refuse to
// be told.
func (s Stream) corpusShare(free int64, divisor int, floor, ceiling int64) int64 {
	tracker, _ := s.LogMaxBytes(free)
	return min(max(tracker/int64(divisor), floor), ceiling)
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

// ObjectsDirFor is where the chunks are kept, with the default applied and a
// relative value resolved against the store's directory — see
// [Store.SnapshotDirFor] for why the store's and not the working directory.
func (s Store) ObjectsDirFor() string {
	dir := strings.TrimSpace(s.Objects.Dir)
	base := filepath.Dir(s.Path)
	if dir == "" {
		return filepath.Join(base, defaultObjectsDirName)
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Join(base, dir)
}

// defaultObjectsDirName is the chunk directory beside the store.
const defaultObjectsDirName = "objects"
