package cache

import "errors"

// EveryFillMeets reports whether a sync failed on what EVERY Gitlet node's copy
// is filled from: the code log (ErrLogRead), the object store (ErrStoreRead,
// ErrPackMissing, ErrCorruptPack), rows this build cannot install
// (ErrBadSnapshot), or damage a whole read did not end (ErrRebuildRefused).
// Work over a copy (internal/gitlet/query's reads, internal/gitlet/host's
// builds) answers such a sync gitlet.ErrGitUnready{syncing}, which keeps the
// walk's filler where it is (gitlet.UnreadyReason.PassesFill): another node
// would pay a download, every node at once against a throttling store, to meet
// the same. A store only this node cannot reach keeps its filler too: a herd of
// fills costs more than one node waiting for its operator. Any other failure is
// the node's own (gitlet.UnreadyNodeFailed). One rule for reads and writes, so
// the walk never fills elsewhere for one and stays home for the other.
func EveryFillMeets(err error) bool {
	var (
		missing *ErrPackMissing
		corrupt *ErrCorruptPack
	)
	return errors.Is(err, ErrLogRead) || errors.Is(err, ErrStoreRead) || errors.As(err, &missing) ||
		errors.As(err, &corrupt) || errors.Is(err, ErrBadSnapshot) || errors.Is(err, ErrRebuildRefused)
}
