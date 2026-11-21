package boxhelper

import (
	"context"
	"fmt"
	"path"
)

// Writer puts a file into a box whole, replacing what was at its path: the
// consumer's view of sandbox.Sandbox's WriteFile, which every cell's box
// implements, so this package imports nothing of the sandbox's.
type Writer interface {
	WriteFile(ctx context.Context, path string, content []byte) error
}

// The lock's two files in the helper's folder (js/tree.mjs's MANIFEST and
// LOCK): names npm, which reads package.json, package-lock.json and
// npm-shrinkwrap.json, and Node, which reads package.json, never take as the
// folder's own.
const (
	manifestFile = "tree-package.json"
	lockFile     = "tree-package-lock.json"
)

// Write puts the helper into the box whose home is home, under [Folder], for
// a launch's settings s: every module in [Files] order, then the lock's two
// files, then config.json, which names the modules by [Version] and the lock
// by its digest, so a config never names files not yet written beside it.
// config.json is rendered first, so nothing is written for settings it
// refuses. Modes are the box's to fix: a box's own upload service gives a
// file its own.
func Write(ctx context.Context, w Writer, home string, s Settings) error {
	body, err := configJSON(s)
	if err != nil {
		return err
	}
	folder := Folder(home)
	for _, f := range Files() {
		if err := write(ctx, w, path.Join(folder, f.Name), f.Body); err != nil {
			return err
		}
	}
	if err := write(ctx, w, path.Join(folder, manifestFile), manifest); err != nil {
		return err
	}
	if err := write(ctx, w, path.Join(folder, lockFile), lock); err != nil {
		return err
	}
	return write(ctx, w, path.Join(folder, configFile), body)
}

// write is one WriteFile, its refusal wrapped so a caller can still tell it
// by errors.Is: a backend writing from the engine host refuses a path holding
// anything but a regular file with its own sentinel.
func write(ctx context.Context, w Writer, p string, body []byte) error {
	if err := w.WriteFile(ctx, p, body); err != nil {
		return fmt.Errorf("boxhelper: writing %s into the box: %w", p, err)
	}
	return nil
}
