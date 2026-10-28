package config

import (
	"fmt"
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
	// — 2 048 globs — and the bytes the repository's settings carry.
	CodeMaxPathOwnerPaths = 32

	// CodeMaxDescriptionBytes bounds a repository description: one
	// paragraph, which rides every settings record and every listing row.
	CodeMaxDescriptionBytes = 1_024

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
	// 1 481 bytes (internal/code's BenchmarkPushLogBytes, its push=agent
	// arm), so about 1.5 MB a day — rather than without bound.
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
	Repos                []CodeRepo  `yaml:"repos,omitempty" json:"repos,omitempty" desc:"The company's repositories, at most 10000. Each activation creates the ones declared and settles their settings; removing one deletes it, restorable for deleted_retention_days by declaring it again."`
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

// CodeRepo is one repository code.repos declares.
type CodeRepo struct {
	Name          string           `yaml:"name" json:"name" js:"pattern=^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$" desc:"The repository's name: one label of lowercase letters, digits and hyphens, starting and ending with a letter or a digit, at most 63 — no dot, which only a seat's own scratch repository's name carries. It is the address a repository is cloned by, so renaming one here deletes it and creates another."`
	Owner         CodeOwner        `yaml:"owner" json:"owner"`
	Visibility    CodeVisibility   `yaml:"visibility,omitempty" json:"visibility,omitempty" js:"enum=company|unit" desc:"Who may read it besides persons: company (unset) — every seat — or unit — the owning unit's members, its leads and the owning seat."`
	DefaultBranch string           `yaml:"default_branch,omitempty" json:"default_branch,omitempty" desc:"The branch a clone checks out; unset is main."`
	Description   string           `yaml:"description,omitempty" json:"description,omitempty" desc:"One paragraph, at most 1024 bytes."`
	Archived      bool             `yaml:"archived,omitempty" json:"archived,omitempty" desc:"Read-only: still fetched and cloned, and takes no push."`
	Protect       []CodeProtection `yaml:"protect,omitempty" json:"protect,omitempty" desc:"The rules a push to the refs they match is held to, at most 64. A ref no rule matches may be force-pushed or deleted by anybody who may push."`
	// PathOwners are who owns which of the repository's paths: whom a change
	// touching them asks for a review. They GRANT NOTHING — who may read and
	// push is the chart's and the tier's, whatever a path owner names.
	PathOwners []CodePathOwner `yaml:"path_owners,omitempty" json:"path_owners,omitempty" desc:"Who owns which of the repository's paths, at most 64 owners: a change touching an owner's paths asks it for a review. A path owner is asked, never granted: it reads and pushes what the chart lets it, as before."`
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

// CodeOwner is who owns a repository: exactly one of a unit or a seat.
type CodeOwner struct {
	Unit string `yaml:"unit,omitempty" json:"unit,omitempty" desc:"A unit of the chart: its id, or its name where it has none. Exactly one of unit and seat."`
	Seat string `yaml:"seat,omitempty" json:"seat,omitempty" desc:"A seat, by its handle. Exactly one of unit and seat."`
}

// CodeProtection is one protection rule: the branches or the tags it matches,
// and what a push to one of them may do.
type CodeProtection struct {
	Branch      string         `yaml:"branch,omitempty" json:"branch,omitempty" desc:"A branch name or glob: * matches one path component, ** any number, and a name with no * matches only itself. Exactly one of branch and tag."`
	Tag         string         `yaml:"tag,omitempty" json:"tag,omitempty" desc:"A tag name or glob, as for branch. Exactly one of branch and tag."`
	AllowForce  bool           `yaml:"allow_force,omitempty" json:"allow_force,omitempty" desc:"Let a push move a matching branch to a commit that does not descend from it. Not on a tag rule: a tag never moves."`
	AllowDelete bool           `yaml:"allow_delete,omitempty" json:"allow_delete,omitempty" desc:"Let a push delete a matching ref."`
	DirectPush  CodeDirectPush `yaml:"direct_push,omitempty" json:"direct_push,omitempty" js:"enum=persons|none" desc:"Who may push to a matching ref directly: persons (unset), a person and never a seat, whose push is refused; or none, which freezes it — nothing pushes to it."`
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
// default branch, its description, its protection rules — at most
// [CodeMaxProtections], each naming exactly one of a branch or a tag pattern
// in gitref's grammar, each pattern protected once, and no force on a tag — and
// its path owners ([validatePathOwners]).
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
	return p.err()
}

// validatePathOwners holds a repository's path owners at path into p: at most
// [CodeMaxPathOwners], each naming exactly one of a unit or a seat the chart
// resolves, and 1 to [CodeMaxPathOwnerPaths] globs in gitref's path grammar,
// each once — and each OWNER once, by what the chart resolves it to, so a unit
// named once by its key and once by its name is one owner named twice.
//
// RUNNABLE, as an owner's rules are: the reconcile records a unit by the key
// the chart resolves, so one it cannot resolve has no key to record, and the
// code domain refuses settings past these bounds, a glob its grammar does not
// read and an owner listed twice — the one form a record carries, which a
// declaration listing an owner twice is not.
func validatePathOwners(p *problems, path Path, list []CodePathOwner, owners *codeOwners) {
	if len(list) > CodeMaxPathOwners {
		p.add(path, ErrOutOfRange,
			"%d path owners, past the %d one repository may name: list a team rather than each of its "+
				"seats — a unit's owner is its effective lead", len(list), CodeMaxPathOwners)
	}
	first := make(map[string]int, len(list))
	for k, po := range list {
		entry := idx(path, k)
		key, resolved := po.validate(p, entry, owners)
		if !resolved {
			continue
		}
		if was, dup := first[key]; dup {
			p.add(entry, ErrConflict,
				"%s owns paths at path_owners[%d] already: one owner is one entry, so list "+
					"everything it owns in that one", key, was)
			continue
		}
		first[key] = k
	}
}

// validate holds one path owner at path into p, and answers who it resolves
// to — "unit <key>" or "seat <handle>", as the chart names the owner whatever
// spelling the entry used — where it resolves at all.
func (po CodePathOwner) validate(p *problems, path Path, owners *codeOwners) (string, bool) {
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
	case unit != "":
		u := owners.index.UnitByRef(unit)
		if u == nil {
			p.add(at(path, "unit"), ErrUnknownValue,
				"%q is no unit of this company's chart, by id or by name; the units are: %s",
				unit, owners.units())
			return "", false
		}
		return "unit " + u.Key(), true
	default:
		if owners.index.SeatByHandle(seat) == nil {
			p.add(at(path, "seat"), ErrUnknownValue,
				"%q is no seat's handle in this company; the seats are: %s", seat, owners.seats())
			return "", false
		}
		return "seat " + seat, true
	}
	return "", false
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
	return ok
}

// codeWarnings is what is valid about the company's code and still worth
// knowing, against its own chart: every seat's code access tier that grants
// nothing ([Company.roleCodeWarnings]), and, where the engine hosts code,
// every declared repository that
//
//   - is `visibility: unit` and owned by a unit with no lead — effective, so
//     inherited counts — which only persons and the unit's own members read:
//     no lead's seat reads it, because there is none, and every other seat is
//     what the visibility shuts out. Located at the visibility;
//   - has a default branch no rule protects: a person who may push to it may
//     force-push it or delete it. Through gitref's own matcher — the one a
//     push's decide asks — so `release/*` warns about main and `**` does not.
//     Located at the field to ADD, the repository's protect.
//
// NOT AN ARCHIVED REPOSITORY'S BRANCH: it takes no push at all — the decide
// refuses the whole push — so nobody can force-push or delete its branch, and
// saying so would be false. Unarchiving one that still has no rule brings the
// warning back with the revision that does it. Its visibility still warns:
// an archived repository is still read.
//
// THE CHART IS BUILT HERE, and only for a company that hosts code: no other
// advisory reads one, and a company with no repositories would otherwise pay
// a normalised chart at every validate and every apply for nothing.
func (c *Company) codeWarnings() []Warning {
	out := c.roleCodeWarnings()
	if !c.Code.Native {
		return out
	}
	o, _ := c.organization()
	units := o.Index()
	for i, r := range c.Code.Repos {
		if w, ok := unitWithNoLead(units, i, r); ok {
			out = append(out, w)
		}
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
			"%s's default branch %s is unprotected: a person can force-push or delete it "+
				"(add {branch: %s} to its protect list)", r.Name, branch, branch)))
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
