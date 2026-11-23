package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/bits"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/words"
)

// Gitlet's limits that both tiers and the domain read: config validates a
// document against them, internal/gitlet decides a push with them, and gitlet
// imports config, so they are defined here once. internal/gitlet names each as
// a constant of its own, which is the same value, so it cannot drift.
const (
	// CodeMaxPushBytesCeiling is the largest pack one push may send, whatever
	// code.limits.max_push_bytes says: worklet.MaxFileBytes, the engine's one
	// "largest object". What a push STORES may be larger (internal/gitlet.MaxPackBytes:
	// a thin pack completed against the repository, with its indexes);
	// internal/objstore/references pins that figure against the collector's write bound.
	CodeMaxPushBytesCeiling int64 = 1 << 30

	// DefaultCodeMaxPushBytes is an unset code.limits.max_push_bytes: the
	// ceiling, because a first push of an existing project is its whole history.
	DefaultCodeMaxPushBytes int64 = 1 << 30

	// DefaultCodeMaxBlobBytes is an unset code.limits.max_blob_bytes: 50 MiB. A
	// larger binary belongs in Worklet's files; the bound sizes pack-objects'
	// window memory on a Gitlet node.
	DefaultCodeMaxBlobBytes int64 = 50 << 20

	// DefaultCodeRepoMaxBytes is an unset code.limits.repo_max_bytes: 20 GiB, the
	// large reference model's 12 GB monorepo with two thirds of headroom. Every
	// Gitlet node serving a repository caches it whole, so this is also what one
	// repository may take of each such node's disk. Memory holds too: the fleet's
	// one bitmap is written by the first node whose background budget holds it
	// (internal/gitlet/cache's PublishBytesPerObject, 840 bytes an object), some
	// 50 GB at 60 million objects, which the large node of
	// docs/guides/deployment.md holds. Without such a node a repository is served
	// without a bitmap.
	DefaultCodeRepoMaxBytes int64 = 20 << 30

	// DefaultCodeDeletedRetentionDays is an unset code.deleted_retention_days: 7,
	// the trim's default min_age, so a deletion is undone from the window the log
	// can replay.
	DefaultCodeDeletedRetentionDays = 7

	// CodeMaxProtections is the most protection rules one repository carries:
	// every matching rule is evaluated per command inside the push decide, and 64
	// is the other domains' scope-term cap.
	CodeMaxProtections = 64

	// CodeMaxPathOwners is the most path owners (units and seats) one repository
	// names: as many as it has protection rules ([CodeMaxProtections]), and so the
	// most review requests one change's paths can make; past any company's.
	CodeMaxPathOwners = 64

	// CodeMaxPathOwnerPaths is the most globs one path owner names: a team's part
	// of a tree is a handful of directories, one glob each (dir/**). With
	// [CodeMaxPathOwners] it bounds what a change's paths are matched against, 2 048 globs.
	CodeMaxPathOwnerPaths = 32

	// CodeMaxPathOwnersBytes is the most a repository's path owners may weigh as
	// the code log records them: their JSON, each owner by what the chart resolves
	// it to, a unit by its key. 256 KiB, because every create, settings change and
	// restore carries them whole and the record is held to the code domain's
	// MaxRecordBytes (1 044 480): sixty-four rules with merge gates, description
	// and default branch make 631 780, path owners at this bound 893 939, leaving
	// 150 KB for later fields (internal/gitlet's
	// TestTheLargestSettingsFitTheirRecordAndTheDetail holds the sum). The counts
	// alone would not fit: 2 048 globs of gitref's 128 bytes, each byte JSON
	// spells as six (`&`, `<`, `>`), weigh 1 565 505. It refuses long globs at the
	// count bounds, or globs JSON spells long.
	CodeMaxPathOwnersBytes = 256 << 10

	// CodeMaxHumanPaths is the most human paths one protection rule's merge gate
	// names: parts of the tree (`deploy/**`) a change touching which waits for a
	// person's approval. A rule requiring checks holds `.crewlet/**` beside them
	// without naming it (gitlet.ChecksDirGlob), so it costs no entry. A judgment:
	// thirty-two is past any rule's. One of the two lists a gate's bytes grow with
	// ([CodeMaxGateBytes]).
	CodeMaxHumanPaths = 32

	// CodeMaxRequiredChecks is the most checks one rule's merge gate requires, by
	// name: a target's tests split a few ways (unit, lint, integration, a build a
	// platform); thirty-two is past any CI's required set. A judgment, and the
	// other list a gate's bytes grow with.
	CodeMaxRequiredChecks = 32

	// CodeMaxGateBytes is the most one rule's merge gate may weigh as the code log
	// records it: its canonical JSON. 8 KiB, because a repository carries up to
	// [CodeMaxProtections] rules and every create, settings change and restore
	// carries them whole: sixty-four gates at the bound are 512 KiB, beside path
	// owners at theirs ([CodeMaxPathOwnersBytes]), under the code domain's
	// MaxRecordBytes (about 1 MB). Each count alone stays under it (thirty-two
	// human paths of 128-byte globs with both `unless` classes about 6 KB,
	// thirty-two checks at their longest names about 2 KB), so it refuses a gate
	// at both counts' bounds at once, or one whose globs JSON spells long.
	CodeMaxGateBytes = 8 << 10

	// CodeMaxQueueDepth is the most candidates a gated target's speculative queue
	// tests at once, each the target's tip with the next waiting changes merged on
	// it. EIGHT: at a 5 % chance a change's checks fail, a round of eight lands
	// whole 66 % of the time (0.95⁸), and past it the candidates a red one
	// rebuilds cost more than the round lands.
	CodeMaxQueueDepth = 8

	// CodeDefaultQueueDepth is an unset landing.depth: FOUR candidates, a round of
	// which lands whole 81 % of the time at the same 5 % (0.95⁴).
	CodeDefaultQueueDepth = 4

	// CodeMaxQueueBatch is the most waiting changes one candidate takes (a red
	// batch is bisected): eight, so a round at both bounds is sixty-four changes,
	// the most one candidate's record names.
	CodeMaxQueueBatch = 8

	// CodeDefaultQueueBatch is an unset landing.batch: TWO, the batch landing
	// soonest at p99 (internal/gitlet/home's BenchmarkQueueThroughput, held by a
	// test; 5 % red, default depth, saturating):
	//
	//	batch  p99 rounds from arrival  check runs a landed change
	//	1      148, growing unbounded   1.14
	//	2      5                        0.98
	//	4      9                        0.95
	//	8      12                       0.94
	//
	// One never catches up. Past two, a red batch stalls those behind it while
	// bisected; eight is for a company pricing checks above latency.
	CodeDefaultQueueBatch = 2

	// CodeMaxCheckNameBytes is the longest name a check takes: sixty-four bytes of
	// [ValidCodeCheckName]'s alphabet, a CI job's name. It is a key in every check
	// row and a component of the token a check's box fetches with.
	CodeMaxCheckNameBytes = 64

	// CodeMaxCheckEnvNameBytes is the longest name a check's secret takes
	// ([ValidCodeCheckEnvName]): 128 bytes, four times the longest a common tool
	// reads (GOOGLE_APPLICATION_CREDENTIALS is 30), so a check's list of at most
	// 32 secret names stays a few KiB on every record carrying its definition.
	CodeMaxCheckEnvNameBytes = 128

	// CodeCheckTimeoutCeiling is the longest a check may run: six hours, GitHub
	// Actions' per-job ceiling, so no pipeline moved here has to be cut. A
	// repository's checks file is held to it, and so is code.checks.max_timeout_seconds.
	CodeCheckTimeoutCeiling = 6 * time.Hour

	// CodeDefaultCheckMaxTimeout is an unset code.checks.max_timeout_seconds: AN
	// HOUR, GitLab CI's and Azure Pipelines' default job timeout, so a pipeline
	// moved here keeps its default. It is how long one hung check holds one of
	// its node's gitlet.max_checks container slots before it ends timed out, and
	// the round validate prices a merge queue at ([Company.mergeRateWarnings]):
	// a company whose checks take minutes lowers it, one whose take hours raises
	// it toward [CodeCheckTimeoutCeiling].
	CodeDefaultCheckMaxTimeout = time.Hour

	// CodeMinCheckMaxTimeout is the least code.checks.max_timeout_seconds takes: a
	// MINUTE, under which no build finishes its own setup, so a smaller value is a
	// typo; a company that means to run no check requires none on its branches.
	CodeMinCheckMaxTimeout = time.Minute

	// CodeMaxDescriptionBytes bounds a repository description: one paragraph,
	// which rides every settings record and listing row.
	CodeMaxDescriptionBytes = 1_024

	// CodeMaxRemoteURLBytes is the longest URL a mirror or import names: ONE
	// KIBIBYTE, as a description is, because every create, settings change and
	// restore carries the mirrors whole; far past any forge's repository URL.
	CodeMaxRemoteURLBytes = 1_024

	// CodeMaxCredentialRefBytes is the longest credential a mirror or import
	// names, the whole ${NAME} reference (all a record carries of one): 128 bytes,
	// twice the longest seat handle (iam.MaxLogin, 64) a provisioner's variable is
	// named after, beside its vendor's prefix. A judgment.
	CodeMaxCredentialRefBytes = 128

	// CodeMaxMirrorRefs is the most ref patterns a push mirror names: as many as
	// its repository has protection rules ([CodeMaxProtections]), so a mirror may
	// name every protected pattern. Every pattern is asked about every ref a round
	// compares.
	CodeMaxMirrorRefs = 64

	// CodeMaxRepos is the most repositories one document may declare: a typo guard
	// on code.repos, two orders of magnitude past the reference company's sixty,
	// so a longer list is a generated document somebody should hear about. An
	// ADMISSION RULE ([Code.validateAdmission]): the reconcile decides any number;
	// at this bound an activation that creates, deletes or restores every one takes
	// 16–19 s on one node (internal/gitlet's doc.go), beside the apply.
	//
	// It is also the RUNTIME BOUND on the project repositories a tool's create may
	// bring the code log to: every status short of purged, declared ones included,
	// so a create past it is refused repo_limit (internal/gitlet's lifecycle). A
	// seat's scratch repositories are held by code.scratch.max_per_seat and never count.
	CodeMaxRepos = 10_000

	// CodeMaxDeletedRetentionDays is the longest code.deleted_retention_days: a
	// year. A deleted repository keeps its rows on every data node and its packs
	// in the object store until purged, and every backup carries them; past a year
	// it is an archive nobody restores, and export is the archive's form. AN
	// ADMISSION RULE ([Code.validateAdmission]): the retention duty purges by any
	// retention a duration holds ([codeMaxHeldRetentionDays]), so running depends
	// not on it.
	CodeMaxDeletedRetentionDays = 365
)

// codeMaxHeldRetentionDays is the longest code.deleted_retention_days a duration
// can hold (106 751 days) and the RUNNABLE bound on it: one more and
// [Code.DeletedRetention] overflows negative, a retention that has passed for
// every deleted repository, purging them all at the next pass. A purge has no
// undo, so a revision carrying one is refused however it was admitted.
const codeMaxHeldRetentionDays = int(math.MaxInt64 / int64(24*time.Hour))

// The bounds code.limits takes beside the ceiling above, named once so the
// schema's js tags and the validator cannot drift
// (TestTheCodeLimitsAreTheSchemasBounds).
const (
	// CodeMinPushBytes, CodeMinBlobBytes and CodeMinRepoBytes are the smallest
	// values code.limits takes: a mebibyte pack, a kibibyte file, a mebibyte
	// repository. Below them a first commit is refused: a typo, not a policy.
	CodeMinPushBytes int64 = 1 << 20
	CodeMinBlobBytes int64 = 1 << 10
	CodeMinRepoBytes int64 = 1 << 20

	// CodeMaxRepoBytesCeiling is the most code.limits.repo_max_bytes may say: 1
	// TiB, a typo guard. Every Gitlet node serving a repository caches it whole,
	// so a ceiling past any node's volume is not runnable.
	CodeMaxRepoBytesCeiling int64 = 1 << 40

	// DefaultCodePushesPerSeatDay is an unset code.limits.pushes_per_seat_day: a
	// THOUSAND pushes one seat may land in a company day, every repository
	// together. A RUNAWAY bound, never a budget: the reference company's seats
	// push about thirty a day, so no seat working as intended meets it, and it
	// caps what a looping seat costs the code log: a push record is at most 1 667
	// bytes (internal/gitlet's BenchmarkPushLogBytes, push=box; 1 528 where the
	// engine builds it for a tool), about 1.7 MB a day.
	DefaultCodePushesPerSeatDay = 1_000

	// CodeMinPushesPerSeatDay and CodePushesPerSeatDayCeiling bound
	// code.limits.pushes_per_seat_day: at least one, since a quota of none would
	// make a seat a reader everywhere, which role.code.access says rather than a
	// limit; at most a hundred thousand, a typo guard past which the count bounds
	// nothing a seat's tool calls and token budget do not.
	CodeMinPushesPerSeatDay     = 1
	CodePushesPerSeatDayCeiling = 100_000

	// DefaultCodeScratchIdleDays is an unset code.scratch.idle_days: a scratch
	// repository with no push, rename or settings change for SEVEN days is
	// deleted, matching the trim's min_age and code.deleted_retention_days'
	// default, restorable for that retention after. A product decision the
	// approved design states.
	DefaultCodeScratchIdleDays = 7

	// CodeScratchIdleDaysFloor and CodeScratchIdleDaysCeiling bound
	// code.scratch.idle_days. A DAY AT LEAST: a turn holding a scratch repository
	// idle gitlet.HoldRecordAfter (22 h) records its hold on the repository's
	// subject, and that threshold is this floor less the two hours two nodes'
	// clocks may disagree by (references' TestAHoldRecordCoversEveryPassInFlight
	// holds the arithmetic), so a shorter idle time would let an expiry race a
	// hold it cannot see. A YEAR AT MOST, code.deleted_retention_days' ceiling:
	// past it an experiment is a project.
	CodeScratchIdleDaysFloor   = 1
	CodeScratchIdleDaysCeiling = 365

	// DefaultCodeScratchMaxPerSeat is an unset code.scratch.max_per_seat: FIFTY
	// scratch repositories one seat may hold, every one it owns short of purged (a
	// deleted one counts until its purge), so a create-and-delete loop cannot hold
	// rows, names and packs past it. A product decision the approved design states.
	DefaultCodeScratchMaxPerSeat = 50

	// CodeScratchMaxPerSeatCeiling bounds code.scratch.max_per_seat: a THOUSAND,
	// a typo guard. A turn holds every scratch repository it names on its run row
	// (internal/agent/turnctx's MaxHolds, 1 024, kept above this), so a seat is
	// never refused a hold for their number.
	CodeScratchMaxPerSeatCeiling = 1_000

	// DefaultCodeIdentityDomain is an unset code.identity_domain: seats.invalid,
	// under RFC 2606's reserved `.invalid`, so no commit address the engine builds
	// can be received or claimed.
	DefaultCodeIdentityDomain = "seats.invalid"

	// CodeMaxIdentityDomainBytes bounds code.identity_domain: 189 bytes, the most
	// that keeps every `<handle>@<domain>` (a handle at most iam.MaxLogin's 64)
	// within the 254 bytes RFC 5321 lets an address be. Stricter than DNS's 253,
	// which would put an address no mail system takes into every commit.
	CodeMaxIdentityDomainBytes = 254 - len("@") - iam.MaxLogin
)

// Code is the company's code: which git host it runs (Gitlet or none) and on
// Gitlet its push limits and declared repositories (docs/concepts/gitlet.md).
//
// THE BACKEND IS PART OF A NODE'S NATIVE RUNTIME, like tracker.backend: built
// once per process from the first company the node runs; a move between gitlet
// and none takes effect at restart (native_backend_needs_restart) unless the node
// has run no company. The LIMITS are the epoch's, read at every decision; turning
// Gitlet off leaves a serving node the last limits until restart.
//
// THE REPOSITORIES ARE THE EPOCH'S TOO: every data node reconciles code.repos at
// each activation and boot, creating or settling a declared name and deleting an
// undeclared managed one (restorable for deleted_retention_days).
type Code struct {
	Backend              CodeBackend `yaml:"backend,omitempty" json:"backend,omitempty" js:"enum=gitlet|none" desc:"Which git host the company runs: gitlet, the engine's own, which hosts its repositories on the engine's own state log, every data node committing their pushes; or none. Empty is none. A node that has run no company yet starts Gitlet at the activation that hands it a company on it; on every other node, whatever backends its first company named, a move between the two takes effect at the node's restart, which the node says (native_backend_needs_restart)."`
	DeletedRetentionDays int         `yaml:"deleted_retention_days,omitempty" json:"deleted_retention_days,omitempty" js:"min=0;max=365" desc:"How long a repository removed from code.repos stays restorable by declaring it again under its name, before it is purged and its name is free: 1..365 days; unset (or 0) is 7."`
	IdentityDomain       string      `yaml:"identity_domain,omitempty" json:"identity_domain,omitempty" js:"pattern=^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?([.][a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$;maxlen=189" desc:"The domain a commit the engine builds through a tool spells its addresses in: its committer's, engine@<domain>, and its author's, <handle>@<domain>, for a seat whose role states no email address. A lowercase DNS name of at most 189 bytes, so every such address is at most 254; unset is seats.invalid, a name nobody can own."`
	Limits               CodeLimits  `yaml:"limits,omitempty" json:"limits,omitzero"`
	Scratch              CodeScratch `yaml:"scratch,omitempty" json:"scratch,omitzero"`
	// Checks is how the company runs the checks its repositories define.
	Checks CodeChecks `yaml:"checks,omitempty" json:"checks,omitzero"`
	// MergeByFloor is the least authority a merge into ANY protected branch
	// needs: a merge is held to the stricter of it and the merge_by of every
	// matching rule. The epoch's, as the limits are (internal/gitlet.Limits.MergeByFloor).
	MergeByFloor CodeMergeBy `yaml:"merge_by_floor,omitempty" json:"merge_by_floor,omitempty" js:"enum=writers|maintainers|persons" desc:"The least a merge into any protected branch needs, whatever its rules say: writers (unset) — anybody who may write to the repository — maintainers, or persons, a person and never a seat. A merge is held to the stricter of this and the merge_by of every rule matching its branch; a rule with no merge gate reads as writers."`
	// RemoteCredentials are the variables a mirror or import may name for its
	// credential, each with the hosts its value may be sent to (coderemotes.go).
	// The epoch's, read at every decide and dial.
	RemoteCredentials CodeRemoteCredentials `yaml:"remote_credentials,omitempty" json:"remote_credentials,omitempty" desc:"The variables a repository's mirror or import may name for its credential, by the variable's name, each with the hosts its value may be sent to: a mirror's or an import's credential — in code.repos, or one a person asks for — is refused unless it is the ${NAME} of a variable declared here for its URL's host, and Gitlet dials no other. At most 64. The value is the secret store's or the environment's, as ever: this table holds names and hosts, never a token."`
	Repos             []CodeRepo            `yaml:"repos,omitempty" json:"repos,omitempty" desc:"The company's repositories, at most 10000. Each activation creates the ones declared and settles their settings; removing one deletes it, restorable for deleted_retention_days by declaring it again."`
}

// CodeBackend is which git host a company runs.
type CodeBackend string

// The git hosts.
const (
	// CodeGitlet is Gitlet, the engine's own git host, served over git by the
	// nodes with the `gitlet` role. See internal/gitlet.
	CodeGitlet CodeBackend = "gitlet"

	// CodeNone hosts no repositories in the engine: what unset reads as. Seats may
	// still use a GitHub or GitLab integration, never a host the engine runs in
	// Gitlet's place, so this axis names no vendor.
	CodeNone CodeBackend = "none"
)

// CodeBackends is the closed set, read by the validator, the schema generator
// and the org projection's `services`, so they cannot drift apart.
var CodeBackends = []CodeBackend{CodeGitlet, CodeNone}

// Valid reports whether b is a git host this build serves. Unset is not a
// member: [Company.CodeBackendFor] resolves it to none first.
func (b CodeBackend) Valid() bool { return slices.Contains(CodeBackends, b) }

// CodeBackendFor resolves the git host: the backend the document names, else
// none. Unlike the tracker, knowledge base and chat, no other block's vendor
// implies Gitlet: a GitHub or GitLab integration is not a host the engine runs.
func (c *Company) CodeBackendFor() CodeBackend {
	if c.Code.Backend != "" {
		return c.Code.Backend
	}
	return CodeNone
}

// CodeScratch is the policy for seats' SCRATCH repositories: the ones a seat's
// own tool creates, named <handle>.<slug> (internal/gitlet's lifecycle requests).
// An unset field takes its default ([CodeScratch.Resolved]).
//
// THE EPOCH'S, read where used (the create's cap at its decide, the idle time at
// every code-retention pass), so a revision holds from activation, no restart.
type CodeScratch struct {
	// IdleDays is how many days a scratch repository may go with no push, rename,
	// settings change or restore before the code-retention pass deletes it
	// (restorable for code.deleted_retention_days). A read is not activity, nor is
	// a turn's hold: it keeps the repository while the turn lasts and moves no
	// clock. 0 is unset ([DefaultCodeScratchIdleDays]).
	IdleDays *int `yaml:"idle_days,omitempty" json:"idle_days,omitempty" js:"min=1;max=365" desc:"How many days a seat's scratch repository may go with no push, rename, settings change or restore before it is deleted — restorable for deleted_retention_days, like any deleted repository. A repository a live turn has named is never deleted while the turn lasts. 1..365; unset is 7, and a written 0 is refused."`
	// MaxPerSeat is how many scratch repositories one seat may hold, every one it
	// owns short of purged. A POINTER because 0 is a setting (no seat may create
	// one); unset is [DefaultCodeScratchMaxPerSeat].
	MaxPerSeat *int `yaml:"max_per_seat,omitempty" json:"max_per_seat,omitempty" js:"min=0;max=1000" desc:"How many scratch repositories one seat may hold, every one it owns short of purged — a deleted one counts until its purge frees its room. 0 lets no seat create one; 0..1000; unset is 50."`
}

// Resolved is the scratch policy with its defaults applied, THE ONE PLACE they
// are applied (also read by internal/gitlet.ScratchOf), so `crewlet validate`
// and the create and expiry read the document alike.
func (s CodeScratch) Resolved() (idleDays, maxPerSeat int) {
	idleDays, maxPerSeat = DefaultCodeScratchIdleDays, DefaultCodeScratchMaxPerSeat
	if s.IdleDays != nil {
		idleDays = *s.IdleDays
	}
	if s.MaxPerSeat != nil {
		maxPerSeat = *s.MaxPerSeat
	}
	return idleDays, maxPerSeat
}

// zero is a block that sets nothing.
func (s CodeScratch) zero() bool { return s.IdleDays == nil && s.MaxPerSeat == nil }

// validate holds code.scratch's rules at path: each field inside its bounds.
func (s CodeScratch) validate(scratch Path) error {
	var p problems
	// Pointers: a written value is held to its range (settingRanges).
	if d := s.IdleDays; d != nil {
		p.within(at(scratch, "idle_days"), float64(*d), fmt.Sprintf("unset is the default %d; a day at "+
			"least, because a live turn's hold on an idle repository is ordered against the expiry by a "+
			"record its holder writes a day less two hours after the repository's last activity",
			DefaultCodeScratchIdleDays))
	}
	if n := s.MaxPerSeat; n != nil {
		p.within(at(scratch, "max_per_seat"), float64(*n), fmt.Sprintf("unset is the default %d",
			DefaultCodeScratchMaxPerSeat))
	}
	return p.err()
}

// CodeChecks is how the company runs the CHECKS its repositories define in
// `.crewlet/checks.yaml` (internal/gitlet's ParseChecks): the secrets a check
// may name, and how long one may run. The file names; this block decides
// (docs/concepts/gitlet.md, "Checks"). Every field unset is a valid block. A
// check runs in a container made from providers.sandbox.local.image on the
// Gitlet node that claims it, and a company that sets no image runs none
// ([Company.ChecksRefusal]).
//
// THE EPOCH'S, read where used (timeout when a check's box is made, secrets
// when its environment is), so a revision holds from activation.
type CodeChecks struct {
	// Env are the secrets a check may be given, by variable name: exactly the
	// entries its file's `env` names. A credential field, as role.sandbox.env is,
	// resolved when a check's box is made, never stored resolved.
	Env map[string]string `secret:"true" yaml:"env,omitempty" json:"env,omitempty" desc:"The secrets a check may be given: a check is given exactly the entries its checks file names in its env. Each key a variable name — a letter or an underscore, then letters, digits and underscores, at most 128 bytes — and none the engine sets in a box itself. ${VAR} supported, resolved when a check's box is made."`

	// MaxTimeoutSeconds is how long the company lets a check run, whatever its
	// file says: 0 is [CodeDefaultCheckMaxTimeout]; else [CodeMinCheckMaxTimeout]
	// to [CodeCheckTimeoutCeiling].
	MaxTimeoutSeconds int `yaml:"max_timeout_seconds,omitempty" json:"max_timeout_seconds,omitempty" js:"min=60;max=21600" desc:"How long the company lets a check run, whatever its checks file's timeout_seconds: 60..21600 seconds; unset is 3600 — an hour, GitLab CI's and Azure Pipelines' default job timeout, and the longest a hung check holds one of its Gitlet node's check slots."`
}

// MaxTimeout is code.checks.max_timeout_seconds as a duration, THE ONE PLACE its
// default is applied: a check's box runs for the lesser of this and its file's timeout.
func (k CodeChecks) MaxTimeout() time.Duration {
	if k.MaxTimeoutSeconds == 0 {
		return CodeDefaultCheckMaxTimeout
	}
	return time.Duration(k.MaxTimeoutSeconds) * time.Second
}

// zero is a block that sets nothing.
func (k CodeChecks) zero() bool { return len(k.Env) == 0 && k.MaxTimeoutSeconds == 0 }

// validate holds code.checks' own rules: each secret key a name
// ([ValidCodeCheckEnvName]); the timeout in bounds. A key the engine sets in a
// box itself is an admission rule's ([Company.validateReservedSandboxEnv]).
func (k CodeChecks) validate(path Path) error {
	var p problems
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
			"%d is outside %d..%d seconds (a minute, under which no build finishes its own setup, to six "+
				"hours, the longest any checks file may ask, GitHub Actions' per-job ceiling), or unset for "+
				"the default %d", s, int(CodeMinCheckMaxTimeout/time.Second), int(CodeCheckTimeoutCeiling/time.Second),
			int(CodeDefaultCheckMaxTimeout/time.Second))
	}
	return p.err()
}

// NeedsRole reports whether the company needs a node role: every role but gitlet
// always, gitlet exactly where the company runs Gitlet. Nil-safe: no company
// needs every role but gitlet.
//
// THE ONE STATEMENT OF THE RULE, read by the unmanned-role check on every node
// (seat.Config.RoleNeeded) and the fleet view's unmanned_roles, so a node's log
// and the operator's screen cannot disagree about a missing role.
func (c *Company) NeedsRole(role placement.NodeRole) bool {
	if role != placement.RoleGitlet {
		return true
	}
	return c != nil && c.CodeBackendFor() == CodeGitlet
}

// DeletedRetention is code.deleted_retention_days as a duration, THE ONE PLACE
// its default is applied, read by the code-retention duty.
func (c Code) DeletedRetention() time.Duration {
	days := c.DeletedRetentionDays
	if days <= 0 {
		days = DefaultCodeDeletedRetentionDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// IdentityDomainResolved is code.identity_domain, THE ONE PLACE its default is
// applied: the domain of an engine-built commit's committer (`engine@<domain>`)
// and of the author for a seat whose role states no email (`<handle>@<domain>`).
func (c Code) IdentityDomainResolved() string {
	if c.IdentityDomain == "" {
		return DefaultCodeIdentityDomain
	}
	return c.IdentityDomain
}

// validDomainName reports a lowercase DNS name: labels of ASCII letters, digits
// and hyphens, none empty, past 63 bytes or hyphen-bounded, joined by dots with
// no trailing dot, at most CodeMaxIdentityDomainBytes. Lowercase because a commit
// records the address as written, and two spellings would be two committers.
func validDomainName(s string) bool {
	if s == "" || len(s) > CodeMaxIdentityDomainBytes {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			switch c := label[i]; {
			case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// ValidCodeCheckName reports a name a check may have: ^[a-z0-9][a-z0-9._-]{0,63}$.
// THE ONE RULE a check is named by, read wherever a name is written: a
// repository's `.crewlet/checks.yaml`, attempts' rows, and required checks here
// and in the domain's merge gate record. One rule, so a gate cannot require a
// check no repository can define, which would keep it shut for good. It lives
// here because config cannot import the domain.
//
// LOWERCASE, NO SPACE, because a name is matched exactly and typed on command
// lines. Nothing any URL encoder escapes (a `~`: Java's URLEncoder writes
// `%7E`), since a name rides a route as spelled. A check box's token carries the
// name base64url-encoded (internal/gitlet/host's CheckTokens), so a `.` never
// collides with the token's separator.
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
// underscore, then letters, digits and underscores (a variable any shell
// exports), at most [CodeMaxCheckEnvNameBytes]. THE ONE RULE, as
// [ValidCodeCheckName] is: code.checks.env's keys and a checks file's `env`
// names are held to it alike, so neither names a secret the other cannot.
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
	// PathOwners are who owns which paths: whom a change touching them asks for a
	// review. They GRANT NOTHING; who may read and push is the chart's and the tier's.
	PathOwners []CodePathOwner `yaml:"path_owners,omitempty" json:"path_owners,omitempty" desc:"Who owns which of the repository's paths, at most 64 owners and 256 KiB as the code log records them: a change touching an owner's paths asks it for a review. A path owner is asked, never granted: it reads and pushes what the chart lets it, as before."`
	// MirrorFrom makes the repository a PULL MIRROR, MirrorTo a PUSH MIRROR, and
	// Import fills it from another host at creation. Only a person or this
	// document sets any (U49): a pull mirror and an import dial another host, a
	// push mirror sends the company's code to one. Each credential is a ${VAR}
	// reference, recorded as such and resolved only when the remote is dialed.
	MirrorFrom *CodeMirrorSource `yaml:"mirror_from,omitempty" json:"mirror_from,omitempty" desc:"Makes it a pull mirror of a repository at another host: it is imported from there first — when it is created, and when an existing repository is given one or moved to another url — then moves only by Gitlet's syncs from it, and takes no push and no change of its own."`
	MirrorTo   *CodeMirrorTarget `yaml:"mirror_to,omitempty" json:"mirror_to,omitempty" desc:"Makes it a push mirror: Gitlet pushes the refs it names to a repository at another host, as the seat named by as."`
	Import     *CodeImportSource `yaml:"import,omitempty" json:"import,omitempty" desc:"Fills it, when it is created, from a repository at another host: it takes no push until its import is done. Read only by its create: a repository that exists already ignores it."`
}

// IdentityKey is the repository's name, unique in code.repos: what a redaction's
// restore matches credentials by, since restoring by position would hand a
// reordered code.repos' credential to another repository.
func (r CodeRepo) IdentityKey() string { return r.Name }

// CodeMirrorSource is the repository a pull mirror follows ([CodeRepo.MirrorFrom]).
type CodeMirrorSource struct {
	URL        string `yaml:"url" json:"url" js:"required" desc:"The repository it follows: an https URL with no user, query or fragment, at most 1024 bytes."`
	Credential string `secret:"true" yaml:"credential,omitempty" json:"credential,omitempty" desc:"What Gitlet fetches with: one whole ${VAR} reference to a token — a variable code.remote_credentials declares for this url's host — resolved from the secret store and then the environment when it dials, never a literal. Unset fetches anonymously."`
}

// CodeMirrorTarget is the repository a push mirror pushes to
// ([CodeRepo.MirrorTo]).
type CodeMirrorTarget struct {
	URL        string   `yaml:"url" json:"url" js:"required" desc:"The repository it pushes to: an https URL with no user, query or fragment, at most 1024 bytes."`
	Credential string   `secret:"true" yaml:"credential" json:"credential" js:"required" desc:"What Gitlet pushes with: one whole ${VAR} reference to the as seat's own token at that host — a variable code.remote_credentials declares for this url's host — resolved from the secret store and then the environment when it dials, never a literal."`
	As         string   `yaml:"as" json:"as" js:"required" desc:"The seat whose identity the push carries at that host, by its handle: a seat that reads this repository — a person's seat reads every one, an agent's what its grant does — never a bot."`
	Refs       []string `yaml:"refs,omitempty" json:"refs,omitempty" desc:"The refs it pushes, at most 64 patterns of refs/heads/<glob> and refs/tags/<glob>, each once; unset is the default branch and refs/tags/*. No pattern may reach a seat's own branches under refs/heads/seats/."`
}

// CodeImportSource is the repository a repository is filled from when it is
// created ([CodeRepo.Import]).
type CodeImportSource struct {
	URL        string `yaml:"url" json:"url" js:"required" desc:"The repository it is filled from: an https URL with no user, query or fragment, at most 1024 bytes."`
	Credential string `secret:"true" yaml:"credential,omitempty" json:"credential,omitempty" desc:"What Gitlet fetches with: one whole ${VAR} reference to a token — a variable code.remote_credentials declares for this url's host — resolved from the secret store and then the environment when it dials, never a literal. Unset fetches anonymously."`
}

// CodePathOwner is one owner of a repository's paths: exactly one of a unit or a
// seat, and the globs it owns (gitref.ParsePathGlob). An owner is named once per
// repository.
type CodePathOwner struct {
	Paths []string `yaml:"paths" json:"paths" desc:"The paths it owns: 1 to 32 globs over the repository's tree, each relative to its root, at most 128 bytes. A component of * matches one component and * within one any run of characters but /; ** matches whole components (zero or more, one or more as the last), so docs/** is everything inside docs and **/*.go every Go file; a glob holding a * has at most eight components, and a glob with no * matches only that path. ?, [, \\ and a leading ! are refused: * is the one metacharacter. Each glob once."`
	Unit  string   `yaml:"unit,omitempty" json:"unit,omitempty" desc:"A unit of the chart: its id, or its name where it has none — its effective lead is whom a review is asked of. Exactly one of unit and seat."`
	Seat  string   `yaml:"seat,omitempty" json:"seat,omitempty" desc:"A seat, by its handle. Exactly one of unit and seat."`
}

// ResolvedDefaultBranch is the default branch with its default applied, read by
// the unprotected-branch warning and the reconcile alike.
func (r CodeRepo) ResolvedDefaultBranch() string {
	if r.DefaultBranch == "" {
		return DefaultCodeBranch
	}
	return r.DefaultBranch
}

// DefaultCodeBranch is a declared repository's default branch where it sets none.
const DefaultCodeBranch = "main"

// CodeVisibility is who among the SEATS may read a repository. A person reads
// every repository whatever it says (ADR-0088): visibility narrows the seats,
// never the people who run the company.
type CodeVisibility string

const (
	// CodeVisibilityCompany lets every seat read it; what unset reads as.
	CodeVisibilityCompany CodeVisibility = "company"
	// CodeVisibilityUnit lets only the owning unit's members, the leads above
	// them, and the owning seat read it.
	CodeVisibilityUnit CodeVisibility = "unit"
)

// CodeVisibilities is the closed set read by the validator and the schema. The
// domain's own set is held to it both ways by the engine's
// TestEveryDeclaredVisibilityIsOneTheDomainRecords, since config cannot import
// the domain.
var CodeVisibilities = []CodeVisibility{CodeVisibilityCompany, CodeVisibilityUnit}

// Valid reports a value this build knows. Unset is valid: it reads as company.
func (v CodeVisibility) Valid() bool { return v == "" || slices.Contains(CodeVisibilities, v) }

// Resolved is the value with its default applied (unset is company), THE ONE
// PLACE it is applied, read by the warnings and the reconcile alike.
func (v CodeVisibility) Resolved() CodeVisibility {
	if v == "" {
		return CodeVisibilityCompany
	}
	return v
}

// CodeSearchMode is how code search reads a declared repository: from an index
// of its default branch, or with git grep only when a search names it. UNSET IS
// INDEXED for a declared repository (a named project is the code seats work in,
// so a company-wide search reads it), while a repository a tool makes starts at
// grep (internal/gitlet's SearchMode).
type CodeSearchMode string

const (
	// CodeSearchIndexed reads it from an index of its default branch; what unset
	// reads as.
	CodeSearchIndexed CodeSearchMode = "indexed"
	// CodeSearchGrep reads it with git grep, only by a search naming it.
	CodeSearchGrep CodeSearchMode = "grep"
)

// CodeSearchModes is the closed set read by the validator and the schema; the
// domain's set is held to it both ways by the engine's
// TestEveryDeclaredSearchModeIsOneTheDomainRecords.
var CodeSearchModes = []CodeSearchMode{CodeSearchIndexed, CodeSearchGrep}

// Valid reports a value this build knows. Unset is valid: it reads as indexed.
func (m CodeSearchMode) Valid() bool { return m == "" || slices.Contains(CodeSearchModes, m) }

// Resolved is the value with its default applied (unset is indexed), THE ONE
// PLACE it is applied, so a silent declaration is recorded as indexed on every node.
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

// CodeProtection is one protection rule: the branches or tags it matches, what a
// push to one may do, and on a branch rule what a merge needs, its MERGE GATE:
// any gate field set forms one, the rest taking defaults ([CodeProtection.Gate]).
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
	AgentsEditChecks bool              `yaml:"agents_edit_checks,omitempty" json:"agents_edit_checks,omitempty" desc:"Let a seat's approval stand for a change to .crewlet/** — where a repository's checks are defined — which, on a rule requiring checks, otherwise waits for a person's."`
}

// CodeHumanPath is one glob a change touching which waits for a person's
// approval, unless every path it matched is of a class Unless lists.
type CodeHumanPath struct {
	Glob   string            `yaml:"glob" json:"glob" desc:"A glob over the repository's tree, from its root, as a path owner's paths are written: * within one component, ** whole components, at most 128 bytes."`
	Unless []CodeChangeClass `yaml:"unless,omitempty" json:"unless,omitempty" js:"enum=whitespace_only|comment_only" desc:"The path classes — whitespace_only, comment_only — that let a change through without the person's approval when every path this glob matched is of one. Each once."`
}

// CodeLanding is how a gated branch's queue lands a change; these two fields are
// its throughput. An unset field takes its default ([CodeLanding.Resolved]); the
// queue lands only where the rule requires checks.
type CodeLanding struct {
	Depth int `yaml:"depth,omitempty" json:"depth,omitempty" js:"min=1;max=8" desc:"How many candidates the queue tests at once: 1..8; unset is 4."`
	Batch int `yaml:"batch,omitempty" json:"batch,omitempty" js:"min=1;max=8" desc:"How many waiting changes one candidate takes: 1..8; unset is 2."`
}

// Resolved is the landing with its defaults applied, THE ONE PLACE they are.
func (l CodeLanding) Resolved() CodeLanding {
	if l.Depth == 0 {
		l.Depth = CodeDefaultQueueDepth
	}
	if l.Batch == 0 {
		l.Batch = CodeDefaultQueueBatch
	}
	return l
}

// LandingCapacity is the most changes a merge queue lands over a span:
// ⌊depth × batch × over / round⌋ (M5-D28). Each round lands the green prefix of
// depth candidates of at most batch changes.
//
// THE ONE FORMULA both judgments of a queue's rate read: `crewlet validate`'s
// advisory (at code.checks.max_timeout_seconds, over a day) and
// code_merge_rate_over_capacity (at the observed round, over internal/gitlet's
// MergeRateWindow), so the alarm's threshold is the queue's own capacity (ADR-0015).
//
// Exact in integers (a 128-bit product before the division). A round of zero or
// less lands without bound, a product past int answers its top, a depth, batch or
// span under one lands nothing.
func LandingCapacity(depth, batch int, round, over time.Duration) int {
	if depth < 1 || batch < 1 || over <= 0 {
		return 0
	}
	if round <= 0 {
		return math.MaxInt
	}
	hi, lo := bits.Mul64(uint64(depth)*uint64(batch), uint64(over))
	if hi >= uint64(round) {
		return math.MaxInt
	}
	q, _ := bits.Div64(hi, lo, uint64(round))
	if q > math.MaxInt {
		return math.MaxInt
	}
	return int(q)
}

// CodeGate is a protection rule's merge gate RESOLVED in the shape the code
// domain records it: gitlet.MergeGate's keys, order and omissions, so its
// encoding is as long as the record's ([CodeGate.EncodedBytes]; the engine's
// TestADeclaredGateWeighsWhatTheDomainRecords holds the two). Lists keep the
// document's order; the reconcile's reordering changes no length.
type CodeGate struct {
	MergeBy          CodeMergeBy       `json:"merge_by"`
	Approvals        CodeApprovals     `json:"approvals"`
	ApprovalsUnless  []CodeChangeClass `json:"approvals_unless,omitempty"`
	HumanPaths       []CodeHumanPath   `json:"human_paths,omitempty"`
	Checks           []string          `json:"checks,omitempty"`
	Landing          CodeLanding       `json:"landing"`
	OwnersApprove    bool              `json:"owners_approve,omitempty"`
	DeleteSource     CodeDeleteSource  `json:"delete_source"`
	AgentsEditChecks bool              `json:"agents_edit_checks,omitempty"`
}

// EncodedBytes is the length of the gate's JSON, which [CodeMaxGateBytes] bounds.
func (g CodeGate) EncodedBytes() int {
	// Cannot fail: a gate holds only strings, integers, booleans and lists of them.
	b, _ := json.Marshal(g)
	return len(b)
}

// Gate is the rule's merge gate RESOLVED: nil where the rule sets no gate field,
// else every field its value or default. THE ONE PLACE those defaults are
// applied, read by the reconcile and by the bound here, so the gate validated is
// the gate recorded.
func (rule CodeProtection) Gate() *CodeGate {
	if len(rule.gateFields()) == 0 {
		return nil
	}
	return &CodeGate{
		MergeBy: rule.MergeBy.Resolved(), Approvals: rule.Approvals.Resolved(),
		ApprovalsUnless: rule.ApprovalsUnless, HumanPaths: rule.HumanPaths, Checks: rule.Checks,
		Landing: rule.Landing.Resolved(), OwnersApprove: rule.OwnersApprove,
		DeleteSource: rule.DeleteSource.Resolved(), AgentsEditChecks: rule.AgentsEditChecks,
	}
}

// gateFields is every merge gate field the rule sets, by document name and order:
// what forms a gate, and what a tag rule is refused for.
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
		{"agents_edit_checks", rule.AgentsEditChecks},
	} {
		if f.set {
			set = append(set, f.name)
		}
	}
	return set
}

// Pattern is the rule as the domain records it: refs/heads/<branch> or
// refs/tags/<tag>, or "" where the rule sets neither or both. THE ONE SPELLING,
// read by the validator's uniqueness rule and the reconcile, so `crewlet
// validate` cannot call distinct two rules the domain refuses as one.
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
	// CodeDirectPersons lets only a person push to it: a seat's push is refused
	// protected_direct. What unset reads as.
	CodeDirectPersons CodeDirectPush = "persons"
	// CodeDirectNone lets nobody push to it: the ref is frozen.
	CodeDirectNone CodeDirectPush = "none"
)

// CodeDirectPushes is the closed set read by the validator and the schema; the
// domain's set is held to it both ways by the engine's
// TestEveryDeclaredDirectPushIsOneTheDomainRecords.
var CodeDirectPushes = []CodeDirectPush{CodeDirectPersons, CodeDirectNone}

// Valid reports a value this build knows. Unset is valid: it reads as persons.
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
	// CodeMergeByWriters lets anybody who may write to the repository merge; what
	// unset reads as.
	CodeMergeByWriters CodeMergeBy = "writers"
	// CodeMergeByMaintainers lets only its maintainers merge.
	CodeMergeByMaintainers CodeMergeBy = "maintainers"
	// CodeMergeByPersons lets only a person merge, never a seat.
	CodeMergeByPersons CodeMergeBy = "persons"
)

// CodeMergeBys is the closed set, loosest first, read by the validator and the
// schema. The domain's set is held to it both ways by the engine's
// TestEveryDeclaredMergeByIsOneTheDomainRecords, as is each merge gate set below.
var CodeMergeBys = []CodeMergeBy{CodeMergeByWriters, CodeMergeByMaintainers, CodeMergeByPersons}

// Valid reports a value this build knows. Unset is valid: it reads as writers.
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

// CodeDeleteSource is whether a merge deletes its change's source branch.
type CodeDeleteSource string

const (
	// CodeDeleteSeats deletes a source branch in a seat's own namespace in the
	// merging record; what unset reads as.
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
	// CodeChangeClassNoCodeChange is a revision that moved nothing: a revision's
	// class, never a path's, so no list here takes it.
	CodeChangeClassNoCodeChange CodeChangeClass = "no_code_change"
	// CodeChangeClassWhitespaceOnly is a path whose only change is whitespace.
	CodeChangeClassWhitespaceOnly CodeChangeClass = "whitespace_only"
	// CodeChangeClassCommentOnly is a path whose only change is in comments, none
	// a directive.
	CodeChangeClassCommentOnly CodeChangeClass = "comment_only"
)

// CodeChangeClasses is the closed set, in the code domain's order.
var CodeChangeClasses = []CodeChangeClass{CodeChangeClassNoCodeChange, CodeChangeClassWhitespaceOnly,
	CodeChangeClassCommentOnly}

// CodePathClasses is the classes a PATH has, narrowest first: those approvals_unless
// and a human path's unless take.
var CodePathClasses = []CodeChangeClass{CodeChangeClassWhitespaceOnly, CodeChangeClassCommentOnly}

// Valid reports a class this build knows. There is no unset class: a list
// names classes, or is empty.
func (c CodeChangeClass) Valid() bool { return slices.Contains(CodeChangeClasses, c) }

// CodeLimits are what one push and one repository may hold. An unset field
// takes its default ([CodeLimits.Resolved]).
type CodeLimits struct {
	MaxPushBytes int64 `yaml:"max_push_bytes,omitempty" json:"max_push_bytes,omitempty" js:"min=1048576;max=1073741824" desc:"The largest pack one push may send; unset is 1 GiB, the most it may be."`
	MaxBlobBytes int64 `yaml:"max_blob_bytes,omitempty" json:"max_blob_bytes,omitempty" js:"min=1024;max=1073741824" desc:"The largest file (blob) a push may carry, at most max_push_bytes; unset is 50 MiB. A larger binary belongs in Worklet's files."`
	RepoMaxBytes int64 `yaml:"repo_max_bytes,omitempty" json:"repo_max_bytes,omitempty" js:"min=1048576;max=1099511627776" desc:"The most packed bytes one repository may hold, at least max_push_bytes; unset is 20 GiB. Every Gitlet node serving a repository caches it whole."`
	// PushesPerSeatDay is how many pushes one seat may land in a company day,
	// every repository together; 0 is [DefaultCodePushesPerSeatDay]. Persons are
	// never counted.
	PushesPerSeatDay int `yaml:"pushes_per_seat_day,omitempty" json:"pushes_per_seat_day,omitempty" js:"min=1;max=100000" desc:"How many pushes one seat may land in one day on the company's calendar, every repository together: 1..100000; unset is 1000. A runaway bound on what a seat pushing in a loop costs the code log, not a budget; a person's pushes are never counted."`
}

// Resolved is the limits with defaults applied, THE ONE PLACE they are: the order
// rules below and internal/gitlet.LimitsOf both read it, so `crewlet validate`
// and a push read the document alike.
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

// validate holds Gitlet's RUNNABLE rules against the chart o of the same
// document: nothing but native where nothing hosts code; a retention a duration
// can hold; the limits ([CodeLimits.validate]); every declared repository
// ([CodeRepo.validate]), each name once.
//
// RUNNABLE because a revision breaking one cannot run as written: the reconcile
// cannot record a repository from an unresolved owner or two rules on one
// pattern, the limits bind every push, and a retention no duration holds purges
// every deleted repository at once. So it is refused before apply. The two
// POLICY bounds (repository count, retention) are admission rules
// ([Code.validateAdmission]).
func (c Code) validate(path Path, o *org.Organization) error {
	var p problems
	// An unknown backend is the one thing said about the rest of the block:
	// whether the settings have a host turns on a value nobody can read, and
	// seven refusals would bury the one line that fixes them all.
	known := c.Backend == "" || c.Backend.Valid()
	if !known {
		p.add(at(path, "backend"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			c.Backend, words.Or(CodeBackends), CodeNone)
	}
	unhosted := known && c.Backend != CodeGitlet
	if unhosted {
		// Nothing hosts any of it, so each would read as live and do nothing.
		if !c.Limits.zero() {
			p.add(at(path, "limits"), ErrConflict,
				"nothing hosts code on this company: set code.backend: gitlet, or remove code.limits")
		}
		if c.DeletedRetentionDays != 0 {
			p.add(at(path, "deleted_retention_days"), ErrConflict,
				"nothing hosts code on this company: set code.backend: gitlet, or remove "+
					"code.deleted_retention_days")
		}
		if c.IdentityDomain != "" {
			p.addSaying(at(path, "identity_domain"), ErrConflict,
				"Turn Gitlet on first: no commit is built for this domain to name while nothing hosts code.",
				"nothing hosts code on this company, so no commit is built to name it: set "+
					"code.backend: gitlet, or remove code.identity_domain")
		}
		if len(c.Repos) > 0 {
			p.add(at(path, "repos"), ErrConflict,
				"nothing hosts code on this company, so none of these %d repositories "+
					"would be created: set code.backend: gitlet, or remove code.repos", len(c.Repos))
		}
		if !c.Scratch.zero() {
			p.add(at(path, "scratch"), ErrConflict,
				"code.scratch configures Gitlet, and nothing hosts code on this "+
					"company: set code.backend: gitlet, or remove code.scratch")
		}
		if !c.Checks.zero() {
			p.add(at(path, "checks"), ErrConflict,
				"nothing hosts code on this company, so no repository defines a check to run: set "+
					"code.backend: gitlet, or remove code.checks")
		}
		if !c.RemoteCredentials.zero() {
			p.add(at(path, "remote_credentials"), ErrConflict,
				"nothing hosts code on this company, so no mirror or import is dialed with these: set "+
					"code.backend: gitlet, or remove code.remote_credentials")
		}
	}
	switch {
	case !c.MergeByFloor.Valid():
		p.add(at(path, "merge_by_floor"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			c.MergeByFloor, words.Or(CodeMergeBys), CodeMergeByWriters)
	case unhosted && c.MergeByFloor != "":
		p.add(at(path, "merge_by_floor"), ErrConflict,
			"nothing hosts code on this company, so no merge is held to it: set code.backend: gitlet, "+
				"or remove code.merge_by_floor")
	}
	if d := c.DeletedRetentionDays; d < 0 || d > codeMaxHeldRetentionDays {
		p.add(at(path, "deleted_retention_days"), ErrOutOfRange,
			"must be 1..%d days, or unset for the default %d, got %d",
			CodeMaxDeletedRetentionDays, DefaultCodeDeletedRetentionDays, d)
	}
	if d := c.IdentityDomain; d != "" && !validDomainName(d) {
		p.addSaying(at(path, "identity_domain"), ErrShape,
			fmt.Sprintf("%q is not a domain name. Use lowercase letters, digits and hyphens, in "+
				"parts joined by dots, like example.com, at most %d characters.", d, CodeMaxIdentityDomainBytes),
			"%q is not a lowercase DNS name of at most %d bytes: labels of letters, digits and hyphens, "+
				"none starting or ending with a hyphen, joined by dots (example.com), or unset for %s",
			d, CodeMaxIdentityDomainBytes, DefaultCodeIdentityDomain)
	}
	p.wrap(c.Limits.validate(at(path, "limits")))
	p.wrap(c.Scratch.validate(at(path, "scratch")))
	p.wrap(c.Checks.validate(at(path, "checks")))
	p.wrap(c.RemoteCredentials.validate(at(path, "remote_credentials")))
	p.wrap(validateCodeRepos(at(path, "repos"), c.Repos, o, c.RemoteCredentials))
	return p.err()
}

// validateAdmission holds Gitlet's ADMISSION rules: at most [CodeMaxRepos]
// repositories, a retention of at most [CodeMaxDeletedRetentionDays], and
// code.remote_credentials' two bounds ([CodeRemoteCredentials.validateAdmission]).
//
// POLICY, which a later build may change: a revision past any still runs, and
// holding them on APPLY would let a rolling upgrade's older build refuse a newer
// build's revisions, splitting the fleet's epoch. A submitted document is
// refused; an applied revision that breaks them is warned about. A retention
// past what a duration holds is the runnable rule's, not reported twice.
func (c Code) validateAdmission(path Path) error {
	var p problems
	if d := c.DeletedRetentionDays; d > CodeMaxDeletedRetentionDays && d <= codeMaxHeldRetentionDays {
		p.add(at(path, "deleted_retention_days"), ErrOutOfRange,
			"must be 1..%d days, or unset for the default %d, got %d: past a year a "+
				"deleted repository is an archive nobody restores, which every backup "+
				"carries: export it instead", CodeMaxDeletedRetentionDays,
			DefaultCodeDeletedRetentionDays, d)
	}
	if n := len(c.Repos); n > CodeMaxRepos {
		p.add(at(path, "repos"), ErrOutOfRange,
			"%d repositories, past the %d one document may declare, a typo guard: "+
				"a list this long is a generated document, so check what wrote it",
			n, CodeMaxRepos)
	}
	p.wrap(c.RemoteCredentials.validateAdmission(at(path, "remote_credentials")))
	return p.err()
}

// validate holds code.limits' rules: each limit in bounds, and the sizes in the
// order a push meets them (a file inside a push, a push inside a repository).
//
// THE ORDER IS OF THE RESOLVED LIMITS, because that is what a push is held to: a
// push limit set alone below the default file limit admits no push carrying a
// file the default allows. Each problem is reported AT THE FIELD THE DOCUMENT
// SETS, naming the default it met, and only once every size is in range, so one
// mistake is not reported twice. The daily quota is judged on its own.
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

// limitNamed is one limit as an order problem names it: its value, marked as the
// default where the document left it unset.
func limitNamed(name string, set, resolved int64) string {
	if set == 0 {
		return fmt.Sprintf("%s (unset: its default, %d)", name, resolved)
	}
	return fmt.Sprintf("%s (%d)", name, resolved)
}

// validateCodeRepos holds code.repos: each entry valid, each name once (the
// count is an admission rule, [Code.validateAdmission]).
//
// A DUPLICATE IS REPORTED AT THE LATER ENTRY, the one an author added: one name
// is one repository (its clone address and create subject), so a second
// declaration would settle the first's settings at every activation. Only among
// valid names: an invalid one is already refused.
func validateCodeRepos(path Path, repos []CodeRepo, o *org.Organization, remotes CodeRemoteCredentials) error {
	var p problems
	owners := newCodeOwners(o)
	first := make(map[string]int, len(repos))
	for i, r := range repos {
		entry := idx(path, i)
		p.wrap(r.validate(entry, owners, remotes))
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

// validate holds one declared repository against the document's chart: name,
// one owner resolving on the chart, visibility, search mode, default branch,
// description, protection rules (at most [CodeMaxProtections], each exactly one
// of a branch or tag pattern in gitref's grammar, each pattern once, no force on
// a tag, each merge gate [CodeProtection.validateGate]) and path owners
// ([validatePathOwners]).
func (r CodeRepo) validate(path Path, owners *codeOwners, remotes CodeRemoteCredentials) error {
	var p problems
	switch {
	case r.Name == "":
		p.add(at(path, "name"), ErrMissing,
			"a repository needs a name: it is the address the repository is cloned by")
	case !gitref.ValidProjectName(r.Name):
		// A dotted name is a seat's scratch repository (gitref's
		// <handle>.<slug>): declared, it would squat the name its create takes.
		why := ""
		if _, scratch := gitref.ScratchName(r.Name); scratch {
			why = " (a name with a dot is a seat's own scratch repository, which only that seat's " +
				"tool creates)"
		}
		p.add(at(path, "name"), ErrShape,
			"%q is not a project repository's name: one label of lowercase letters, digits and "+
				"hyphens, starting and ending with a letter or a digit, at most %d%s",
			r.Name, gitref.MaxRepoNameBytes, why)
	}
	r.Owner.validate(&p, at(path, "owner"), owners)
	if !r.Visibility.Valid() {
		p.add(at(path, "visibility"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			r.Visibility, words.Or(CodeVisibilities), CodeVisibilityCompany)
	}
	if !r.Search.Valid() {
		p.add(at(path, "search"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			r.Search, words.Or(CodeSearchModes), CodeSearchIndexed)
	}
	switch {
	case r.DefaultBranch == "":
	case !gitref.ValidBranch(r.DefaultBranch):
		p.add(at(path, "default_branch"), ErrShape,
			"%q is not a branch name git would take (git check-ref-format): unset it for %s, "+
				"or name a branch", r.DefaultBranch, DefaultCodeBranch)
	case gitref.ReservedSeatBranch(r.DefaultBranch):
		// Valid in the grammar but Gitlet's own: no push may create it.
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
	r.validateRemotes(&p, path, owners, remotes)
	return p.err()
}

// validateRemotes holds the repository's mirrors and import into p: each URL
// readable ([validateCodeRemote]), each credential a declared reference (required
// on a push mirror), a push mirror's seat one that reads the repository
// ([CodeRepo.validateMirrorSeat]) and its refs ([validateMirrorRefs]); and no
// combination that does nothing: an import beside a pull mirror (imported from
// its source at creation), or either beside archived, which takes neither.
//
// RUNNABLE, as an owner's rules are: the domain refuses an unreadable URL or a
// non-reference credential; a dial sends no credential undeclared for its host;
// a push mirror whose seat the chart lacks or that cannot read what it publishes
// has nobody to push as.
func (r CodeRepo) validateRemotes(p *problems, path Path, owners *codeOwners, remotes CodeRemoteCredentials) {
	if m := r.MirrorFrom; m != nil {
		from := at(path, "mirror_from")
		validateCodeRemote(p, from, m.URL, m.Credential, false, remotes)
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
		validateCodeRemote(p, at(path, "import"), i.URL, i.Credential, false, remotes)
		if r.Archived && r.MirrorFrom == nil {
			p.add(at(path, "import"), ErrConflict,
				"an import fills an active repository, and an archived one takes nothing: leave archived "+
					"unset until the import is done: import is read only when the repository is created, "+
					"so remove it then, and archive the repository")
		}
	}
	if m := r.MirrorTo; m != nil {
		to := at(path, "mirror_to")
		validateCodeRemote(p, to, m.URL, m.Credential, true, remotes)
		r.validateMirrorSeat(p, at(to, "as"), m.As, owners)
		validateMirrorRefs(p, at(to, "refs"), m.Refs)
	}
}

// validateCodeRemote holds one remote into p: its URL present, within
// [CodeMaxRemoteURLBytes] and readable ([gitref.ParseRemoteURL]), and its
// credential, required where needed, ONE WHOLE ${VAR} REFERENCE within
// [CodeMaxCredentialRefBytes].
//
// A REFERENCE, NEVER A TOKEN: the domain records the credential as written on
// every settings record (data nodes and backups carry it), so a token would be a
// secret at rest. Resolved only at dial. The refusal never quotes the value.
//
// AND ONE code.remote_credentials DECLARES FOR THE URL'S HOST
// ([CodeRemoteCredentials.Refusal]), judged once the URL and reference are valid:
// the dial sends no other.
func validateCodeRemote(p *problems, path Path, url, credential string, needCredential bool,
	remotes CodeRemoteCredentials) {
	urlRead := false
	switch {
	case strings.TrimSpace(url) == "":
		p.add(at(path, "url"), ErrMissing, "the repository at the other host: an https URL")
	case len(url) > CodeMaxRemoteURLBytes:
		p.add(at(path, "url"), ErrOutOfRange, "%d bytes, past the %d a remote's URL may hold", len(url),
			CodeMaxRemoteURLBytes)
	default:
		if _, err := gitref.ParseRemoteURL(url); err != nil {
			p.add(at(path, "url"), ErrShape, "%v", err)
		} else {
			urlRead = true
		}
	}
	switch {
	case credential == "" && needCredential:
		p.add(at(path, "credential"), ErrMissing,
			"a push authenticates: name the ${VAR} that holds the token, which the secret store or the "+
				"environment answers when Gitlet dials")
	case credential == "":
	case len(credential) > CodeMaxCredentialRefBytes:
		p.add(at(path, "credential"), ErrOutOfRange, "%d bytes, past the %d a credential's reference may hold",
			len(credential), CodeMaxCredentialRefBytes)
	case !wholeReference(credential):
		p.add(at(path, "credential"), ErrShape,
			"a credential is one whole ${VAR} reference and nothing else, never the token itself, which "+
				"every record of the repository's settings would carry: put the token in the secret store "+
				"(crewlet secrets set NAME) and write ${NAME}")
	case urlRead:
		if why := remotes.Refusal(url, credential); why != "" {
			p.add(at(path, "credential"), ErrConflict, "%s", why)
		}
	}
}

// validateMirrorSeat holds a push mirror's seat: a chart seat, by handle, that
// reads the repository ([CodeRepo.mirrorSeatReads]).
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
				"a push mirror publishes only what the seat it pushes as reads: name a seat the chart "+
				"relates to the owner, or a person's seat", handle, r.Name)
	}
}

// mirrorSeatReads reports whether seat reads the repository r, as the domain
// grants a read (internal/gitlet's GrantOf, ADR-0088). A PERSON'S seat reads
// every repository as far as the chart goes; whether its holder uses Gitlet
// (ADR-0110) is the identity directory's, which no configuration reads, so a push
// mirror asks it at a person's create and settings change and before every push.
// An agent's seat reads every company-visible one and a unit-visible one only
// where the chart relates it to the owner: in the owning unit or below, a lead
// over it, or for a seat-owned repository that seat or a lead over its unit.
//
// THE DOMAIN'S RULE IN THE DOCUMENT'S TERMS, since config cannot import the
// domain: the engine's TestADeclaredMirrorSeatReadsWhatTheDomainGrantsIt holds the
// two to one verdict over every relation a chart can give.
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

// validateMirrorRefs holds a push mirror's refs: at most [CodeMaxMirrorRefs],
// each a protection pattern under refs/heads/ or refs/tags/ that reaches no
// seat's namespace ([gitref.PatternReachesSeats]), each once.
//
// NO SEAT'S BRANCHES LEAVE: refs/heads/seats/<handle>/ is where a seat's work
// waits for a person, and a mirror publishes what the company merged, never what
// a seat has not yet offered, so a pattern that could match a ref there is
// refused (refs/heads/** among them; refs/heads/* matches only top-level).
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
					"publishes what the company merged: name the branches it pushes (refs/heads/*, or a "+
					"family such as refs/heads/release/*)", pattern, gitref.SeatsRoot)
		case dup:
			p.add(entry, ErrConflict, "%s is named twice (first at refs[%d])", pattern, was)
		default:
			first[pattern] = k
		}
	}
}

// validatePathOwners holds a repository's path owners: at most
// [CodeMaxPathOwners], each exactly one of a unit or seat the chart resolves and 1
// to [CodeMaxPathOwnerPaths] globs in gitref's grammar, each once; each OWNER once
// by what the chart resolves it to (a unit named by key and by name is one owner
// twice); the whole within [CodeMaxPathOwnersBytes] as recorded, judged last and
// only of a list nothing else is refused in.
//
// RUNNABLE, as an owner's rules are: the reconcile records a unit by its resolved
// key, so an unresolved one has none, and the domain refuses settings past these
// bounds, an unreadable glob and an owner listed twice.
func validatePathOwners(p *problems, path Path, list []CodePathOwner, owners *codeOwners) {
	before := len(*p)
	if len(list) > CodeMaxPathOwners {
		p.add(path, ErrOutOfRange,
			"%d path owners, past the %d one repository may name: list a team rather than each of its "+
				"seats: a unit's owner is its effective lead", len(list), CodeMaxPathOwners)
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
				"with dir/** rather than each of its files (a glob's &, < and > are six bytes each)",
			n, CodeMaxPathOwnersBytes)
	}
}

// PathOwnersBytes is the length of the path owners' JSON as the code log records
// them (each owner by what chart index o resolves it to), which
// [CodeMaxPathOwnersBytes] bounds; false where an owner does not resolve.
func (r CodeRepo) PathOwnersBytes(o *org.Index) (int, bool) {
	return codePathOwnersBytes(o, r.PathOwners)
}

// codePathOwnersBytes is [CodeRepo.PathOwnersBytes] over list.
//
// IN THE SHAPE THE DOMAIN RECORDS ([codeRecordedPathOwner]), so its figure is the
// record's: the engine's TestDeclaredPathOwnersWeighWhatTheDomainRecords holds
// the two, or `crewlet validate` would disagree with every node's reconcile.
func codePathOwnersBytes(o *org.Index, list []CodePathOwner) (int, bool) {
	recorded := make([]codeRecordedPathOwner, len(list))
	for i, po := range list {
		who, ok := po.recordedOwner(o)
		if !ok {
			return 0, false
		}
		recorded[i] = codeRecordedPathOwner{Owner: who, Paths: po.Paths}
	}
	// Cannot fail: only strings and lists of them.
	b, _ := json.Marshal(recorded)
	return len(b), true
}

// codeRecordedPathOwner is a path owner in the shape the domain records:
// gitlet.PathOwner's keys, globs in document order (the record's sort changes no
// length).
type codeRecordedPathOwner struct {
	Owner codeRecordedOwner `json:"owner"`
	Paths []string          `json:"paths"`
}

// codeRecordedOwner is an owner as the domain records it (gitlet.Owner's keys): a
// unit by key, a seat by handle.
type codeRecordedOwner struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// recordedOwner is who the entry names as the domain records it, resolved on chart
// index o as the reconcile does (a unit by key, whichever spelling the entry
// used, or a seat by handle); false for an entry naming neither or both, or an
// owner o does not hold.
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

// validate holds one path owner and answers who it resolves to
// ([CodePathOwner.recordedOwner]) where it does.
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

// codeOwners is the chart as code.repos' owners resolve against it: an
// [org.Index] built once, and the two lists a refusal names, each joined once at
// the first refusal needing it.
//
// ONCE A DOCUMENT, because the list is up to [CodeMaxRepos] long and these are
// runnable rules, run at every apply on every node: the chart's one-off lookups
// walk every unit or seat per owner. Ten thousand owners on 300 units and 900
// seats cost 1.92 s that way, 95–101 ms through the index
// (BenchmarkValidatingTheMostRepositories).
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

// unitRefs is every unit's key in x, in walk order, as a refusal names it, with
// the name beside it where the two differ: "engineering (Engineering)".
//
// THE KEY AND THE NAME, because a write mints each key from the name
// ([MintIdentities]): by key alone a typist cannot tell which unit it is; by name
// alone nothing says what the chart holds it under. A unit with no key is keyed
// on its name and listed once. The name is looked up BY THE KEY
// ([org.Index.UnitByRef]), so a key two units of an unadmitted chart share lists
// the unit an owner naming it would get.
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

// validate holds an owner: exactly one of a unit or seat, resolving on the chart.
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

// validate holds one protection rule and reports whether it names one valid
// pattern, which the caller's uniqueness check needs.
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
				"number of them, a glob at most %d components below refs/",
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
			rule.DirectPush, words.Or(CodeDirectPushes), CodeDirectPersons)
		ok = false
	}
	rule.validateGate(p, path)
	return ok
}

// validateGate holds the rule's merge gate (at the rule's path, fields beneath),
// RUNNABLE as its pattern is: the domain refuses a gate on a tag rule, a value of
// no closed set, a landing outside its bounds, a list past its bound or naming a
// value twice, an unreadable glob, an unnameable check, no_code_change as an
// exempting class, and a gate past [CodeMaxGateBytes], and the reconcile cannot
// record what the domain refuses. The bytes are judged last and only of a gate
// whose every field passed, so one mistake is named once. Whether the gate does
// anything is a warning's ([Company.codeWarnings]).
func (rule CodeProtection) validateGate(p *problems, path Path) {
	set := rule.gateFields()
	if len(set) == 0 {
		return
	}
	if rule.Tag != "" {
		// At the first field set, naming every one: the gate as a whole is one mistake.
		p.add(at(path, set[0]), ErrConflict,
			"a tag never moves, so no merge reaches one: %s gate merges, which only a branch rule has: "+
				"move them to a rule over the branches they are for", words.And(set))
		return
	}
	before := len(*p)
	if !rule.MergeBy.Valid() {
		p.add(at(path, "merge_by"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			rule.MergeBy, words.Or(CodeMergeBys), CodeMergeByWriters)
	}
	if !rule.Approvals.Valid() {
		p.add(at(path, "approvals"), ErrUnknownValue, "%q (want %s, or unset for %s)",
			rule.Approvals, words.Or(CodeApprovalCounts), CodeApprovalsNone)
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
			rule.DeleteSource, words.Or(CodeDeleteSources), CodeDeleteSeats)
	}
	if len(*p) > before {
		return
	}
	if n := rule.Gate().EncodedBytes(); n > CodeMaxGateBytes {
		p.add(path, ErrOutOfRange, "the rule's merge gate is %d bytes as the code log records it, past the %d "+
			"one gate may weigh: require fewer checks or name fewer human paths (a glob's &, < and > are "+
			"six bytes each)", n, CodeMaxGateBytes)
	}
}

// validateGateClasses holds a list of exempting classes (field name): each a PATH
// class, each once. no_code_change is refused: an approval already carries across
// a revision that changed nothing, so it has nothing to exempt.
func validateGateClasses(p *problems, path Path, name string, list []CodeChangeClass) {
	seen := make(map[CodeChangeClass]int, len(list))
	for k, c := range list {
		switch was, dup := seen[c]; {
		case c == CodeChangeClassNoCodeChange:
			p.add(idx(path, k), ErrConflict, "no_code_change is a revision's class, never a path's: an "+
				"approval already carries across a revision that changed nothing, so it has nothing to "+
				"exempt (want %s)", words.Or(CodePathClasses))
		case !slices.Contains(CodePathClasses, c):
			p.add(idx(path, k), ErrUnknownValue, "%q (want %s)", c, words.Or(CodePathClasses))
		case dup:
			p.add(idx(path, k), ErrConflict, "%q is listed twice (first at %s[%d])", c, name, was)
		default:
			seen[c] = k
		}
	}
}

// codeWarnings is what is valid about the company's code and still worth knowing:
// seat code access tiers that grant nothing ([Company.roleCodeWarnings]); where
// the engine hosts code, cells placing a seat's box in `direct`
// ([Company.directBoxWarnings]); and every declared repository that
//
//   - is `visibility: unit` and owned by a unit with no effective lead;
//   - has a default branch no rule protects (matched by the matcher a push's
//     decide asks, so `release/*` warns about main and `**` does not), at the
//     field to ADD, protect;
//   - has a merge gate field that does nothing ([idleGateFields]), at that field;
//   - has a merge queue that cannot land one change a day per seat that may merge
//     ([Company.mergeRateWarnings]), at the rule's landing.
func (c *Company) codeWarnings() []Warning {
	out := c.roleCodeWarnings()
	if c.CodeBackendFor() != CodeGitlet {
		return out
	}
	out = append(out, c.directBoxWarnings()...)
	// The chart is built only for a company that hosts code: no other advisory
	// reads one.
	o, _ := c.organization()
	units := o.Index()
	// Every human seat is a person who may merge (a person maintains every
	// repository, ADR-0085), counted once here rather than per rule.
	persons := 0
	for s := range o.AllRoles() {
		if s.IsHuman() {
			persons++
		}
	}
	for i, r := range c.Code.Repos {
		if w, ok := unitWithNoLead(units, i, r); ok {
			out = append(out, w)
		}
		// Not a pull mirror's branch: it moves only by Gitlet's syncs. Its protect
		// list is warned instead, every rule there holding nothing.
		if r.MirrorFrom != nil {
			if len(r.Protect) > 0 {
				w := mirrorProtectAdvice.raise(at(idx(field("code.repos"), i), "protect"), fmt.Sprintf(
					"%s is a pull mirror (mirror_from): every ref moves by Gitlet's syncs from its "+
						"source, and it takes no push and no change of its own, so no rule of its protect "+
						"list holds anything (remove protect, or remove mirror_from to push to it here)",
					r.Name))
				w.Item = r.Name
				out = append(out, w)
			}
			continue
		}
		out = append(out, idleGateFields(i, r)...)
		// Not an archived repository's branch: it takes no push, so a warning
		// would be false.
		if r.Archived {
			continue
		}
		out = append(out, c.mergeRateWarnings(units, persons, i, r)...)
		branch := r.ResolvedDefaultBranch()
		ref := "refs/heads/" + branch
		if !gitref.ValidBranch(branch) || gitref.ReservedSeatBranch(branch) {
			// Validation refuses it, the line to read first.
			continue
		}
		if slices.ContainsFunc(r.Protect, func(rule CodeProtection) bool {
			return gitref.MatchPattern(rule.Pattern(), ref)
		}) {
			continue
		}
		w := unprotectedBranchAdvice.raise(at(idx(field("code.repos"), i), "protect"), fmt.Sprintf(
			"%s's default branch %s is unprotected: a person can force-push it, and no merge "+
				"gate holds what merges into it (add {branch: %s} to its protect list, with merge_by, "+
				"approvals, human_paths or checks to gate its merges)", r.Name, branch, branch))
		w.Item = branchItem(r.Name, branch)
		out = append(out, w)
	}
	return out
}

// The words of [Company.codeWarnings]' own two.
var (
	mirrorProtectAdvice = says("Branch rules on a mirrored repository",
		"This repository copies another and takes no pushes, so its branch rules do nothing. "+
			"Remove them, or stop mirroring to push here.")
	unprotectedBranchAdvice = says("Main branches anyone can overwrite",
		"Anyone can overwrite this repository's main branch, and nothing checks what merges "+
			"into it. Add a protection rule for the branch.")
)

// branchItem is a branch of a repository as a warning's Item names it.
func branchItem(repo, branch string) string {
	return repo + " (" + branch + ")"
}

// directBoxWarnings is one warning per place a company hosting code puts a seat's
// box in `direct`: an executor entry's `run_in`, or once at
// `providers.sandbox.default_run_in` for every box inheriting it ([Company.SeatBoxes]).
//
// A `direct` box runs as the engine's own user, so it reads every repository a
// Gitlet node's cache holds and, on ANY node, the Tier A keyring every run token
// derives from: it can mint any live executor run's token and push as that seat,
// whatever `visibility: unit` or a run's grants say.
//
// A WARNING, NOT A REFUSAL (M3-decisions U1 a): refusing would close none of those
// reads while taking every single-node deployment its `direct` cell; the boundary
// is the cell (docs/concepts/code-sandbox.md). Not for the double (`fake: true`).
func (c *Company) directBoxWarnings() []Warning {
	catalogue := c.Providers.Sandbox
	if !catalogue.Enabled() || catalogue.Fake {
		return nil
	}
	const reads = "a `direct` box runs as the engine's own user: it can read every repository cached on a " +
		"Gitlet node and, on any node, the Tier A keyring that signs run tokens, so it can mint the token of " +
		"ANY live executor run and push as that run's seat, and a repository's `visibility: unit` and a " +
		"run's grants do not bind "
	const remedy = "; run it in `container` to hold it to them"
	// One wording for both places ([directBoxAdvice]), told apart by Item.
	var out []Warning
	inherited := false
	entries := map[string]bool{}
	for box := range c.SeatBoxes() {
		if box.Cell != PlacementDirect {
			continue
		}
		// An entry's own run_in, or the default it inherits: a seat names no cell.
		if box.Executor == "" || c.Providers.LLM[box.Executor].RunIn == "" {
			inherited = true
			continue
		}
		if entries[box.Executor] {
			continue
		}
		entries[box.Executor] = true
		w := classed(ClassDirectBox, directBoxAdvice, at(entryPath(box.Executor), "run_in"),
			reads+"the executors of the seats this entry launches on"+remedy)
		w.Item = c.ModelName(box.Executor)
		out = append(out, w)
	}
	if inherited {
		w := classed(ClassDirectBox, directBoxAdvice, field("providers.sandbox.default_run_in"),
			reads+"the executors of the seats that take this default"+remedy)
		w.Item = "the sandbox default"
		out = append(out, w)
	}
	return out
}

// idleGateFields is a warning for every merge gate field of code.repos[i], r, that
// does nothing as the rule stands (valid, but the rule says less than its author
// meant), at the field to remove or the one beside it to set:
//
//   - owners_approve on a repository naming no path owners: it waits for nobody;
//   - landing on a rule requiring no checks: only a branch whose merges wait for
//     checks has a queue;
//   - approvals_unless beside approvals: none, exempting from approvals never asked.
//
// A tag rule is skipped: any gate on one is validation's refusal.
func idleGateFields(i int, r CodeRepo) []Warning {
	var out []Warning
	for k, rule := range r.Protect {
		if rule.Branch == "" || rule.Tag != "" {
			continue
		}
		rulePath := idx(at(idx(field("code.repos"), i), "protect"), k)
		idle := func(a advice, name, why string) {
			w := a.raise(at(rulePath, name), fmt.Sprintf("%s's rule on %s sets %s, %s",
				r.Name, rule.Branch, name, why))
			w.Item = branchItem(r.Name, rule.Branch)
			out = append(out, w)
		}
		if rule.OwnersApprove && len(r.PathOwners) == 0 {
			idle(idleOwnersApproveAdvice, "owners_approve", "and the repository names no path owners: "+
				"no change touches an owned path, so it waits for nobody's approval (name path_owners, "+
				"or remove owners_approve)")
		}
		if len(rule.Checks) == 0 && rule.Landing != (CodeLanding{}) {
			idle(idleLandingAdvice, "landing", "and requires no checks: only a branch whose merges wait "+
				"for checks has a queue to land them, so every merge into this one lands directly "+
				"(require checks, or remove landing)")
		}
		if len(rule.ApprovalsUnless) > 0 && rule.Approvals.Resolved() == CodeApprovalsNone {
			idle(idleApprovalsUnlessAdvice, "approvals_unless", "beside approvals: none, so it exempts "+
				"changes from approvals the rule never asks (set approvals: one, or remove approvals_unless)")
		}
	}
	return out
}

// The words of [idleGateFields], one per merge gate field that does nothing.
var (
	idleOwnersApproveAdvice = says("Owner approval with no owners named",
		"This branch rule asks file owners to approve, but the repository names no owners, so "+
			"it waits for nobody. Name owners or drop the setting.")
	idleLandingAdvice = says("Merge queues with no checks to wait for",
		"This branch rule sets up a merge queue but requires no checks, so every merge skips "+
			"the queue. Require checks or remove the queue.")
	idleApprovalsUnlessAdvice = says("Approval exemptions with no approvals asked",
		"This branch rule exempts changes from approvals it never asks for, so the exemption "+
			"does nothing. Require an approval or remove it.")
)

// codeMergeRateDay is the span `crewlet validate` prices a queue's capacity over:
// a DAY at one landing a seat; a queue that cannot keep up with a quiet writer
// falls behind on an ordinary day (M5-D28).
const codeMergeRateDay = 24 * time.Hour

// mergeRateWarnings is a warning for every rule of code.repos[i], r, whose branch
// has a merge queue (a gate requiring a check) landing fewer changes a day than
// the seats that may merge into it at one a day each (M5-D28): [LandingCapacity]
// at the rule's depth and batch, a round as long as code.checks.max_timeout_seconds
// (the slowest the company allows), over [codeMergeRateDay]. At the rule's landing.
//
// `crewlet validate` CANNOT OBSERVE A RATE: it warns from configuration;
// code_merge_rate_over_capacity, raised by the repository's home from observed
// enqueues and rounds, is the other half.
//
// WHO MAY MERGE is counted through the chart's index ([seatsThatMayMerge]), under
// the stricter of merge_by and code.merge_by_floor (M5-D2).
func (c *Company) mergeRateWarnings(units *org.Index, persons, i int, r CodeRepo) []Warning {
	var out []Warning
	floor := c.Code.MergeByFloor.Resolved()
	round := c.Code.Checks.MaxTimeout()
	for k, rule := range r.Protect {
		if rule.Branch == "" || rule.Tag != "" {
			continue
		}
		g := rule.Gate()
		if g == nil || len(g.Checks) == 0 {
			continue
		}
		by := g.MergeBy
		if slices.Index(CodeMergeBys, floor) > slices.Index(CodeMergeBys, by) {
			by = floor
		}
		seats := seatsThatMayMerge(units, persons, r.Owner, by)
		capacity := LandingCapacity(g.Landing.Depth, g.Landing.Batch, round, codeMergeRateDay)
		if capacity >= seats {
			continue
		}
		w := mergeRateAdvice.raise(at(idx(at(idx(field("code.repos"), i), "protect"), k), "landing"),
			fmt.Sprintf("%s's queue on %s lands at most %d change(s) a day (%d candidate(s) of up to %d "+
				"change(s) a round, a round as long as the longest check may run (%s, "+
				"code.checks.max_timeout_seconds)), fewer than the %d seat(s) that may merge into it "+
				"(merge_by %s), at one change a day each, so its queue falls behind once they merge "+
				"(raise landing.depth or landing.batch, or lower code.checks.max_timeout_seconds to what "+
				"its checks take)", r.Name, rule.Branch, capacity, g.Landing.Depth, g.Landing.Batch, round,
				seats, by))
		w.Item = branchItem(r.Name, rule.Branch)
		out = append(out, w)
	}
	return out
}

// mergeRateAdvice is [Company.mergeRateWarnings]' rule.
var mergeRateAdvice = says("Merge queues too slow for their team",
	"This merge queue lands fewer changes a day than the people and agents allowed to merge, "+
		"so it falls behind. Let it land more each round.")

// seatsThatMayMerge is how many seats may merge into a branch of a repository
// owned by owner under merge authority by (M5-D2), counted as Gitlet's grant
// rules relate a seat to a repository:
//
//   - every HUMAN seat: a person maintains every repository;
//   - under writers, every agent seat that WRITES it (unit-owned: its effective
//     lead and every seat of its subtree; seat-owned: that seat); under
//     maintainers, the one that MAINTAINS it (effective lead or owning seat);
//     under persons, none;
//
// each agent seat only at code.access full_access, where any grant is capped. An
// unresolved owner is validation's refusal and relates no seat here.
func seatsThatMayMerge(units *org.Index, persons int, owner CodeOwner, by CodeMergeBy) int {
	n := persons
	if by == CodeMergeByPersons {
		return n
	}
	may := func(s *org.Role) bool { return s != nil && !s.IsHuman() && s.CodeAccess == TierFullAccess }
	switch {
	case strings.TrimSpace(owner.Unit) != "":
		u := units.UnitByRef(strings.TrimSpace(owner.Unit))
		if u == nil {
			return n
		}
		lead := units.EffectiveLead(u)
		if may(lead) {
			n++
		}
		if by == CodeMergeByMaintainers {
			return n
		}
		for _, s := range units.Members(u) {
			if s != lead && may(s) {
				n++
			}
		}
	case owner.Seat != "":
		if may(units.Role(owner.Seat)) {
			n++
		}
	}
	return n
}

// unitWithNoLead is the warning about code.repos[i], r, when it is `visibility:
// unit` and owned by a unit with no effective lead, both found through the chart's
// index. An unresolved owner is validation's refusal, not warned twice.
//
// THE LEAD THROUGH THE INDEX, never [org.Organization.EffectiveLead]: a team's lead
// is usually inherited, which the chart's method finds by searching the subtree
// then walking every seat, costing repositories × seats.
// BenchmarkWarningAboutTheMostUnitRepositories (10 000 repositories, 930 seats):
// 166–184 ms through the chart, 16–18 ms through the index.
func unitWithNoLead(units *org.Index, i int, r CodeRepo) (Warning, bool) {
	ref := strings.TrimSpace(r.Owner.Unit)
	if r.Visibility.Resolved() != CodeVisibilityUnit || ref == "" {
		return Warning{}, false
	}
	u := units.UnitByRef(ref)
	if u == nil || units.EffectiveLead(u) != nil {
		return Warning{}, false
	}
	w := unitRepoNoLeadAdvice.raise(at(idx(field("code.repos"), i), "visibility"), fmt.Sprintf(
		"%s is visibility: unit and owned by %s, which has no lead: only persons and "+
			"%s's members read it (set a lead on %s, or the owner)", r.Name, ref, ref, ref))
	w.Unit, w.Item = u.Name, r.Name
	return w, true
}

// unitRepoNoLeadAdvice is [unitWithNoLead]'s rule.
var unitRepoNoLeadAdvice = says("Team repositories whose team has no lead",
	"Only people and the team's members can read this repository, because its team has no "+
		"lead. Give the team a lead, or the repository another owner.")

// roleCodeWarnings is every agent seat whose `code.access` nothing reads: the
// company hosts no code and Gitlet is the only thing a tier caps. Valid, since
// tiers may be written before Gitlet is on, but the line reads as a grant. At the
// seat's code.access.
//
// A SEAT'S GITHUB APP DOES NOT READ IT: an App's access is its own
// (`integrations.github.permissions`), so the warning says so rather than point
// at GitHub as a second place the tier counts.
//
// EVERY TIER, NOT ONLY full_access: read_only and review read as grants too. NOT A
// HUMAN SEAT: a tier there is its own admission refusal
// ([Company.validateHumanSeatCode]), as is a tier validation refuses.
func (c *Company) roleCodeWarnings() []Warning {
	backend := c.CodeBackendFor()
	if backend == CodeGitlet {
		return nil
	}
	var out []Warning
	for role, path := range c.EachRole() {
		if role.Kind == org.KindHuman || role.Code.IsZero() ||
			!slices.Contains(CodeAccessTiers, role.Code.Access) {
			continue
		}
		w := accessWithoutHostingAdvice.raise(at(path, "code.access"), fmt.Sprintf(
			"code.access %s grants nothing: this company hosts no code (code.backend is %s), "+
				"and Gitlet is the only place a tier caps what a seat may do. "+
				"A GitHub App's access is its integrations.github.permissions. Set code.backend: gitlet, "+
				"or remove code",
			role.Code.Access, backend))
		w.Seat, w.Item = role.Seat().Handle(), strings.ReplaceAll(role.Code.Access, "_", " ")
		out = append(out, w)
	}
	return out
}

// accessWithoutHostingAdvice is [Company.roleCodeWarnings]' rule; the
// warning's Item names the seat's tier.
var accessWithoutHostingAdvice = says("Code access with no code hosting",
	"This agent has a code access level, but the company hosts no code, so it grants nothing. "+
		"Turn on code hosting or remove the level.")
