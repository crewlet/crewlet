package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// How old this node's newest backup is, read from the artefacts it wrote.
//
// # It feeds one gauge, and neither the trim nor the alarm
//
// These functions answer `crewlet.backup.age`, which a node publishes for the
// copies IT took: the manifests in the directories the positions register says
// this node announced. The trim's backup term and the `backup_age` alarm do not
// read them. Both read the register instead — per log, the newest verified
// point the operator's policy counts that covers it, aged from when the copy
// was TAKEN ([coord.NewestBackupOf]) — because the node holding the trim duty
// is rarely the node that took the copy, and can never see a directory on
// another host.
//
// # The gauge reads the disk because the register cannot say what it says
//
// The register records that a backup was announced; the directory records that
// one still EXISTS. Those differ in exactly the cases an operator watching
// the gauge wants caught — a copy announced and then deleted, a volume that
// was never mounted, a schedule pointing at a path nobody ships from — and in
// every one of them the register, and so the trim and the alarm, still count
// the copy while the disk says it is gone. So the two answer different
// questions and can disagree: the alarm says how old the backup the trim
// counts is, per log and fleet-wide; the gauge says how old the newest copy on
// this node's own disk is.
//
// A MANIFEST IS THE CLAIM, so a directory without one contributes nothing: it
// is the debris of a run that did not finish, and reading its mtime as a
// backup would be the gauge silenced by a failure.

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
		// A path that does not exist yet is a node with no backup
		// rather than a fault: it is what every deployment looks like
		// before its first run, and refusing here would make the gauge
		// unreadable exactly when its absence is most true.
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
// facts are three different things and the gauge treats them differently: a
// node with no backup at all publishes no series, a node whose newest is old
// publishes its age, and a path that could not be read is logged and skipped.
// Collapsing the first two would give a node that never backed up an age
// somebody alerts on as a real one, and collapsing the third into either would
// let an unreadable volume read as a protected node.
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
	// THE FINISHING INSTANT, not the start. What the gauge asks is how
	// long this node has gone without a copy on its disk, and a backup
	// protects nothing until its manifest is written.
	if m.FinishedAt.IsZero() {
		return time.Time{}, false, nil
	}
	return m.FinishedAt, true, nil
}
