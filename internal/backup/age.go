package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// How old the newest backup is, and why it is read from the artefacts.
//
// # The alarm needs a number nobody can be wrong about
//
// The trim refuses to advance while the newest backup is older than the
// operator's policy — a company that never backs up never trims, loudly and
// by design — and the alarm beside it says so. Both need one number: how long
// ago did a COMPLETE backup finish.
//
// It is derived from the artefacts on disk rather than from a counter the
// engine keeps, and that is the decision. A counter records that this process
// believed it took a backup; the directory records that one EXISTS. Those
// differ in exactly the cases the alarm is for — a copy that was taken and
// then deleted, a volume that was never mounted, a schedule pointing at a path
// nobody ships from — and in every one of them the counter says the fleet is
// protected and the disk says it is not.
//
// A MANIFEST IS THE CLAIM, so a directory without one contributes nothing: it
// is the debris of a run that did not finish, and reading its mtime as a
// backup would be the alarm silenced by a failure.

// Newest is the finishing instant of the newest complete backup under root,
// reporting false when there is none.
//
// It looks at root itself and at ONE level of subdirectories beneath it,
// because those are the two shapes an operator's schedule produces: a single
// destination overwritten each run, and a dated directory per run under a
// parent. It does not walk deeper — a backup directory holds a `streams`
// subdirectory of its own, and a walk that recursed would find manifests by
// depth rather than by meaning.
func Newest(root string) (time.Time, bool, error) {
	if root == "" {
		return time.Time{}, false, nil
	}
	newest, found, err := finishedAt(root)
	if err != nil {
		return time.Time{}, false, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		// A path that does not exist yet is a fleet with no backup
		// rather than a fault: it is what every deployment looks like
		// before its first run, and refusing here would make the alarm
		// unreadable exactly when it is most true.
		return newest, found, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("backup: read %s: %w", root, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		at, ok, err := finishedAt(filepath.Join(root, entry.Name()))
		if err != nil {
			return time.Time{}, false, err
		}
		if ok && at.After(newest) {
			newest, found = at, true
		}
	}
	return newest, found, nil
}

// Age is how long ago the newest complete backup under root finished.
//
// IT ANSWERS (value, ok, error) rather than a duration, because the three
// facts are three different things and the alarm treats them differently: a
// fleet with no backup at all, a fleet whose newest is old, and a path that
// could not be read. Collapsing the first two would make an unconfigured
// deployment page as a failing one, and collapsing the third into either would
// let an unreadable volume read as a healthy fleet.
func Age(root string, now time.Time) (time.Duration, bool, error) {
	at, ok, err := Newest(root)
	if err != nil || !ok {
		return 0, false, err
	}
	if age := now.Sub(at); age > 0 {
		return age, true, nil
	}
	// A MANIFEST FROM THE FUTURE IS AGE ZERO rather than a negative
	// duration: clocks differ between the host that took a backup and the
	// host reading it, and a negative age would compare below every
	// threshold and read as the freshest backup imaginable.
	return 0, true, nil
}

// finishedAt reads one directory's manifest.
func finishedAt(dir string) (time.Time, bool, error) {
	body, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("backup: read the manifest in %s: %w", dir, err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		// RAISED, NOT SKIPPED. A manifest that does not decode is the
		// one case where "there is no backup here" and "there is a
		// backup I cannot read" have opposite remedies, and only one of
		// them is the operator's.
		return time.Time{}, false, fmt.Errorf("backup: the manifest in %s does "+
			"not decode, so whether this fleet holds a backup cannot be "+
			"established: %w", dir, err)
	}
	// THE FINISHING INSTANT, not the start. What the alarm asks is how
	// long the fleet has been unprotected, and a backup protects nothing
	// until its manifest is written.
	if m.FinishedAt.IsZero() {
		return time.Time{}, false, nil
	}
	return m.FinishedAt, true, nil
}
