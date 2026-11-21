package boxhelpertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/shellword"
)

// Env is what a case of the helper runs under: a Node and the npm it brings.
type Env struct {
	// Node is the node the case runs the helper's modules under, the box's
	// bin/node.
	Node string
	// NPM is the npm the box brings, its bin/npm, run under the box's node as
	// a box's own npm is.
	NPM string
}

// EachNode runs f once under each Node a box may bring, each with its own
// npm, as the subtests newest, the machine's node and npm ([Node], [NPM]),
// and floor, the floor's pinned build and the npm it bundles ([Floor],
// [FloorNPM]): the names chromiumtest.Each gives its arms.
func EachNode(t *testing.T, f func(t *testing.T, env Env)) {
	t.Helper()
	t.Run("newest", func(t *testing.T) { f(t, Env{Node: Node(t), NPM: NPM(t)}) })
	t.Run("floor", func(t *testing.T) { f(t, Env{Node: Floor(t), NPM: FloorNPM(t)}) })
}

// Cache is the npm cache a box starts with.
type Cache int

const (
	// CacheSuite is a copy of the npm cache the suite's tree filled
	// ([Tree]), at the box's ~/.npm, so the box installs the tree offline.
	CacheSuite Cache = iota
	// CacheNone is an empty cache, so npm fetches every package from the
	// registry the case names.
	CacheNone
)

// Options is how a case's box differs from what the engine writes.
type Options struct {
	// Cache is the npm cache the box starts with.
	Cache Cache
	// Config holds config.json's keys the case sets over the engine's values,
	// merged into objects key by key: a bound shortened where the case's
	// subject is that bound.
	Config map[string]any
}

// Box is a seat's box on disk, as the engine leaves one before the helper
// first runs in it: the helper written, and the lock's two files beside it.
type Box struct {
	// Home is the box's home.
	Home string
	// Folder is the helper's folder in it, boxhelper.Folder(Home).
	Folder string
	// GlobalNPMRC is the box's global npmrc, which its environment names,
	// empty, so the machine's own never reaches the box: where a case writes
	// what an image's global npmrc would say.
	GlobalNPMRC string

	env  Env
	opts Options
}

// homePrefix starts the name of every home Start makes: short, since a box's
// sockets lie under its home and macOS bounds a socket's path.
const homePrefix = "cb"

// ownerFile is a home's record of the test binary that made it.
const ownerFile = "owner"

// globalRC is the name of the box's global npmrc in its home, and npmCalls of
// where a stand-in npm records its command lines.
const (
	globalRC = ".npm-globalrc"
	npmCalls = "npm-calls"
)

// refusedRegistry is the registry a box's npm is pointed at unless a case
// names one: a port nothing listens on, so a box installs from its cache or
// fails in about 2.4 s, and never reads the network.
const refusedRegistry = "http://127.0.0.1:1/"

// self is this test binary, as every home it makes names it.
var self = sync.OnceValues(func() (procgroup.Leader, error) { return procgroup.Identify(os.Getpid()) })

// swept is the sweep of the temporary directory's homes, once a test binary.
var swept = sync.OnceValue(func() error { return sweep(os.TempDir()) })

// Start lays a box out on disk for a case run under env, as o says: a home of
// its own holding the box's bin directory (env's node and npm), its npm cache
// and an empty global npmrc, and the helper written into it ([Box.Write]).
// The case's end removes it.
func Start(t testing.TB, env Env, o Options) *Box {
	t.Helper()
	if err := swept(); err != nil {
		t.Logf("sweeping the homes of test binaries that are gone: %v", err)
	}
	owner, err := self()
	if err != nil {
		t.Fatalf("identifying this test binary, which its homes name: %v", err)
	}
	home, err := os.MkdirTemp("", homePrefix) //nolint:usetesting // a test's name is too long for the box's sockets
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Errorf("removing the box's home: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(home, ownerFile), []byte(owner.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &Box{Home: home, Folder: boxhelper.Folder(home), GlobalNPMRC: filepath.Join(home, globalRC), env: env, opts: o}
	b.layOut(t)
	b.Write(t)
	return b
}

// layOut makes the box's bin directory, linking its node and npm, its npm
// cache, and its empty global npmrc.
func (b *Box) layOut(t testing.TB) {
	t.Helper()
	bin := filepath.Join(b.Home, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	link(t, b.env.Node, filepath.Join(bin, "node"))
	link(t, b.env.NPM, filepath.Join(bin, "npm"))
	if b.opts.Cache == CacheSuite {
		if err := os.CopyFS(filepath.Join(b.Home, ".npm"), os.DirFS(filepath.Join(Tree(t), "npm-cache"))); err != nil {
			t.Fatalf("copying the suite tree's npm cache into the box: %v", err)
		}
	}
	if err := os.WriteFile(b.GlobalNPMRC, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// link links name to target, failing t.
func link(t testing.TB, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
}

// Write writes the helper into the box through [disk], as the engine does on
// every launch, then sets the case's config over the engine's.
func (b *Box) Write(t testing.TB) {
	t.Helper()
	if err := boxhelper.Write(t.Context(), disk{}, b.Home); err != nil {
		t.Fatal(err)
	}
	if len(b.opts.Config) > 0 {
		config := b.Config(t)
		merge(config, b.opts.Config)
		b.SetConfig(t, config)
	}
}

// merge sets over's keys in into, an object of one merged into an object of
// the other key by key, and anything else in its place.
func merge(into, over map[string]any) {
	for key, v := range over {
		sub, ok := v.(map[string]any)
		if have, isObject := into[key].(map[string]any); ok && isObject {
			merge(have, sub)
			continue
		}
		into[key] = v
	}
}

// Config is the box's config.json, decoded.
func (b *Box) Config(t testing.TB) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.Folder, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

// SetConfig writes config as the box's config.json, as the engine's writes
// leave it.
func (b *Box) SetConfig(t testing.TB, config map[string]any) {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := (disk{}).WriteFile(t.Context(), filepath.Join(b.Folder, "config.json"), append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
}

// Environ is the environment the box runs the helper in: its own bin
// directory alone on PATH, so its npm runs under its node and no case finds
// the machine's own programs; its home; the refused registry; and its empty
// global npmrc. A case's own variables go after it, where the last of a name
// wins.
func (b *Box) Environ() []string {
	return []string{"PATH=" + filepath.Join(b.Home, "bin"), "HOME=" + b.Home, "NPM_CONFIG_REGISTRY=" + refusedRegistry,
		"NPM_CONFIG_GLOBALCONFIG=" + b.GlobalNPMRC}
}

// StandInNPM puts a stand-in for npm at the box's bin/npm: an sh script that
// records each command line it is run with ([Box.NPMCalls]), answers one that
// carries --version with version, and otherwise runs body, where $REAL_NPM
// is the case's own npm and $PREFIX the directory --prefix names. The box's
// PATH is its bin alone, so the script finds the system's tools after it,
// and the real npm's `env node` still finds the box's node first.
func (b *Box) StandInNPM(t testing.TB, version, body string) {
	t.Helper()
	script := "#!/bin/sh\nPATH=\"$PATH:/usr/bin:/bin\"\nREAL_NPM=" + shellword.Quote(b.env.NPM) + "\n" +
		"printf '%s\\000' \"$@\" >>" + shellword.Quote(filepath.Join(b.Home, npmCalls)) + "\n" +
		"printf '\\n' >>" + shellword.Quote(filepath.Join(b.Home, npmCalls)) + "\n" +
		"for a; do [ \"$a\" = --version ] && { echo " + shellword.Quote(version) + "; exit 0; }; done\n" +
		"PREFIX= prev=\nfor a; do [ \"$prev\" = --prefix ] && PREFIX=$a; prev=$a; done\n" + body + "\n"
	npm := filepath.Join(b.Home, "bin", "npm")
	if err := os.Remove(npm); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(npm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// NPMCalls is each command line a stand-in npm was run with, in order, its
// arguments after the executable.
func (b *Box) NPMCalls(t testing.TB) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.Home, npmCalls))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for line := range bytes.Lines(raw) {
		args := strings.Split(strings.TrimSuffix(string(line), "\n"), "\x00")
		calls = append(calls, args[:len(args)-1])
	}
	return calls
}

// disk is a box's own upload service, on this machine: each file staged
// beside its path and renamed over it, at the loose modes such a service may
// leave, 0644 files in 0755 directories, which the helper holds to its own.
type disk struct{}

// WriteFile implements boxhelper.Writer.
func (disk) WriteFile(ctx context.Context, path string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	staged, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged.Name()) }()
	_, err = staged.Write(content)
	if err == nil {
		err = staged.Chmod(0o644)
	}
	if closeErr := staged.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(staged.Name(), path)
}

// sweep removes every home in dir whose test binary is gone, and reports the
// homes it could not judge or remove.
func sweep(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), homePrefix) {
			continue
		}
		home := filepath.Join(dir, entry.Name())
		owner, ok := ownerOf(home)
		if !ok {
			continue
		}
		running, err := alive(owner)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", home, err))
		case !running:
			if err := os.RemoveAll(home); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// ownerBound is the longest owner file a home holds: a pid and a start time
// take under 64 bytes, so a longer file is none Start wrote.
const ownerBound = 256

// ownerOf is the test binary home's owner file names, where it names one.
func ownerOf(home string) (procgroup.Leader, bool) {
	file := filepath.Join(home, ownerFile)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > ownerBound {
		return procgroup.Leader{}, false
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return procgroup.Leader{}, false
	}
	owner, err := procgroup.ParseLeader(strings.TrimSpace(string(raw)))
	return owner, err == nil
}

// alive is whether the test binary owner names still runs: the kernel reports
// its pid with the start time recorded, and not exited.
func alive(owner procgroup.Leader) (bool, error) {
	proc, found, err := procgroup.Inspect(owner.PID)
	if err != nil {
		return false, err
	}
	return found && proc.Start == owner.Start && !proc.Zombie, nil
}
