package config

import "fmt"

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

	// CodeMaxDescriptionBytes bounds a repository description: one
	// paragraph, which rides every settings record and every listing row.
	CodeMaxDescriptionBytes = 1_024
)

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
)

// Code is the company's own git host: whether it hosts its repositories in
// the engine, and the limits a push is held to.
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
type Code struct {
	Native bool       `yaml:"native,omitempty" json:"native,omitempty" desc:"Host the company's git repositories in the engine itself, on its own state log: every data node commits their pushes. A node running no native backend yet starts the code host at the activation that sets it; on a node whose native runtime already runs, turning it on or off takes effect at the node's restart, which the node says (native_backend_needs_restart)."`
	Limits CodeLimits `yaml:"limits,omitempty" json:"limits,omitzero"`
}

// CodeLimits are what one push and one repository may hold. An unset field
// takes its default ([CodeLimits.Resolved]).
type CodeLimits struct {
	MaxPushBytes int64 `yaml:"max_push_bytes,omitempty" json:"max_push_bytes,omitempty" js:"min=1048576;max=1073741824" desc:"The largest pack one push may send; unset is 1 GiB, the most it may be."`
	MaxBlobBytes int64 `yaml:"max_blob_bytes,omitempty" json:"max_blob_bytes,omitempty" js:"min=1024;max=1073741824" desc:"The largest file (blob) a push may carry, at most max_push_bytes; unset is 50 MiB. A larger binary belongs in tracker files."`
	RepoMaxBytes int64 `yaml:"repo_max_bytes,omitempty" json:"repo_max_bytes,omitempty" js:"min=1048576;max=1099511627776" desc:"The most packed bytes one repository may hold, at least max_push_bytes; unset is 20 GiB. Every git node serving a repository caches it whole."`
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
	return l
}

// zero is a block that sets no limit.
func (l CodeLimits) zero() bool { return l == CodeLimits{} }

// validate holds the code host's rules: code.limits only where something hosts
// code, each limit inside its bounds, and the three in the order a push meets
// them — a file inside a push, a push inside a repository.
//
// THE ORDER IS OF THE RESOLVED LIMITS, because that is what a push is held to:
// a push limit set alone below the default file limit admits no push carrying
// a file the default allows. Each such problem is reported AT THE FIELD THE
// DOCUMENT SETS — the line its author can find — naming the default it met.
// And only once every limit is in range: a value outside its bounds is one
// mistake, and an order problem beside it would be the same mistake twice.
func (c Code) validate(path Path) error {
	var p problems
	limits := at(path, "limits")
	if !c.Native && !c.Limits.zero() {
		p.add(limits, ErrConflict,
			"nothing hosts code on this company: set code.native, or remove code.limits")
	}
	l := c.Limits
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
