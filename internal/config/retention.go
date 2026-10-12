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

	// LogMaxBytesFloor is the floor EVERY state log's byte ceiling shares,
	// and it is ONE constant because there is no honest reason for the
	// five to differ: below a gibibyte a log is not a log, it is a window
	// that refuses appends within a week of a company starting work, and
	// that sentence is about the mechanism rather than about any one
	// domain's corpus. It was four constants of the same value — each
	// doc comment asserting it matched the others, with nothing enforcing
	// it — which is the shape internal/textcut and internal/whsec each
	// exist to record.
	//
	// The engine scales a DERIVED ceiling down to this same number and no
	// lower (`engine.MinDomainCeiling`), so an operator who sets a value
	// and an operator who sets none reach the same floor.
	LogMaxBytesFloor int64 = 1 << 30

	// TrackerLogMaxBytesCeiling bounds the mutation log's ceiling from
	// above. A tebibyte, because the mutation log is the one an operator
	// genuinely sizes into a storage array.
	TrackerLogMaxBytesCeiling int64 = 1 << 40

	// TrackerVectorsMaxBytesCeiling bounds the vector changelog's.
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
	// scale, and far above the most an unset value derives
	// ([Stream.VectorsMaxBytes]).
	TrackerVectorsMaxBytesCeiling int64 = 2 << 40

	// PagesLogMaxBytesCeiling bounds the knowledge base's log, at a quarter
	// of the mutation log's for the corpus ratio DerivedPagesLogDivisor
	// states.
	PagesLogMaxBytesCeiling int64 = 256 << 30

	// ChatLogMaxBytesCeiling bounds the chat log's from above, and it is a
	// TYPO GUARD rather than a sizing: at the supported fifty thousand
	// messages a day (about 75 MB of log a day at a kibibyte and a half a
	// record) 256 GiB is about a decade of a COMPLETELY BLOCKED trim, which
	// no backup gap or donor outage a retention gate tolerates comes near,
	// and far above anything an unset value asks for.
	//
	// THE SAME NUMBER AS THE KNOWLEDGE BASE'S, NOT FOR ITS REASON. That one
	// is a quarter of the mutation log's for the corpus ratio
	// DerivedPagesLogDivisor states; this one follows from the census and a
	// window, so the two are separate constants and either may move without
	// the other.
	ChatLogMaxBytesCeiling int64 = 256 << 30

	// DefaultUsageLogMaxBytes is the usage log's ceiling: four times the
	// modelled 181-day census of a three-node, sixty-object fleet (about
	// 217 MB), and the floor every state log shares.
	DefaultUsageLogMaxBytes int64 = 1 << 30

	// UsageLogMaxBytesCeiling bounds it from above: sixty-four times the
	// default, which is a fleet two orders of magnitude past the census
	// before any typo.
	UsageLogMaxBytesCeiling int64 = 64 << 30

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

	// DerivedChatLogDivisor caps an unset ChatLogMaxBytes on a small
	// volume: the chat log asks for its domain's excursion budget
	// (`chat.ChatLogMaxBytes`, 8 GiB), but never for more than this
	// fraction of what an unset mutation log derives from the same volume
	// ([DerivedLogMaxBytes]) — a sixteenth of the free space, so the full
	// ask from 128 GiB free and 4 GiB on a 64 GiB volume.
	//
	// FOUR, MEASURED rather than borrowed from the knowledge base's ratio,
	// although it is the same number. Every derived ask is scaled into one
	// budget in proportion, so what the chat log asks for comes out of the
	// vector changelog's room for a model change — the excursion that log
	// is sized for, and one designed not to alarm. On a 64 GiB first boot
	// under an embedded broker the logs share 24 GiB; the reference
	// company's fifth-year model change puts 7.9 GiB in the changelog, and
	// `log_headroom` fires below a tenth of headroom, so the changelog has
	// to be created at 8.8 GiB or more. With chat's ask capped at a quarter
	// it is created at 9.2 GiB (about 14 % headroom at the peak); at a
	// third, 8.9 (about 11 %, a margin thinner than the corpus estimate
	// it rests on); uncapped at 8 GiB, 8.4 (about 6 %, under the alarm). A
	// first boot clears the alarm from about 62 GiB free, against 56
	// before the chat log joined the budget and 67 uncapped.
	//
	// WHAT IT COSTS the chat log, stated: its horizon under a completely
	// blocked trim on a small volume. Created at 2.3 GiB on that 64 GiB
	// first boot, rather than 4.2 uncapped, it refuses ordinary writes after
	// about 77 days of a fully blocked trim at the declared census rather
	// than 140, and after about 31 at the supported 50 000 messages a day
	// rather than 56. The log_headroom alarm names the log a tenth of the way
	// before either, and from 128 GiB free the cap no longer binds.
	DerivedChatLogDivisor = 4

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
	// THE FLOOR IS SIX GIBIBYTES, which is the smallest limit the engine's
	// own logs fit inside: five state-log domains, none of which may be
	// sized below LogMaxBytesFloor, plus a gibibyte for the
	// mailboxes, the event stream and every coordination bucket, which
	// reserve nothing and grow against the same number. Below it a node
	// provisions its way to a refusal on whichever stream happens to be
	// last. It was five while there were four; beside the chat log's
	// gibibyte, five would leave the unreserved streams nothing.
	//
	// THE CEILING IS A TYPO GUARD rather than a policy: 64 TiB is more than
	// an order of magnitude above the largest estate the domain ceilings can
	// describe (1 TiB of mutation log, 2 TiB of vectors, 256 GiB each of
	// knowledge base and chat, 64 GiB of usage — about 3.6 TiB together), so
	// anything past it is a unit mistake rather than a deployment.
	StoreMaxBytesFloor   int64 = 6 << 30
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
//
// BOTH BOUNDS ARE PARAMETERS, although every state log shares
// [LogMaxBytesFloor]: `store_max_bytes` bounds the WHOLE embedded broker
// rather than one log, so its floor is six gibibytes against a log's one,
// and hard-coding the log's would admit a broker limit of as little as one
// gibibyte, which the five logs' own floors cannot fit inside.
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
// in the ratio their records grow at: 1 GiB beside the mutation log's 4 GiB
// derived floor, 16 GiB beside its 64 GiB clamp, 256 GiB beside the largest
// value an operator may write. The ratio holds at every horizon because both
// logs hold the same kind of thing — a trailing window of records, as long as
// a blocked trim lasts — so a blocked trim fills both in the same time. It used
// to take the DERIVED value alone, which kept the ratio on every volume and on
// no configured deployment: an operator who sized the mutation log for their
// company left the knowledge base's log sized for their disk.
//
// Held inside the bounds Tier A accepts for the field, so a node never derives
// a ceiling its own validation would refuse to be told: a quarter of the
// smallest mutation log an operator may write is under a gibibyte.
func (s Stream) PagesMaxBytes(free int64) (int64, bool) {
	if s.PagesLogMaxBytes > 0 {
		return s.PagesLogMaxBytes, false
	}
	tracker, _ := s.LogMaxBytes(free)
	return min(max(tracker/DerivedPagesLogDivisor, LogMaxBytesFloor),
		PagesLogMaxBytesCeiling), true
}

// VectorsMaxBytes is the vector changelog's ceiling, and whether it was
// derived.
//
// # Sized for the PEAK, and the peak is the whole corpus
//
// The peak and the steady state differ by 93x: the stream keeps one message per
// source and bounds their age, so a week's minting is small — but changing the
// embedding model rewrites every source in a few hours, and for the following
// week every source's current message is inside the window: the whole corpus,
// about 17 MB per agent seat per year of the company's history (8.46 GB for the
// reference company's 100 seats in its fifth year). Sizing an unset value from
// the steady state would refuse the one operation it exists to survive.
//
// # From the VOLUME, as the mutation log is, and never from the mutation log
//
// The two hold different things. The mutation log's ceiling bounds a TRAILING
// WINDOW — the records of however long a blocked trim lasts — while the
// changelog's peak is everything the company has written since it began. So
// no ratio relates them: the corpus over a mutation-log ceiling is 0.21 times
// the company's age over that ceiling's horizon, which grows every year a
// company trims healthily. Half the mutation log's ceiling was tried on that
// ratio, and on the volumes most deployments have it was half what it
// replaced: the reference company on 64 GiB free had its fifth-year model
// change refused partway, and an operator who set a small mutation log for a
// fleet that trims shrank the changelog with it.
//
// What the two logs share is the volume, so an unset changelog asks for the
// same quarter of its free space the mutation log derives ([DerivedLogMaxBytes]),
// whatever the mutation log is set to: on every volume at least what the fixed
// 16 GiB default capped by that same share asked for, and more wherever the
// volume can back more. At the 64 GiB clamp it holds a model change twice over
// for about 2 000 seat-years of corpus — 400 seats in their fifth year. A
// larger or older company sets the field, at about 34 MB per agent seat per
// year of history (twice the corpus), and the headroom alarm says when one has
// outgrown it.
//
// An operator who WROTE a number gets it: they named a ceiling for a broker they
// can see, and silently lowering it would be the engine deciding a limit an
// emergency grant had just raised.
func (s Stream) VectorsMaxBytes(free int64) (int64, bool) {
	if s.TrackerVectorsMaxBytes > 0 {
		return s.TrackerVectorsMaxBytes, false
	}
	return DerivedLogMaxBytes(free), true
}

// UsageMaxBytes is the usage log's ceiling, and whether the operator set it.
//
// NOT DERIVED FROM THE DISK, unlike the mutation log's: the default is already
// the floor every state log shares, so a disk-derived value could only ever
// equal it — and the stream's size is a census of objects rather than a rate
// a larger disk should buy more of.
func (s Stream) UsageMaxBytes() (int64, bool) {
	if s.UsageLogMaxBytes > 0 {
		return s.UsageLogMaxBytes, true
	}
	return DefaultUsageLogMaxBytes, false
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
