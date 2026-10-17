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
	// scale, and far above the most an unset value derives
	// ([Stream.VectorsMaxBytes]).
	TrackerVectorsMaxBytesFloor   int64 = 1 << 30
	TrackerVectorsMaxBytesCeiling int64 = 2 << 40

	// PagesLogMaxBytesFloor and PagesLogMaxBytesCeiling bound the
	// knowledge base's log. The floor is the gibibyte every corpus-sized
	// log takes, and the ceiling is a quarter of the mutation log's for the
	// corpus ratio DerivedPagesLogDivisor states.
	PagesLogMaxBytesFloor   int64 = 1 << 30
	PagesLogMaxBytesCeiling int64 = 256 << 30

	// ChatLogMaxBytesFloor and ChatLogMaxBytesCeiling bound the chat log's.
	//
	// THE FLOOR IS THE GIBIBYTE every corpus-sized log takes, because this
	// is one: it grows with what the company says, as the mutation log
	// grows with what it files, and below a gibibyte it is a window that
	// refuses a message somebody typed within a month of a blocked trim at
	// the declared census. Not the identity and usage logs' 64 MiB, which
	// size a headcount and a census of objects a small company has few of.
	//
	// THE CEILING IS A TYPO GUARD rather than a sizing: at the supported
	// fifty thousand messages a day (about 75 MB of log a day at a
	// kibibyte and a half a record) 256 GiB is about a decade of a
	// COMPLETELY BLOCKED trim, which no backup gap or donor outage a
	// retention gate tolerates comes near, and far above anything an unset
	// value asks for.
	//
	// THE SAME NUMBER AS THE KNOWLEDGE BASE'S, NOT FOR ITS REASON. That one
	// is a quarter of the mutation log's for the corpus ratio
	// DerivedPagesLogDivisor states; this one follows from the census and a
	// window, so the two are separate constants and either may move without
	// the other.
	ChatLogMaxBytesFloor   int64 = 1 << 30
	ChatLogMaxBytesCeiling int64 = 256 << 30

	// IamLogMaxBytesFloor and IamLogMaxBytesCeiling bound the identity
	// estate's log, and DefaultIamLogMaxBytes is what an unset value takes.
	//
	// THE DEFAULT IS NOT THE FLOOR, and the arithmetic below is why. Like
	// the usage log's, this ceiling is NOT derived from the disk: what it
	// grows with is the company's headcount and how often people sign in,
	// and a volume has nothing to say about either.
	//
	// # Half a gibibyte, and where the number comes from
	//
	// SESSIONS DOMINATE. People, credentials and invitations are hundreds
	// of records a year at any company that fits on one broker. A session
	// writes one record when it opens
	// and one when it closes and nothing in between, because a re-issue
	// moves a deadline inside the signed bearer rather than recording one;
	// so the rate is about (people × sign-ins a day × 2).
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
	// # And no barriers, which is a declaration rather than an omission
	//
	// A `linearizable` read appends a barrier record to the log of the
	// domain it reads, so a log a seat's tools read at that level carries a
	// barrier term: its domain's declared rate per seat (statelog.ReadModel)
	// × seats × 365 days × the ≈ 140 bytes a signed barrier costs. The
	// mutation log's is 125 × 100 × 365 × 140 B ≈ 639 MB a year at the
	// reference company (docs/guides/retention.md) — more than this whole
	// default. THIS log's is zero: no read of the identity estate takes a
	// barrier, because every identity answer is a node's own rows with their
	// lag beside them and a session not yet applied is waited for at the
	// position its bearer states, so its domain declares a rate of zero in
	// the engine's register and the term is 0 × 100 × 365 × 140 B = 0 bytes.
	// The `census_drift` alarm holds the log to that zero and names this
	// field on its first barrier: a reader that starts reading the estate
	// at `linearizable` brings its own bytes into this arithmetic — 5.1 MB a
	// year for every read a day each of the reference company's 100 seats
	// makes.
	//
	// IT IS NOT 64 MiB. This log moves every morning, and at 64 MiB the
	// pessimistic rate fills it in under three months — a window that
	// could refuse an append before somebody got back from leave.
	//
	// IT IS NOT THE GIBIBYTE [engine.MinDomainCeiling] names either,
	// because a FLOOR IS PER LOG: the broker grants a stream its whole
	// ceiling at create time, so this number is free space a node must have
	// BEFORE IT CAN BOOT AT ALL, and a domain at the framework floor raises
	// that by a gibibyte for a log that will not fill one. The framework
	// floor is a property of the corpus-sized logs it was written for — a
	// mutation log below a gibibyte really is a window that refuses appends
	// within a week — and the engine's scaling needs no change for a log
	// that asks for less: scaling never raises. Half a gibibyte is the
	// smallest power of two that clears a year at the pessimistic rate with
	// margin.
	//
	// THE FLOOR IS 64 MiB so a small company can pay the small price: ten
	// people write a fiftieth of the reference rate, for which 64 MiB is
	// decades. THE CEILING IS A TYPO GUARD rather than a policy, like the
	// store limit's — 16 GiB is orders of magnitude past any modelled
	// volume, so anything beyond it is a unit mistake and not a deployment.
	DefaultIamLogMaxBytes int64 = 512 << 20
	IamLogMaxBytesFloor   int64 = 64 << 20
	IamLogMaxBytesCeiling int64 = 16 << 30

	// DefaultUsageLogMaxBytes is the usage log's ceiling, and
	// UsageLogMaxBytesFloor and UsageLogMaxBytesCeiling bound it.
	//
	// # A gibibyte, and the arithmetic behind it
	//
	// The log is COMPACTED and holds its whole replay window: one message
	// per (node, company day, seat or schedule) for the 181 days the
	// domain's history covers, and the stream forgets a day when the rows
	// do. So its size is a census rather than a rate. A three-node fleet of
	// forty seats and twenty schedules is 60 × 3 × 181 ≈ 32 600 messages,
	// and at a busy seat-day's ≈ 6.5 KiB that is about 217 MB: a gibibyte
	// holds that window four times over, which is room for a fleet to grow
	// before anybody has to touch the field. Beside the other logs it is
	// one gibibyte of the 5.5 GiB that six unset ceilings reserve at their
	// smallest, which is what [StoreMaxBytesFloor] is sized from.
	//
	// THE FLOOR IS 64 MiB, by the per-log rule [IamLogMaxBytesFloor]
	// records: the broker grants a stream its whole ceiling when it creates
	// it, so a floor is free space a node must have before it boots, and a
	// ten-seat node's 181-day census — ten seats × 181 days × 6.5 KiB — is
	// about 12 MB. A gibibyte floor would make a small company reserve
	// eighty times what it can ever write.
	//
	// THE CEILING IS A TYPO GUARD rather than a policy: sixty-four times
	// the default is a fleet two orders of magnitude past the reference
	// census, so anything beyond it is a unit mistake.
	DefaultUsageLogMaxBytes int64 = 1 << 30
	UsageLogMaxBytesFloor   int64 = 64 << 20
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
	// ([DerivedLogMaxBytes]) — a sixteenth of the free space, so the whole
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
	// it is created at 9.0 GiB (about 12 % headroom at the peak); at a
	// third, 8.7 (about 9.5 %, under the alarm); uncapped at 8 GiB, 8.2
	// (about 4 %). Of the three, only the quarter keeps that boot clear of
	// the alarm. A first boot clears it from about 62 GiB free, against 57
	// before the chat log joined the budget and 68 uncapped.
	//
	// WHAT IT COSTS the chat log, stated: its horizon under a completely
	// blocked trim on a small volume. Created at 2.25 GiB on that 64 GiB
	// first boot, rather than 4.1 uncapped, it refuses ordinary writes after
	// about 70 days of a fully blocked trim at the declared census rather
	// than 127, and after about 29 at the supported 50 000 messages a day
	// rather than 53 — its read barriers counted in both. The log_headroom
	// alarm names the log a tenth of the way before either, and from
	// 128 GiB free the cap no longer binds.
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
	// THE FLOOR IS SEVEN GIBIBYTES, which is the smallest limit the
	// engine's own logs fit inside with room beside them. Every node
	// reserves all SIX state-log domains' ceilings whatever it runs, and
	// left unset they come to 5.5 GiB at their smallest: the tracker's, the
	// vectors', the knowledge base's and the chat log's at the gibibyte
	// scaling stops at, the usage log's 1 GiB default and the identity
	// log's 512 MiB one (neither scaled below what it asks). What is left,
	// about 1.5 GiB, is for the mailboxes, the event stream and every
	// coordination bucket, which reserve nothing and grow against the same
	// number. Below it a node provisions its way to a refusal on whichever
	// stream happens to be last.
	//
	// SEVEN AND NOT SIX: at six gibibytes the unreserved streams would have
	// half a gibibyte, so a document `crewlet validate` admits — every
	// ceiling unset, which the cross-field check above cannot sum — would
	// fail at boot on whichever stream the broker reached last, which is
	// the failure that check exists to move forward to `crewlet validate`.
	// It was six while five logs reserved 4.5 GiB, and the chat log's
	// gibibyte is what moved it: the 1.5 GiB beside the logs is the margin
	// this floor exists for, so it moves with what they reserve.
	//
	// THE COMPANY'S FILES RESERVE NOTHING HERE: the object store's NATS
	// bucket is created with no byte ceiling, so it grows against this
	// number beside the unreserved streams rather than claiming a share of
	// it up front.
	//
	// THE CEILING IS A TYPO GUARD rather than a policy: 64 TiB is more than
	// an order of magnitude above the largest estate the domain ceilings can
	// describe (1 TiB of mutation log, 2 TiB of vectors, 256 GiB each of
	// knowledge base and chat, 64 GiB of usage and 16 GiB of identity —
	// about 3.6 TiB together), so anything past it is a unit mistake rather
	// than a deployment.
	StoreMaxBytesFloor   int64 = 7 << 30
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
	return min(max(tracker/DerivedPagesLogDivisor, PagesLogMaxBytesFloor),
		PagesLogMaxBytesCeiling), true
}

// IamMaxBytes is the identity estate's log ceiling, and whether it was derived.
//
// IT TAKES NO FREE-SPACE ARGUMENT, like the usage log's and unlike every other
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

// UsageMaxBytes is the usage log's ceiling, and whether it was derived.
//
// IT TAKES NO FREE-SPACE ARGUMENT, like the identity estate's: the stream's size is a census of objects rather than a rate a
// larger disk should buy more of — see [DefaultUsageLogMaxBytes] — so a
// parameter it ignored would be a signature claiming a relationship that does
// not exist.
//
// The second return is what the engine logs when it sizes the stream, and it
// reports `true` for the default, as its sibling's does: a value nobody
// wrote is one the shared-budget scaling may lower, and one an operator wrote
// is not. It used to report whether the operator SET it — the inverse of
// every other ceiling's second return, read by a register that asks the same
// question of every log.
func (s Stream) UsageMaxBytes() (int64, bool) {
	if s.UsageLogMaxBytes > 0 {
		return s.UsageLogMaxBytes, false
	}
	return DefaultUsageLogMaxBytes, true
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
