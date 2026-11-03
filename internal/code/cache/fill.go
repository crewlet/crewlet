package cache

import "errors"

// EveryFillMeets reports whether a sync failed on what EVERY git node's copy
// of a repository is filled from: the code log its rows are read from
// (ErrLogRead), the object store its packs are (ErrStoreRead, a pack the store
// does not hold or holds damaged, a bundle that is not the one its row
// describes), rows this build cannot install (ErrBadSnapshot), or a repair the
// cache refuses to repeat (ErrRebuildRefused) — refused because a whole read of
// the repository from the store and the rows did not end the damage, so the
// damage is theirs, and refused AT ONCE precisely so that no other node pays a
// whole download to meet it again.
//
// What a git node's work over its copy — a seat's read by object id
// (internal/code/query), an engine write's build (internal/code/host) —
// answers such a sync is code.ErrGitUnready{syncing}, which keeps the walk's
// filler where it is (code.UnreadyReason.PassesFill): another node's fill
// would only meet the same, a download for a failure — against an object
// store that throttles, every git node's at once — while a node already
// holding a copy answers around it. An object store THIS node alone cannot
// reach looks the same from here and keeps its filler too, deliberately: a
// herd of fills against a store that is failing costs the fleet more than one
// node's repositories waiting for its operator. Every other failure — the
// node's disk refusing a write, its cache stopping — is the node's own
// (code.UnreadyNodeFailed), and the walk lets the next node fill.
//
// ONE RULE FOR EVERY KIND OF A GIT NODE'S WORK, here beside the sync it reads,
// because a read and a write that answered one failure two ways would send the
// walk to fill on a node for one of them and keep it home for the other.
func EveryFillMeets(err error) bool {
	var (
		missing *ErrPackMissing
		corrupt *ErrCorruptPack
	)
	return errors.Is(err, ErrLogRead) || errors.Is(err, ErrStoreRead) || errors.As(err, &missing) ||
		errors.As(err, &corrupt) || errors.Is(err, ErrBadSnapshot) || errors.Is(err, ErrRebuildRefused)
}
