package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ancestorRefusal is why a call of harness is not run where from, or a
// directory above it, holds one of names ([harnessprofile.Profile.AncestorFiles]):
// the harness would read it into the call's requests, and no marker in the
// call's directory stops it. From's real path is read, as the harness reads its
// own working directory, from its deepest existing part; a file a stat cannot
// reach is one the harness cannot read either. Nil where none is there.
func ancestorRefusal(harness, from string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	dir, err := existingRealPath(from)
	if err != nil {
		return fmt.Errorf("harness: %s's call directory %q cannot be resolved, so the files above it it would read "+
			"cannot be checked: give this entry a state_dir the engine can read: %w", harness, from, err)
	}
	for ; ; dir = filepath.Dir(dir) {
		for _, name := range names {
			if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
				return fmt.Errorf("harness: %s reads %s, above its call's directory, into every request "+
					"whatever its flags say, so the call is not run: set this entry's state_dir to a "+
					"directory with no %s above it, or remove that file",
					harness, filepath.Join(dir, name), strings.Join(names, ", "))
			}
		}
		if parent := filepath.Dir(dir); parent == dir {
			return nil
		}
	}
}

// existingRealPath is path's deepest part that exists, its links resolved.
func existingRealPath(path string) (string, error) {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return real, nil
		}
		if !errors.Is(err, fs.ErrNotExist) || filepath.Dir(p) == p {
			return "", err
		}
	}
}

// AncestorRefusal is why no call of this entry runs in its state directory:
// a file the harness reads from every directory above its call is above every
// call it makes there ([ancestorRefusal]). Nil for a harness that reads none,
// or where none is there. The per-call refusal stays for one that appears later.
func (p *Provider) AncestorRefusal() error {
	return ancestorRefusal(p.harness, p.ws.root, p.profile.AncestorFiles)
}
