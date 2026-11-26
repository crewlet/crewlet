package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/nativelib"
	"github.com/crewlet/crewlet/internal/speech"
)

// runSpeech serves `crewlet speech prepare`: the dictation recognizer (a
// platform's library and the model) downloaded and verified into a directory,
// for the container image to bake in and for a host with no route to the
// releases to be handed.
func runSpeech(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "prepare" {
		fmt.Fprintln(stderr, "usage: crewlet speech prepare -dir DIR [-platform GOOS/GOARCH]")
		return errors.New("speech: expected `prepare`")
	}
	fs := flag.NewFlagSet("speech prepare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "directory to write the recognizer into (required); name it in "+
		"dictation.recognizer_dir, or bake it beside the binary under "+speech.BesideBinary)
	platform := fs.String("platform", runtime.GOOS+"/"+runtime.GOARCH,
		"the platform whose library to fetch: "+strings.Join(speech.Platforms(), ", "))
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *dir == "" || fs.NArg() != 0 {
		fs.Usage()
		return errors.New("speech prepare: -dir is required and takes no arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	began := time.Now()
	fmt.Fprintf(stderr, "preparing the %s recognizer in %s\n", *platform, *dir)
	last := -1
	var read int64
	err := speech.Prepare(ctx, *dir, *platform, func(done, total int64) {
		read = done
		if pct := int(done * 100 / max(total, 1)); pct/10 != last/10 {
			last = pct
			fmt.Fprintf(stderr, "  %d%% of %d MB\n", pct, total>>20)
		}
	})
	if err != nil {
		return fmt.Errorf("speech prepare: %w", err)
	}
	fmt.Fprintf(stdout, "prepared the %s recognizer in %s (%d MB read in %s)\n",
		*platform, *dir, read>>20, time.Since(began).Round(time.Second))
	return nil
}

// speechDataDir is where a node keeps a model it fetched: beside its
// database, on the data volume; a node with no database file keeps it in
// the native-library cache.
func speechDataDir(storePath string) string {
	if storePath == "" || strings.HasPrefix(storePath, ":memory:") {
		return filepath.Join(nativelib.CacheRoot(), "crewlet-speech")
	}
	return filepath.Dir(storePath)
}

// dictationRecognizer is the recognizer /ws/dictation serves on this node, or
// nil where `dictation.enabled` is false.
func dictationRecognizer(boot *config.Bootstrap) speech.Streamer {
	if !boot.Dictation.On() {
		return nil
	}
	return speech.NewLocal(speech.LocalConfig{
		Dir:      boot.Dictation.RecognizerDirFor(boot.Store),
		DataDir:  speechDataDir(boot.Store.Path),
		Capacity: boot.Dictation.MaxConcurrent,
	})
}
