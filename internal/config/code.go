package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// The code host's limits that both tiers and the domain read: config VALIDATES
// a document against them and internal/code DECIDES a push with them, and code
// imports config, so they are defined here once. internal/code names each of
// them again as a constant of its own, which is the same value rather than a
// copy — a Go constant alias cannot drift.
const (
	// CodeMaxPushBytesCeiling is the largest pack one push may send,
	// whatever code.limits.max_push_bytes says — equal to
	// tracker.MaxFileBytes, the engine's one "largest object". What a
	// push STORES may be larger (internal/code.MaxPackBytes: a thin pack
	// completed against the repository, with its indexes), and
	// internal/objstore/references pins that larger figure against the
	// collector's write bound.
	CodeMaxPushBytesCeiling int64 = 1 << 30

	// DefaultCodeMaxPushBytes is an unset code.limits.max_push_bytes: the
	// ceiling itself, because a person's first push of an existing project
	// is its whole history.
	DefaultCodeMaxPushBytes int64 = 1 << 30

	// DefaultCodeMaxBlobBytes is an unset code.limits.max_blob_bytes:
	// 50 MiB. A larger binary belongs in tracker files, and the bound is
	// what sizes pack-objects' window memory on a git node.
	DefaultCodeMaxBlobBytes int64 = 50 << 20

	// DefaultCodeRepoMaxBytes is an unset code.limits.repo_max_bytes:
	// 20 GiB — the large reference model's 12 GB monorepo with two thirds
	// of headroom. Every git node serving a repository caches it whole, so
	// this is also what one repository may take of each such node's disk.
	// And of ONE node's memory: a repository's bitmap is written once for
	// the fleet, by the first git node whose background budget holds its
	// publication — internal/code/cache's PublishBytesPerObject an object
	// with a clone's room beside it, 840 bytes measured on the node pin —
	// some 50 GB at this size's 60 million objects, which the large git node
	// docs/guides/deployment.md names (96 GiB and 16 cores, about 68 GiB of
	// background budget) holds. So the size is KEPT when weighed against
	// memory as well: one such node gives the whole fleet the bitmap, and a
	// fleet without one still serves the repository, without a bitmap, and
	// refuses it nothing.
	DefaultCodeRepoMaxBytes int64 = 20 << 30

	// DefaultCodeDeletedRetentionDays is an unset
	// code.deleted_retention_days: 7, the trim's default min_age, so a
	// deletion is undone from the same window the log can replay.
	DefaultCodeDeletedRetentionDays = 7

	// CodeMaxProtections is the most protection rules one repository
	// carries: every matching rule is evaluated per command inside the push
	// decide, and 64 is the scope-term cap of the other domains.
	CodeMaxProtections = 64

	// CodeMaxPathOwners is the most PATH OWNERS one repository names — units
	// and seats, each owning the paths its globs match: as many as it carries
	// protection rules ([CodeMaxProtections]). A path owner is whom a change
	// touching its paths asks for a review, so this is also the most review
	// requests one change's paths can make; sixty-four teams or seats is past
	// any company's on one repository.
	CodeMaxPathOwners = 64

	// CodeMaxPathOwnerPaths is the most globs one path owner names: a team's
	// part of a tree is a handful of directories, each one glob (dir/**), and
	// thirty-two is past what one team owns of one repository. With
	// [CodeMaxPathOwners] it bounds what a change's paths are matched against
	// — 2 048 globs.
	CodeMaxPathOwnerPaths = 32

	// CodeMaxPathOwnersBytes is the most a repository's path owners may weigh
	// as the code log records them: their JSON, each owner by what the chart
	// resolves it to, a unit by its key. TWO HUNDRED AND FIFTY-SIX KIBIBYTES,
	// because every create, settings change and restore carries them whole,
	// and every record the code domain forms is held to its MaxRecordBytes —
	// an eighth of the node's appends in flight on the code log, 1 044 480
	// bytes — beside the rest of the settings at their widest: sixty-four
	// rules with their merge gates, the description and the default branch
	// make such a record 631 780 bytes, so path owners at this bound make it
	// 893 939, some 150 KB to spare for whatever a later field adds
	// (internal/code's TestTheLargestSettingsFitTheirRecordAndTheDetail holds
	// the sum). The counts alone would not: 2 048 globs at gitref's 128 bytes,
	// each byte JSON spells as six (`&`, `<` and `>`), weigh 1 565 505 bytes,
	// and made such a record 2 197 300. Generous for any ownership a person
	// writes — 2 048 globs of a hundred plain bytes each are about 214 KB — so
	// what it refuses is the counts at their bounds with long globs, or globs
	// JSON spells long.
	CodeMaxPathOwnersBytes = 256 << 10

	// CodeMaxHumanPaths is the most HUMAN PATHS one protection rule's merge
	// gate names: the parts of a repository's tree a change touching which
	// waits for a person's approval — `.crewlet/**`, `deploy/**`,
	// `migrations/**`. A JUDGMENT: a gate keeps a person in front of a few
	// directories, and thirty-two is past any rule's; it is one of the two
	// lists a gate's bytes grow with ([CodeMaxGateBytes]).
	CodeMaxHumanPaths = 32

	// CodeMaxRequiredChecks is the most checks one rule's merge gate
	// requires, by name: a target's checks are its tests split a few ways —
	// unit, lint, integration, a build a platform — and thirty-two is past any
	// CI's required set. A JUDGMENT, as [CodeMaxHumanPaths] is, and the other
	// list a gate's bytes grow with.
	CodeMaxRequiredChecks = 32

	// CodeMaxGateBytes is the most one rule's merge gate may weigh as the
	// code log records it: its canonical JSON. EIGHT KIBIBYTES, because a
	// repository carries up to [CodeMaxProtections] rules and every create,
	// settings change and restore carries them whole: sixty-four gates at the
	// bound are 512 KiB of the settings, beside path owners at theirs
	// ([CodeMaxPathOwnersBytes]), under the one record the code domain forms
	// (its MaxRecordBytes, about 1 MB) — and of the repository row every read,
	// sync and cache state carries. Each count alone stays under it —
	// thirty-two human paths of gitref's widest ordinary globs (128 bytes)
	// with both classes in `unless` are about 6 KB, and thirty-two checks at
	// their longest names about 2 KB — so what it refuses is a gate at both
	// counts' bounds at once, or one whose globs JSON spells long (`&`, `<`
	// and `>` are six bytes each).
	CodeMaxGateBytes = 8 << 10

	// CodeMaxQueueDepth is the most candidates a gated target's speculative
	// queue tests at once — each the target's tip with the next waiting
	// changes merged on it. EIGHT: at a 5 % chance that a change's checks
	// fail, a round of eight lands whole under two times in three
	// (0.95⁸ = 66 %), and past it the candidates a red one rebuilds cost
	// more than the round lands.
	CodeMaxQueueDepth = 8

	// CodeDefaultQueueDepth is an unset landing.depth: FOUR candidates at
	// once, a round of which lands whole 81 % of the time at the same 5 %
	// (0.95⁴) — the rate the queue is sized by.
	CodeDefaultQueueDepth = 4

	// CodeMaxQueueBatch is the most waiting changes one candidate takes (a
	// red batch is bisected): eight, so a round at both bounds is sixty-four
	// changes — the most one candidate's record names.
	CodeMaxQueueBatch = 8

	// CodeDefaultQueueBatch is an unset landing.batch: FOUR changes a
	// candidate, PROVISIONAL. A batch of four is green 81 % of the time at
	// the 5 % failure rate the depth is sized by, a red one's culprit is
	// found in log₂ 4 = 2 more rounds, and it spends about 0.4 check runs a
	// landed change against 1.05 unbatched; the queue's throughput
	// benchmark, which lands with the queue that reads this, decides it on
	// measurement.
	CodeDefaultQueueBatch = 4

	// CodeMaxCheckNameBytes is the longest name a check takes: sixty-four
	// bytes of [ValidCodeCheckName]'s alphabet, a CI job's name rather than a
	// sentence — and a key in every check row and a component of the token a
	// check's box fetches with, each of which carries it whole.
	CodeMaxCheckNameBytes = 64

	// CodeMaxCheckEnvNameBytes is the longest name a check's secret takes
	// ([ValidCodeCheckEnvName]): 128 bytes, four times the longest a common
	// tool reads (GOOGLE_APPLICATION_CREDENTIALS is 30), so a check's list of
	// the secrets it is given — at most 32 names — stays a few KiB on every
	// record that carries its definition.
	CodeMaxCheckEnvNameBytes = 128

	// CodeCheckTimeoutCeiling is the longest a check may run: six hours,
	// GitHub Actions' own per-job ceiling — the bound the CI a company
	// already runs holds its longest job to, so no pipeline moved here has to
	// be cut. A repository's checks file is held to it, and so is what a
	// company lets its checks run (code.checks.max_timeout_seconds).
	CodeCheckTimeoutCeiling = 6 * time.Hour

	// CodeDefaultCheckMaxTimeout is an unset code.checks.max_timeout_seconds:
	// FIFTY MINUTES. A remote box on E2B's smallest plan lives an hour, and a
	// check's box is given ten minutes past its timeout for its setup — its
	// git's check and its checkout, each a setup step under the sandbox's ten
	// minutes (internal/code's CheckBoxGrace, which a test there holds this
	// sum to) — so a check at the default fits the shortest session a
	// company's remote cell may have. A company on a larger plan, or running
	// its checks in containers alone, raises it to what its longest job takes.
	CodeDefaultCheckMaxTimeout = 50 * time.Minute

	// CodeMinCheckMaxTimeout is the least code.checks.max_timeout_seconds
	// takes: a MINUTE, under which no build of a repository finishes its own
	// setup — so a smaller value is a typo, and a company that means to run
	// no check requires none on its branches.
	CodeMinCheckMaxTimeout = time.Minute

	// CodeMaxDescriptionBytes bounds a repository description: one
	// paragraph, which rides every settings record and every listing row.
	CodeMaxDescriptionBytes = 1_024

	// CodeMaxRemoteURLBytes is the longest URL a repository's mirror or its
	// import names: ONE KIBIBYTE, the bound a description is held to
	// ([CodeMaxDescriptionBytes]), because every create, settings change and
	// restore carries a repository's mirrors whole as it carries its
	// description — and far past any forge's repository URL, a host and an
	// owner/name path a few dozen bytes long.
	CodeMaxRemoteURLBytes = 1_024

	// CodeMaxCredentialRefBytes is the longest credential a mirror or an
	// import names — the whole ${NAME} reference, which is all a record ever
	// carries of one: 128 bytes, twice the longest seat handle
	// (iam.MaxLogin, 64) a provisioner's variable is named after beside its
	// vendor's prefix. A JUDGMENT, which bounds what a settings record carries
	// rather than any name a person writes.
	CodeMaxCredentialRefBytes = 128

	// CodeMaxMirrorRefs is the most ref patterns a push mirror names: as many
	// as its repository carries protection rules ([CodeMaxProtections]), so
	// a mirror may name every protected pattern of its repository — the
	// "protected branches only" a forge's own mirror offers — and, like a
	// rule, every pattern is asked about every ref a round of the mirror
	// compares.
	CodeMaxMirrorRefs = 64

	// CodeMaxRepos is the most repositories one document may declare: a
	// typo guard on code.repos, two orders of magnitude past the reference
	// company's sixty or so — a list longer than it is a generated document
	// somebody should be told about rather than a company's repositories.
	// AN ADMISSION RULE for a document ([Code.validateAdmission]): the
	// reconcile decides any number of declarations. At this bound an
	// activation that creates, deletes or restores every declared repository
	// takes 16–19 s on one node before the last has its record
	// (internal/code's doc.go), beside the apply rather than inside it, and
	// one that changes nothing half a second.
	//
	// AND THE RUNTIME BOUND on the PROJECT repositories a tool's create may
	// bring the code log to: every status short of purged, declared ones
	// included, so declared and tool-made projects count alike and a tool's
	// create past it is refused repo_limit (internal/code's lifecycle). Read
	// there, one bound for both: a company whose declarations already reach
	// it has no room a tool could fill beside them. A seat's scratch
	// repositories are held by code.scratch.max_per_seat instead and never
	// count here, so seats' experiments cannot refuse a person's project. The
	// value stays ten thousand.
	CodeMaxRepos = 10_000

	// CodeMaxDeletedRetentionDays is the longest code.deleted_retention_days:
	// a year. A deleted repository keeps its rows on every data node and its
	// packs in the object store until it is purged, and every backup carries
	// them, so past a year a retention is an archive nobody restores rather
	// than an undo of a removal — and an export is the archive's form. AN
	// ADMISSION RULE ([Code.validateAdmission]): the retention duty purges by
	// any retention a duration can hold ([codeMaxHeldRetentionDays], the
	// runnable bound), so nothing about running depends on it.
	CodeMaxDeletedRetentionDays = 365
)

// codeMaxHeldRetentionDays is the longest code.deleted_retention_days a
// duration can hold — 106 751 days, about 292 years — and the RUNNABLE bound
// on it: one day more and [Code.DeletedRetention] overflows to a negative
// duration, a retention that has passed for every deleted repository, which
// would purge them all at the next pass. A purge has no undo, so a revision
// carrying one is refused to run however it was admitted.
const codeMaxHeldRetentionDays = int(math.MaxInt64 / int64(24*time.Hour))

// The bounds code.limits takes beside the ceiling above, named once so the
// schema's js tags and the validator cannot drift
// (TestTheCodeLimitsAreTheSchemasBounds).
const (
	// CodeMinPushBytes, CodeMinBlobBytes and CodeMinRepoBytes are the
	// smallest values code.limits takes: a mebibyte pack, a kibibyte file
	// and a mebibyte repository. Below them a person's first commit is
	// refused, which is a typo rather than a policy.
	CodeMinPushBytes int64 = 1 << 20
	CodeMinBlobBytes int64 = 1 << 10
	CodeMinRepoBytes int64 = 1 << 20

	// CodeMaxRepoBytesCeiling is the most code.limits.repo_max_bytes may
	// say: 1 TiB, a typo guard two orders of magnitude past the large
	// reference model's 12 GB monorepo. Every git node serving a repository
	// caches it whole, so a ceiling past any one node's volume is not a
	// setting anybody can run.
	CodeMaxRepoBytesCeiling int64 = 1 << 40

	// DefaultCodePushesPerSeatDay is an unset code.limits.pushes_per_seat_day:
	// a THOUSAND pushes one seat may land in one company day, every
	// repository together — a RUNAWAY bound, never a budget: the reference
	// company's seats push about thirty times a day each, so a thousand is
	// some thirty-three times that, which no seat working as intended meets,
	// and it caps what a seat pushing in a loop costs the code log at a
	// thousand push records a day — a seat's push alone in its record is
	// at most 1 555 bytes, from its coding box (internal/code's
	// BenchmarkPushLogBytes, its push=box arm; 1 481 where the engine builds
	// it for a tool), so about 1.6 MB a day — rather than without bound.
	DefaultCodePushesPerSeatDay = 1_000

	// CodeMinPushesPerSeatDay and CodePushesPerSeatDayCeiling bound
	// code.limits.pushes_per_seat_day: at least one push, since a quota of
	// none would make a seat a reader everywhere, which its role.code.access
	// says rather than a limit; and at most a hundred thousand, a typo guard
	// a hundred times the default — past it the count bounds nothing a
	// seat's own tool calls and token budget do not already bound.
	CodeMinPushesPerSeatDay     = 1
	CodePushesPerSeatDayCeiling = 100_000

	// DefaultCodeScratchIdleDays is an unset code.scratch.idle_days: a
	// seat's scratch repository with no push, rename or settings change for
	// SEVEN days is deleted — the trim's default min_age and
	// code.deleted_retention_days' default, so an experiment nobody returned
	// to for a week leaves as a deleted repository does, restorable for the
	// retention after. A product decision the approved design states.
	DefaultCodeScratchIdleDays = 7

	// CodeScratchIdleDaysFloor and CodeScratchIdleDaysCeiling bound
	// code.scratch.idle_days. A DAY AT LEAST: a turn holding a scratch
	// repository idle code.HoldRecordAfter (22 h) records its hold on the
	// repository's subject, and that threshold is this floor less the two
	// hours two nodes' clocks may disagree by (references'
	// TestAHoldRecordCoversEveryPassInFlight holds the arithmetic), so a
	// shorter idle time would let an expiry race a hold it cannot see. A YEAR
	// AT MOST, code.deleted_retention_days' ceiling: past it an experiment is
	// a project, and a project repository is what keeps one.
	CodeScratchIdleDaysFloor   = 1
	CodeScratchIdleDaysCeiling = 365

	// DefaultCodeScratchMaxPerSeat is an unset code.scratch.max_per_seat:
	// FIFTY scratch repositories one seat may hold, every one it owns short
	// of purged — a deleted one counting until its purge frees its room, so
	// a create-and-delete loop cannot hold rows, names and packs past it. A
	// product decision the approved design states.
	DefaultCodeScratchMaxPerSeat = 50

	// CodeScratchMaxPerSeatCeiling bounds code.scratch.max_per_seat: a
	// THOUSAND, a typo guard twenty times the default. A turn holds every
	// scratch repository it names on its run row (internal/agent/turnctx's
	// MaxHolds, 1 024, kept above this), so a seat working through all of
	// them is never refused a hold for their number.
	CodeScratchMaxPerSeatCeiling = 1_000

	// DefaultCodeIdentityDomain is an unset code.identity_domain:
	// seats.invalid, under RFC 2606's reserved `.invalid`, so an address a
	// commit the engine builds names belongs to nobody who could receive
	// mail at it — or claim it.
	DefaultCodeIdentityDomain = "seats.invalid"

	// CodeMaxIdentityDomainBytes bounds code.identity_domain: 189 bytes,
	// the most that leaves every address spelled in it — a seat's
	// `<handle>@<domain>`, a handle being at most iam.MaxLogin's 64 bytes —
	// within the 254 bytes RFC 5321 lets an address be. Stricter than the
	// 253 a DNS name may be, which would let an address no mail system
	// takes into every commit.
	CodeMaxIdentityDomainBytes = 254 - len("@") - iam.MaxLogin
)

// Code is the company's own git host: whether it hosts its repositories in
// the engine, the limits a push is held to, and the repositories it declares
// (docs/concepts/code-host.md).
//
// NATIVE IS PART OF A NODE'S NATIVE RUNTIME, like tracker.backend: the
// committer and the lifecycle store are built with it, once per process, from
// the company that starts it. On a node that runs no native runtime yet — a
// company on vendors for its tracker and its knowledge base — the activation
// that sets it starts that runtime, the code host in it, with no restart. On a
// node whose runtime is already running, a revision that turns it on or off
// activates normally and takes effect at the node's restart, which the node
// says once per activation (native_backend_needs_restart). The LIMITS are the
// epoch's, read at every decision, so a revision that moves one holds from its
// activation; a revision that turns the host off, which must drop them,
// leaves a node still serving it the limits of the last revision that hosted
// code until that restart.
//
// THE REPOSITORIES ARE THE EPOCH'S TOO: every data node reconciles code.repos
// at every activation and every boot — a declared name is created, or its
// settings settled; a configuration-managed repository no longer declared is
// deleted, restorably for deleted_retention_days, and purged after it.
// Nothing deletes a repository because the HOST was turned off: that revision
// changes nothing on a running node until its restart, and after the restart
// nothing reconciles at all.
type Code struct {
	Native               bool        `yaml:"native,omitempty" json:"native,omitempty" desc:"Host the company's git repositories in the engine itself, on its own state log: every data node commits their pushes. A node running no native backend yet starts the code host at the activation that sets it; on a node whose native runtime already runs, turning it on or off takes effect at the node's restart, which the node says (native_backend_needs_restart)."`
	DeletedRetentionDays int         `yaml:"deleted_retention_days,omitempty" json:"deleted_retention_days,omitempty" js:"min=0;max=365" desc:"How long a repository removed from code.repos stays restorable by declaring it again under its name, before it is purged and its name is free: 1..365 days; unset (or 0) is 7."`
	IdentityDomain       string      `yaml:"identity_domain,omitempty" json:"identity_domain,omitempty" js:"pattern=^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?([.][a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$;maxlen=189" desc:"The domain a commit the engine builds through a tool spells its addresses in: its committer's, engine@<domain>, and its author's, <handle>@<domain>, for a seat whose role states no email address. A lowercase DNS name of at most 189 bytes, so every such address is at most 254; unset is seats.invalid, a name nobody can own."`
	Limits               CodeLimits  `yaml:"limits,omitempty" json:"limits,omitzero"`
	Scratch              CodeScratch `yaml:"scratch,omitempty" json:"scratch,omitzero"`
	// Checks is how the company runs the checks its repositories define:
	// where, with which of its secrets, and for how long at most.
	Checks CodeChecks `yaml:"checks,omitempty" json:"checks,omitzero"`
	// MergeByFloor is the least authority a merge into ANY protected branch
	// needs, whatever its rules say: a merge is held to the stricter of it
	// and the merge_by of every rule matching its branch. The EPOCH'S, read at
	// every decision as the limits are (internal/code.Limits.MergeByFloor).
	MergeByFloor CodeMergeBy `yaml:"merge_by_floor,omitempty" json:"merge_by_floor,omitempty" js:"enum=writers|maintainers|persons" desc:"The least a merge into any protected branch needs, whatever its rules say: writers (unset) — anybody who may write to the repository — maintainers, or persons, a person and never a seat. A merge is held to the stricter of this and the merge_by of every rule matching its branch; a rule with no merge gate reads as writers."`
	Repos        []CodeRepo  `yaml:"repos,omitempty" json:"repos,omitempty" desc:"The company's repositories, at most 10000. Each activation creates the ones declared and settles their settings; removing one deletes it, restorable for deleted_retention_days by declaring it again."`
}

// CodeScratch is the policy for seats' SCRATCH repositories: the ones a seat's
// own tool creates for its experiments, each named <handle>.<slug> after the
// seat that owns it (internal/code's lifecycle requests). An unset field takes
// its default ([CodeScratch.Resolved]).
//
// THE EPOCH'S, read where it is used: the create's cap at the create's decide,
// the idle time at every pass of the code-retention duty — so a revision that
// moves either holds from its activation, with no restart.
type CodeScratch struct {
	// IdleDays is how many days a scratch repository may go with no push,
	// rename, settings change or restore before the code-retention pass
	// deletes it — restorable for code.deleted_retention_days like any
	// deleted repository. A read is not activity, and neither is a turn's
	// hold: a live turn's hold keeps the repository while the turn lasts and
	// moves no clock. 0 is unset, the default ([DefaultCodeScratchIdleDays]).
	IdleDays int `yaml:"idle_days,omitempty" json:"idle_days,omitempty" js:"min=1;max=365" desc:"How many days a seat's scratch repository may go with no push, rename, settings change or restore before it is deleted — restorable for deleted_retention_days, like any deleted repository. A repository a live turn has named is never deleted while the turn lasts. 1..365; unset is 7."`
	// MaxPerSeat is how many scratch repositories one seat may hold — every
	// one it owns short of purged, a deleted one counting until its purge
	// frees its room. A POINTER because 0 is a setting — no seat may create
	// one — and unset is the default ([DefaultCodeScratchMaxPerSeat]): a
	// zero value that is a valid setting needs one.
	MaxPerSeat *int `yaml:"max_per_seat,omitempty" json:"max_per_seat,omitempty" js:"min=0;max=1000" desc:"How many scratch repositories one seat may hold, every one it owns short of purged — a deleted one counts until its purge frees its room. 0 lets no seat create one; 0..1000; unset is 50."`
}

// Resolved is the scratch policy with its defaults applied — THE ONE PLACE
// they are applied, read by the code domain's own conversion
// (internal/code.ScratchOf), so what `crewlet validate` judges and what a
// create and an expiry are held to are one reading of the document.
func (s CodeScratch) Resolved() (idleDays, maxPerSeat int) {
	idleDays, maxPerSeat = s.IdleDays, DefaultCodeScratchMaxPerSeat
	if idleDays == 0 {
		idleDays = DefaultCodeScratchIdleDays
	}
	if s.MaxPerSeat != nil {
		maxPerSeat = *s.MaxPerSeat
	}
	return idleDays, maxPerSeat
}

// zero is a block that sets nothing.
func (s CodeScratch) zero() bool { return s.IdleDays == 0 && s.MaxPerSeat == nil }

// validate holds code.scratch's rules at path: each field inside its bounds.
func (s CodeScratch) validate(scratch Path) error {
	var p problems
	if d := s.IdleDays; d != 0 && (d < CodeScratchIdleDaysFloor || d > CodeScratchIdleDaysCeiling) {
		p.add(at(scratch, "idle_days"), ErrOutOfRange,
			"%d is outside %d..%d days, or unset for the default %d — a day at least, because a "+
				"live turn's hold on an idle repository is ordered against the expiry by a record "+
				"its holder writes a day less two hours after the repository's last activity",
			d, CodeScratchIdleDaysFloor, CodeScratchIdleDaysCeiling, DefaultCodeScratchIdleDays)
	}
	if n := s.MaxPerSeat; n != nil && (*n < 0 || *n > CodeScratchMaxPerSeatCeiling) {
		p.add(at(scratch, "max_per_seat"), ErrOutOfRange,
			"%d is outside 0..%d scratch repositories a seat, or unset for the default %d",
			*n, CodeScratchMaxPerSeatCeiling, DefaultCodeScratchMaxPerSeat)
	}
	return p.err()
}

// CodeChecks is how the company runs the CHECKS its repositories define in
// their `.crewlet/checks.yaml` (internal/code's ParseChecks): which cells a
// check runs in, which of the company's secrets a check may name, and how
// long the company lets one run. The file names; this block decides
// (docs/concepts/code-host.md, "Checks"). Every field unset is a valid block.
//
// THE EPOCH'S, read where each is used — the cell and the timeout when a
// check's box is made, the secrets when its environment is — so a revision
// moving any of them holds from its activation, with no restart.
type CodeChecks struct {
	// RunIn is the cell a check whose file names none runs in: one of
	// [CodeCheckCells]. Unset is providers.sandbox's own default where that is
	// one of them, else no cell ([Company.CheckRunIn]).
	RunIn Placement `yaml:"run_in,omitempty" json:"run_in,omitempty" js:"enum=container|e2b" desc:"The cell a check whose checks file names none runs in: container — a container on a git node — or e2b, a remote box. Unset is providers.sandbox.default_run_in where that is one of the two, else no cell. direct and self are refused: a check runs a change's own code, which direct would run as the engine's own user on the engine host, and self is a seat's own executor box, which no check has."`

	// AllowRunIn are the cells a repository's checks file may name in a
	// check's own run_in. Unset is the one RunIn resolves to; set, it holds
	// that one, so a file naming the cell a check naming none runs in is
	// never refused where the check would have run anyway.
	AllowRunIn []Placement `yaml:"allow_run_in,omitempty" json:"allow_run_in,omitempty" js:"enum=container|e2b" desc:"The cells a repository's checks file may name in a check's run_in, each once: container or e2b. Unset is the one run_in resolves to; set, it holds that one."`

	// Env are the secrets a check may be given, by variable name: a check
	// is given exactly the entries its file's `env` names, and none it does
	// not. A credential field, as role.sandbox.env is: what is declared here
	// is a registry token, a cloud key, a test database's URL — resolved
	// when a check's box is made, never stored resolved.
	Env map[string]string `secret:"true" yaml:"env,omitempty" json:"env,omitempty" desc:"The secrets a check may be given: a check is given exactly the entries its checks file names in its env. Each key a variable name — a letter or an underscore, then letters, digits and underscores, at most 128 bytes — and none the engine sets in a box itself. ${VAR} supported, resolved when a check's box is made."`

	// MaxTimeoutSeconds is how long the company lets a check run, whatever
	// its file's timeout_seconds says: 0 is unset, the default
	// ([CodeDefaultCheckMaxTimeout]); else [CodeMinCheckMaxTimeout] to
	// [CodeCheckTimeoutCeiling], in seconds.
	MaxTimeoutSeconds int `yaml:"max_timeout_seconds,omitempty" json:"max_timeout_seconds,omitempty" js:"min=60;max=21600" desc:"How long the company lets a check run, whatever its checks file's timeout_seconds: 60..21600 seconds; unset is 3000 — an e2b box on the smallest plan's one-hour session, with ten minutes of it for the box's setup."`
}

// MaxTimeout is code.checks.max_timeout_seconds as a duration, its default
// applied where it is unset — THE ONE PLACE the default is applied: a check's
// box runs for the lesser of this and its own file's timeout.
func (k CodeChecks) MaxTimeout() time.Duration {
	if k.MaxTimeoutSeconds == 0 {
		return CodeDefaultCheckMaxTimeout
	}
	return time.Duration(k.MaxTimeoutSeconds) * time.Second
}

// zero is a block that sets nothing.
func (k CodeChecks) zero() bool {
	return k.RunIn == "" && len(k.AllowRunIn) == 0 && len(k.Env) == 0 && k.MaxTimeoutSeconds == 0
}

// validate holds code.checks' own rules at path: each cell one a check runs in
// ([CodeCheckCells]) — `direct` and `self` refused by name with why
// ([CodeCheckCellRefusal]) — and each allowed once; each secret's key a name a
// check can name ([ValidCodeCheckEnvName]); and the timeout inside its bounds.
// Whether a cell it names is configured, and whether the cells it allows hold
// the one a check naming none runs in, turn on providers.sandbox:
// [Company.validateSandboxPlacement] holds those. A key the engine sets in a
// box itself is an admission rule's ([Company.validateReservedSandboxEnv]).
func (k CodeChecks) validate(path Path) error {
	var p problems
	cell := func(at Path, cell Placement) {
		switch {
		case slices.Contains(CodeCheckCells, cell):
		case CodeCheckCellRefusal(cell) != "":
			p.add(at, ErrUnknownValue, "a check never runs in %s: %s", cell, CodeCheckCellRefusal(cell))
		default:
			p.add(at, ErrUnknownValue, "%q (want %s)", cell, names(CodeCheckCells))
		}
	}
	if k.RunIn != "" {
		cell(at(path, "run_in"), k.RunIn)
	}
	first := make(map[Placement]int, len(k.AllowRunIn))
	for i, allowed := range k.AllowRunIn {
		where := idx(at(path, "allow_run_in"), i)
		if j, twice := first[allowed]; twice {
			p.add(where, ErrConflict, "%q is allow_run_in[%d] too", allowed, j)
			continue
		}
		first[allowed] = i
		cell(where, allowed)
	}
	for _, name := range slices.Sorted(maps.Keys(k.Env)) {
		if !ValidCodeCheckEnvName(name) {
			p.add(entry(at(path, "env"), name), ErrShape,
				"%q is not a name a check can be given a secret under: a letter or an underscore, then letters, "+
					"digits and underscores, at most %d bytes", name, CodeMaxCheckEnvNameBytes)
		}
	}
	if s := k.MaxTimeoutSeconds; s != 0 &&
		(s < int(CodeMinCheckMaxTimeout/time.Second) || s > int(CodeCheckTimeoutCeiling/time.Second)) {
		p.add(at(path, "max_timeout_seconds"), ErrOutOfRange,
			"%d is outside %d..%d seconds — a minute, under which no build finishes its own setup, to six "+
				"hours, the longest any checks file may ask (GitHub Actions' per-job ceiling) — or unset for "+
				"the default %d", s, int(CodeMinCheckMaxTimeout/time.Second), int(CodeCheckTimeoutCeiling/time.Second),
			int(CodeDefaultCheckMaxTimeout/time.Second))
	}
	return p.err()
}

// NeedsRole reports whether the company needs a node role at all: every role
// but git always, and git exactly where the company hosts its repositories in
// the engine (code.native) — a git node serves nothing for a company that does
// not. Nil-safe: no company needs every role but git.
//
// THE ONE STATEMENT OF THE RULE, read by the fleet's unmanned-role check on
// every node (the engine's seat.Config.RoleNeeded) and by the fleet view's
// unmanned_roles, so a warning in one node's log and the screen an operator
// reads can never disagree about whether the fleet is missing a role. Each
// held its own copy once, and the screen's — a role nobody declared — named
// git missing on every fleet that hosts no code and lists its nodes' roles.
func (c *Company) NeedsRole(role placement.NodeRole) bool {
	if role != placement.RoleGit {
		return true
	}
	return c != nil && c.Code.Native
}

// DeletedRetention is code.deleted_retention_days as a duration, its default
// applied where it is unset — THE ONE PLACE the default is applied, read by the
// engine's code-retention duty at every pass.
func (c Code) DeletedRetention() time.Duration {
	days := c.DeletedRetentionDays
	if days <= 0 {
		days = DefaultCodeDeletedRetentionDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// IdentityDomainResolved is code.identity_domain, its default applied where
// it is unset — THE ONE PLACE the default is applied: the domain the engine's
// signer spells an engine-built commit's committer in (`engine@<domain>`),
// and its author in for a seat whose role states no email address
// (`<handle>@<domain>`).
func (c Code) IdentityDomainResolved() string {
	if c.IdentityDomain == "" {
		return DefaultCodeIdentityDomain
	}
	return c.IdentityDomain
}

// validDomainName reports a lowercase DNS name: labels of ASCII letters,
// digits and hyphens, none empty or past 63 bytes and none beginning or
// ending with a hyphen, joined by dots — no trailing dot, since an address
// spells none — at most CodeMaxIdentityDomainBytes in all. Lowercase because
// a commit records the address as written, and two spellings of one domain
// would be two committers.
func validDomainName(s string) bool {
	if s == "" || len(s) > CodeMaxIdentityDomainBytes {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			if c := label[i]; !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// ValidCodeCheckName reports a name a check may have: a lowercase letter or a
// digit, then up to sixty-three more of them, `.`, `_` and `-` —
// ^[a-z0-9][a-z0-9._-]{0,63}$, at most [CodeMaxCheckNameBytes]. THE ONE RULE a
// check is named by, read wherever a name is written: a check's own name in
// a repository's `.crewlet/checks.yaml` and on every row of its attempts (the
// code domain's checks file and check records), and a protection rule's
// required checks here and in the code domain's record of a merge gate. One
// rule, never a copy in each, so a rule cannot require a check no repository
// can define — a gate that requirement would keep shut for good — and every
// check a file defines a rule can require. It lives here, not in the domain,
// because config's own rules read it and config cannot import the domain.
//
// LOWERCASE AND NO SPACE because a name is matched exactly wherever it is
// read and is typed on command lines: two spellings of one name would be two
// checks, and a name with a space one a shell splits. And nothing any URL
// encoder escapes — a `~` among them, which some (Java's URLEncoder) write
// `%7E` — since a name rides a route as it is spelled. The token a check's box
// fetches with carries its name base64url-encoded (internal/code/host's
// CheckTokens), so no character a name holds is ever the token's: a `.`,
// which this rule admits, is the token's own separator.
func ValidCodeCheckName(name string) bool {
	if name == "" || len(name) > CodeMaxCheckNameBytes {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// ValidCodeCheckEnvName reports a name a check's secret may have: a letter or
// an underscore, then letters, digits and underscores — a variable any shell
// exports — at most [CodeMaxCheckEnvNameBytes]. THE ONE RULE, for the same
// reason as [ValidCodeCheckName]: a key of the company's code.checks.env here
// and a name a repository's checks file lists in a check's `env` in the code
// domain are held to it alike, so a file never names a secret the company
// could not define, nor the company one no file could name.
func ValidCodeCheckEnvName(name string) bool {
	if name == "" || len(name) > CodeMaxCheckEnvNameBytes {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c == '_', 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z':
		case i > 0 && '0' <= c && c <= '9':
		default:
			return false
		}
	}
	return true
}

// CodeCheckCells are the cells a check runs in, in this order: TWO OF THE
// SANDBOX'S FOUR PLACEMENTS ([Placements]) — a container on a git node, which
// holds the repository's copy beside it, or a remote e2b box, which fetches
// the one commit it checks. A check runs a change's own code — its build, its
// tests, whatever its author put there — so it never runs where that code
// reaches the engine ([CodeCheckCellRefusal]). THE ONE LIST, for the reason
// [ValidCodeCheckName] is one rule: the company's code.checks here and a
// repository's checks file in the code domain name their cells from it.
var CodeCheckCells = []Placement{PlacementContainer, PlacementE2B}

// CodeCheckCellRefusal is why a check never runs in p, "" for a cell of
// [CodeCheckCells] and for a value no placement has — which its reader
// refuses as unknown. BY NAME (M3 decision U1 a), each of the two placements
// a check is refused with a sentence of its own: `direct` runs a box as the
// engine's own user on the engine host, where a change's code would read what
// the engine reads — the keyring that signs every run's token among it — and
// `self` is a seat's own executor box, which no check, run by no seat, has.
func CodeCheckCellRefusal(p Placement) string {
	switch p {
	case PlacementDirect:
		return "direct runs a box as the engine's own user on the engine host, and a check runs a change's " +
			"own code, which would read what the engine reads; run it in a container or on e2b"
	case PlacementSelf:
		return "self is a seat's own executor box, and a check is run by no seat; run it in a container or " +
			"on e2b"
	}
	return ""
}

// CodeRepo is one repository code.repos declares.
type CodeRepo struct {
	Name          string           `yaml:"name" json:"name" js:"pattern=^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$" desc:"The repository's name: one label of lowercase letters, digits and hyphens, starting and ending with a letter or a digit, at most 63 — no dot, which only a seat's own scratch repository's name carries. It is the address a repository is cloned by, so renaming one here deletes it and creates another."`
	Owner         CodeOwner        `yaml:"owner" json:"owner"`
	Visibility    CodeVisibility   `yaml:"visibility,omitempty" json:"visibility,omitempty" js:"enum=company|unit" desc:"Who may read it besides persons: company (unset) — every seat — or unit — the owning unit's members, its leads and the owning seat."`
	Search        CodeSearchMode   `yaml:"search,omitempty" json:"search,omitempty" js:"enum=indexed|grep" desc:"How code search reads it: indexed (unset) — from an index of its default branch, by a search across the company's repositories as well as one naming it — or grep — with git grep, and only by a search naming it."`
	DefaultBranch string           `yaml:"default_branch,omitempty" json:"default_branch,omitempty" desc:"The branch a clone checks out; unset is main."`
	Description   string           `yaml:"description,omitempty" json:"description,omitempty" desc:"One paragraph, at most 1024 bytes."`
	Archived      bool             `yaml:"archived,omitempty" json:"archived,omitempty" desc:"Read-only: still fetched and cloned, and takes no push."`
	Protect       []CodeProtection `yaml:"protect,omitempty" json:"protect,omitempty" desc:"The rules a push to the refs they match is held to, at most 64. A ref no rule matches may be force-pushed or deleted by anybody who may push."`
	// PathOwners are who owns which of the repository's paths: whom a change
	// touching them asks for a review. They GRANT NOTHING — who may read and
	// push is the chart's and the tier's, whatever a path owner names.
	PathOwners []CodePathOwner `yaml:"path_owners,omitempty" json:"path_owners,omitempty" desc:"Who owns which of the repository's paths, at most 64 owners and 256 KiB as the code log records them: a change touching an owner's paths asks it for a review. A path owner is asked, never granted: it reads and pushes what the chart lets it, as before."`
	// MirrorFrom makes the repository a PULL MIRROR of a repository at
	// another host, MirrorTo a PUSH MIRROR to one, and Import fills it from
	// one when it is created. Only a person or this document sets any of the
	// three (U49): a pull mirror and an import dial another host, and a push
	// mirror sends the company's code to one. Each credential is a ${VAR}
	// reference and nothing else, recorded as the reference and resolved
	// only when the remote is dialed.
	MirrorFrom *CodeMirrorSource `yaml:"mirror_from,omitempty" json:"mirror_from,omitempty" desc:"Makes it a pull mirror of a repository at another host: it is imported from there when it is created, then moves only by the code host's syncs from it, and takes no push and no change of its own."`
	MirrorTo   *CodeMirrorTarget `yaml:"mirror_to,omitempty" json:"mirror_to,omitempty" desc:"Makes it a push mirror: the code host pushes the refs it names to a repository at another host, as the seat named by as."`
	Import     *CodeImportSource `yaml:"import,omitempty" json:"import,omitempty" desc:"Fills it, when it is created, from a repository at another host: it takes no push until its import is done. Read only by its create: a repository that exists already ignores it."`
}

// IdentityKey is the repository's name, which code.repos holds unique: what a
// redaction's restore matches a repository's credentials by, since a mirror's
// or an import's credential is masked wherever a read surface finds one that
// is not a reference — and restored by position, a reordered code.repos would
// hand one repository's credential to another.
func (r CodeRepo) IdentityKey() string { return r.Name }

// CodeMirrorSource is the repository a pull mirror follows ([CodeRepo.MirrorFrom]).
type CodeMirrorSource struct {
	URL        string `yaml:"url" json:"url" js:"required" desc:"The repository it follows: an https URL with no user, query or fragment, at most 1024 bytes."`
	Credential string `secret:"true" yaml:"credential,omitempty" json:"credential,omitempty" desc:"What the code host fetches with: one whole ${VAR} reference to a token, resolved from the secret store and then the environment when it dials, never a literal. Unset fetches anonymously."`
}

// CodeMirrorTarget is the repository a push mirror pushes to
// ([CodeRepo.MirrorTo]).
type CodeMirrorTarget struct {
	URL        string   `yaml:"url" json:"url" js:"required" desc:"The repository it pushes to: an https URL with no user, query or fragment, at most 1024 bytes."`
	Credential string   `secret:"true" yaml:"credential" json:"credential" js:"required" desc:"What the code host pushes with: one whole ${VAR} reference to the as seat's own token at that host, resolved from the secret store and then the environment when it dials, never a literal."`
	As         string   `yaml:"as" json:"as" js:"required" desc:"The seat whose identity the push carries at that host, by its handle: a seat that reads this repository — a person's seat reads every one, an agent's what its grant does — never a bot."`
	Refs       []string `yaml:"refs,omitempty" json:"refs,omitempty" desc:"The refs it pushes, at most 64 patterns of refs/heads/<glob> and refs/tags/<glob>, each once; unset is the default branch and refs/tags/*. No pattern may reach a seat's own branches under refs/heads/seats/."`
}

// CodeImportSource is the repository a repository is filled from when it is
// created ([CodeRepo.Import]).
type CodeImportSource struct {
	URL        string `yaml:"url" json:"url" js:"required" desc:"The repository it is filled from: an https URL with no user, query or fragment, at most 1024 bytes."`
	Credential string `secret:"true" yaml:"credential,omitempty" json:"credential,omitempty" desc:"What the code host fetches with: one whole ${VAR} reference to a token, resolved from the secret store and then the environment when it dials, never a literal. Unset fetches anonymously."`
}

// CodePathOwner is one owner of a repository's paths: exactly one of a unit or
// a seat, and the globs over the repository's tree it owns
// (gitref.ParsePathGlob). An owner is named once in a repository's list, so
// everything one owner owns is one entry.
type CodePathOwner struct {
	Paths []string `yaml:"paths" json:"paths" desc:"The paths it owns: 1 to 32 globs over the repository's tree, each relative to its root, at most 128 bytes. A component of * matches one component and * within one any run of characters but /; ** matches whole components (zero or more, one or more as the last), so docs/** is everything inside docs and **/*.go every Go file; a glob holding a * has at most eight components, and a glob with no * matches only that path. ?, [, \\ and a leading ! are refused: * is the one metacharacter. Each glob once."`
	Unit  string   `yaml:"unit,omitempty" json:"unit,omitempty" desc:"A unit of the chart: its id, or its name where it has none — its effective lead is whom a review is asked of. Exactly one of unit and seat."`
	Seat  string   `yaml:"seat,omitempty" json:"seat,omitempty" desc:"A seat, by its handle. Exactly one of unit and seat."`
}

// ResolvedDefaultBranch is the repository's default branch with its default
// applied — read by the unprotected-branch warning and by the engine's
// reconcile alike, so the branch warned about is the branch recorded.
func (r CodeRepo) ResolvedDefaultBranch() string {
	if r.DefaultBranch == "" {
		return DefaultCodeBranch
	}
	return r.DefaultBranch
}

// DefaultCodeBranch is a declared repository's default branch where it sets
// none.
const DefaultCodeBranch = "main"

// CodeVisibility is who among the SEATS may read a repository. A person — a
// bound or unbound API token — reads every repository whatever it says
// (ADR-0058): visibility narrows the seats an AI company runs, never the
// people who run it.
type CodeVisibility string

const (
	// CodeVisibilityCompany lets every seat read it — what unset reads as.
	CodeVisibilityCompany CodeVisibility = "company"
	// CodeVisibilityUnit lets only the seats the chart relates to its owner
	// read it: the owning unit's members, the leads above them, and the
	// owning seat.
	CodeVisibilityUnit CodeVisibility = "unit"
)

// CodeVisibilities is the closed set: the one list the validator and the
// schema read. The code domain's own set is held to it in both directions by
// the engine's TestEveryDeclaredVisibilityIsOneTheDomainRecords, because
// config cannot import the domain that imports it.
var CodeVisibilities = []CodeVisibility{CodeVisibilityCompany, CodeVisibilityUnit}

// Valid reports a value this build knows. Unset is valid: it reads as company
// ([CodeVisibility.Resolved]).
func (v CodeVisibility) Valid() bool { return v == "" || slices.Contains(CodeVisibilities, v) }

// Resolved is the value with its default applied: unset is company — THE ONE
// PLACE the default is applied, read by the warnings and by the engine's
// reconcile alike, so the visibility warned about is the visibility recorded.
func (v CodeVisibility) Resolved() CodeVisibility {
	if v == "" {
		return CodeVisibilityCompany
	}
	return v
}

// CodeSearchMode is how code search reads a declared repository: from an index
// of its default branch, or with git grep only when a search names it. UNSET IS
// INDEXED for a repository this document declares — a project the company
// named is the code its seats work in, so it is what a search across the
// company's repositories reads — while a repository a tool makes starts at grep
// (the code domain's rule, internal/code's SearchMode).
type CodeSearchMode string

const (
	// CodeSearchIndexed reads it from an index of its default branch — what
	// unset reads as.
	CodeSearchIndexed CodeSearchMode = "indexed"
	// CodeSearchGrep reads it with git grep, only by a search naming it.
	CodeSearchGrep CodeSearchMode = "grep"
)

// CodeSearchModes is the closed set: the one list the validator and the schema
// read. The code domain's own set is held to it in both directions by the
// engine's TestEveryDeclaredSearchModeIsOneTheDomainRecords, because config
// cannot import the domain that imports it.
var CodeSearchModes = []CodeSearchMode{CodeSearchIndexed, CodeSearchGrep}

// Valid reports a value this build knows. Unset is valid: it reads as indexed
// ([CodeSearchMode.Resolved]).
func (m CodeSearchMode) Valid() bool { return m == "" || slices.Contains(CodeSearchModes, m) }

// Resolved is the value with its default applied: unset is indexed — THE ONE
// PLACE the default is applied, read by the engine's reconcile, so a
// declaration that says nothing is recorded as indexed on every node.
func (m CodeSearchMode) Resolved() CodeSearchMode {
	if m == "" {
		return CodeSearchIndexed
	}
	return m
}

// CodeOwner is who owns a repository: exactly one of a unit or a seat.
type CodeOwner struct {
	Unit string `yaml:"unit,omitempty" json:"unit,omitempty" desc:"A unit of the chart: its id, or its name where it has none. Exactly one of unit and seat."`
	Seat string `yaml:"seat,omitempty" json:"seat,omitempty" desc:"A seat, by its handle. Exactly one of unit and seat."`
}

// CodeProtection is one protection rule: the branches or the tags it matches,
// what a push to one of them may do, and — on a branch rule — what a merge
// into one needs, its MERGE GATE: any of the gate's fields set forms one, every
// field it leaves unset taking its default ([CodeProtection.Gate]).
type CodeProtection struct {
	Branch      string         `yaml:"branch,omitempty" json:"branch,omitempty" desc:"A branch name or glob: * matches one path component, ** any number, and a name with no * matches only itself. Exactly one of branch and tag."`
	Tag         string         `yaml:"tag,omitempty" json:"tag,omitempty" desc:"A tag name or glob, as for branch. Exactly one of branch and tag."`
	AllowForce  bool           `yaml:"allow_force,omitempty" json:"allow_force,omitempty" desc:"Let a push move a matching branch to a commit that does not descend from it. Not on a tag rule: a tag never moves."`
	AllowDelete bool           `yaml:"allow_delete,omitempty" json:"allow_delete,omitempty" desc:"Let a push delete a matching ref."`
	DirectPush  CodeDirectPush `yaml:"direct_push,omitempty" json:"direct_push,omitempty" js:"enum=persons|none" desc:"Who may push to a matching ref directly: persons (unset), a person and never a seat, whose push is refused; or none, which freezes it — nothing pushes to it."`

	// THE MERGE GATE: none of it on a tag rule, which no merge reaches.

	MergeBy          CodeMergeBy       `yaml:"merge_by,omitempty" json:"merge_by,omitempty" js:"enum=writers|maintainers|persons" desc:"Who may merge into a matching branch: writers (unset) — anybody who may write to the repository — maintainers, or persons, a person and never a seat. The stricter of this and code.merge_by_floor holds. Merge gate fields are refused on a tag rule: no merge reaches a tag."`
	Approvals        CodeApprovals     `yaml:"approvals,omitempty" json:"approvals,omitempty" js:"enum=none|one" desc:"How many counted approvals a merge waits for: none (unset) or one."`
	ApprovalsUnless  []CodeChangeClass `yaml:"approvals_unless,omitempty" json:"approvals_unless,omitempty" js:"enum=whitespace_only|comment_only" desc:"The path classes that exempt a change from those approvals: a change whose every touched path is of a class listed — whitespace_only, comment_only — waits for none. Each once."`
	HumanPaths       []CodeHumanPath   `yaml:"human_paths,omitempty" json:"human_paths,omitempty" desc:"At most 32 globs of the repository's paths a change touching which waits for a person's approval, whatever else approved it. Each glob once."`
	Checks           []string          `yaml:"checks,omitempty" json:"checks,omitempty" js:"pattern=^[a-z0-9][a-z0-9._-]{0,63}$" desc:"At most 32 checks a merge waits for, by name: a lowercase letter or digit, then up to 63 more of them, '.', '_' and '-'. Each once."`
	Landing          CodeLanding       `yaml:"landing,omitempty" json:"landing,omitzero" desc:"How the branch's queue lands a change: each field its default when unset."`
	OwnersApprove    bool              `yaml:"owners_approve,omitempty" json:"owners_approve,omitempty" desc:"Make the repository's path owners gating: a change waits for one counted approval from somebody eligible for every owned glob it touches."`
	DeleteSource     CodeDeleteSource  `yaml:"delete_source,omitempty" json:"delete_source,omitempty" js:"enum=seats|none" desc:"Whether a merge deletes its change's source branch in the record that merges it: seats (unset), a branch in a seat's own namespace, or none."`
	AutoRevert       bool              `yaml:"auto_revert,omitempty" json:"auto_revert,omitempty" desc:"Let whoever queues a change consent, when it does, to reverting a landing that breaks the branch."`
	AgentsEditChecks bool              `yaml:"agents_edit_checks,omitempty" json:"agents_edit_checks,omitempty" desc:"Let a seat's approval stand for a change to .crewlet/** — where a repository's checks are defined — which, on a rule requiring checks, otherwise waits for a person's."`
}

// CodeHumanPath is one glob of a repository's tree a change touching which
// waits for a person's approval, unless every path of the change the glob
// matched is of a class Unless lists.
type CodeHumanPath struct {
	Glob   string            `yaml:"glob" json:"glob" desc:"A glob over the repository's tree, from its root, as a path owner's paths are written: * within one component, ** whole components, at most 128 bytes."`
	Unless []CodeChangeClass `yaml:"unless,omitempty" json:"unless,omitempty" js:"enum=whitespace_only|comment_only" desc:"The path classes — whitespace_only, comment_only — that let a change through without the person's approval when every path this glob matched is of one. Each once."`
}

// CodeLanding is how a gated branch's queue lands a change. An unset field
// takes its default ([CodeLanding.Resolved]); the queue lands only where the
// rule requires checks.
type CodeLanding struct {
	Policy CodeLandingPolicy `yaml:"policy,omitempty" json:"policy,omitempty" js:"enum=pre_land|post_land" desc:"pre_land (unset) tests each change merged on the branch before it lands; post_land lands changes ahead of their checks, which then run on the branch's newest tip."`
	Depth  int               `yaml:"depth,omitempty" json:"depth,omitempty" js:"min=1;max=8" desc:"How many candidates the queue tests at once: 1..8; unset is 4."`
	Batch  int               `yaml:"batch,omitempty" json:"batch,omitempty" js:"min=1;max=8" desc:"How many waiting changes one candidate takes: 1..8; unset is 4."`
}

// Resolved is the landing with its defaults applied — THE ONE PLACE they are
// applied ([CodeProtection.Gate] reads it).
func (l CodeLanding) Resolved() CodeLanding {
	l.Policy = l.Policy.Resolved()
	if l.Depth == 0 {
		l.Depth = CodeDefaultQueueDepth
	}
	if l.Batch == 0 {
		l.Batch = CodeDefaultQueueBatch
	}
	return l
}

// CodeGate is a protection rule's merge gate RESOLVED — every field its value
// or its default — in the shape the code domain records it: code.MergeGate's
// keys, in its order and with its omissions, so its encoding is as long as
// the record's ([CodeGate.EncodedBytes]; the engine's
// TestADeclaredGateWeighsWhatTheDomainRecords holds the two). Its lists are in
// the document's order: the reconcile records them in the domain's one order,
// which changes no length.
type CodeGate struct {
	MergeBy          CodeMergeBy       `json:"merge_by"`
	Approvals        CodeApprovals     `json:"approvals"`
	ApprovalsUnless  []CodeChangeClass `json:"approvals_unless,omitempty"`
	HumanPaths       []CodeHumanPath   `json:"human_paths,omitempty"`
	Checks           []string          `json:"checks,omitempty"`
	Landing          CodeLanding       `json:"landing"`
	OwnersApprove    bool              `json:"owners_approve,omitempty"`
	DeleteSource     CodeDeleteSource  `json:"delete_source"`
	AutoRevert       bool              `json:"auto_revert,omitempty"`
	AgentsEditChecks bool              `json:"agents_edit_checks,omitempty"`
}

// EncodedBytes is what the gate weighs as the code log records it — the
// length of its JSON — which [CodeMaxGateBytes] bounds.
func (g CodeGate) EncodedBytes() int {
	// IT CANNOT FAIL: strings, whole numbers, booleans and lists of them are
	// every value a gate holds, and json.Marshal encodes each of them.
	b, _ := json.Marshal(g)
	return len(b)
}

// Gate is the rule's merge gate RESOLVED: nil where the rule sets none of its
// fields, and otherwise every field its value or its default — writers, none,
// pre_land at depth 4 and batch 4, seats. THE ONE PLACE those defaults are
// applied, read by the engine's reconcile, which records it, and by the
// gate's bound here, so the gate validated is the gate recorded.
func (rule CodeProtection) Gate() *CodeGate {
	if len(rule.gateFields()) == 0 {
		return nil
	}
	return &CodeGate{
		MergeBy: rule.MergeBy.Resolved(), Approvals: rule.Approvals.Resolved(),
		ApprovalsUnless: rule.ApprovalsUnless, HumanPaths: rule.HumanPaths, Checks: rule.Checks,
		Landing: rule.Landing.Resolved(), OwnersApprove: rule.OwnersApprove,
		DeleteSource: rule.DeleteSource.Resolved(), AutoRevert: rule.AutoRevert,
		AgentsEditChecks: rule.AgentsEditChecks,
	}
}

// gateFields is every field of a merge gate the rule sets, by its name in the
// document and in the document's order: what a gate is formed from, and what
// a tag rule is refused for.
func (rule CodeProtection) gateFields() []string {
	var set []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"merge_by", rule.MergeBy != ""}, {"approvals", rule.Approvals != ""},
		{"approvals_unless", len(rule.ApprovalsUnless) > 0}, {"human_paths", len(rule.HumanPaths) > 0},
		{"checks", len(rule.Checks) > 0}, {"landing", rule.Landing != CodeLanding{}},
		{"owners_approve", rule.OwnersApprove}, {"delete_source", rule.DeleteSource != ""},
		{"auto_revert", rule.AutoRevert}, {"agents_edit_checks", rule.AgentsEditChecks},
	} {
		if f.set {
			set = append(set, f.name)
		}
	}
	return set
}

// Pattern is the rule as the domain records it: the namespaced protection
// pattern, refs/heads/<branch> or refs/tags/<tag>, or "" where the rule sets
// neither or both. THE ONE SPELLING, read by the validator's uniqueness rule
// and by the engine's reconcile alike, so `crewlet validate` cannot call two
// rules distinct that the domain then refuses as one.
func (rule CodeProtection) Pattern() string {
	switch {
	case rule.Branch != "" && rule.Tag == "":
		return "refs/heads/" + rule.Branch
	case rule.Tag != "" && rule.Branch == "":
		return "refs/tags/" + rule.Tag
	}
	return ""
}

// CodeDirectPush is who may push to a protected ref directly.
type CodeDirectPush string

const (
	// CodeDirectPersons lets a person push to it, and nobody else: a seat's
	// push is refused protected_direct, so a seat's work reaches the ref when
	// a person pushes it — what unset reads as.
	CodeDirectPersons CodeDirectPush = "persons"
	// CodeDirectNone lets nobody push to it: the ref is frozen.
	CodeDirectNone CodeDirectPush = "none"
)

// CodeDirectPushes is the closed set: the one list the validator and the
// schema read. The code domain's own set is held to it in both directions by
// the engine's TestEveryDeclaredDirectPushIsOneTheDomainRecords, because
// config cannot import the domain that imports it.
var CodeDirectPushes = []CodeDirectPush{CodeDirectPersons, CodeDirectNone}

// Valid reports a value this build knows. Unset is valid: it reads as
// persons ([CodeDirectPush.Resolved]).
func (d CodeDirectPush) Valid() bool { return d == "" || slices.Contains(CodeDirectPushes, d) }

// Resolved is the value with its default applied: unset is persons.
func (d CodeDirectPush) Resolved() CodeDirectPush {
	if d == "" {
		return CodeDirectPersons
	}
	return d
}

// CodeMergeBy is who may merge into the branches a gated rule matches, and
// code.merge_by_floor's least one for every protected branch.
type CodeMergeBy string

const (
	// CodeMergeByWriters lets anybody who may write to the repository merge —
	// what unset reads as.
	CodeMergeByWriters CodeMergeBy = "writers"
	// CodeMergeByMaintainers lets only those who maintain the repository
	// merge.
	CodeMergeByMaintainers CodeMergeBy = "maintainers"
	// CodeMergeByPersons lets only a person merge, never a seat.
	CodeMergeByPersons CodeMergeBy = "persons"
)

// CodeMergeBys is the closed set, loosest first: the one list the validator
// and the schema read. The code domain's own set is held to it in both
// directions by the engine's TestEveryDeclaredMergeByIsOneTheDomainRecords,
// because config cannot import the domain that imports it — and so is each of
// the merge gate's closed sets below.
var CodeMergeBys = []CodeMergeBy{CodeMergeByWriters, CodeMergeByMaintainers, CodeMergeByPersons}

// Valid reports a value this build knows. Unset is valid: it reads as writers
// ([CodeMergeBy.Resolved]).
func (m CodeMergeBy) Valid() bool { return m == "" || slices.Contains(CodeMergeBys, m) }

// Resolved is the value with its default applied: unset is writers.
func (m CodeMergeBy) Resolved() CodeMergeBy {
	if m == "" {
		return CodeMergeByWriters
	}
	return m
}

// CodeApprovals is how many counted approvals a merge waits for.
type CodeApprovals string

const (
	// CodeApprovalsNone waits for none — what unset reads as.
	CodeApprovalsNone CodeApprovals = "none"
	// CodeApprovalsOne waits for one.
	CodeApprovalsOne CodeApprovals = "one"
)

// CodeApprovalCounts is the closed set, fewest first.
var CodeApprovalCounts = []CodeApprovals{CodeApprovalsNone, CodeApprovalsOne}

// Valid reports a value this build knows. Unset is valid: it reads as none.
func (a CodeApprovals) Valid() bool { return a == "" || slices.Contains(CodeApprovalCounts, a) }

// Resolved is the value with its default applied: unset is none.
func (a CodeApprovals) Resolved() CodeApprovals {
	if a == "" {
		return CodeApprovalsNone
	}
	return a
}

// CodeLandingPolicy is how a gated branch's queue lands a change.
type CodeLandingPolicy string

const (
	// CodeLandingPreLand tests each change merged on the branch before it
	// lands — what unset reads as.
	CodeLandingPreLand CodeLandingPolicy = "pre_land"
	// CodeLandingPostLand lands changes ahead of their checks, which then run
	// on the branch's newest tip.
	CodeLandingPostLand CodeLandingPolicy = "post_land"
)

// CodeLandingPolicies is the closed set, the default first.
var CodeLandingPolicies = []CodeLandingPolicy{CodeLandingPreLand, CodeLandingPostLand}

// Valid reports a value this build knows. Unset is valid: it reads as
// pre_land.
func (l CodeLandingPolicy) Valid() bool { return l == "" || slices.Contains(CodeLandingPolicies, l) }

// Resolved is the value with its default applied: unset is pre_land.
func (l CodeLandingPolicy) Resolved() CodeLandingPolicy {
	if l == "" {
		return CodeLandingPreLand
	}
	return l
}

// CodeDeleteSource is whether a merge deletes its change's source branch.
type CodeDeleteSource string

const (
	// CodeDeleteSeats deletes a source branch in a seat's own namespace, in
	// the record that merges it — what unset reads as.
	CodeDeleteSeats CodeDeleteSource = "seats"
	// CodeDeleteNone keeps every source branch.
	CodeDeleteNone CodeDeleteSource = "none"
)

// CodeDeleteSources is the closed set, the default first.
var CodeDeleteSources = []CodeDeleteSource{CodeDeleteSeats, CodeDeleteNone}

// Valid reports a value this build knows. Unset is valid: it reads as seats.
func (d CodeDeleteSource) Valid() bool { return d == "" || slices.Contains(CodeDeleteSources, d) }

// Resolved is the value with its default applied: unset is seats.
func (d CodeDeleteSource) Resolved() CodeDeleteSource {
	if d == "" {
		return CodeDeleteSeats
	}
	return d
}

// CodeChangeClass names what a change does NOT touch, as the code domain's
// lexer classes it: a list of them exempts a change from an approval.
type CodeChangeClass string

const (
	// CodeChangeClassNoCodeChange is a revision that moved nothing — a
	// revision's class and never a path's, so no list here takes it.
	CodeChangeClassNoCodeChange CodeChangeClass = "no_code_change"
	// CodeChangeClassWhitespaceOnly is a path whose only change is the space
	// between its tokens.
	CodeChangeClassWhitespaceOnly CodeChangeClass = "whitespace_only"
	// CodeChangeClassCommentOnly is a path whose only change is in its
	// comments, none of them a directive.
	CodeChangeClassCommentOnly CodeChangeClass = "comment_only"
)

// CodeChangeClasses is the closed set, in the code domain's order.
var CodeChangeClasses = []CodeChangeClass{CodeChangeClassNoCodeChange, CodeChangeClassWhitespaceOnly,
	CodeChangeClassCommentOnly}

// CodePathClasses is the classes a PATH has, narrowest first: the ones a
// gate's approvals_unless and a human path's unless take.
var CodePathClasses = []CodeChangeClass{CodeChangeClassWhitespaceOnly, CodeChangeClassCommentOnly}

// Valid reports a class this build knows. There is no unset class: a list
// names classes, or is empty.
func (c CodeChangeClass) Valid() bool { return slices.Contains(CodeChangeClasses, c) }

// CodeLimits are what one push and one repository may hold. An unset field
// takes its default ([CodeLimits.Resolved]).
type CodeLimits struct {
	MaxPushBytes int64 `yaml:"max_push_bytes,omitempty" json:"max_push_bytes,omitempty" js:"min=1048576;max=1073741824" desc:"The largest pack one push may send; unset is 1 GiB, the most it may be."`
	MaxBlobBytes int64 `yaml:"max_blob_bytes,omitempty" json:"max_blob_bytes,omitempty" js:"min=1024;max=1073741824" desc:"The largest file (blob) a push may carry, at most max_push_bytes; unset is 50 MiB. A larger binary belongs in tracker files."`
	RepoMaxBytes int64 `yaml:"repo_max_bytes,omitempty" json:"repo_max_bytes,omitempty" js:"min=1048576;max=1099511627776" desc:"The most packed bytes one repository may hold, at least max_push_bytes; unset is 20 GiB. Every git node serving a repository caches it whole."`
	// PushesPerSeatDay is how many pushes one seat may land in one company
	// day, every repository together; 0 is unset, the default
	// ([DefaultCodePushesPerSeatDay]). Persons are never counted.
	PushesPerSeatDay int `yaml:"pushes_per_seat_day,omitempty" json:"pushes_per_seat_day,omitempty" js:"min=1;max=100000" desc:"How many pushes one seat may land in one day on the company's calendar, every repository together: 1..100000; unset is 1000. A runaway bound on what a seat pushing in a loop costs the code log, not a budget; a person's pushes are never counted."`
}

// Resolved is the limits with their defaults applied — THE ONE PLACE they are
// applied: config's own order rules below and the code domain's
// (internal/code.LimitsOf) both read it, so what `crewlet validate` judges and
// what a push is held to are one reading of the document.
func (l CodeLimits) Resolved() CodeLimits {
	if l.MaxPushBytes == 0 {
		l.MaxPushBytes = DefaultCodeMaxPushBytes
	}
	if l.MaxBlobBytes == 0 {
		l.MaxBlobBytes = DefaultCodeMaxBlobBytes
	}
	if l.RepoMaxBytes == 0 {
		l.RepoMaxBytes = DefaultCodeRepoMaxBytes
	}
	if l.PushesPerSeatDay == 0 {
		l.PushesPerSeatDay = DefaultCodePushesPerSeatDay
	}
	return l
}

// zero is a block that sets no limit.
func (l CodeLimits) zero() bool { return l == CodeLimits{} }

// validate holds the code host's RUNNABLE rules, against the chart o of the
// same document: nothing of the block but native where nothing hosts code; a
// retention a duration can hold; the limits ([CodeLimits.validate]); and every
// declared repository ([CodeRepo.validate]), each name declared once.
//
// RUNNABLE because a revision that breaks one cannot run as written: the
// reconcile cannot record a repository from it — an owner the chart does not
// resolve has no owner to record, two rules on one pattern are one rule the
// domain refuses — the limits are what every push is held to, and a retention
// no duration holds purges every deleted repository at once. So a revision
// that breaks one is refused before it is applied, like the tracker's and the
// knowledge base's own rules. The two POLICY bounds — how many repositories
// one document declares and how long a deletion may be kept — are admission
// rules instead ([Code.validateAdmission]): nothing about running depends on
// either.
func (c Code) validate(path Path, o *org.Organization) error {
	var p problems
	if !c.Native {
		// NOTHING HOSTS ANY OF IT, so each would read as live and do
		// nothing: limits holding no push, a retention purging nothing,
		// repositories nobody creates.
		if !c.Limits.zero() {
			p.add(at(path, "limits"), ErrConflict,
				"nothing hosts code on this company: set code.native, or remove code.limits")
		}
		if c.DeletedRetentionDays != 0 {
			p.add(at(path, "deleted_retention_days"), ErrConflict,
				"nothing hosts code on this company: set code.native, or remove "+
					"code.deleted_retention_days")
		}
		if c.IdentityDomain != "" {
			p.add(at(path, "identity_domain"), ErrConflict,
				"nothing hosts code on this company, so no commit is built to name it: set "+
					"code.native, or remove code.identity_domain")
		}
		if len(c.Repos) > 0 {
			p.add(at(path, "repos"), ErrConflict,
				"nothing hosts code on this company, so none of these %d repositories "+
					"would be created: set code.native, or remove code.repos", len(c.Repos))
		}
		if !c.Scratch.zero() {
			p.add(at(path, "scratch"), ErrConflict,
				"code.scratch configures the native code host, and nothing hosts code on this "+
					"company: set code.native: true, or remove code.scratch")
		}
		if !c.Checks.zero() {
			p.add(at(path, "checks"), ErrConflict,
				"nothing hosts code on this company, so no repository defines a check to run: set "+
					"code.native, or remove code.checks")
		}
	}
	switch {
	case !c.MergeByFloor.Valid():
		p.add(at(path, "merge_by_floor"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			c.MergeByFloor, names(CodeMergeBys), CodeMergeByWriters)
	case !c.Native && c.MergeByFloor != "":
		p.add(at(path, "merge_by_floor"), ErrConflict,
			"nothing hosts code on this company, so no merge is held to it: set code.native, or remove "+
				"code.merge_by_floor")
	}
	if d := c.DeletedRetentionDays; d < 0 || d > codeMaxHeldRetentionDays {
		p.add(at(path, "deleted_retention_days"), ErrOutOfRange,
			"must be 1..%d days, or unset for the default %d, got %d",
			CodeMaxDeletedRetentionDays, DefaultCodeDeletedRetentionDays, d)
	}
	if d := c.IdentityDomain; d != "" && !validDomainName(d) {
		p.add(at(path, "identity_domain"), ErrShape,
			"%q is not a lowercase DNS name of at most %d bytes: labels of letters, digits and hyphens, "+
				"none starting or ending with a hyphen, joined by dots (example.com), or unset for %s",
			d, CodeMaxIdentityDomainBytes, DefaultCodeIdentityDomain)
	}
	p.wrap(c.Limits.validate(at(path, "limits")))
	p.wrap(c.Scratch.validate(at(path, "scratch")))
	p.wrap(c.Checks.validate(at(path, "checks")))
	p.wrap(validateCodeRepos(at(path, "repos"), c.Repos, o))
	return p.err()
}

// validateAdmission holds the code host's ADMISSION rules at path: at most
// [CodeMaxRepos] repositories, and a retention of at most
// [CodeMaxDeletedRetentionDays].
//
// POLICY, which a later build is free to change: the reconcile decides any
// number of declarations and the retention duty purges by any retention a
// duration holds, so a revision past either still runs — and held to them on
// APPLY, a build that raised one would have its revisions refused by this one
// during a rolling upgrade, splitting the fleet's epoch. A submitted document
// is refused for them; an applied revision that breaks them is warned about.
// A retention past what a duration holds is the runnable rule's, and is not
// reported twice.
func (c Code) validateAdmission(path Path) error {
	var p problems
	if d := c.DeletedRetentionDays; d > CodeMaxDeletedRetentionDays && d <= codeMaxHeldRetentionDays {
		p.add(at(path, "deleted_retention_days"), ErrOutOfRange,
			"must be 1..%d days, or unset for the default %d, got %d: past a year a "+
				"deleted repository is an archive nobody restores, which every backup "+
				"carries — export it instead", CodeMaxDeletedRetentionDays,
			DefaultCodeDeletedRetentionDays, d)
	}
	if n := len(c.Repos); n > CodeMaxRepos {
		p.add(at(path, "repos"), ErrOutOfRange,
			"%d repositories, past the %d one document may declare — a typo guard: "+
				"a list this long is a generated document, so check what wrote it",
			n, CodeMaxRepos)
	}
	return p.err()
}

// validate holds code.limits' rules at path: each limit inside its bounds, and
// the three sizes in the order a push meets them — a file inside a push, a
// push inside a repository.
//
// THE ORDER IS OF THE RESOLVED LIMITS, because that is what a push is held to:
// a push limit set alone below the default file limit admits no push carrying
// a file the default allows. Each such problem is reported AT THE FIELD THE
// DOCUMENT SETS — the line its author can find — naming the default it met.
// And only once every size is in range: a value outside its bounds is one
// mistake, and an order problem beside it would be the same mistake twice. The
// seat's daily quota is a count no order rule reads, judged on its own.
func (l CodeLimits) validate(limits Path) error {
	var p problems
	if n := l.PushesPerSeatDay; n != 0 && (n < CodeMinPushesPerSeatDay || n > CodePushesPerSeatDayCeiling) {
		p.add(at(limits, "pushes_per_seat_day"), ErrOutOfRange,
			"%d is outside %d..%d pushes a seat a day, or unset for the default %d", n,
			CodeMinPushesPerSeatDay, CodePushesPerSeatDayCeiling, DefaultCodePushesPerSeatDay)
	}
	before := len(p)
	bytesInRange(&p, limits, "max_push_bytes", l.MaxPushBytes, CodeMinPushBytes, CodeMaxPushBytesCeiling)
	bytesInRange(&p, limits, "max_blob_bytes", l.MaxBlobBytes, CodeMinBlobBytes, CodeMaxPushBytesCeiling)
	bytesInRange(&p, limits, "repo_max_bytes", l.RepoMaxBytes, CodeMinRepoBytes, CodeMaxRepoBytesCeiling)
	if len(p) > before {
		return p.err()
	}
	r := l.Resolved()
	if r.MaxBlobBytes > r.MaxPushBytes {
		field := "max_blob_bytes"
		if l.MaxBlobBytes == 0 {
			field = "max_push_bytes"
		}
		p.add(at(limits, field), ErrConflict,
			"%s is more than %s: a file cannot travel in a push smaller than it. "+
				"Set max_blob_bytes at most max_push_bytes",
			limitNamed("max_blob_bytes", l.MaxBlobBytes, r.MaxBlobBytes),
			limitNamed("max_push_bytes", l.MaxPushBytes, r.MaxPushBytes))
	}
	if r.MaxPushBytes > r.RepoMaxBytes {
		field := "max_push_bytes"
		if l.MaxPushBytes == 0 {
			field = "repo_max_bytes"
		}
		p.add(at(limits, field), ErrConflict,
			"%s is more than %s: a push cannot land in a repository smaller than it. "+
				"Set max_push_bytes at most repo_max_bytes",
			limitNamed("max_push_bytes", l.MaxPushBytes, r.MaxPushBytes),
			limitNamed("repo_max_bytes", l.RepoMaxBytes, r.RepoMaxBytes))
	}
	return p.err()
}

// limitNamed is one limit as an order problem names it: its value, and that
// it is the default where the document left it unset — the half of the
// problem its author did not write.
func limitNamed(name string, set, resolved int64) string {
	if set == 0 {
		return fmt.Sprintf("%s (unset: its default, %d)", name, resolved)
	}
	return fmt.Sprintf("%s (%d)", name, resolved)
}

// validateCodeRepos holds code.repos at path: each entry valid, and each name
// declared once. (How many entries is an admission rule:
// [Code.validateAdmission].)
//
// A DUPLICATE IS REPORTED AT THE LATER ENTRY, which is the one an author
// added: one name is one repository — the address it is cloned by, and the
// subject a create arbitrates on — so a second declaration would settle the
// first's settings to its own at every activation. Only among names that are
// names: one that is not is already refused, and calling it a duplicate too
// would report one mistake twice.
func validateCodeRepos(path Path, repos []CodeRepo, o *org.Organization) error {
	var p problems
	owners := newCodeOwners(o)
	first := make(map[string]int, len(repos))
	for i, r := range repos {
		entry := idx(path, i)
		p.wrap(r.validate(entry, owners))
		if !gitref.ValidProjectName(r.Name) {
			continue
		}
		if was, dup := first[r.Name]; dup {
			p.add(at(entry, "name"), ErrConflict,
				"%q is declared twice (first at repos[%d]): a name is one repository, so "+
					"each activation would settle one declaration's settings over the "+
					"other's. Declare it once", r.Name, was)
			continue
		}
		first[r.Name] = i
	}
	return p.err()
}

// validate holds one declared repository at path, against the document's
// chart: its name, its one owner resolving on the chart, its visibility, its
// search mode, its default branch, its description, its protection rules — at most
// [CodeMaxProtections], each naming exactly one of a branch or a tag pattern
// in gitref's grammar, each pattern protected once, no force on a tag, and
// each one's merge gate ([CodeProtection.validateGate]) — and its path owners
// ([validatePathOwners]).
func (r CodeRepo) validate(path Path, owners *codeOwners) error {
	var p problems
	switch {
	case r.Name == "":
		p.add(at(path, "name"), ErrMissing,
			"a repository needs a name: it is the address the repository is cloned by")
	case !gitref.ValidProjectName(r.Name):
		// A NAME WITH A DOT IS A SEAT'S OWN SCRATCH REPOSITORY (gitref's
		// <handle>.<slug>), which only that seat's tool creates: declared, it
		// would read as the seat's and squat the name its create would take.
		why := ""
		if _, scratch := gitref.ScratchName(r.Name); scratch {
			why = " — a name with a dot is a seat's own scratch repository, which only that seat's " +
				"tool creates"
		}
		p.add(at(path, "name"), ErrShape,
			"%q is not a project repository's name: one label of lowercase letters, digits and "+
				"hyphens, starting and ending with a letter or a digit, at most %d%s",
			r.Name, gitref.MaxRepoNameBytes, why)
	}
	r.Owner.validate(&p, at(path, "owner"), owners)
	if !r.Visibility.Valid() {
		p.add(at(path, "visibility"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			r.Visibility, names(CodeVisibilities), CodeVisibilityCompany)
	}
	if !r.Search.Valid() {
		p.add(at(path, "search"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			r.Search, names(CodeSearchModes), CodeSearchIndexed)
	}
	switch {
	case r.DefaultBranch == "":
	case !gitref.ValidBranch(r.DefaultBranch):
		p.add(at(path, "default_branch"), ErrShape,
			"%q is not a branch name git would take (git check-ref-format): unset it for %s, "+
				"or name a branch", r.DefaultBranch, DefaultCodeBranch)
	case gitref.ReservedSeatBranch(r.DefaultBranch):
		// A shape the rest of the grammar takes, conflicting with the code
		// host's own use of the name: no push may ever create it.
		p.add(at(path, "default_branch"), ErrConflict,
			"%q is reserved: seats/ holds only each seat's own namespace, seats/<handle>/, "+
				"and no push may create seats or seats/<one name> as a branch; unset it for %s, "+
				"or name another branch", r.DefaultBranch, DefaultCodeBranch)
	}
	switch {
	case len(r.Description) > CodeMaxDescriptionBytes:
		p.add(at(path, "description"), ErrOutOfRange,
			"%d bytes, past the %d a description may hold: it rides every settings "+
				"record and every listing row, so keep it to one paragraph",
			len(r.Description), CodeMaxDescriptionBytes)
	case !utf8.ValidString(r.Description):
		p.add(at(path, "description"), ErrShape, "is not valid UTF-8")
	}
	protect := at(path, "protect")
	if len(r.Protect) > CodeMaxProtections {
		p.add(protect, ErrOutOfRange,
			"%d rules, past the %d one repository may carry: every rule that matches is "+
				"asked about every ref a push moves, so fold rules into globs",
			len(r.Protect), CodeMaxProtections)
	}
	first := make(map[string]int, len(r.Protect))
	for k, rule := range r.Protect {
		rulePath := idx(protect, k)
		if !rule.validate(&p, rulePath) {
			continue
		}
		pattern := rule.Pattern()
		if was, dup := first[pattern]; dup {
			p.add(rulePath, ErrConflict,
				"%s is protected twice (first at protect[%d]): one pattern is one rule, "+
					"so merge the two", pattern, was)
			continue
		}
		first[pattern] = k
	}
	validatePathOwners(&p, at(path, "path_owners"), r.PathOwners, owners)
	r.validateRemotes(&p, path, owners)
	return p.err()
}

// validateRemotes holds the repository's mirrors and its import at path into
// p: each URL a remote's, read by the one rule its dial reads it by
// ([gitref.ParseRemoteURL]) and at most [CodeMaxRemoteURLBytes], and each
// credential one whole ${VAR} reference ([validateCodeRemote]) — a push
// mirror's required; a push mirror's seat one of the chart's that reads the
// repository ([CodeRepo.validateMirrorSeat]) and its refs ([validateMirrorRefs]);
// and none of the combinations that would do nothing: an import beside a pull
// mirror, which is imported from its own source when it is created, and either
// beside archived, which takes neither a sync nor an import.
//
// RUNNABLE, as an owner's rules are: the code domain refuses settings carrying
// a URL no dial reads or a credential that is not a reference, so the
// reconcile could not record one, and a push mirror whose seat the chart
// lacks, or that does not read what it would publish, has nobody to push as.
func (r CodeRepo) validateRemotes(p *problems, path Path, owners *codeOwners) {
	if m := r.MirrorFrom; m != nil {
		from := at(path, "mirror_from")
		validateCodeRemote(p, from, m.URL, m.Credential, false)
		switch {
		case r.Import != nil:
			p.add(at(path, "import"), ErrConflict,
				"a pull mirror is imported from mirror_from when it is created, so import beside it is a "+
					"second source: remove import")
		case r.Archived:
			p.add(from, ErrConflict,
				"an archived repository takes no sync, so a pull mirror of it would never move: remove "+
					"mirror_from to keep it as it stands, or archived to follow its source")
		}
	}
	if i := r.Import; i != nil {
		validateCodeRemote(p, at(path, "import"), i.URL, i.Credential, false)
		if r.Archived && r.MirrorFrom == nil {
			p.add(at(path, "import"), ErrConflict,
				"an import fills an active repository, and an archived one takes nothing: leave archived "+
					"unset until the import is done — import is read only when the repository is created, "+
					"so remove it then, and archive the repository")
		}
	}
	if m := r.MirrorTo; m != nil {
		to := at(path, "mirror_to")
		validateCodeRemote(p, to, m.URL, m.Credential, true)
		r.validateMirrorSeat(p, at(to, "as"), m.As, owners)
		validateMirrorRefs(p, at(to, "refs"), m.Refs)
	}
}

// validateCodeRemote holds one remote at path into p: its URL present, at most
// [CodeMaxRemoteURLBytes] and one the dial reads ([gitref.ParseRemoteURL]), and
// its credential — required where needed — ONE WHOLE ${VAR} REFERENCE of at
// most [CodeMaxCredentialRefBytes] and nothing else.
//
// A REFERENCE, NEVER A TOKEN: the code domain records the credential as it is
// written, on every record of the repository's settings, which every data node
// keeps and every backup carries — so a token written here would be a secret
// at rest in the code log, and the reference is resolved only when the remote
// is dialed, from the secret store and then the environment. The refusal never
// quotes the value: a literal is a token.
func validateCodeRemote(p *problems, path Path, url, credential string, needCredential bool) {
	switch {
	case strings.TrimSpace(url) == "":
		p.add(at(path, "url"), ErrMissing, "the repository at the other host: an https URL")
	case len(url) > CodeMaxRemoteURLBytes:
		p.add(at(path, "url"), ErrOutOfRange, "%d bytes, past the %d a remote's URL may hold", len(url),
			CodeMaxRemoteURLBytes)
	default:
		if _, err := gitref.ParseRemoteURL(url); err != nil {
			p.add(at(path, "url"), ErrShape, "%v", err)
		}
	}
	switch {
	case credential == "" && needCredential:
		p.add(at(path, "credential"), ErrMissing,
			"a push authenticates: name the ${VAR} that holds the token, which the secret store or the "+
				"environment answers when the code host dials")
	case credential == "":
	case len(credential) > CodeMaxCredentialRefBytes:
		p.add(at(path, "credential"), ErrOutOfRange, "%d bytes, past the %d a credential's reference may hold",
			len(credential), CodeMaxCredentialRefBytes)
	case !wholeReference(credential):
		p.add(at(path, "credential"), ErrShape,
			"a credential is one whole ${VAR} reference and nothing else — never the token itself, which "+
				"every record of the repository's settings would carry: put the token in the secret store "+
				"(crewlet secrets set NAME) and write ${NAME}")
	}
}

// validateMirrorSeat holds a push mirror's seat at path into p: a seat of the
// chart, by its handle, that reads the repository ([CodeRepo.mirrorSeatReads]).
func (r CodeRepo) validateMirrorSeat(p *problems, path Path, as string, owners *codeOwners) {
	handle := strings.TrimSpace(as)
	seat := owners.index.SeatByHandle(handle)
	switch {
	case handle == "":
		p.add(path, ErrMissing,
			"a push mirror pushes as a seat's own identity at the other host, never a bot: name the seat "+
				"by its handle")
	case seat == nil:
		p.add(path, ErrUnknownValue, "%q is no seat's handle in this company; the seats are: %s", as,
			owners.seats())
	case !r.mirrorSeatReads(owners.index, seat):
		p.add(path, ErrConflict,
			"seat %s does not read %s, which only its owner's own seats and leads read (visibility: unit): "+
				"a push mirror publishes only what the seat it pushes as reads — name a seat the chart "+
				"relates to the owner, or a person's seat", handle, r.Name)
	}
}

// mirrorSeatReads reports whether seat reads the repository r, as the code
// domain grants a read (internal/code's GrantOf, ADR-0058): a PERSON'S seat
// reads every repository, and an agent's every one visible to the company —
// every tier this document can spell reads, so no tier narrows it — and one
// visible to its owner's unit alone where the chart relates the seat to the
// owner: a seat in the owning unit or a unit below it, a lead over it (the
// effective lead of it or of a unit above it), or — owned by a seat — that
// seat, or a lead over the unit holding it.
//
// THE DOMAIN'S RULE IN THE DOCUMENT'S TERMS, because config cannot import the
// domain that imports it: the engine's
// TestADeclaredMirrorSeatReadsWhatTheDomainGrantsIt holds the two to one
// verdict over every relation a chart can give.
func (r CodeRepo) mirrorSeatReads(x *org.Index, seat *org.Role) bool {
	if seat.IsHuman() || r.Visibility.Resolved() == CodeVisibilityCompany {
		return true
	}
	var over *org.Unit
	switch unit, owner := strings.TrimSpace(r.Owner.Unit), strings.TrimSpace(r.Owner.Seat); {
	case unit != "":
		if over = x.UnitByRef(unit); over == nil {
			return false
		}
		if slices.Contains(x.UnitChainFor(seat), over) {
			return true
		}
	case owner != "":
		if owner == seat.Handle() {
			return true
		}
		holder := x.SeatByHandle(owner)
		if holder == nil {
			return false
		}
		over = x.UnitFor(holder)
	}
	for u := over; u != nil; u = x.Parent(u) {
		if lead := x.EffectiveLead(u); lead != nil && lead.Handle() == seat.Handle() {
			return true
		}
	}
	return false
}

// validateMirrorRefs holds a push mirror's refs at path into p: at most
// [CodeMaxMirrorRefs], each a protection pattern of gitref's grammar — under
// refs/heads/ or refs/tags/ — that reaches no seat's own namespace
// ([gitref.PatternReachesSeats]), and each once.
//
// NO SEAT'S BRANCHES LEAVE: refs/heads/seats/<handle>/ is where a seat's work
// waits for a person, its experiments among it, and a mirror publishes what
// the company merged, never what a seat has not yet offered — so a pattern
// that could match one ref there is refused, refs/heads/** among them, while
// refs/heads/*, which matches only top-level branches, is not.
func validateMirrorRefs(p *problems, path Path, refs []string) {
	if len(refs) > CodeMaxMirrorRefs {
		p.add(path, ErrOutOfRange, "%d patterns, past the %d one push mirror may name", len(refs),
			CodeMaxMirrorRefs)
	}
	first := make(map[string]int, len(refs))
	for k, pattern := range refs {
		entry := idx(path, k)
		switch was, dup := first[pattern]; {
		case !gitref.ValidPattern(pattern):
			p.add(entry, ErrShape, "%q is not refs/heads/<glob> or refs/tags/<glob>", pattern)
		case gitref.PatternReachesSeats(pattern):
			p.add(entry, ErrConflict,
				"%s reaches %s<handle>/, where each seat's own branches wait for a person, and a mirror "+
					"publishes what the company merged: name the branches it pushes — refs/heads/*, or a "+
					"family such as refs/heads/release/*", pattern, gitref.SeatsRoot)
		case dup:
			p.add(entry, ErrConflict, "%s is named twice (first at refs[%d])", pattern, was)
		default:
			first[pattern] = k
		}
	}
}

// validatePathOwners holds a repository's path owners at path into p: at most
// [CodeMaxPathOwners], each naming exactly one of a unit or a seat the chart
// resolves, and 1 to [CodeMaxPathOwnerPaths] globs in gitref's path grammar,
// each once — and each OWNER once, by what the chart resolves it to, so a unit
// named once by its key and once by its name is one owner named twice — and
// the whole at most [CodeMaxPathOwnersBytes] as the code log records it,
// judged last and only of a list nothing else is refused in, since an entry
// refused for what it says is refused for that.
//
// RUNNABLE, as an owner's rules are: the reconcile records a unit by the key
// the chart resolves, so one it cannot resolve has no key to record, and the
// code domain refuses settings past these bounds, a glob its grammar does not
// read and an owner listed twice — the one form a record carries, which a
// declaration listing an owner twice is not.
func validatePathOwners(p *problems, path Path, list []CodePathOwner, owners *codeOwners) {
	before := len(*p)
	if len(list) > CodeMaxPathOwners {
		p.add(path, ErrOutOfRange,
			"%d path owners, past the %d one repository may name: list a team rather than each of its "+
				"seats — a unit's owner is its effective lead", len(list), CodeMaxPathOwners)
	}
	first := make(map[codeRecordedOwner]int, len(list))
	for k, po := range list {
		entry := idx(path, k)
		who, resolved := po.validate(p, entry, owners)
		if !resolved {
			continue
		}
		if was, dup := first[who]; dup {
			p.add(entry, ErrConflict,
				"%s %s owns paths at path_owners[%d] already: one owner is one entry, so list "+
					"everything it owns in that one", who.Kind, who.Ref, was)
			continue
		}
		first[who] = k
	}
	if len(*p) > before {
		return
	}
	if n, _ := codePathOwnersBytes(owners.index, list); n > CodeMaxPathOwnersBytes {
		p.add(path, ErrOutOfRange,
			"the path owners are %d bytes as the code log records them, past the %d one repository's path "+
				"owners may weigh, since every record of its settings carries them whole: own a directory "+
				"with dir/** rather than each of its files — a glob's &, < and > are six bytes each",
			n, CodeMaxPathOwnersBytes)
	}
}

// PathOwnersBytes is what the repository's path owners weigh as the code log
// records them — the length of their JSON, each owner by what the chart index
// o resolves it to — which [CodeMaxPathOwnersBytes] bounds; false where an
// owner does not resolve, and so has no recorded form to weigh.
func (r CodeRepo) PathOwnersBytes(o *org.Index) (int, bool) {
	return codePathOwnersBytes(o, r.PathOwners)
}

// codePathOwnersBytes is [CodeRepo.PathOwnersBytes] over list.
//
// IN THE SHAPE THE CODE DOMAIN RECORDS THEM ([codeRecordedPathOwner]), so its
// figure is the record's: the engine's
// TestDeclaredPathOwnersWeighWhatTheDomainRecords holds the two, or `crewlet
// validate` passes path owners every node's reconcile refuses, or refuses ones
// the domain would record.
func codePathOwnersBytes(o *org.Index, list []CodePathOwner) (int, bool) {
	recorded := make([]codeRecordedPathOwner, len(list))
	for i, po := range list {
		who, ok := po.recordedOwner(o)
		if !ok {
			return 0, false
		}
		recorded[i] = codeRecordedPathOwner{Owner: who, Paths: po.Paths}
	}
	// IT CANNOT FAIL: strings and lists of them are every value it holds, and
	// json.Marshal encodes each of them.
	b, _ := json.Marshal(recorded)
	return len(b), true
}

// codeRecordedPathOwner is a path owner in the shape the code domain records
// it: code.PathOwner's keys, the owner by what the chart resolves it to, and
// its globs as the document lists them, whose canonical order the record
// sorts them into changes no length.
type codeRecordedPathOwner struct {
	Owner codeRecordedOwner `json:"owner"`
	Paths []string          `json:"paths"`
}

// codeRecordedOwner is a path owner's owner as the code domain records it,
// code.Owner's keys: a unit by its key, a seat by its handle.
type codeRecordedOwner struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// recordedOwner is who the entry names as the code domain records it, resolved
// on the chart index o as the reconcile resolves it — a unit by its key,
// whichever spelling the entry used, or a seat by its handle — and false for
// an entry naming neither or both, or an owner o does not hold.
func (po CodePathOwner) recordedOwner(o *org.Index) (codeRecordedOwner, bool) {
	switch unit, seat := strings.TrimSpace(po.Unit), strings.TrimSpace(po.Seat); {
	case unit != "" && seat == "":
		if u := o.UnitByRef(unit); u != nil {
			return codeRecordedOwner{Kind: "unit", Ref: u.Key()}, true
		}
	case seat != "" && unit == "":
		if o.SeatByHandle(seat) != nil {
			return codeRecordedOwner{Kind: "seat", Ref: seat}, true
		}
	}
	return codeRecordedOwner{}, false
}

// validate holds one path owner at path into p, and answers who it resolves
// to ([CodePathOwner.recordedOwner]) where it resolves at all.
func (po CodePathOwner) validate(p *problems, path Path, owners *codeOwners) (codeRecordedOwner, bool) {
	paths := at(path, "paths")
	switch n := len(po.Paths); {
	case n == 0:
		p.add(paths, ErrMissing,
			"a path owner names the paths it owns: one glob or more, such as dir/** for a directory")
	case n > CodeMaxPathOwnerPaths:
		p.add(paths, ErrOutOfRange,
			"%d globs, past the %d one path owner may name: own a directory with dir/** rather than "+
				"each of its files", n, CodeMaxPathOwnerPaths)
	}
	seen := make(map[string]int, len(po.Paths))
	for m, glob := range po.Paths {
		if _, err := gitref.ParsePathGlob(glob); err != nil {
			p.add(idx(paths, m), ErrShape, "%q is not a path glob: %v", glob, err)
			continue
		}
		if was, dup := seen[glob]; dup {
			p.add(idx(paths, m), ErrConflict, "%q is listed twice (first at paths[%d])", glob, was)
			continue
		}
		seen[glob] = m
	}
	switch unit, seat := strings.TrimSpace(po.Unit), strings.TrimSpace(po.Seat); {
	case unit == "" && seat == "":
		p.add(path, ErrMissing,
			"a path owner is a unit or a seat: set unit to a unit's id or name, or seat to a seat's handle")
	case unit != "" && seat != "":
		p.add(path, ErrConflict, "a path owner is one unit or one seat: set unit or seat, not both")
	default:
		if who, ok := po.recordedOwner(owners.index); ok {
			return who, true
		}
		if unit != "" {
			p.add(at(path, "unit"), ErrUnknownValue,
				"%q is no unit of this company's chart, by id or by name; the units are: %s",
				unit, owners.units())
		} else {
			p.add(at(path, "seat"), ErrUnknownValue,
				"%q is no seat's handle in this company; the seats are: %s", seat, owners.seats())
		}
	}
	return codeRecordedOwner{}, false
}

// codeOwners is the document's chart as code.repos' owners are resolved
// against it: an [org.Index] built once for the whole list, and the two lists
// a refusal names, each joined once at the first refusal that needs it.
//
// ONCE A DOCUMENT, because the list is up to [CodeMaxRepos] long: resolved
// through the chart's one-off lookups, every owner walked every unit or every
// seat, deriving each seat's handle, and every owner the chart did not have
// walked it again to list it in its message — at every apply on every node,
// since these are runnable rules. Ten thousand owners on a chart of 300 units
// and 900 seats cost ValidateRunnable 1.92 s that way and 95–101 ms through
// the index (BenchmarkValidatingTheMostRepositories).
type codeOwners struct {
	index        *org.Index
	units, seats func() string
}

// newCodeOwners indexes the chart o for code.repos' owners.
func newCodeOwners(o *org.Organization) *codeOwners {
	x := o.Index()
	return &codeOwners{
		index: x,
		units: sync.OnceValue(func() string { return listOrNone(unitRefs(x), "none") }),
		seats: sync.OnceValue(func() string { return listOrNone(x.Handles(), "none") }),
	}
}

// unitRefs is every unit's key in x, in walk order, as a refusal of an owner's
// unit names it: the key an owner references it by, with the name of the unit
// that key resolves to beside it where the two differ —
// "engineering (Engineering)".
//
// THE KEY AND THE NAME, because a write mints each unit's key from its name
// ([MintIdentities]): listed by key alone, a unit everybody knows as
// Engineering read as "engineering", and the person who typed a name could
// not tell which key it was; listed by name alone, nothing said what the chart
// now holds it under. A unit with no key of its own is keyed on its name, and
// listed once. The name is looked up BY THE KEY ([org.Index.UnitByRef]) rather
// than walked beside it, so a key two units of an unadmitted chart share is
// listed with the unit an owner naming it would get.
func unitRefs(x *org.Index) []string {
	refs := x.UnitKeys()
	for i, key := range refs {
		name := ""
		if u := x.UnitByRef(key); u != nil {
			name = strings.TrimSpace(u.Name)
		}
		if name != "" && name != key {
			refs[i] = key + " (" + name + ")"
		}
	}
	return refs
}

// validate holds an owner at path into p: exactly one of a unit or a seat,
// resolving on the document's chart.
func (w CodeOwner) validate(p *problems, path Path, owners *codeOwners) {
	switch unit, seat := strings.TrimSpace(w.Unit), strings.TrimSpace(w.Seat); {
	case unit == "" && seat == "":
		p.add(path, ErrMissing,
			"a repository needs an owner: set unit to a unit's id or name, or seat to a "+
				"seat's handle")
	case unit != "" && seat != "":
		p.add(path, ErrConflict,
			"a repository has one owner: set unit or seat, not both")
	case unit != "" && owners.index.UnitByRef(unit) == nil:
		p.add(at(path, "unit"), ErrUnknownValue,
			"%q is no unit of this company's chart, by id or by name; the units are: %s",
			unit, owners.units())
	case seat != "" && owners.index.SeatByHandle(seat) == nil:
		p.add(at(path, "seat"), ErrUnknownValue,
			"%q is no seat's handle in this company; the seats are: %s",
			seat, owners.seats())
	}
}

// validate holds one protection rule at path into p, and reports whether it
// names one valid pattern — the rules a caller's uniqueness check reads.
func (rule CodeProtection) validate(p *problems, path Path) bool {
	ok := true
	switch {
	case rule.Branch == "" && rule.Tag == "":
		p.add(path, ErrMissing, "a rule names the refs it protects: set branch or tag")
		return false
	case rule.Branch != "" && rule.Tag != "":
		p.add(path, ErrConflict,
			"a rule protects branches or tags, not both: make it two rules")
		return false
	}
	which, value := "branch", rule.Branch
	if rule.Tag != "" {
		which, value = "tag", rule.Tag
	}
	if !gitref.ValidPattern(rule.Pattern()) {
		p.add(at(path, which), ErrShape,
			"%q is not a %s pattern: a %s name git would take (git check-ref-format), in "+
				"which * matches within one component and a component of ** matches any "+
				"number of them — a glob at most %d components below refs/",
			value, which, which, gitref.MaxPatternComponents-1)
		ok = false
	}
	if rule.AllowForce && rule.Tag != "" {
		p.add(at(path, "allow_force"), ErrConflict,
			"a tag never moves: allow_force has nothing to allow on a tag rule")
		ok = false
	}
	if !rule.DirectPush.Valid() {
		p.add(at(path, "direct_push"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			rule.DirectPush, names(CodeDirectPushes), CodeDirectPersons)
		ok = false
	}
	rule.validateGate(p, path)
	return ok
}

// validateGate holds the rule's merge gate at path — the rule's own path, its
// fields beneath it — into p, RUNNABLE as its pattern is: the code domain
// refuses a gate on a tag rule, a value of no closed set, a landing outside its
// bounds, a list past its bound or naming a value twice, a glob gitref does not
// read, a check no repository can name, no_code_change as a class that
// exempts, and a gate past [CodeMaxGateBytes] — and the reconcile cannot
// record a declaration the domain refuses. The bytes are judged last and only
// of a gate whose every field passed: a field refused is the one mistake to
// name. Whether the gate does anything is a warning's ([Company.codeWarnings]).
func (rule CodeProtection) validateGate(p *problems, path Path) {
	set := rule.gateFields()
	if len(set) == 0 {
		return
	}
	if rule.Tag != "" {
		// AT THE FIRST FIELD THE RULE SETS, naming every one: a tag never
		// moves, so the gate as a whole has nothing to hold — one mistake.
		p.add(at(path, set[0]), ErrConflict,
			"a tag never moves, so no merge reaches one: %s gate merges, which only a branch rule has — "+
				"move them to a rule over the branches they are for", strings.Join(set, ", "))
		return
	}
	before := len(*p)
	if !rule.MergeBy.Valid() {
		p.add(at(path, "merge_by"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			rule.MergeBy, names(CodeMergeBys), CodeMergeByWriters)
	}
	if !rule.Approvals.Valid() {
		p.add(at(path, "approvals"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			rule.Approvals, names(CodeApprovalCounts), CodeApprovalsNone)
	}
	validateGateClasses(p, at(path, "approvals_unless"), "approvals_unless", rule.ApprovalsUnless)
	humans := at(path, "human_paths")
	if n := len(rule.HumanPaths); n > CodeMaxHumanPaths {
		p.add(humans, ErrOutOfRange, "%d paths, past the %d one rule's merge gate may name: a glob holds a "+
			"whole directory (deploy/**), so name fewer", n, CodeMaxHumanPaths)
	}
	globs := make(map[string]int, len(rule.HumanPaths))
	for k, h := range rule.HumanPaths {
		entry := idx(humans, k)
		switch _, err := gitref.ParsePathGlob(h.Glob); {
		case h.Glob == "":
			p.add(at(entry, "glob"), ErrMissing,
				"a human path names the paths it holds: a glob such as .crewlet/** or deploy/**")
		case err != nil:
			p.add(at(entry, "glob"), ErrShape, "%q is not a path glob: %v", h.Glob, err)
		default:
			if was, dup := globs[h.Glob]; dup {
				p.add(at(entry, "glob"), ErrConflict, "%q is listed twice (first at human_paths[%d])", h.Glob,
					was)
				break
			}
			globs[h.Glob] = k
		}
		validateGateClasses(p, at(entry, "unless"), "unless", h.Unless)
	}
	checks := at(path, "checks")
	if n := len(rule.Checks); n > CodeMaxRequiredChecks {
		p.add(checks, ErrOutOfRange, "%d checks, past the %d one rule's merge gate may require", n,
			CodeMaxRequiredChecks)
	}
	named := make(map[string]int, len(rule.Checks))
	for k, name := range rule.Checks {
		switch was, dup := named[name]; {
		case !ValidCodeCheckName(name):
			p.add(idx(checks, k), ErrShape, "%q is not a check's name: a lowercase letter or digit, then up "+
				"to %d more of them, '.', '_' and '-'", name, CodeMaxCheckNameBytes-1)
		case dup:
			p.add(idx(checks, k), ErrConflict, "%q is listed twice (first at checks[%d])", name, was)
		default:
			named[name] = k
		}
	}
	landing := at(path, "landing")
	if !rule.Landing.Policy.Valid() {
		p.add(at(landing, "policy"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			rule.Landing.Policy, names(CodeLandingPolicies), CodeLandingPreLand)
	}
	if d := rule.Landing.Depth; d < 0 || d > CodeMaxQueueDepth {
		p.add(at(landing, "depth"), ErrOutOfRange, "%d is outside 1..%d candidates, or unset for the default %d",
			d, CodeMaxQueueDepth, CodeDefaultQueueDepth)
	}
	if b := rule.Landing.Batch; b < 0 || b > CodeMaxQueueBatch {
		p.add(at(landing, "batch"), ErrOutOfRange, "%d is outside 1..%d changes a candidate, or unset for the "+
			"default %d", b, CodeMaxQueueBatch, CodeDefaultQueueBatch)
	}
	if !rule.DeleteSource.Valid() {
		p.add(at(path, "delete_source"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			rule.DeleteSource, names(CodeDeleteSources), CodeDeleteSeats)
	}
	if len(*p) > before {
		return
	}
	if n := rule.Gate().EncodedBytes(); n > CodeMaxGateBytes {
		p.add(path, ErrOutOfRange, "the rule's merge gate is %d bytes as the code log records it, past the %d "+
			"one gate may weigh: require fewer checks or name fewer human paths — a glob's &, < and > are "+
			"six bytes each", n, CodeMaxGateBytes)
	}
}

// validateGateClasses holds a list of the classes that exempt a change, the
// field name at path, into p: each a PATH class, each once. no_code_change is
// refused for why it is no path's: an approval already carries across a
// revision that changed nothing, so it has nothing to exempt.
func validateGateClasses(p *problems, path Path, name string, list []CodeChangeClass) {
	seen := make(map[CodeChangeClass]int, len(list))
	for k, c := range list {
		switch was, dup := seen[c]; {
		case c == CodeChangeClassNoCodeChange:
			p.add(idx(path, k), ErrConflict, "no_code_change is a revision's class, never a path's: an "+
				"approval already carries across a revision that changed nothing, so it has nothing to "+
				"exempt (want %s)", names(CodePathClasses))
		case !slices.Contains(CodePathClasses, c):
			p.add(idx(path, k), ErrUnknownValue, "%q (want %s)", c, names(CodePathClasses))
		case dup:
			p.add(idx(path, k), ErrConflict, "%q is listed twice (first at %s[%d])", c, name, was)
		default:
			seen[c] = k
		}
	}
}

// codeWarnings is what is valid about the company's code and still worth
// knowing, against its own chart: every seat's code access tier that grants
// nothing ([Company.roleCodeWarnings]); where the engine hosts code, every
// cell that places a seat's box in `direct` ([Company.directBoxWarnings]);
// and every declared repository that
//
//   - is `visibility: unit` and owned by a unit with no lead — effective, so
//     inherited counts — which only persons and the unit's own members read:
//     no lead's seat reads it, because there is none, and every other seat is
//     what the visibility shuts out. Located at the visibility;
//   - has a default branch no rule protects: a person who may push to it may
//     force-push it or delete it, and no merge gate holds what merges into
//     it. Through gitref's own matcher — the one a push's decide asks — so
//     `release/*` warns about main and `**` does not. Located at the field to
//     ADD, the repository's protect;
//   - has a rule whose merge gate sets a field that does nothing as the rule
//     stands ([idleGateFields]), located at that field.
//
// NOT AN ARCHIVED REPOSITORY'S BRANCH: it takes no push at all — the decide
// refuses the whole push — so nobody can force-push or delete its branch, and
// saying so would be false. Unarchiving one that still has no rule brings the
// warning back with the revision that does it. Its visibility still warns:
// an archived repository is still read.
//
// NOR A PULL MIRROR'S, for the same reason: it takes no push and no change of
// its own (mirror_from), so its default branch moves only by the code host's
// syncs. Its protect list is warned instead, located at it — every rule there
// holds nothing, and a person reading it would believe it gates what it
// cannot. Its visibility still warns: a mirror is read.
//
// THE CHART IS BUILT HERE, and only for a company that hosts code: no other
// advisory reads one, and a company with no repositories would otherwise pay
// a normalised chart at every validate and every apply for nothing.
func (c *Company) codeWarnings() []Warning {
	out := c.roleCodeWarnings()
	if !c.Code.Native {
		return out
	}
	out = append(out, c.directBoxWarnings()...)
	o, _ := c.organization()
	units := o.Index()
	for i, r := range c.Code.Repos {
		if w, ok := unitWithNoLead(units, i, r); ok {
			out = append(out, w)
		}
		if r.MirrorFrom != nil {
			if len(r.Protect) > 0 {
				out = append(out, advisory(at(idx(field("code.repos"), i), "protect"), fmt.Sprintf(
					"%s is a pull mirror (mirror_from): every ref moves by the code host's syncs from its "+
						"source, and it takes no push and no change of its own, so no rule of its protect "+
						"list holds anything (remove protect, or remove mirror_from to push to it here)",
					r.Name)))
			}
			continue
		}
		out = append(out, idleGateFields(i, r)...)
		if r.Archived {
			continue
		}
		branch := r.ResolvedDefaultBranch()
		ref := "refs/heads/" + branch
		if !gitref.ValidBranch(branch) || gitref.ReservedSeatBranch(branch) {
			// Refused by validation, which is the line to read first: a
			// branch no push can create is no branch anybody force-pushes.
			continue
		}
		if slices.ContainsFunc(r.Protect, func(rule CodeProtection) bool {
			return gitref.MatchPattern(rule.Pattern(), ref)
		}) {
			continue
		}
		out = append(out, advisory(at(idx(field("code.repos"), i), "protect"), fmt.Sprintf(
			"%s's default branch %s is unprotected: a person can force-push or delete it, and no merge "+
				"gate holds what merges into it (add {branch: %s} to its protect list, with merge_by, "+
				"approvals, human_paths or checks to gate its merges)", r.Name, branch, branch)))
	}
	return out
}

// directBoxWarnings is one warning for every place a company hosting code puts
// a seat's box in the `direct` cell: at the seat's `sandbox.run_in`, at the
// `run_in` of the providers.llm entry an agent-mode executor's box is placed
// by (once an entry), or once at `providers.sandbox.default_run_in` for every
// box that inherits it — each box as [Company.SeatBoxes] places it, the walk
// the engine's launches are held to.
//
// WHAT IT SAYS IS WHAT THE CELL IS: a `direct` box runs as the engine's own
// user, so it reads every repository a git node's cache holds and, on ANY
// node, the Tier A keyring every run token's key is derived from — so it can
// mint the token of any live coding run and push as that run's seat. Neither
// a repository's `visibility: unit` nor a run's grants bind it. Said whatever
// the repositories' visibility, because the impersonation half holds on a
// company whose every repository is `company`.
//
// A WARNING, NOT A REFUSAL (M3-decisions U1 a): refusing a `direct` run given
// repositories on a git node would close none of those reads — the cache and
// the keyring are files of the engine's user with or without repositories,
// and the keyring is on every node — while it took every single-node
// deployment, whose node holds every role, its `direct` cell. The boundary is
// the cell, which docs/concepts/code-sandbox.md's security model states.
//
// NOT FOR THE DOUBLE (`fake: true`), which answers every cell and runs nothing.
func (c *Company) directBoxWarnings() []Warning {
	catalogue := c.Providers.Sandbox
	if !catalogue.Enabled() || catalogue.Fake {
		return nil
	}
	const reads = "a `direct` box runs as the engine's own user: it can read every repository cached on a " +
		"git node and, on any node, the Tier A keyring that signs run tokens — so it can mint the token of " +
		"ANY live coding run and push as that run's seat, and a repository's `visibility: unit` and a " +
		"run's grants do not bind "
	const remedy = "; run it in `container` or `e2b` to hold it to them"
	var out []Warning
	inherited := false
	entries := map[string]bool{}
	for box := range c.SeatBoxes() {
		if box.Cell != PlacementDirect {
			continue
		}
		switch {
		case box.Executor != "" && c.Providers.LLM[box.Executor].RunIn == "",
			box.Executor == "" && (box.Seat.Sandbox == nil || box.Seat.Sandbox.RunIn == ""):
			inherited = true
		case box.Executor != "":
			if entries[box.Executor] {
				continue
			}
			entries[box.Executor] = true
			out = append(out, advisory(at(entryPath(box.Executor), "run_in"),
				reads+"the agent-mode runs of the seats this entry places"+remedy))
		default:
			w := advisory(at(box.At, "sandbox.run_in"), reads+"this seat's coding runs"+remedy)
			w.Seat = box.Seat.IdentityKey()
			out = append(out, w)
		}
	}
	if inherited {
		out = append(out, advisory(field("providers.sandbox.default_run_in"),
			reads+"the coding runs of the seats that take this default"+remedy))
	}
	return out
}

// idleGateFields is a warning for every merge gate field of code.repos[i], r,
// that does nothing as the rule beside it stands — valid, and a sign the rule
// says less than its author meant:
//
//   - owners_approve on a repository that names no path owners: no change
//     touches an owned path, so it waits for nobody's approval;
//   - landing or auto_revert on a rule that requires no checks: only a branch
//     whose merges wait for checks has a queue to land them, whose enqueuer
//     consents to a revert — every merge into this one lands directly;
//   - approvals_unless beside approvals: none, which exempts changes from
//     approvals the rule never asks.
//
// Each at the field that does nothing — the line to remove, or the one beside
// it to set. A tag rule is skipped: any gate on one is validation's refusal,
// the line to read.
func idleGateFields(i int, r CodeRepo) []Warning {
	var out []Warning
	for k, rule := range r.Protect {
		if rule.Branch == "" || rule.Tag != "" {
			continue
		}
		rulePath := idx(at(idx(field("code.repos"), i), "protect"), k)
		idle := func(name, why string) {
			out = append(out, advisory(at(rulePath, name), fmt.Sprintf("%s's rule on %s sets %s, %s",
				r.Name, rule.Branch, name, why)))
		}
		if rule.OwnersApprove && len(r.PathOwners) == 0 {
			idle("owners_approve", "and the repository names no path owners: no change touches an owned "+
				"path, so it waits for nobody's approval (name path_owners, or remove owners_approve)")
		}
		if len(rule.Checks) == 0 && rule.Landing != (CodeLanding{}) {
			idle("landing", "and requires no checks: only a branch whose merges wait for checks has a queue "+
				"to land them, so every merge into this one lands directly (require checks, or remove landing)")
		}
		if len(rule.Checks) == 0 && rule.AutoRevert {
			idle("auto_revert", "and requires no checks: a revert is consented to when a change is queued, "+
				"and only a branch whose merges wait for checks has a queue (require checks, or remove "+
				"auto_revert)")
		}
		if len(rule.ApprovalsUnless) > 0 && rule.Approvals.Resolved() == CodeApprovalsNone {
			idle("approvals_unless", "beside approvals: none, so it exempts changes from approvals the rule "+
				"never asks (set approvals: one, or remove approvals_unless)")
		}
	}
	return out
}

// unitWithNoLead is the warning about code.repos[i], r, when it is
// `visibility: unit` and owned by a unit that has no effective lead — the unit
// and its lead both found through units, the chart's index. An owner the chart
// does not resolve is validation's refusal, which is the line to read, and is
// not warned about too.
//
// THE LEAD THROUGH THE INDEX, never [org.Organization.EffectiveLead]: a team's
// lead is usually inherited from its division, which the chart's own method
// finds by searching the team's subtree and then walking every seat of the
// chart, so asked of every repository's owner it cost the warnings repositories
// × seats. Measured (BenchmarkWarningAboutTheMostUnitRepositories, ten thousand
// `visibility: unit` repositories owned by 300 teams inheriting their 30
// divisions' leads, 930 seats; four vCPUs of a 2.8 GHz Xeon under a load
// average near 14): 166–184 ms, 85 MB and 3.4 million allocations a call
// through the chart, 16–18 ms and 6.9 MB through the index — against
// 14–17 ms and 6.7 MB for the same document with the visibility unset.
func unitWithNoLead(units *org.Index, i int, r CodeRepo) (Warning, bool) {
	ref := strings.TrimSpace(r.Owner.Unit)
	if r.Visibility.Resolved() != CodeVisibilityUnit || ref == "" {
		return Warning{}, false
	}
	u := units.UnitByRef(ref)
	if u == nil || units.EffectiveLead(u) != nil {
		return Warning{}, false
	}
	w := advisory(at(idx(field("code.repos"), i), "visibility"), fmt.Sprintf(
		"%s is visibility: unit and owned by %s, which has no lead: only persons and "+
			"%s's members read it (set a lead on %s, or the owner)", r.Name, ref, ref, ref))
	w.Unit = u.Name
	return w, true
}

// roleCodeWarnings is every agent seat whose `code.access` no code host
// reads: the company hosts no code of its own (`code.native` unset), and the
// seat has no GitHub App the engine drives — the company enables no GitHub
// integration (`integrations.github.enabled`), or it does and the seat has no
// app of its own (the seat's `integrations.github`). Those are the only two
// things a tier caps. Valid — a company may write its seats' tiers before it
// turns on its code host — and worth knowing, because the line reads as a
// grant. Located at the field, the seat's code.access, and naming which of the
// two is missing, since each is remedied differently.
//
// THE SEAT'S APP COUNTS ONLY UNDER THE COMPANY'S ENABLED INTEGRATION: the
// engine reconciles a seat's app, and mints the tokens its tier scopes, only
// while `integrations.github` is enabled (it unregisters GitHub on an apply
// that disables it), so an app recorded on a seat of a company with the
// integration off or absent is a tier nothing reads.
//
// EVERY TIER, NOT ONLY full_access: read_only and review read as grants as
// much, and grant as little. NOT A HUMAN SEAT: a tier there is an admission
// refusal of its own ([Company.validateHumanSeatCode]), the line to read — as
// is a tier validation refuses.
func (c *Company) roleCodeWarnings() []Warning {
	if c.Code.Native {
		return nil
	}
	integration := c.Integrations.GitHub != nil && c.Integrations.GitHub.Enabled
	var out []Warning
	for role, path := range c.EachRole() {
		if role.Kind == org.KindHuman || role.Code.IsZero() ||
			!slices.Contains(CodeAccessTiers, role.Code.Access) {
			continue
		}
		ownApp := role.Integrations.GitHub != nil
		var why string
		switch {
		case integration && ownApp:
			continue
		case integration:
			why = "and this seat has no GitHub App of its own (integrations.github), the only two " +
				"places a tier caps what a seat may do (set code.native, give the seat an app, " +
				"or remove code)"
		case ownApp:
			why = "and has no GitHub integration enabled (integrations.github.enabled), so nothing " +
				"drives this seat's own GitHub App: the only two places a tier caps what a seat may " +
				"do (set code.native, enable integrations.github, or remove code)"
		default:
			why = "and has no GitHub integration enabled (integrations.github.enabled), the only two " +
				"places a tier caps what a seat may do (set code.native, or enable integrations.github " +
				"and give the seat an app, or remove code)"
		}
		w := advisory(at(path, "code.access"), fmt.Sprintf(
			"code.access %s grants nothing: this company hosts no code (code.native is unset) %s",
			role.Code.Access, why))
		w.Seat = role.Seat().Handle()
		out = append(out, w)
	}
	return out
}
