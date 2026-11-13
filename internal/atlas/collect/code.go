package collect

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
)

// The seam between a forge and the reading of its repositories' deploy and
// dependency files (ADR-0136). A forge's source that also implements
// [CodeHost] lists its repositories and serves their trees and blobs; what is
// listed, which file is read and what a file declares are decided by ONE
// reader for every forge (internal/atlas/codescan), never by the vendor.

// CodeHost is a source whose vendor hosts repositories.
type CodeHost interface {
	Source

	// OpenCode opens the vendor for one pass over its repositories' files,
	// under the access the source's own read uses and nothing wider. It
	// answers [integration.ErrNotConfigured] where the company does not
	// connect the forge. Every call the session makes is a read.
	OpenCode(ctx context.Context, in Input) (CodeSession, error)
}

// CodeSession is one pass's access to a forge. It is closed when the pass
// ends, however it ends.
type CodeSession interface {
	// Repositories lists every repository whose files the pass may read:
	// neither archived nor empty. Whole is false where part of the forge
	// could not be listed, so a repository that is not in the listing is not
	// known to be gone.
	Repositories(ctx context.Context) (repos []CodeRepository, whole bool, err error)

	// Tip is the commit the repository's default branch is at, "" for a
	// branch that holds none.
	Tip(ctx context.Context, repo CodeRepository) (string, error)

	// Files lists every regular file of the tree at tip, whatever its
	// name: no directory, no submodule and no symbolic link, whose blob is
	// a path and never a file. It answers [ErrCodeTooLarge] for a tree the
	// forge does not list whole.
	Files(ctx context.Context, repo CodeRepository, tip string) ([]CodeFile, error)

	// Blob reads the object oid of the repository, at most limit bytes of
	// it. Oversize is a blob past limit, of which nothing is answered.
	Blob(ctx context.Context, repo CodeRepository, oid string, limit int64) (raw []byte, oversize bool, err error)

	// Calls is how many requests the session has made at the vendor.
	Calls() int

	// Close ends the session, withdrawing any credential it minted.
	Close(ctx context.Context)
}

// CodeRepository is one repository of a forge.
type CodeRepository struct {
	// Key is the repository's key in the atlas: what its forge's address
	// reduces to ([atlas.ForgeRepositoryKey]), which is also what a mirror's
	// source address reduces to.
	Key string

	// ID is the vendor's own handle for the repository, opaque to the pass.
	ID string

	// Branch is the default branch.
	Branch string

	// Stamp is a value of the listing that changes whenever the default
	// branch may have moved, and "" where the listing carries none. A
	// repository whose stamp is the one it was last read whole at is not
	// asked for its tip.
	Stamp string
}

// CodeFile is one regular file of a tree. Size is -1 where the forge's
// listing does not say it.
type CodeFile struct {
	Path string
	OID  string
	Size int64
}

// ErrCodeTooLarge is a tree a forge does not list whole.
var ErrCodeTooLarge = errors.New("the repository's tree is past what the forge lists whole")

// ErrCodeRefused is a forge refusing the session one repository's code: a
// permission the credential lacks there, or the repository gone since the
// listing. The pass leaves that repository as it was and goes on; any other
// fault ends the pass.
var ErrCodeRefused = errors.New("the forge refused the repository's code")

// CodePass reads the files of the repositories one forge hosts
// (codescan.Forges). The duty asks it about every source that is a
// [CodeHost], after the source's own read; the pass keeps its own standing,
// apart from the source's, since it comes due on its own.
type CodePass interface {
	// Due reports whether a pass over the forge is owed at now: none was
	// made, the interval has passed since the last, or the last left work
	// or a fault behind and [RetryAfter] has passed.
	Due(ctx context.Context, kind integration.Kind, interval time.Duration, now time.Time) bool

	// Pass runs one pass over host.
	Pass(ctx context.Context, host CodeHost, in Input, now time.Time)

	// Forget withdraws what the files of every repository of a forge the
	// company no longer connects declared.
	Forget(ctx context.Context, kind integration.Kind)
}
