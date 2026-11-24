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

	// TrackerVectorsMaxBytesFloor and TrackerVectorsMaxBytesCeiling bound the vector
	// changelog's.
	//
	// THE CEILING IS TWO TEBIBYTES because the peak is a DESIGNED operation: a model
	// change puts every source's message inside the window at once, 1.69 GB per 100
	// seats per year of corpus (846 GB for 10 000 seats in their fifth year, 1.69 TB
	// at twice that). Two tebibytes is the next power of two above it, a typo guard
	// at that scale, and far above the most an unset value derives
	// ([Stream.VectorsMaxBytes]).
	TrackerVectorsMaxBytesFloor   int64 = 1 << 30
	TrackerVectorsMaxBytesCeiling int64 = 2 << 40

	// PagesLogMaxBytesFloor and PagesLogMaxBytesCeiling bound the
	// knowledge base's log. The floor is the gibibyte every corpus-sized
	// log takes, and the ceiling is a quarter of the mutation log's for the
	// corpus ratio DerivedPagesLogDivisor states.
	PagesLogMaxBytesFloor   int64 = 1 << 30
	PagesLogMaxBytesCeiling int64 = 256 << 30

	// ArtifactsLogMaxBytesFloor and ArtifactsLogMaxBytesCeiling bound the
	// artifacts log, for the knowledge base's reasons and at its ratio
	// ([DerivedArtifactsLogDivisor]).
	ArtifactsLogMaxBytesFloor   int64 = 1 << 30
	ArtifactsLogMaxBytesCeiling int64 = 256 << 30

	// ArtifactDataLogMaxBytesFloor and ArtifactDataLogMaxBytesCeiling bound
	// the datastore's log: the corpus floor, and half the largest mutation
	// log an operator may write, twice the artifacts log's bound, because a
	// datastore write is a click rather than a publish and a company whose
	// pages write heavily outgrows the derived quarter
	// ([DerivedArtifactDataLogDivisor]).
	ArtifactDataLogMaxBytesFloor   int64 = 1 << 30
	ArtifactDataLogMaxBytesCeiling int64 = 512 << 30

	// NatsObjectsMaxBytesFloor and NatsObjectsMaxBytesCeiling bound the
	// nats object bucket's reservation (store.objects.nats_max_bytes): a
	// gibibyte is the smallest bucket worth reserving a share of the broker
	// for, and the ceiling is the store limit's own typo guard.
	NatsObjectsMaxBytesFloor   int64 = 1 << 30
	NatsObjectsMaxBytesCeiling int64 = 64 << 40

	// ChatLogMaxBytesFloor and ChatLogMaxBytesCeiling bound the chat log's.
	//
	// THE FLOOR IS THE GIBIBYTE every corpus-sized log takes: this log grows with
	// what the company says, and below a gibibyte a blocked trim refuses a message
	// somebody typed within a month at the declared census. Not the identity and
	// usage logs' 64 MiB, which size a headcount and a census.
	//
	// THE CEILING IS A TYPO GUARD: at the supported fifty thousand messages a day
	// (about 75 MB of log, a kibibyte and a half a record) 256 GiB is a decade of a
	// COMPLETELY BLOCKED trim. It equals the knowledge base's by coincidence: that
	// one follows [DerivedPagesLogDivisor], this one the census and a window, so
	// either may move alone.
	ChatLogMaxBytesFloor   int64 = 1 << 30
	ChatLogMaxBytesCeiling int64 = 256 << 30

	// DefaultIamLogMaxBytes is the identity estate's ceiling when unset: half a
	// gibibyte, not the floor and not derived from the disk, since the log grows
	// with headcount and sign-ins. A session writes two records, so the reference
	// company (docs/guides/retention.md) at three new sessions a person a day writes
	// about 285 MB a year at about 700 signed bytes: eighteen months of a COMPLETELY
	// BLOCKED trim at that pessimistic rate, five years at a realistic one, and
	// `trim_blocked` fires long before. At 64 MiB it would fill in under three
	// months.
	//
	// NO BARRIER TERM, by declaration: no identity read is `linearizable`, so the
	// domain declares a read rate of zero (statelog.ReadModel); a reader that starts
	// at that level makes `census_drift` name this field.
	DefaultIamLogMaxBytes int64 = 512 << 20

	// IamLogMaxBytesFloor is 64 MiB, not the gibibyte [engine.MinDomainCeiling]
	// names: the broker grants a stream its whole ceiling at create time, so a floor
	// is free space a node needs before it can boot at all. Ten people write a
	// fiftieth of the reference rate, for which 64 MiB is decades; scaling never
	// raises, so a log asking for less needs no engine change.
	IamLogMaxBytesFloor int64 = 64 << 20

	// IamLogMaxBytesCeiling is a typo guard rather than a policy: 16 GiB is orders
	// of magnitude past any modelled volume, so more is a unit mistake.
	IamLogMaxBytesCeiling int64 = 16 << 30

	// DefaultUsageLogMaxBytes is the usage log's ceiling when unset: a gibibyte. The
	// log is COMPACTED and holds its replay window, one message per (node, company
	// day, seat or schedule) for 181 days, so its size is a census, not a rate:
	// forty seats and twenty schedules on three nodes are 60 × 3 × 181 ≈ 32 600
	// messages at a busy seat-day's ≈ 9.5 KiB (6.5 KiB of spend, turns and reads,
	// about 150 bytes for each of twenty MCP tools), about 320 MB, which a gibibyte
	// holds three times over.
	DefaultUsageLogMaxBytes int64 = 1 << 30

	// UsageLogMaxBytesFloor is 64 MiB, by the per-log rule [IamLogMaxBytesFloor]
	// records: a ten-seat node's census is about 18 MB, and a gibibyte would reserve
	// sixty times what it can write.
	UsageLogMaxBytesFloor int64 = 64 << 20

	// UsageLogMaxBytesCeiling is a typo guard: sixty-four times the default is two
	// orders of magnitude past the reference census.
	UsageLogMaxBytesCeiling int64 = 64 << 30

	// DefaultAssistantLogMaxBytes is the Dashboard Assistant's ceiling when unset:
	// half a gibibyte. THE LOG GROWS WITH HEADCOUNT, NOT THE DISK, so it is a fixed
	// default, as the identity estate's is. A turn writes about eight records, about
	// 20 KiB signed; two hundred people asking ten questions a day write about 40 MB
	// a day, so 512 MiB is two weeks of a COMPLETELY BLOCKED trim against a
	// `trim_blocked` alarm after a day. Conversations go after their own retention
	// whatever the log holds. No seat reads it, so its domain declares a read rate
	// of zero (statelog.ReadModel); the object collector's pin, 25 a day, is its one
	// linearizable reader, about 1.3 MB a year of barriers.
	DefaultAssistantLogMaxBytes int64 = 512 << 20

	// AssistantLogMaxBytesFloor is 64 MiB, by the per-log rule [IamLogMaxBytesFloor]
	// records (ten people write a twentieth of the reference rate: a month). It is
	// also where SCALING STOPS: the engine scales an unset ceiling no lower than
	// [MinDomainCeiling] in internal/engine, but this log's register entry lets it
	// go to the floor, so a fresh node reserves only that and a deployment sized for
	// everything else still boots for a feature it may never turn on (a fixed 512
	// MiB overran crewlet-operator's smallest data volume). The default is what it
	// gets wherever the broker has the room.
	AssistantLogMaxBytesFloor int64 = 64 << 20

	// AssistantLogMaxBytesCeiling is a typo guard: sixty-four times the default is
	// far past any modelled company.
	AssistantLogMaxBytesCeiling int64 = 64 << 30

	// DefaultExecutorLogMaxBytes is the executor log's ceiling when unset (every
	// seat's executor sessions, internal/executordomain): half a gibibyte. THE LOG
	// GROWS WITH THE TURNS SEATS RUN, NOT THE DISK. At thirty turns a seat a day
	// over 100 seats, a turn writes a begin (about 1 KiB), about six captures (about
	// 1 KiB each, and about 250 bytes more for each media object one names), an end
	// (about 3 KiB), a retention delete (about 400 bytes) and five linearizable
	// reads at about 140 bytes each
	// (executordomain.LinearizableReadsPerSeatDay): about 32 MB a day, so 512 MiB is
	// about seventeen days of a COMPLETELY BLOCKED trim against a `trim_blocked`
	// alarm after a day. A record is bounded at executordomain.MaxRecordBytes, 128
	// KiB, but that bounds one record; the census sizes the ceiling.
	DefaultExecutorLogMaxBytes int64 = 512 << 20

	// ExecutorLogMaxBytesFloor is 64 MiB, by the per-log rule [IamLogMaxBytesFloor]
	// records, and where scaling stops, for [AssistantLogMaxBytesFloor]'s reason: a
	// fresh node reserves only the floor, so the smallest volumes crewlet-operator
	// admits (12Gi with objects in S3, 16Gi on the nats bucket) still boot. IT IS
	// NOT THE REFERENCE COMPANY'S: at 32 MB a day the floor holds two days, short of
	// the seven-day `min_age` window, so a company of more than about thirty seats
	// (64 MiB over seven days of 0.32 MB a seat) fills it with every term of the
	// trim satisfied. `log_ceiling_short` names the log; the remedy is this field
	// set outright, which no scaling moves, or a broker with the room (docs:
	// deployment's floors). A 256 MiB floor would hold nine days but cost a fresh
	// node 192 MiB more (internal/engine's broker-floor cases measure it): a choice
	// about which companies the smallest volumes serve.
	ExecutorLogMaxBytesFloor int64 = 64 << 20

	// ExecutorLogMaxBytesCeiling is a typo guard: sixty-four times the default is
	// far past any modelled company.
	ExecutorLogMaxBytesCeiling int64 = 64 << 30

	// DefaultAccessLogMaxBytes is the access log's ceiling when unset (what each
	// seat was told about its access, and every SSH certificate and command:
	// internal/accessdomain). THE LOG GROWS WITH WHAT SEATS ARE TOLD AND THE SSH
	// THEY USE, NOT THE DISK. At 100 seats: sixty linearizable reads a seat a day
	// (accessdomain.LinearizableReadsPerSeatDay) at about 140 bytes, 840 KB a day; a
	// told record of about 8 KiB a few times a week; ten seats at SSH, about one
	// certificate and three commands a turn, 1.5 MB a day. About 2.4 MB a day, so
	// 128 MiB is about seven weeks of a COMPLETELY BLOCKED trim against a
	// `trim_blocked` alarm after a day. Records go with
	// `access_history.retention_days`.
	DefaultAccessLogMaxBytes int64 = 128 << 20

	// AccessLogMaxBytesFloor is 64 MiB, by the per-log rule [IamLogMaxBytesFloor]
	// records, about a month at the reference rate, and where scaling stops, for
	// [AssistantLogMaxBytesFloor]'s reason.
	AccessLogMaxBytesFloor int64 = 64 << 20

	// AccessLogMaxBytesCeiling is a typo guard.
	AccessLogMaxBytesCeiling int64 = 64 << 30

	// DefaultAtlasLogMaxBytes is the atlas log's ceiling when unset (what people
	// record about what the company runs on, with its history:
	// internal/atlasdomain). THE LOG GROWS WITH WHAT PEOPLE RECORD, NOT THE DISK. At
	// 100 seats: about twenty edits a day of about a kibibyte, signed, and thirty
	// barriers a seat a day of about 140 bytes
	// (atlasdomain.LinearizableReadsPerSeatDay): about 450 KB a day, about a
	// mebibyte with the agents' and integrations' records later versions add. So 128
	// MiB is about four months of a COMPLETELY BLOCKED trim against a `trim_blocked`
	// alarm after a day. History goes with `atlas.retention_days`.
	DefaultAtlasLogMaxBytes int64 = 128 << 20

	// AtlasLogMaxBytesFloor is 64 MiB, by the per-log rule [IamLogMaxBytesFloor]
	// records, about two months at the reference rate, and where scaling stops, for
	// [AssistantLogMaxBytesFloor]'s reason.
	AtlasLogMaxBytesFloor int64 = 64 << 20

	// AtlasLogMaxBytesCeiling is a typo guard.
	AtlasLogMaxBytesCeiling int64 = 64 << 30

	// DefaultAdvisorLogMaxBytes is the advisor log's ceiling when unset (the
	// Advisor's findings, runs, reports and alarm episodes, with their history:
	// internal/advisordomain). THE LOG GROWS WITH WHAT THE ADVISOR RECORDS, NOT
	// THE DISK: about 2 KB a record (an episode's body and history line, a run's
	// per-class summary), signed, and a few thousand records a day at the
	// reference company (each class's runs, the refreshes and moves they make,
	// people's decisions), about 6 MB a day. So 64 MiB is about ten days of a
	// COMPLETELY BLOCKED trim against a `trim_blocked` alarm after a day, and
	// the default is its floor: no scaling takes it lower.
	DefaultAdvisorLogMaxBytes int64 = 64 << 20

	// AdvisorLogMaxBytesFloor is 64 MiB, by the per-log rule
	// [IamLogMaxBytesFloor] records.
	AdvisorLogMaxBytesFloor int64 = 64 << 20

	// AdvisorLogMaxBytesCeiling is a typo guard.
	AdvisorLogMaxBytesCeiling int64 = 64 << 30

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

	// CodeLogMaxBytesFloor is the gibibyte every corpus-sized log takes, and the floor of the code
	// log's ceiling.
	CodeLogMaxBytesFloor int64 = 1 << 30

	// CodeLogMaxBytesCeiling is the mutation log's ceiling, because the two grow at the same
	// order. What each takes a day at the reference company is its records plus the barriers its
	// linearizable reads append (176 bytes each, gitlet.BarrierRecordBytes; every figure is of
	// SIGNED records, ADR-0031): the mutation log about 12.1 MB a day: 9.9 MB of records
	// (MEASURED, internal/worklet's "What a day costs the log, measured") and 2.2 MB of
	// barriers, its seats' 12 500 reads and the object collector's 25; the code log 22.7–100.5
	// MB a day: 3 000 pushes at 1 667 bytes (a seat's coding box's push alone, the most a push
	// takes; internal/gitlet's BenchmarkPushLogBytes, its push=box arm beside a person's; 1 528
	// where the engine builds the push for a tool), 5.0 MB; the compactions that merge those
	// pushes' packs, 0.32 MB at 108 bytes a push, and the code-retention duty's sweeps of what
	// they retired, 0.13 MB at 44 (internal/gitlet/compact's BenchmarkCompact steady arm, every
	// merge swept a day later).
	CodeLogMaxBytesCeiling int64 = 1 << 40

	// The code log's day goes on: the publications of their layers, 0.18–3.51 MB; the barriers
	// of its git clients, 2.4–5.0 MB — so a seat's push three times, 9 000 a day, beside thirty
	// people fetching every three minutes of a working day (4 800) and, where one runs, a CI
	// polling ten repositories a minute (14 400); and the records of its 300 changes a day,
	// 14.7–86.6 MB (TestAChangesRecordsCostTheLogWhatTheCodeLogsDayPricesThemAt in
	// internal/gitlet). So the code log takes 1.87–8.27 of the mutation log's day — 0.41 on its
	// push records alone (0.50 of the tracker's records) — and TWO THIRDS IS BELOW THAT RANGE:
	// under a blocked trim a code log at two thirds of the mutation log's ceiling fills in 0.36
	// of the mutation log's time at the least and 0.08 at the most.

	// Even a company whose seats only push takes 0.66–1.15 of the mutation log's day here; two
	// thirds is at the least of that range: such a code log fills in 1.00 of the mutation log's
	// time at the least and 0.58 at the most.

	// DerivedCodeLogNumerator and DerivedCodeLogDenominator: an unset CodeLogMaxBytes is TWO THIRDS of
	// the mutation log's ceiling, set or derived. THE SHARE IS A RESERVATION EVERY NODE MAKES, codeless companies included: a node sizes its
	// logs on its first boot, before the company says whether it hosts code, so the share is
	// weighed against what it costs the logs beside it. Below about 653 GiB free it is room the
	// other logs give up in the fit (docs/guides/replication.md; internal/engine's
	// TestThePagesPrintTheCeilingsAFirstBootCreates computes it): two thirds makes a first boot's
	// mutation log about a fifth shorter, and a year-five re-embedding still fits the vector
	// changelog at 100 GiB free with 13 % of it to spare. At the code log's byte rate every
	// company would pay far more (TestAShareAtTheCodeLogsByteRateCostsEveryCompanyFarMore): at
	// twice the mutation log's ceiling a first boot on 100 GiB free creates the mutation log
	// about 40 % shorter than without the code log, and at three times about 50 % shorter, and
	// the vector changelog beside it too small for a year-five re-embedding.
	DerivedCodeLogNumerator   = 2
	DerivedCodeLogDenominator = 3

	// A COMPANY WHOSE SEATS WILL WORK THROUGH CHANGES SIZES THE CODE LOG IN TWO STEPS. Before
	// the first boot neither log exists and nothing of its own has been measured, so the
	// reference company's day above is the figure it has: it sets stream.code_log_max_bytes at
	// twice the mutation log's ceiling, that day's least — a company whose checks run and whose
	// targets move writes up to about eight times it. A ceiling set comes off the logs' share
	// whole, so the derived logs pay for it: at 100 GiB free a code log set at twice the
	// mutation log beside it is 13.3 GiB, and the mutation log and the vector changelog are each
	// created at 6.6 GiB, too small for a year-five re-embedding (docs/guides/replication.md).
	// Once the streams exist, its own ratio is the two logs' crewlet.statelog.log.bytes_per_day,
	// domain code against domain tracker, and `crewlet retention set-capacity` moves the code
	// log to that share of the mutation log's ceiling, since a log keeps the ceiling it was
	// created with.

	// A HALF SUFFICES WHILE THE CODE LOG TAKES AT MOST HALF OF WHAT THE MUTATION LOG TAKES A DAY, so
	// half the mutation log's ceiling fills no sooner under a blocked trim. That is only a company
	// whose seats open no changes: at the reference company the changes' records alone, 14.7 MB a day
	// at their least, are more than the mutation log's whole day. HALF IS OF THE CEILING THE MUTATION
	// LOG IS CREATED WITH, never of its ask: a ceiling set in stream.code_log_max_bytes comes off the
	// logs' share whole and is never scaled, while the derived ones are scaled into what it leaves, so
	// on a first boot below about 623 GiB free half the mutation log's ask is more than half of what
	// that log is created with beside it — at 100 GiB free, 12½ GiB beside a mutation log created at
	// 6.9 GiB. Before the first boot the half is about 53 % of what the mutation log is created with
	// beside a derived code log, and no more than 32 GiB, half its 64 GiB clamp: at 100 GiB free 4.7
	// GiB, beside which the mutation log is created at 9.5 GiB rather than 9.0. Once the streams exist
	// it is half the ceiling in crewlet.statelog.log.max_bytes.

	// DerivedArtifactsLogDivisor is how much smaller an unset
	// ArtifactsLogMaxBytes is than the mutation log's ceiling: FOUR, the
	// knowledge base's ratio, because a publish is a page-rate event.
	//
	// Its log carries the mutation log's BARRIER TERM as well: a seat's
	// artifact tools read at `linearizable`, so its domain declares the
	// reference rate (statelog.ReadModel) and the term is 125 × 100 × 365 ×
	// 140 B ≈ 639 MB a year at the reference company, which the 16 GiB a
	// quarter of the mutation log's clamp gives it holds many times over.
	DerivedArtifactsLogDivisor = 4

	// DerivedArtifactDataLogDivisor is how much smaller an unset
	// ArtifactDataLogMaxBytes is than the mutation log's ceiling: FOUR, the
	// knowledge base's and the artifacts log's ratio, and the largest share
	// the state logs' one pool can give it on the volume most deployments
	// have. Every unset ceiling is scaled down together when the asks exceed
	// the pool, and at HALF the mutation log's on a 64 GiB volume the
	// vector changelog would be created below the corpus a model change
	// republishes. A company whose pages write heavily sets the field: the
	// bound it accepts is twice the artifacts log's for that reason. Its
	// barrier term is the artifacts log's: a seat's datastore tools read at
	// `linearizable`, about 639 MB a year at the reference company.
	DerivedArtifactDataLogDivisor = 4

	// DerivedChatLogDivisor caps an unset ChatLogMaxBytes at this fraction of what an unset
	// mutation log derives from the volume ([DerivedLogMaxBytes]); the whole 8 GiB ask
	// (`chatlet.ChatLogMaxBytes`) binds no more from 128 GiB free.
	//
	// FOUR, MEASURED: every derived ask is scaled into one budget, so the chat log's ask comes out
	// of the vector changelog's room for a model change. The reference company's fifth-year change
	// puts 7.9 GiB in it and `log_headroom` fires below a tenth of headroom, so it needs 8.8 GiB:
	// a first boot reaches that from about 100 GiB free with the quarter, and on 64 GiB nothing
	// does, so that volume sets `stream.tracker_vectors_max_bytes` (docs/guides/retention.md).
	//
	// The cost: the chat log refuses writes after about 42 days of a blocked trim at the declared
	// census (17 at 50 000 messages a day) rather than 77 (32).
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

	// StoreMaxBytesFloor is six gibibytes, a bound on what the field may say, not what a node
	// needs: every node reserves every state-log domain's ceiling whatever it runs, and left unset
	// the eleven come to 8.625 GiB at their smallest, beside the object store's nats bucket. So a
	// value this floor admits can be short of what a configuration boots in, which is said where
	// it can be acted on: engine.BrokerFloorFor computes the least a fresh node reserves, `crewlet
	// validate` and every start's advisories warn below it, and a refused reservation quotes it.
	StoreMaxBytesFloor int64 = 6 << 30

	// StoreMaxBytesCeiling is a typo guard: 64 TiB is over an order of magnitude above the largest
	// estate the domain ceilings can describe (about 5.45 TiB together), so anything past it is a
	// unit mistake. The object bucket's own bound is this one, and its reservation has to fit
	// inside whatever limit is set: the nats bucket is reserved before any log is sized, a quarter
	// of the limit unless store.objects.nats_max_bytes sets it.
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

// PagesMaxBytes is the knowledge base's log ceiling, and whether it was derived.
//
// DERIVED FROM THE MUTATION LOG'S CEILING, the one an operator wrote or the volume
// derives, not from the disk: both logs hold a trailing window of records as long as a
// blocked trim lasts, so a blocked trim fills both in the same time (1 GiB beside the
// mutation log's 4 GiB derived floor, 16 GiB beside its 64 GiB clamp, 256 GiB beside
// the largest value an operator may write). Deriving from the disk alone kept the
// ratio on no configured deployment.
//
// Held inside the bounds Tier A accepts for the field, so a node never derives a
// ceiling its own validation would refuse: a quarter of the smallest mutation log an
// operator may write is under a gibibyte.
func (s Stream) PagesMaxBytes(free int64) (int64, bool) {
	if s.PagesLogMaxBytes > 0 {
		return s.PagesLogMaxBytes, false
	}
	tracker, _ := s.LogMaxBytes(free)
	return min(max(tracker/DerivedPagesLogDivisor, PagesLogMaxBytesFloor),
		PagesLogMaxBytesCeiling), true
}

// ArtifactsMaxBytes is the artifacts log's ceiling, and whether it was derived:
// a quarter of the mutation log's, set or derived, held inside the bounds Tier
// A accepts for the field, exactly as [Stream.PagesMaxBytes] is.
func (s Stream) ArtifactsMaxBytes(free int64) (int64, bool) {
	if s.ArtifactsLogMaxBytes > 0 {
		return s.ArtifactsLogMaxBytes, false
	}
	tracker, _ := s.LogMaxBytes(free)
	return min(max(tracker/DerivedArtifactsLogDivisor, ArtifactsLogMaxBytesFloor),
		ArtifactsLogMaxBytesCeiling), true
}

// ArtifactDataMaxBytes is the datastore log's ceiling, and whether it was
// derived: a quarter of the mutation log's, set or derived, held inside the
// bounds Tier A accepts for the field.
func (s Stream) ArtifactDataMaxBytes(free int64) (int64, bool) {
	if s.ArtifactDataLogMaxBytes > 0 {
		return s.ArtifactDataLogMaxBytes, false
	}
	tracker, _ := s.LogMaxBytes(free)
	return min(max(tracker/DerivedArtifactDataLogDivisor, ArtifactDataLogMaxBytesFloor),
		ArtifactDataLogMaxBytesCeiling), true
}

// DeclaredLogCeilings is the sum of the state-log ceilings an operator WROTE.
// A derived ceiling is scaled to fit whatever limit is in force, so only the
// explicit ones can conflict with stream.store_max_bytes.
func (s Stream) DeclaredLogCeilings() int64 {
	return s.TrackerLogMaxBytes + s.TrackerVectorsMaxBytes + s.PagesLogMaxBytes +
		s.IamLogMaxBytes + s.UsageLogMaxBytes + s.ArtifactsLogMaxBytes +
		s.ArtifactDataLogMaxBytes + s.AssistantLogMaxBytes + s.ChatLogMaxBytes +
		s.ExecutorLogMaxBytes + s.CodeLogMaxBytes + s.AccessLogMaxBytes + s.AtlasLogMaxBytes +
		s.AdvisorLogMaxBytes
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

// VectorsMaxBytes is the vector changelog's ceiling, and whether it was derived.
//
// SIZED FOR THE PEAK, the whole corpus: a model change rewrites every source in hours, and for
// the next week each one's current message is inside the window (8.46 GB at the reference
// company's fifth year), 93x the steady state an unset value must not be sized from.
//
// FROM THE VOLUME, never the mutation log, whose ceiling bounds a trailing window while this
// peak is everything written since the company began: half of it refused the reference
// company's fifth-year change on 64 GiB. So an unset value takes the same quarter of free
// space the mutation log derives ([DerivedLogMaxBytes]), twice a model change for about 400
// seats in their fifth year at the 64 GiB clamp; a larger company sets the field at about 34
// MB per agent seat-year. A number an operator WROTE is never lowered.
func (s Stream) VectorsMaxBytes(free int64) (int64, bool) {
	if s.TrackerVectorsMaxBytes > 0 {
		return s.TrackerVectorsMaxBytes, false
	}
	return DerivedLogMaxBytes(free), true
}

// UsageMaxBytes is the usage log's ceiling, and whether it was derived.
//
// IT TAKES NO FREE-SPACE ARGUMENT, like the identity estate's: its size is a census,
// not a rate a larger disk should buy more of ([DefaultUsageLogMaxBytes]), so a
// parameter it ignored would claim a relationship that does not exist.
//
// The second return is `true` for the default, as its sibling's is: a value nobody
// wrote is one the shared-budget scaling may lower, and one an operator wrote is not.
func (s Stream) UsageMaxBytes() (int64, bool) {
	if s.UsageLogMaxBytes > 0 {
		return s.UsageLogMaxBytes, false
	}
	return DefaultUsageLogMaxBytes, true
}

// CodeMaxBytes is the code log's ceiling, and whether it was derived.
//
// DERIVED FROM THE MUTATION LOG'S CEILING, set or derived: both logs hold a trailing
// window as long as a blocked trim lasts. The share is two thirds
// ([DerivedCodeLogNumerator]), not the byte rate [Stream.PagesMaxBytes] uses: every
// node reserves this log whether or not the company hosts code, so the share is
// weighed against what it costs the other logs. A company whose seats work through
// changes sets stream.code_log_max_bytes to its own ratio.
//
// Held inside the bounds Tier A accepts for the field, so a node never derives a
// ceiling its own validation would refuse.
func (s Stream) CodeMaxBytes(free int64) (int64, bool) {
	if s.CodeLogMaxBytes > 0 {
		return s.CodeLogMaxBytes, false
	}
	tracker, _ := s.LogMaxBytes(free)
	return min(max(tracker*DerivedCodeLogNumerator/DerivedCodeLogDenominator,
		CodeLogMaxBytesFloor), CodeLogMaxBytesCeiling), true
}

// AssistantMaxBytes is the Dashboard Assistant's log ceiling, and whether it
// was derived: the operator's value, or the fixed default
// ([DefaultAssistantLogMaxBytes]), which reports `true` because a value nobody
// wrote is one the shared-budget scaling may lower.
//
// IT TAKES NO FREE-SPACE ARGUMENT, for [Stream.IamMaxBytes]'s reason: the log
// grows with the company's headcount, which a volume has nothing to say about.
func (s Stream) AssistantMaxBytes() (int64, bool) {
	if s.AssistantLogMaxBytes > 0 {
		return s.AssistantLogMaxBytes, false
	}
	return DefaultAssistantLogMaxBytes, true
}

// ExecutorMaxBytes is the executor log's ceiling, and whether it was derived:
// the operator's value, or the fixed default ([DefaultExecutorLogMaxBytes]),
// which reports `true` because a value nobody wrote is one the shared-budget
// scaling may lower.
//
// IT TAKES NO FREE-SPACE ARGUMENT, for [Stream.IamMaxBytes]'s reason: the log
// grows with the turns the company's seats run, which a volume has nothing to
// say about.
func (s Stream) ExecutorMaxBytes() (int64, bool) {
	if s.ExecutorLogMaxBytes > 0 {
		return s.ExecutorLogMaxBytes, false
	}
	return DefaultExecutorLogMaxBytes, true
}

// AccessMaxBytes is the access log's ceiling, and whether it was derived: the
// operator's value, or the fixed default ([DefaultAccessLogMaxBytes]), which
// reports `true` because a value nobody wrote is one the shared-budget scaling
// may lower. It takes no free-space argument, for [Stream.IamMaxBytes]'s
// reason: the log grows with the seats' records, not with the volume.
func (s Stream) AccessMaxBytes() (int64, bool) {
	if s.AccessLogMaxBytes > 0 {
		return s.AccessLogMaxBytes, false
	}
	return DefaultAccessLogMaxBytes, true
}

// AtlasMaxBytes is the atlas log's ceiling, and whether it was derived: the
// operator's value, or the fixed default ([DefaultAtlasLogMaxBytes]), which
// reports `true` because a value nobody wrote is one the shared-budget scaling
// may lower. It takes no free-space argument, for [Stream.IamMaxBytes]'s
// reason: the log grows with what people record, not with the volume.
func (s Stream) AtlasMaxBytes() (int64, bool) {
	if s.AtlasLogMaxBytes > 0 {
		return s.AtlasLogMaxBytes, false
	}
	return DefaultAtlasLogMaxBytes, true
}

// AdvisorMaxBytes is the advisor log's ceiling, and whether it was derived:
// the operator's value, or the fixed default ([DefaultAdvisorLogMaxBytes]),
// which reports `true` because a value nobody wrote is one the shared-budget
// scaling may lower, to the floor it already is. It takes no free-space
// argument: the log grows with what the Advisor records, not with the volume.
func (s Stream) AdvisorMaxBytes() (int64, bool) {
	if s.AdvisorLogMaxBytes > 0 {
		return s.AdvisorLogMaxBytes, false
	}
	return DefaultAdvisorLogMaxBytes, true
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
