package coord

import "context"

// AtlasWake is one record of [AtlasWakes] as a listing reads it.
type AtlasWake struct {
	Value    []byte
	Revision uint64
}

// AtlasWakes holds, for each agent seat and atlas entry it owns, the day the
// seat was last woken about the entry and what it was told then (ADR-0143).
// The duty may change hands mid-pass, so every write is conditional on the
// revision it read; the value is internal/atlaswake's.
type AtlasWakes interface {
	// AtlasWakes is every record, by its key ([AtlasWakeKey]).
	AtlasWakes(ctx context.Context) (map[string]AtlasWake, error)
	// AtlasWake is one seat's record on one entry and its revision, false
	// where none.
	AtlasWake(ctx context.Context, handle, entry string) ([]byte, uint64, bool, error)
	// CreateAtlasWake writes a first record, false where one exists.
	CreateAtlasWake(ctx context.Context, handle, entry string, value []byte) (bool, error)
	// UpdateAtlasWake replaces the record read at revision, false where it moved.
	UpdateAtlasWake(ctx context.Context, handle, entry string, value []byte, revision uint64) (bool, error)
	// DeleteAtlasWake drops the record read at revision, false where it moved.
	DeleteAtlasWake(ctx context.Context, handle, entry string, revision uint64) (bool, error)
}

// AtlasWakeClass is the key class a record is filed under in the positions
// register, which no clock ages: the duty drops a record itself.
const AtlasWakeClass = "atlas_wake"

// AtlasWakeKey is the key of one seat's record on one entry.
func AtlasWakeKey(handle, entry string) string { return DocumentKey(AtlasWakeClass, handle, entry) }

// AtlasWakeFilter selects every record.
func AtlasWakeFilter() string { return DocumentFilter(AtlasWakeClass) }

// AtlasWakeOf recovers the seat's handle and the entry from a key, false for
// another class.
func AtlasWakeOf(key string) (handle, entry string, ok bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 3 || segments[0] != AtlasWakeClass || segments[1] == "" || segments[2] == "" {
		return "", "", false
	}
	return segments[1], segments[2], true
}
