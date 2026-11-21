package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RESTART THE LINE ASKS FOR IS THE CURE, HOWEVER STALE THE FILE.
//
// A revision that moves a native backend says native_backend_needs_restart. That
// cures only if the restarted node BOOTS on the revision: one booted on its stale
// -company file applied the store's revision at its first tick and said the line
// again, at every restart. So the node restarts as
// `crewlet run crewlet.yaml -company company.yaml` does, and its first tick says
// nothing. THE CONTROL boots it on the stale file and hears the line, so a silent
// tick for another reason cannot pass; it is also the node a live switch leaves
// behind, whose tracker is refused as `native_backend_needs_restart`. Mutation:
// answer [queries.ErrUnknown] for every absent half (internal/api's
// `nativeAbsent`), and the control fails. NOT PARALLEL: it swaps the logger.
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
			dir := brokertest.TempDir(t)
			cfg := filepath.Join(dir, "crewlet.yaml")
			tierA := fmt.Sprintf("node:\n  id: cli-test\nstore:\n  path: %s\n", filepath.Join(dir, "crewlet.db")) +
				fixtureStream(dir) + "api:\n  port: 0\n" + fixtureKeyring
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

			// AND WHAT THE LINE SAYS TO THE LOG, THE NODE SAYS TO ITS
			// READERS: the service is named as awaiting a restart, which the
			// org projection carries, and a question about it is refused
			// under the line's own code with no wait, rather than as a
			// tracker this company does not run. A node that serves it
			// names nothing and answers.
			awaiting := e.AwaitingRestart(e.Company().Config)
			kept := api.NativeSources(e).Work.(queries.Keeper).Kept()
			if !tc.fromFile {
				if len(awaiting) != 0 || kept != nil {
					t.Errorf("a node serving Worklet awaits %v and answers its "+
						"questions %v, want neither", awaiting, kept)
				}
				return
			}
			if !slices.Equal(awaiting, []string{"tracker"}) {
				t.Errorf("the node awaits a restart for %v, want the tracker alone", awaiting)
			}
			if !errors.Is(kept, queries.ErrUnavailable) || errors.Is(kept, queries.ErrUnknown) {
				t.Fatalf("a Worklet question on a node built without it is %v, want "+
					"unavailable and never unknown", kept)
			}
			said := stream.UnavailableOf(kept)
			if said.Refusal != string(httpjson.CodeNativeBackendNeedsRestart) ||
				said.RetryAfter != 0 || !strings.Contains(said.Detail, "Worklet") {
				t.Errorf("the refusal reads %+v, want native_backend_needs_restart "+
					"naming Worklet with no wait", said)
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
