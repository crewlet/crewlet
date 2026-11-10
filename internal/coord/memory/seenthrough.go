package memory

import (
	"context"
	"errors"
)

// ---- the seats' seen-through marks ------------------------------------- //

// seenMark is one stored mark and its revision.
type seenMark struct {
	mark     string
	revision uint64
}

// SeenThrough is the mark scope holds for conversation and its revision.
func (f *Fleet) SeenThrough(_ context.Context, scope, conversation string) (string, uint64, error) {
	if scope == "" || conversation == "" {
		return "", 0, errors.New("coord/memory: a seen-through mark names its seat and its conversation")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held := f.seen[scope+"\x00"+conversation]
	return held.mark, held.revision, nil
}

// MarkSeenThrough writes mark over the one read at revision, false where it moved.
func (f *Fleet) MarkSeenThrough(_ context.Context, scope, conversation, mark string,
	revision uint64) (bool, error) {

	if scope == "" || conversation == "" || mark == "" {
		return false, errors.New("coord/memory: a seen-through mark names its seat, its conversation and its position")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := scope + "\x00" + conversation
	if f.seen[key].revision != revision {
		return false, nil
	}
	f.seenRevision++
	f.seen[key] = seenMark{mark: mark, revision: f.seenRevision}
	return true, nil
}
