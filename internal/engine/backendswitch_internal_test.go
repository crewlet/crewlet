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

// WHICH CHOICES WAIT FOR A RESTART: a half moving to or from the engine's own
// — Worklet, Doclet, Chatlet or the code host — and nothing else.
//
// The runtime is built around whether each half is the engine's own, so that
// is the one change it cannot take live. Moving between two vendors is not one
// — a vendor's parser is rebuilt by its own reconciler at the apply — and a
// line for it would tell an operator to restart a fleet for nothing.
func TestABackendSwitchSaysItWaitsForARestart(t *testing.T) {
	t.Parallel()
	native := builtWith{tracker: config.TrackerWorklet, knowledge: config.KnowledgeDoclet,
		chat: config.ChatChatlet}
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
		name: "worklet to jira", built: native, activated: jira,
		want: []backendSwitch{{
			Field: "tracker.backend", Half: "Worklet",
			Running: "worklet", Activated: "jira", Holds: true,
		}},
	}, {
		// VENDOR TO VENDOR, which the runtime was never built around.
		name: "jira to none",
		built: builtWith{tracker: config.TrackerJira, knowledge: config.KnowledgeDoclet,
			chat: config.ChatChatlet},
		activated: &config.Company{Tracker: config.Tracker{Backend: config.TrackerNone}},
	}, {
		// AN UNSET BACKEND IS ITS DERIVED ONE: no Confluence declared, so
		// the knowledge base is Doclet, which a runtime built on Confluence
		// does not hold.
		name: "confluence to a derived doclet",
		built: builtWith{tracker: config.TrackerWorklet, knowledge: config.KnowledgeConfluence,
			chat: config.ChatChatlet},
		activated: &config.Company{},
		want: []backendSwitch{{
			Field: "knowledge.backend", Half: "Doclet",
			Running: "confluence", Activated: "doclet", Holds: false,
		}},
	}, {
		name: "three halves, tracker first",
		built: builtWith{
			tracker: config.TrackerNone, knowledge: config.KnowledgeDoclet,
			chat: config.ChatNone,
		},
		activated: &config.Company{
			Knowledge: config.Knowledge{Backend: config.KnowledgeNone},
		},
		want: []backendSwitch{{
			Field: "tracker.backend", Half: "Worklet",
			Running: "none", Activated: "worklet", Holds: false,
		}, {
			Field: "knowledge.backend", Half: "Doclet",
			Running: "doclet", Activated: "none", Holds: true,
		}, {
			Field: "chat.backend", Half: "Chatlet",
			Running: "none", Activated: "chatlet", Holds: false,
		}},
	}, {
		// CHATLET TURNED OFF: the runtime keeps its rooms' reader and
		// writer until a restart.
		name: "chatlet to none", built: native,
		activated: &config.Company{Chat: config.Chat{Backend: config.ChatNone}},
		want: []backendSwitch{{
			Field: "chat.backend", Half: "Chatlet",
			Running: "chatlet", Activated: "none", Holds: true,
		}},
	}, {
		// THE CODE HOST TURNED ON: its one field is already whether the
		// half is native, so every change of it is a switch.
		name: "the code host turned on", built: native,
		activated: &config.Company{Code: config.Code{Native: true}},
		want: []backendSwitch{{
			Field: "code.native", Half: "the code host",
			Running: "false", Activated: "true", Holds: false,
		}},
	}, {
		name: "the code host turned off, after the other halves",
		built: builtWith{
			tracker: config.TrackerWorklet, knowledge: config.KnowledgeConfluence,
			chat: config.ChatChatlet, code: true,
		},
		activated: &config.Company{},
		want: []backendSwitch{{
			Field: "knowledge.backend", Half: "Doclet",
			Running: "confluence", Activated: "doclet", Holds: false,
		}, {
			Field: "code.native", Half: "the code host",
			Running: "true", Activated: "false", Holds: true,
		}},
	}, {
		name: "built as activated", built: native, activated: &config.Company{},
	}, {
		name: "built as activated, hosting code",
		built: builtWith{tracker: config.TrackerWorklet, knowledge: config.KnowledgeDoclet,
			chat: config.ChatChatlet, code: true},
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
	kept := backendSwitch{Field: "tracker.backend", Half: "Worklet",
		Running: "worklet", Activated: "jira", Holds: true}.detail()
	if !strings.Contains(kept, "keeps serving it") || !strings.Contains(kept, "tracker.backend: jira") ||
		!strings.Contains(kept, "built with Worklet (tracker.backend: worklet)") {
		t.Errorf("a half turned off reads %q: it must say the node keeps serving "+
			"it and name what the revision asked for", kept)
	}
	lacking := backendSwitch{Field: "knowledge.backend", Half: "Doclet",
		Running: "confluence", Activated: "doclet", Holds: false}.detail()
	if !strings.Contains(lacking, "cannot serve it") ||
		!strings.Contains(lacking, "knowledge.backend: confluence") ||
		!strings.Contains(lacking, "knowledge.backend: doclet") {
		t.Errorf("a half turned on reads %q: it must say the node cannot serve it, "+
			"what it was built with and what the revision asked for", lacking)
	}
}

// A REVISION THAT MOVES A NATIVE BACKEND SAYS IT WAITS FOR A RESTART, once per
// activation, on the node it does not take effect on.
//
// The runtime is built once per process, so such a revision activated cleanly,
// reported ok on every node, and changed nothing: a node went on serving
// Worklet's tools after a revision turned it off, with nothing anywhere
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

	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	SeedStore(t, &b)
	booted := parseCompany(t, nativeCleanupCompany)
	if booted.TrackerBackendFor() != config.TrackerWorklet {
		t.Fatalf("the premise: the node boots on tracker.backend %q, want worklet",
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
		t.Fatalf("a revision turning Worklet off said %d restart lines, "+
			"want exactly 1: %v", len(lines), lines)
	}
	for key, want := range map[string]string{
		"field": "tracker.backend", "running": "worklet", "activated": "none",
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

// restartLineVendorsOnly is [nativeCleanupCompany] on neither Worklet nor
// Doclet: a company whose tracker and knowledge base both live elsewhere.
const restartLineVendorsOnly = restartLineTrackerNone + `knowledge:
  backend: none
`

// restartLineTrackerOnly turns Worklet on and nothing else.
const restartLineTrackerOnly = nativeCleanupCompany + `knowledge:
  backend: none
`

// A NODE WITH NO RUNTIME SAYS NOTHING, and the apply that gives it one says
// nothing either.
//
// A data node runs a native runtime for EVERY company it holds, whatever
// backends that company names: the object collector runs on it, since a
// company on Jira and Confluence still holds the rows that name its files'
// objects ([Engine.startNativeCollector]). So the only node with no runtime is
// one that has no company yet, and the runtime its first apply meets is the
// one its own `native` stage just started, from the revision being applied
// ([Engine.startNativeFor]): the two agree by construction, whichever halves
// that revision keeps, and a line there would tell an operator to restart a
// node for a revision it is already serving. Once the runtime exists the line
// is live: a later revision moving the tracker is said.
//
// NOT PARALLEL, for [TestTheRestartLineIsSaidAtAnActivation]'s reason.
func TestTheApplyThatStartsTheRuntimeSaysNothing(t *testing.T) {
	logs := &stopLineBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	for _, tc := range []struct {
		name               string
		first, then        string
		running, activated string
	}{
		{"a first company on the vendors", restartLineVendorsOnly, restartLineTrackerOnly,
			"none", "worklet"},
		{"a first company on Worklet", restartLineTrackerOnly, restartLineVendorsOnly,
			"worklet", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSandboxNode(t, nil)
			if _, runs := e.runtimeBuilt(); runs {
				t.Fatal("the premise: a node booted on no company runs a native runtime")
			}
			before := len(restartLines(t, logs))

			// THE FIRST COMPANY: the runtime is built from this revision,
			// so it agrees with it.
			applyOK(t, e, tc.first)
			built, runs := e.runtimeBuilt()
			if want := builtFrom(parseCompany(t, tc.first)); !runs || built != want {
				t.Fatalf("the node's first apply built %+v (runs %v), want the runtime that "+
					"revision chose, %+v", built, runs, want)
			}
			if lines := restartLines(t, logs)[before:]; len(lines) != 0 {
				t.Errorf("the apply that started the runtime said %d restart lines, want none: "+
					"it built the runtime from the very revision it applied: %v", len(lines), lines)
			}

			// THE CONTROL: the line is live on this node now.
			applyOK(t, e, tc.then)
			lines := restartLines(t, logs)[before:]
			if len(lines) != 1 || lines[0]["field"] != "tracker.backend" ||
				lines[0]["running"] != tc.running || lines[0]["activated"] != tc.activated {
				t.Errorf("a revision moving the tracker the apply started said %v, want one "+
					"tracker.backend line, %s to %s", lines, tc.running, tc.activated)
			}
		})
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
