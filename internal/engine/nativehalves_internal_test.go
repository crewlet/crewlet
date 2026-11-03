package engine

import (
	"context"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/alarmwake"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY READER OF WHICH HALVES A NODE RUNS AGREES, because each reads them
// from [Engine.nativeHalves] rather than from the two runtime pointers itself.
//
// Nine places ask the question, the seats' chat seams among them. Five of
// the eight there were before chat — the seats' tracker and knowledge-base
// seams, the inbound parsers, the knowledge searcher and the
// skill source — loaded the pointers on their own, and one of those, the
// parsers, counted a data node's halves by its WRITERS where the rest counted
// its readers. The same fact while one block
// builds both, and two facts the first time a runtime is built with one and
// not the other: a node would then route a half's changes to seats that are
// handed no tool for it, or hand tools for a half whose changes wake nobody.
// So this walks every shape a node's runtime takes — none, a data node's own
// with each half, a node without `data` with each half — and asks each reader.
func TestEveryReaderOfTheNativeHalvesAgrees(t *testing.T) {
	t.Parallel()
	router := halvesRouter(t)
	own := ownRuntime
	company := &Company{Config: &config.Company{
		Knowledge: config.Knowledge{Backend: config.KnowledgeDoclet},
	}}
	for _, tc := range []struct {
		name          string
		native        *native
		remote        *remoteNative
		tracker, wiki bool
		// chat is the company running Chatlet, whose changes the chat
		// parser routes and whose seams the seats are handed.
		chat bool
		// code is the company hosting its repositories in the engine, whose
		// changes the code parser routes.
		code bool
		// walk is the skill walk the node takes, by the method's name, or
		// "" where it reads no Doclet skills at all.
		walk string
	}{
		{name: "a data node hosting code beside both halves", native: codeRuntime(own(true, true)),
			tracker: true, wiki: true, code: true, walk: "walkDocletSkills"},
		{name: "a data node hosting code alone", native: codeRuntime(own(false, false)), code: true},
		{name: "a node without data hosting code alone",
			remote: &remoteNative{built: builtWith{code: true}}, code: true},
		{name: "a node running no native runtime"},
		{name: "a data node running both halves", native: own(true, true),
			tracker: true, wiki: true, walk: "walkDocletSkills"},
		{name: "a data node running the tracker alone", native: own(true, false),
			tracker: true},
		{name: "a data node running the knowledge base alone", native: own(false, true),
			wiki: true, walk: "walkDocletSkills"},
		{name: "a data node on vendors for both", native: own(false, false)},
		{name: "a data node running chat beside both halves", native: chatRuntime(own(true, true)),
			tracker: true, wiki: true, chat: true, walk: "walkDocletSkills"},
		{name: "a data node running chat alone", native: chatRuntime(own(false, false)), chat: true},
		{name: "a node without data running both halves",
			remote:  &remoteNative{tracker: true, wiki: true},
			tracker: true, wiki: true, walk: "walkRemoteSkills"},
		{name: "a node without data running the tracker alone",
			remote: &remoteNative{tracker: true}, tracker: true},
		{name: "a node without data running the knowledge base alone",
			remote: &remoteNative{wiki: true}, wiki: true, walk: "walkRemoteSkills"},
		{name: "a node without data on vendors for both", remote: &remoteNative{}},
		{name: "a node without data running chat alone",
			remote: &remoteNative{chat: true}, chat: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &Engine{router: router, mode: statelog.ModeNormal}
			if tc.native != nil {
				e.native.Store(tc.native)
			}
			if tc.remote != nil {
				e.remote.Store(tc.remote)
			}

			reads := OperatorNative(e)
			if reads.Tracker != tc.tracker || reads.Wiki != tc.wiki {
				t.Errorf("the operator is handed tracker=%v wiki=%v, want %v/%v",
					reads.Tracker, reads.Wiki, tc.tracker, tc.wiki)
			}
			if _, ok := e.trackerHalves(); ok != tc.tracker {
				t.Errorf("the seats are handed tracker tools = %v, want %v", ok, tc.tracker)
			}
			if _, _, _, ok := e.pageHalves(); ok != tc.wiki {
				t.Errorf("the seats are handed knowledge-base tools = %v, want %v", ok, tc.wiki)
			}
			if _, ok := e.chatHalves(); ok != tc.chat {
				t.Errorf("the seats are handed chat tools = %v, want %v", ok, tc.chat)
			}
			var routed []string
			parsers, prompts := e.nativeParsers(company)
			for _, p := range parsers {
				routed = append(routed, p.Source())
			}
			// THE ALARMS' WAKES ROUTE ON EVERY NODE, whatever halves it
			// runs: a transition is routed by the chart alone.
			want := []string{alarmwake.Source}
			if tc.tracker {
				want = append(want, worklet.Source)
			}
			if tc.wiki {
				want = append(want, doclet.Source)
			}
			if tc.chat {
				want = append(want, chatlet.Source)
			}
			if tc.code {
				want = append(want, code.Source)
			}
			if !slices.Equal(routed, want) || len(prompts) != len(want) {
				t.Errorf("the inbound edge routes %v with %d prompts, want %v",
					routed, len(prompts), want)
			}
			if got := e.Knowledge() != nil; got != tc.wiki {
				t.Errorf("a knowledge searcher handed out = %v, want %v", got, tc.wiki)
			}
			src := e.skillSource(company)
			switch {
			case tc.walk == "" && (src.Walk != nil || src.Unreadable == ""):
				t.Errorf("the skills are walked (%v) with no reason given (%q), "+
					"on a node that runs no Doclet",
					src.Walk != nil, src.Unreadable)
			case tc.walk != "" && (src.Walk == nil || !strings.Contains(
				runtime.FuncForPC(reflect.ValueOf(src.Walk).Pointer()).Name(), tc.walk)):

				t.Errorf("the skills are walked by %v, want %s", walkName(src.Walk), tc.walk)
			}
		})
	}
}

// walkName names a skill walk for a failure message.
func walkName(walk any) string {
	if walk == nil || reflect.ValueOf(walk).IsNil() {
		return "nothing"
	}
	return runtime.FuncForPC(reflect.ValueOf(walk).Pointer()).Name()
}

// halvesRouter is an estate router over an in-memory queue, for a case that
// asks which seams a node hands out rather than what they answer.
func halvesRouter(t *testing.T) *estate.Router {
	t.Helper()
	q := memory.NewBroker().Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "data-self", Queue: q, Placement: twoDataNodes{}, Git: noGitNodes{},
		Session: estate.NewSession(), Timing: estate.Production(), Envelopes: routerEnvelopes(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return router
}

// ownRuntime is a data node's runtime as its build makes it: the reader, the
// writer and the half's search are built together or not at all
// ([Engine.startNative]), and the item search under either half.
func ownRuntime(runTracker, wiki bool) *native {
	n := &native{itemSearch: &worklet.Searcher{}}
	if runTracker {
		n.trackerReader, n.writer = &worklet.Reader{}, &worklet.Writer{}
	}
	if wiki {
		n.pageReader, n.pages, n.searcher = &doclet.Reader{}, &doclet.Store{}, &doclet.Searcher{}
	}
	return n
}

// chatRuntime is n with the chat half, as a data node running Chatlet builds
// it: the read path and the write side together, as [ownRuntime] builds the
// tracker's, since a seat is handed chat tools only where it can also post
// ([Engine.chatHalves] counts the half by its writer).
func chatRuntime(n *native) *native {
	n.chatReader, n.chat = &chatlet.Reader{}, &chatlet.Store{}
	return n
}

// codeRuntime is n built for a company that hosts its repositories in the
// engine (`code.native`).
func codeRuntime(n *native) *native {
	n.built.code = true
	return n
}

// A NODE IN A MODE THAT PUBLISHES NOTHING HANDS OUT NO WRITER, WHATEVER IT
// HOLDS — and goes on serving its reads.
//
// The mode is this node's promise that it starts nothing that writes
// ([statelog.MaintenanceMode.Publishes]), and a write it routes to a data node
// is one it starts. A node without `data` used to be exempt, on the reading
// that the data node's mode was that node's to answer for: restarted into the
// mode for a capacity window, which puts every data node in it too, it handed
// its operator surfaces the router's writer, so a file's upload streamed up to
// a gibibyte into the object store before a data node refused its row — as
// `unavailable`, which a client retries — and left an object the collector,
// which stands down in the mode, could not free until the window closed. Every
// writer surface is handed its writer through the two seams asked here
// ([Engine.trackerHalves], [Engine.pageHalves]) or the two operator accessors
// over them, so each is asked of both kinds of node in every mode — normal
// included, the control in which both hand their writers out.
//
// And a data node hands out only the writers its own write side holds, in any
// mode: a half is counted by its reader, so a runtime holding a half's reader
// and not its writer runs the half and writes none of it.
func TestANodeInAMaintenanceModeHandsOutNoWriterWhateverItHolds(t *testing.T) {
	t.Parallel()
	router := halvesRouter(t)
	readersAlone := &native{trackerReader: &worklet.Reader{}, pageReader: &doclet.Reader{}}
	for _, mode := range statelog.MaintenanceModes {
		for _, shape := range []struct {
			name   string
			own    *native
			remote *remoteNative
			// writable is whether the node hands out writers in a mode
			// that publishes.
			writable bool
		}{
			{name: "a data node", own: ownRuntime(true, true), writable: true},
			{name: "a data node holding readers alone", own: readersAlone},
			{name: "a node without data", remote: &remoteNative{tracker: true, wiki: true},
				writable: true},
		} {
			t.Run(string(mode)+"/"+shape.name, func(t *testing.T) {
				t.Parallel()
				e := &Engine{router: router, mode: mode}
				if shape.own != nil {
					e.native.Store(shape.own)
				}
				if shape.remote != nil {
					e.remote.Store(shape.remote)
				}
				writes := mode.Publishes() && shape.writable

				// THE READS ARE SERVED IN EVERY MODE — and the premise that
				// this node runs both halves at all.
				if reads := OperatorNative(e); !reads.Tracker || !reads.Wiki {
					t.Fatalf("the operator is handed tracker=%v wiki=%v reads, want "+
						"both in every mode", reads.Tracker, reads.Wiki)
				}
				if _, ok := e.trackerHalves(); ok != writes {
					t.Errorf("the seats are handed tracker tools = %v, want %v", ok, writes)
				}
				if _, _, _, ok := e.pageHalves(); ok != writes {
					t.Errorf("the seats are handed knowledge-base tools = %v, want %v",
						ok, writes)
				}
				if _, ok := OperatorWorkWriter(e); ok != writes {
					t.Errorf("the operator's surfaces are handed the tracker's writer "+
						"= %v, want %v", ok, writes)
				}
				if _, ok := OperatorPageWriter(e); ok != writes {
					t.Errorf("the operator's surfaces are handed the knowledge base's "+
						"writer = %v, want %v", ok, writes)
				}
			})
		}
	}
}
