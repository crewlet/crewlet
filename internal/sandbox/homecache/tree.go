package homecache

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// scanChunk is how much of a file the scan holds at once: a home's files run
// to hundreds of megabytes (a harness's binary), never read whole.
const scanChunk = 1 << 20

// linkPrefix names a link being swapped into a tree, beside the objects.
const linkPrefix = ".link-"

// publish makes home key's tree: the box's own paths dropped, every credential
// value looked for, identical files shared, and the whole renamed into place.
func (c *Cache) publish(home string, key Key, secrets []string) error {
	for _, rel := range c.exclude {
		if err := removeTree(filepath.Join(home, rel)); err != nil {
			return fmt.Errorf("homecache: dropping %s from a built home: %w", rel, err)
		}
	}
	needles := needlesOf(secrets)
	var files []string
	var dirs []dirMode
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(home, path)
		if err != nil || rel == "." {
			return err
		}
		if containsAny([]byte(rel), needles) {
			// The name itself holds the value, so only its directory is said.
			return &secretAt{path: filepath.Join(filepath.Dir(rel), "(a name in it)")}
		}
		switch t := d.Type(); {
		case t.IsDir():
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			dirs = append(dirs, dirMode{rel: rel, mode: info.Mode().Perm()})
			return nil
		case t&fs.ModeSymlink != 0:
			target, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			if containsAny([]byte(target), needles) {
				return &secretAt{path: rel}
			}
			return nil
		case t.IsRegular():
			files = append(files, rel)
			return nil
		default:
			// A socket, pipe or device a process left: nothing a copy can make.
			return os.Remove(path)
		}
	})
	if err != nil {
		if se := (*secretAt)(nil); errors.As(err, &se) {
			return se
		}
		return fmt.Errorf("homecache: reading a built home: %w", err)
	}
	// A shared file replaces its copy by a rename into its directory, which a
	// directory without write permission (Go's module cache) refuses: each is
	// opened for the sharing and given its mode back after.
	for _, d := range dirs {
		if err = os.Chmod(filepath.Join(home, d.rel), d.mode|0o700); err != nil {
			return fmt.Errorf("homecache: opening %s of a built home: %w", d.rel, err)
		}
	}
	for _, rel := range files {
		path := filepath.Join(home, rel)
		name, hit, derr := digest(path, needles)
		if derr != nil {
			return fmt.Errorf("homecache: reading %s of a built home: %w", rel, derr)
		}
		if hit {
			return &secretAt{path: rel}
		}
		if err = c.share(path, name); err != nil {
			return err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err = os.Chmod(filepath.Join(home, dirs[i].rel), dirs[i].mode); err != nil {
			return fmt.Errorf("homecache: closing %s of a built home: %w", dirs[i].rel, err)
		}
	}
	if err = os.Rename(home, c.tree(key)); err != nil {
		return fmt.Errorf("homecache: publishing %s: %w", key, err)
	}
	return nil
}

// dirMode is a directory of a home and its permissions.
type dirMode struct {
	rel  string
	mode fs.FileMode
}

// removeTree removes the tree at path, directories without write permission
// included, which os.RemoveAll cannot empty.
func removeTree(path string) error {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// minSecretBytes is the shortest value looked for as a credential: the floor
// of the kept sessions' scrub (internal/sandbox/executor's minScrubBytes), for
// its reason. A shorter value (a port, a version, a word) is as likely to be
// in a harness's binary as a secret is, and would cut every key it touched.
const minSecretBytes = 8

// secretAt is a build that left a credential value at path, relative to the
// home: never the value.
type secretAt struct{ path string }

func (e *secretAt) Error() string {
	return fmt.Sprintf("homecache: a build left a credential value at %s in the home", e.path)
}

// needlesOf is every value of secrets at least [minSecretBytes] long, as bytes.
func needlesOf(secrets []string) [][]byte {
	var needles [][]byte
	for _, s := range secrets {
		if len(s) >= minSecretBytes {
			needles = append(needles, []byte(s))
		}
	}
	return needles
}

func containsAny(b []byte, needles [][]byte) bool {
	for _, n := range needles {
		if bytes.Contains(b, n) {
			return true
		}
	}
	return false
}

// digest reads the file at path once: its object name (its content's digest
// and its permissions, which every hard link of it shares) and whether it
// holds one of needles, across chunk boundaries too.
func digest(path string, needles [][]byte) (string, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", false, err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	keep := 0
	for _, n := range needles {
		keep = max(keep, len(n)-1)
	}
	h := sha256.New()
	buf := make([]byte, scanChunk)
	window := make([]byte, 0, scanChunk+keep)
	var tail []byte
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			window = append(append(window[:0], tail...), buf[:n]...)
			if containsAny(window, needles) {
				return "", true, nil
			}
			tail = append(tail[:0], window[max(0, len(window)-keep):]...)
		}
		if errors.Is(rerr, io.EOF) {
			break
		} else if rerr != nil {
			return "", false, rerr
		}
	}
	name := hex.EncodeToString(h.Sum(nil)) + "-" + strconv.FormatUint(uint64(info.Mode().Perm()), 8)
	return name, false, nil
}

// share makes the file at path one inode with every other tree's file of the
// same object name: linked in as that object, or replaced by a link to it.
// A file this user may not link (fs.protected_hardlinks) stays as it is.
func (c *Cache) share(path, name string) error {
	obj := filepath.Join(c.objects(), name)
	for {
		err := os.Link(path, obj)
		switch {
		case err == nil, errors.Is(err, fs.ErrPermission):
			return nil
		case !errors.Is(err, fs.ErrExist):
			return fmt.Errorf("homecache: sharing %s: %w", path, err)
		}
		tmp := filepath.Join(c.objects(), linkPrefix+rand.Text())
		err = os.Link(obj, tmp)
		if errors.Is(err, fs.ErrNotExist) {
			continue // collected since: this file becomes the object
		} else if err != nil {
			return fmt.Errorf("homecache: sharing %s: %w", path, err)
		}
		err = os.Rename(tmp, path)
		// A rename onto another name of the same inode does nothing, and
		// leaves tmp: a home holding the same file twice under two names.
		_ = removeMissingOK(tmp)
		if err != nil {
			return fmt.Errorf("homecache: sharing %s: %w", path, err)
		}
		return nil
	}
}

// sweepObjects removes every object no tree holds: one with no name but its
// own. A link a publish is swapping in is left; one a crash left keeps its
// object, which costs that object's bytes.
func (c *Cache) sweepObjects() error {
	entries, err := os.ReadDir(c.objects())
	if err != nil {
		return fmt.Errorf("homecache: listing %s: %w", c.objects(), err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), linkPrefix) {
			continue
		}
		info, ierr := e.Info()
		if errors.Is(ierr, fs.ErrNotExist) {
			continue
		} else if ierr != nil {
			return fmt.Errorf("homecache: reading %s: %w", e.Name(), ierr)
		}
		if linksOf(info) == 1 {
			if rerr := removeMissingOK(filepath.Join(c.objects(), e.Name())); rerr != nil {
				return fmt.Errorf("homecache: removing %s: %w", e.Name(), rerr)
			}
		}
	}
	return nil
}

// linksOf is the file's link count; a platform whose stat gives none is taken
// to have another name, so nothing is swept on a guess.
func linksOf(info fs.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Nlink)
	}
	return 2
}

// copyTree copies the tree at src into dst, a box's home: files, directories
// and links, with their permissions and times. Within dst through [os.Root],
// so no link already in dst reaches past it.
func copyTree(src, dst string) error {
	root, oerr := os.OpenRoot(dst)
	if oerr != nil {
		return fmt.Errorf("homecache: opening %s: %w", dst, oerr)
	}
	defer func() { _ = root.Close() }()
	type dir struct {
		rel  string
		mode fs.FileMode
		at   time.Time
	}
	var dirs []dir
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if merr := root.Mkdir(rel, 0o700); merr != nil && !errors.Is(merr, fs.ErrExist) {
				return merr
			}
			dirs = append(dirs, dir{rel: rel, mode: info.Mode().Perm(), at: info.ModTime()})
			return nil
		case d.Type()&fs.ModeSymlink != 0:
			target, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			if rerr := root.Remove(rel); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				return rerr
			}
			return root.Symlink(target, rel)
		default:
			return copyFile(root, path, rel, info)
		}
	})
	if err != nil {
		return fmt.Errorf("homecache: copying a home into %s: %w", dst, err)
	}
	// Deepest first, and after their contents: a directory without write
	// permission takes no file, and writing into one moves its time.
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if jerr := errors.Join(root.Chmod(d.rel, d.mode), root.Chtimes(d.rel, d.at, d.at)); jerr != nil {
			return fmt.Errorf("homecache: copying a home into %s: %w", dst, jerr)
		}
	}
	return nil
}

// copyFile copies one file; io.Copy between two files is copy_file_range on
// Linux, which shares the blocks where the filesystem can (XFS, btrfs).
func copyFile(root *os.Root, path, rel string, info fs.FileInfo) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, in)
	if err = errors.Join(cerr, out.Close()); err != nil {
		return err
	}
	return errors.Join(root.Chmod(rel, info.Mode().Perm()), root.Chtimes(rel, info.ModTime(), info.ModTime()))
}
