package accessdomain

import "time"

// THE BOUNDS ON WHAT IS STORED, held by the writer before a record is published
// so the log never refuses one as too large after the fact.
const (
	// SnapshotMaxBytes bounds a told record's snapshot: a seat at every
	// integration with its scopes, its tool servers, the company's policies
	// cut to their first 300 characters and its setup steps fit in a few KiB,
	// so 64 KiB is a seat's whole surface many times over.
	SnapshotMaxBytes = 64 << 10

	// ChangesMaxBytes bounds a told record's change list, every change kept
	// whole rather than the twenty lines a turn is shown.
	ChangesMaxBytes = 64 << 10

	// MaxAuthors bounds the authors a told record names, and AuthorMaxBytes
	// each: the people and credentials behind the revisions it spans.
	MaxAuthors     = 32
	AuthorMaxBytes = 256

	// RevisionIDMaxBytes bounds a company revision's id.
	RevisionIDMaxBytes = 128

	// KeyIDMaxBytes bounds a certificate's key id: `crewlet/<seat>/<turn>/<call>`.
	KeyIDMaxBytes = 512

	// MaxHosts bounds the hosts a certificate or a command names, the
	// integration's own cap on registered hosts.
	MaxHosts = 200

	// HostIDMaxBytes and UserMaxBytes bound a host id and a login user, the
	// config's own patterns with room.
	HostIDMaxBytes = 64
	UserMaxBytes   = 64

	// FingerprintMaxBytes bounds a key's fingerprint as ssh-keygen prints it.
	FingerprintMaxBytes = 128

	// RefMaxBytes bounds the ids a record carries beside its own: a turn, a
	// held call, a node.
	RefMaxBytes = 128

	// CommandMaxBytes bounds a command's text, the SSH tools' own bound.
	CommandMaxBytes = 4096

	// VerdictSubjectMaxBytes bounds what a verdict decided, in clear: an
	// ssh_operate's command at the SSH tools' own bound, and a later kind's
	// summary cut to the same by its writer. The digest binds the exact
	// request; the subject is for a person to recognise it.
	VerdictSubjectMaxBytes = CommandMaxBytes

	// VerdictReasonMaxRunes, MaxVerdictFindings and VerdictFindingMaxRunes
	// bound a delegate's reason and findings: the answer's own bounds, which
	// the held call holds a delegate to before a verdict is recorded.
	VerdictReasonMaxRunes  = 500
	MaxVerdictFindings     = 8
	VerdictFindingMaxRunes = 300

	// MaxVerdictRule bounds a verdict's rule number: the company's own cap on
	// delegation rules (`delegated_decisions.rules`).
	MaxVerdictRule = 16

	// MaxOverruled bounds the engine's words on one verdict, and OverruledMaxBytes
	// each: a reason per limit the engine checks, with room.
	MaxOverruled      = 16
	OverruledMaxBytes = 40

	// MaxPageSize is the most rows a page answers, DefaultPageSize what a page
	// that names none takes.
	MaxPageSize     = 100
	DefaultPageSize = 25

	// MaxRecentTold is the most told records [Reader.Recent] returns beside the
	// baseline: a seat told more than this in its memory window is told its
	// notes are stale by the newest of them.
	MaxRecentTold = 64
)

// ExpiryBucket is the span one expiry removes: an hour of records, so one
// record's apply stays well inside the framework's row budget.
const ExpiryBucket = time.Hour
