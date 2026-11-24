package memory

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- model checks -------------------------------------------------------- //

// The model retries' class machinery (modelrecords.go), keyed by the entry's
// key: one record per entry, its latest check. Nothing watches it, so the
// update shape is the record itself, as the MCP checks' is.

func newModelCheckRecords() entryRecords[coord.ModelCheck, coord.ModelCheck] {
	return entryRecords[coord.ModelCheck, coord.ModelCheck]{
		stamp: func(v coord.ModelCheck, version uint64) coord.ModelCheck {
			v.StartedAt, v.Deadline, v.EndedAt = v.StartedAt.UTC(), v.Deadline.UTC(), v.EndedAt.UTC()
			v.Version = version
			return v
		},
		update: func(v *coord.ModelCheck, _ string, _ bool) coord.ModelCheck {
			if v == nil {
				return coord.ModelCheck{}
			}
			return *v
		},
	}
}

// ModelCheck reads one entry's check.
func (f *Fleet) ModelCheck(_ context.Context, entry string) (coord.ModelCheck, bool, error) {
	if entry == "" {
		return coord.ModelCheck{}, false, errNoEntry
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.modelChecks.records[entry]
	return c, ok, nil
}

// ListModelChecks returns every check, ordered by entry key.
func (f *Fleet) ListModelChecks(context.Context) ([]coord.ModelCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modelChecks.sortedLocked(), nil
}

// CreateModelCheck writes a check for an entry that has no record.
func (f *Fleet) CreateModelCheck(_ context.Context, c coord.ModelCheck) (coord.ModelCheck, bool, error) {
	if err := c.Validate(); err != nil {
		return coord.ModelCheck{}, false, err
	}
	if err := holdRecord(coord.ModelCheckKey(c.Entry), c, coord.MaxModelCheckBytes); err != nil {
		return coord.ModelCheck{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.modelChecks.records[c.Entry]; exists {
		return coord.ModelCheck{}, false, nil
	}
	return f.modelChecks.storeLocked(f, c.Entry, c), true, nil
}

// UpdateModelCheck writes a check at the version it was read at.
func (f *Fleet) UpdateModelCheck(_ context.Context, c coord.ModelCheck) (coord.ModelCheck, bool, error) {
	if err := c.Validate(); err != nil {
		return coord.ModelCheck{}, false, err
	}
	if err := holdRecord(coord.ModelCheckKey(c.Entry), c, coord.MaxModelCheckBytes); err != nil {
		return coord.ModelCheck{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.modelChecks.records[c.Entry]
	if !ok || c.Version == 0 || held.Version != c.Version {
		return coord.ModelCheck{}, false, nil
	}
	return f.modelChecks.storeLocked(f, c.Entry, c), true, nil
}

// DeleteModelCheck removes an entry's check at a version.
func (f *Fleet) DeleteModelCheck(_ context.Context, entry string, version uint64) (bool, error) {
	if entry == "" {
		return false, errNoEntry
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.modelChecks.records[entry]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.modelChecks.removeLocked(entry)
	return true, nil
}
