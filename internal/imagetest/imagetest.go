// Package imagetest provides the container image a test case runs, once a
// process ([Provide]), defined by the text of its Dockerfile and taken
// cheapest first: the image the runtime holds under its tag; else the tar a
// cache directory keeps, loaded; else one built, then saved to that directory.
// CI restores a cache before the suite and saves it after, so a run needs no
// package mirror (ci.yml's `test` job says why images rather than archives).
//
// THE TAG NAMES THE TEXT: an image's tag carries a hash of its Dockerfile, so
// an image held or kept under it was built from exactly that text, and a
// changed Dockerfile builds afresh. A caller names in the text whatever else
// defines the image (a base's digest, a package's version, a lock's hash). A
// context directory is never hashed: its files carry whatever times and modes
// a checkout wrote, and the caller already holds a digest of what it copies.
//
// THE CACHE IS ONLY A SHORTCUT: a tar that will not load is built over, and a
// save that fails is logged, never a failure. A tar is written whole or not at
// all, so a directory that holds one holds the image.
//
// It needs a usable docker or podman. Where neither answers, [Require] skips
// the case, and internal/skipgate declares each such skip as an environment
// one: CI's runners have docker, so the cases run there.
package imagetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The bounds on the runtime: providing an image may build it, installing
// packages over the network, while `info` answers from a live daemon in well
// under a second, so twenty seconds tells a daemon that hangs from a busy one.
const (
	buildTimeout = 10 * time.Minute
	infoTimeout  = 20 * time.Second
)

// An Image is a container image a case runs.
type Image struct {
	// Repo is its tag's repository, which names the package it serves:
	// crewlet-sshdtest.
	Repo string
	// Name is its tag's readable part, before the hash: a base image's
	// reference, ubuntu:22.04, reads ubuntu-22.04.
	Name string
	// Text is its Dockerfile, which names everything that defines it (the
	// package doc's "The tag names the text").
	Text string
	// Context is the directory the build copies files from, "" for none: the
	// build then reads Text alone, as its whole context.
	Context string
	// CacheEnv names the variable that names its cache directory, a tar per
	// image. Unset, as on a workstation, an image the runtime does not hold
	// is built.
	CacheEnv string
}

// Tag is the image's tag, <Repo>:<Name>-<hash>: each ':' and '/' of Name a
// '-', and the hash one of Text.
func (i Image) Tag() string { return i.Repo + ":" + i.label() }

// Tar is where the cache directory dir keeps the image, a file named as the
// tag is less its repository: a tar kept for other text is never loaded.
func (i Image) Tar(dir string) string { return filepath.Join(dir, i.label()+".tar") }

// label is the tag's part after the repository.
func (i Image) label() string {
	sum := sha256.Sum256([]byte(i.Text))
	return strings.NewReplacer(":", "-", "/", "-").Replace(i.Name) + "-" + hex.EncodeToString(sum[:6])
}

// runtimeName is the container runtime that answers, found once a process:
// docker, else podman, else "" for none. On PATH is not enough, since a daemon
// can be installed and unreachable.
var runtimeName = sync.OnceValue(func() string {
	for _, name := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(name); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), infoTimeout)
		err := exec.CommandContext(ctx, name, "info").Run()
		cancel()
		if err == nil {
			return name
		}
	}
	return ""
})

// Require skips t where no container runtime answers, and answers the one that
// does. A case that runs subtests calls it before them, so the skip is of the
// case internal/skipgate declares, never of a subtest nothing declares.
func Require(t testing.TB) string {
	t.Helper()
	name := runtimeName()
	if name == "" {
		t.Skip("imagetest: needs a usable docker or podman")
	}
	return name
}

// The tags provided in this process.
var (
	providing sync.Mutex
	provided  = map[string]bool{}
)

// Provide answers img's tag once the runtime holds it, provided at most once a
// process, cheapest first ([provide]). It skips t where no runtime answers
// ([Require]), and fails it where the image could not be built; a later call
// tries again.
func Provide(t testing.TB, img Image) string {
	t.Helper()
	name := Require(t)
	tag := img.Tag()
	providing.Lock()
	defer providing.Unlock()
	if provided[tag] {
		return tag
	}
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	if err := provide(ctx, runtimeOf(name), img, os.Getenv(img.CacheEnv), t.Logf); err != nil {
		t.Fatal(err)
	}
	provided[tag] = true
	return tag
}

// runtime is the container runtime by its name, and how one of its commands
// runs: stdin from in where non-nil, answering the combined output.
type runtime struct {
	name string
	run  func(ctx context.Context, in io.Reader, args ...string) ([]byte, error)
}

func runtimeOf(name string) runtime {
	return runtime{name: name, run: func(ctx context.Context, in io.Reader, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		if in != nil {
			cmd.Stdin = in
		}
		return cmd.CombinedOutput()
	}}
}

// provide makes rt hold img, cheapest first: the image it holds under the tag;
// else the one cacheDir keeps, loaded; else one built, then saved to cacheDir.
// With no cacheDir an image is held or built. A tar that will not load is
// built over, and a save that fails is said through logf, never a failure.
func provide(ctx context.Context, rt runtime, img Image, cacheDir string, logf func(string, ...any)) error {
	tag := img.Tag()
	if _, err := rt.run(ctx, nil, "image", "inspect", tag); err == nil {
		return nil
	}
	if cacheDir != "" {
		if _, err := os.Stat(img.Tar(cacheDir)); err == nil {
			out, err := rt.run(ctx, nil, "load", "-i", img.Tar(cacheDir))
			if err == nil {
				return nil
			}
			logf("imagetest: the cached %s did not load, so it is built: %v\n%s", tag, err, out)
		}
	}
	if err := build(ctx, rt, img); err != nil {
		return err
	}
	if cacheDir != "" {
		if err := save(ctx, rt, img, cacheDir); err != nil {
			logf("%v", err)
		}
	}
	return nil
}

// save writes img to the cache directory dir, whole or not at all: the runtime
// writes beside the tar, which a rename then takes, and a save that fails
// removes what it wrote, which nothing else would.
func save(ctx context.Context, rt runtime, img Image, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("imagetest: the image cache %s (%s): %w", dir, img.CacheEnv, err)
	}
	path := img.Tar(dir)
	next := path + ".next"
	defer func() { _ = os.Remove(next) }()
	if out, err := rt.run(ctx, nil, "save", "-o", next, img.Tag()); err != nil {
		return fmt.Errorf("imagetest: save %s to %s (%s): %w\n%s", img.Tag(), dir, img.CacheEnv, err, out)
	}
	if err := os.Rename(next, path); err != nil {
		return fmt.Errorf("imagetest: save %s to %s (%s): %w", img.Tag(), dir, img.CacheEnv, err)
	}
	return nil
}

// build builds img, answering an error that carries the build's whole output:
// a failed step's own messages, apt's among them, so a mirror that could not
// be reached reads differently from a package that is gone. The Dockerfile is
// read on stdin, as the whole context (`-`) or beside the directory (`-f -`).
//
// NEVER QUIET: docker's -q leaves only "exit code: 100" of a failed step.
// docker's BuildKit is asked for plain progress, which prints every step's
// output whether or not a terminal is attached; podman prints it unasked, and
// takes no such flag.
func build(ctx context.Context, rt runtime, img Image) error {
	args := []string{"build", "-t", img.Tag()}
	if rt.name == "docker" {
		args = append(args, "--progress=plain")
	}
	if img.Context == "" {
		args = append(args, "-")
	} else {
		args = append(args, "-f", "-", img.Context)
	}
	if out, err := rt.run(ctx, strings.NewReader(img.Text), args...); err != nil {
		return fmt.Errorf("imagetest: build %s: %w\n%s", img.Tag(), err, out)
	}
	return nil
}
