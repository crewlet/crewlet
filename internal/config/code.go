package config

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
