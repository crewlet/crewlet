package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// refuseAncestorFiles refuses a call of harness in work where a directory above
// work holds one of names ([harnessprofile.Profile.AncestorFiles]): the harness
// would read it into the call's requests, and no marker in work stops it. Read
// from work's real path, as the harness reads its own working directory; a
// file a stat cannot reach is one the harness cannot read either.
func refuseAncestorFiles(harness, work string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	dir, err := filepath.EvalSymlinks(work)
	if err != nil {
		return fmt.Errorf("harness: resolving the call's directory %q: %w", work, err)
	}
	for dir = filepath.Dir(dir); ; dir = filepath.Dir(dir) {
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
