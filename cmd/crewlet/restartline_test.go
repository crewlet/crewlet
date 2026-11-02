package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RESTART THE LINE ASKS FOR IS THE CURE, HOWEVER STALE THE FILE.
//
// A revision that moves a native backend says native_backend_needs_restart and
// asks for a restart. That is the cure only if the restarted node BOOTS on the
// revision: one that booted on its stale -company file built its runtime from
// the file, applied the store's revision at its first tick, and said the line
// again — at every restart, for ever. So the node here restarts exactly as
// `crewlet run crewlet.yaml -company company.yaml` does, with the live switch
// in its store and the original file on its command line, and its first tick
// says nothing.
//
// THE CONTROL boots the same node on the stale file, as `crewlet run` used
// to, and hears the line at that first tick: without it, a first tick that
// said nothing for any reason would pass here.
//
// NOT PARALLEL: it swaps the process-wide logger to hear the engine's line.
// See [TestMain].
func TestARestartAfterALiveSwitchSaysNothingAtTheFirstTick(t *testing.T) {
	logs := &jsonLines{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelError+1, logging.FormatText, io.Discard) })

	for _, tc := range []struct {
		name     string
		fromFile bool
		want     int
	}{
		{"booted as crewlet run boots it", false, 0},
		{"booted on the stale file", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := filepath.Join(dir, "crewlet.yaml")
			tierA := fmt.Sprintf("node:\n  id: cli-test\nstore:\n  path: %s\n"+
				"stream:\n  store_dir: %s\napi:\n  port: 0\n",
				filepath.Join(dir, "crewlet.db"), filepath.Join(dir, "stream")) + fixtureKeyring
			if err := os.WriteFile(cfg, []byte(tierA), 0o600); err != nil {
				t.Fatal(err)
			}
			boot, err := config.LoadBootstrap(cfg, config.EnvOnly())
			if err != nil {
				t.Fatalf("load Tier A: %v", err)
			}
			seedEngineStore(t, boot)

			// THE LIVE SWITCH: the store's revision runs Worklet, which the
			// file the node was started with did not.
			payload, err := json.Marshal(parse(t, companyYAML))
			if err != nil {
				t.Fatal(err)
			}
			serveStored(t, cfg, string(payload))
			stale := parse(t, companyYAML+"tracker:\n  backend: none\n")

			// THE DECISION `crewlet run` boots on ([bootEngine]), and the
			// options it hands the engine.
			plan, err := decideOn(t, boot, seedOf(stale), cfg, quiet())
			if err != nil {
				t.Fatalf("decide the boot: %v", err)
			}
			opts, err := bootOptions(boot, plan)
			if err != nil {
				t.Fatalf("bootOptions: %v", err)
			}
			if tc.fromFile {
				opts.Company, opts.ActivatedAt = stale, time.Time{}
			}
			opts.Mode = statelog.ModeNormal
			e, err := engine.New(t.Context(), opts)
			if err != nil {
				t.Fatalf("engine.New: %v", err)
			}
			t.Cleanup(func() { e.Stop(context.Background()) })

			before := logs.count(t, "native_backend_needs_restart")
			if _, err := startReconciler(t.Context(), e, boot, plan, fixtureCipher, quiet()); err != nil {
				t.Fatalf("startReconciler: %v", err)
			}
			if got := logs.count(t, "native_backend_needs_restart") - before; got != tc.want {
				t.Errorf("the first tick said %d restart lines, want %d", got, tc.want)
			}
		})
	}
}

// jsonLines is the process logger's sink for a case that reads what the
// engine said: JSON records, one per line, safe to write from the engine's
// goroutines while the case reads.
type jsonLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *jsonLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// count is how many records written so far carry msg.
func (l *jsonLines) count(t *testing.T, msg string) int {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not JSON: %q: %v", line, err)
		}
		if record["msg"] == msg {
			n++
		}
	}
	return n
}
