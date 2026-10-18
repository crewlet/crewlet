package engine

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// restartLineTrackerNone is [nativeCleanupCompany] with its tracker switched
// off: a revision that turns a native half the runtime was built with into
// one it was not.
const restartLineTrackerNone = nativeCleanupCompany + `
tracker:
  backend: none
`

// WHICH CHOICES WAIT FOR A RESTART: a half moving to or from native, and
// nothing else.
//
// The runtime is built around whether each half is the engine's own, so that
// is the one change it cannot take live. Moving between two vendors is not one
// — a vendor's parser is rebuilt by its own reconciler at the apply — and a
// line for it would tell an operator to restart a fleet for nothing.
func TestABackendSwitchSaysItWaitsForARestart(t *testing.T) {
	t.Parallel()
	native := builtWith{tracker: config.TrackerNative, knowledge: config.KnowledgeNative}
	jira := &config.Company{
		Tracker:      config.Tracker{Backend: config.TrackerJira},
		Integrations: config.Integrations{Jira: &config.Jira{}},
	}
	cases := []struct {
		name      string
		built     builtWith
		activated *config.Company
		want      []backendSwitch
	}{{
		name: "native tracker to jira", built: native, activated: jira,
		want: []backendSwitch{{
			Field: "tracker.backend", Half: "tracker",
			Running: "native", Activated: "jira", Holds: true,
		}},
	}, {
		// VENDOR TO VENDOR, which the runtime was never built around.
		name:      "jira to none",
		built:     builtWith{tracker: config.TrackerJira, knowledge: config.KnowledgeNative},
		activated: &config.Company{Tracker: config.Tracker{Backend: config.TrackerNone}},
	}, {
		// AN UNSET BACKEND IS ITS DERIVED ONE: no Confluence declared, so
		// the knowledge base is the engine's own, which a runtime built on
		// Confluence does not hold.
		name:      "confluence to a derived native",
		built:     builtWith{tracker: config.TrackerNative, knowledge: config.KnowledgeConfluence},
		activated: &config.Company{},
		want: []backendSwitch{{
			Field: "knowledge.backend", Half: "knowledge base",
			Running: "confluence", Activated: "native", Holds: false,
		}},
	}, {
		name: "both halves, tracker first",
		built: builtWith{
			tracker: config.TrackerNone, knowledge: config.KnowledgeNative,
		},
		activated: &config.Company{
			Knowledge: config.Knowledge{Backend: config.KnowledgeNone},
		},
		want: []backendSwitch{{
			Field: "tracker.backend", Half: "tracker",
			Running: "none", Activated: "native", Holds: false,
		}, {
			Field: "knowledge.backend", Half: "knowledge base",
			Running: "native", Activated: "none", Holds: true,
		}},
	}, {
		// THE CODE HOST TURNED ON: its one field is already whether the
		// half is native, so every change of it is a switch.
		name: "the code host turned on", built: native,
		activated: &config.Company{Code: config.Code{Native: true}},
		want: []backendSwitch{{
			Field: "code.native", Half: "code host",
			Running: "false", Activated: "true", Holds: false,
		}},
	}, {
		name: "the code host turned off, after the other halves",
		built: builtWith{
			tracker: config.TrackerNative, knowledge: config.KnowledgeConfluence, code: true,
		},
		activated: &config.Company{},
		want: []backendSwitch{{
			Field: "knowledge.backend", Half: "knowledge base",
			Running: "confluence", Activated: "native", Holds: false,
		}, {
			Field: "code.native", Half: "code host",
			Running: "true", Activated: "false", Holds: true,
		}},
	}, {
		name: "built as activated", built: native, activated: &config.Company{},
	}, {
		name:      "built as activated, hosting code",
		built:     builtWith{tracker: config.TrackerNative, knowledge: config.KnowledgeNative, code: true},
		activated: &config.Company{Code: config.Code{Native: true}},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := backendSwitches(tc.built, tc.activated)
			if !slices.Equal(got, tc.want) {
				t.Errorf("backendSwitches = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// THE LINE SAYS WHICH WAY THE NODE IS WRONG: a half the runtime holds is still
// served, and a half it lacks is not served at all — two different things for
// an operator reading why their revision did nothing.
func TestTheRestartLineSaysWhatTheNodeServesMeanwhile(t *testing.T) {
	t.Parallel()
	kept := backendSwitch{Field: "tracker.backend", Half: "tracker",
		Running: "native", Activated: "jira", Holds: true}.detail()
	if !strings.Contains(kept, "keeps serving it") || !strings.Contains(kept, "tracker.backend: jira") {
		t.Errorf("a half turned off reads %q: it must say the node keeps serving "+
			"it and name what the revision asked for", kept)
	}
	lacking := backendSwitch{Field: "knowledge.backend", Half: "knowledge base",
		Running: "confluence", Activated: "native", Holds: false}.detail()
	if !strings.Contains(lacking, "cannot serve it") ||
		!strings.Contains(lacking, "knowledge.backend: confluence") ||
		!strings.Contains(lacking, "knowledge.backend: native") {
		t.Errorf("a half turned on reads %q: it must say the node cannot serve it, "+
			"what it was built with and what the revision asked for", lacking)
	}
}

// A REVISION THAT MOVES A NATIVE BACKEND SAYS IT WAITS FOR A RESTART, once per
// activation, on the node it does not take effect on.
//
// The runtime is built once per process, so such a revision activated cleanly,
// reported ok on every node, and changed nothing: a node went on serving the
// native tracker's tools after a revision turned it off, with nothing anywhere
// saying why. Said at EVERY activation that still asks for the switch — a
// re-activation of the same document is the operator asking again, and it is
// still not served — and at none once a revision agrees with the runtime.
//
// AND ON A NODE WITH NO INBOUND EDGE: the line is about the runtime, never
// about the edge, and a maintenance-mode node starts none — so the line is
// said ahead of the parsers' swap, whose return on a missing edge would
// otherwise swallow it.
//
// NOT PARALLEL: it swaps the process-wide logger, which this package otherwise
// leaves at its boot default.
func TestTheRestartLineIsSaidAtAnActivation(t *testing.T) {
	logs := &stopLineBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	SeedStore(t, &b)
	booted := parseCompany(t, nativeCleanupCompany)
	if booted.TrackerBackendFor() != config.TrackerNative {
		t.Fatalf("the premise: the node boots on tracker.backend %q, want native",
			booted.TrackerBackendFor())
	}
	back, err := OpenBackends(t.Context(), &b, booted, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: booted, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	at := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	apply := func(doc string, at time.Time) {
		t.Helper()
		status, applied, err := e.Apply(t.Context(), parseCompany(t, doc), at)
		if err != nil || status != configplane.StatusOK {
			t.Fatalf("Apply = (%s, %v, %v), want ok", status, applied, err)
		}
	}

	apply(restartLineTrackerNone, at)
	lines := restartLines(t, logs)
	if len(lines) != 1 {
		t.Fatalf("a revision turning the native tracker off said %d restart lines, "+
			"want exactly 1: %v", len(lines), lines)
	}
	for key, want := range map[string]string{
		"field": "tracker.backend", "running": "native", "activated": "none",
	} {
		if got := lines[0][key]; got != want {
			t.Errorf("the restart line's %s = %v, want %q", key, got, want)
		}
	}
	if lines[0]["level"] != "WARN" {
		t.Errorf("the restart line is at %v, want WARN: a revision the node does "+
			"not serve is something an operator has to act on", lines[0]["level"])
	}

	// A LATER ACTIVATION OF THE SAME DOCUMENT is asked again, and is still
	// not served.
	apply(restartLineTrackerNone, at.Add(time.Minute))
	if got := len(restartLines(t, logs)); got != 2 {
		t.Errorf("a second activation of the same switch said %d lines in all, want 2", got)
	}

	// AND A REVISION BACK ON WHAT THE RUNTIME SERVES says nothing.
	apply(nativeCleanupCompany, at.Add(2*time.Minute))
	if got := len(restartLines(t, logs)); got != 2 {
		t.Errorf("a revision agreeing with the runtime said a restart line: %d in all, "+
			"want still 2", got)
	}

	// A NODE WITH NO INBOUND EDGE, which a maintenance-mode node is: its
	// constructor returns before the edge is started, and an apply starts
	// none there ([Engine.startInbound]). Through APPLY, the way a node meets
	// a revision, so a stage order that skipped the line on such a node is
	// caught here too.
	quiet, _ := aNodeInMode(t, statelog.ModeMaintenance)
	if quiet.inboundStarted() {
		t.Fatal("the premise: a maintenance-mode node started its inbound edge")
	}
	applyOK(t, quiet, restartLineTrackerNone)
	if quiet.inboundStarted() {
		t.Fatal("the premise: an apply in a maintenance mode started the inbound edge")
	}
	if got := len(restartLines(t, logs)); got != 3 {
		t.Errorf("a node with no inbound edge said %d restart lines in all, want 3: "+
			"the line belongs to the runtime, not to the edge", got)
	}
}

// restartLineVendorsOnly is [nativeCleanupCompany] on no native half at all:
// a company a node runs no native runtime for.
const restartLineVendorsOnly = restartLineTrackerNone + `knowledge:
  backend: none
`

// restartLineTrackerOnly turns the engine's own tracker on and nothing else.
const restartLineTrackerOnly = nativeCleanupCompany + `knowledge:
  backend: none
`

// A NODE WITH NO RUNTIME SAYS NOTHING, and the apply that gives it one says
// nothing either.
//
// The runtime an apply meets on such a node is the one its own `native` stage
// just started, from the revision being applied ([Engine.startNativeFor]), so
// the two agree by construction — and a line there would tell an operator to
// restart a node for a revision it is already serving. Once the runtime
// exists the line is live: a later revision turning the tracker off is said.
//
// NOT PARALLEL, for [TestTheRestartLineIsSaidAtAnActivation]'s reason.
func TestTheApplyThatStartsTheRuntimeSaysNothing(t *testing.T) {
	logs := &stopLineBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	e := newSandboxNode(t, parseCompany(t, restartLineVendorsOnly))
	if _, runs := e.runtimeBuilt(); runs {
		t.Fatal("the premise: a node booted on no native half runs a native runtime")
	}

	// STAYING ON THE VENDORS: nothing to build, nothing to say.
	applyOK(t, e, restartLineVendorsOnly)
	if _, runs := e.runtimeBuilt(); runs {
		t.Error("a revision naming no native half started a native runtime")
	}
	if lines := restartLines(t, logs); len(lines) != 0 {
		t.Errorf("a node with no runtime said %d restart lines: %v", len(lines), lines)
	}

	// THE FIRST NATIVE HALF: built from this revision, so it agrees with it.
	applyOK(t, e, restartLineTrackerOnly)
	built, runs := e.runtimeBuilt()
	if !runs || built.tracker != config.TrackerNative || built.knowledge != config.KnowledgeNone {
		t.Fatalf("the apply naming the native tracker built %+v (runs %v), want the "+
			"runtime that revision chose", built, runs)
	}
	if lines := restartLines(t, logs); len(lines) != 0 {
		t.Errorf("the apply that started the runtime said %d restart lines, want none: "+
			"it built the runtime from the very revision it applied: %v", len(lines), lines)
	}

	// THE CONTROL: the line is live on this node now.
	applyOK(t, e, restartLineVendorsOnly)
	lines := restartLines(t, logs)
	if len(lines) != 1 || lines[0]["field"] != "tracker.backend" ||
		lines[0]["running"] != "native" || lines[0]["activated"] != "none" {
		t.Errorf("a revision turning off the tracker the apply started said %v, want "+
			"one tracker.backend line, native to none", lines)
	}
}

// restartLines is every `native_backend_needs_restart` record written so far.
func restartLines(t *testing.T, logs *stopLineBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, record := range logs.records(t) {
		if record["msg"] == "native_backend_needs_restart" {
			out = append(out, record)
		}
	}
	return out
}
