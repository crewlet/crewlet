package engine

import (
	"context"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY READER OF WHICH HALVES A NODE RUNS AGREES, because each reads them
// from [Engine.nativeHalves] rather than from the two runtime pointers itself.
//
// Eight places ask the question. Five of them — the seats' tracker and
// knowledge-base seams, the inbound parsers, the knowledge searcher and the
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
	q := memory.NewBroker().Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "data-self", Queue: q, Placement: twoDataNodes{}, Session: estate.NewSession(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A DATA NODE'S RUNTIME AS ITS BUILD MAKES IT: the reader, the writer
	// and the half's search are built together or not at all
	// ([Engine.startNative]), and the item search under either half.
	own := func(runTracker, wiki bool) *native {
		n := &native{itemSearch: &tracker.Searcher{}}
		if runTracker {
			n.trackerReader, n.writer = &tracker.Reader{}, &tracker.Writer{}
		}
		if wiki {
			n.pageReader, n.pages, n.searcher = &pages.Reader{}, &pages.Store{}, &pages.Searcher{}
		}
		return n
	}
	company := &Company{Config: &config.Company{
		Knowledge: config.Knowledge{Backend: config.KnowledgeNative},
	}}
	for _, tc := range []struct {
		name          string
		native        *native
		remote        *remoteNative
		tracker, wiki bool
		// walk is the skill walk the node takes, by the method's name, or
		// "" where it reads no native skills at all.
		walk string
	}{
		{name: "a node running no native runtime"},
		{name: "a data node running both halves", native: own(true, true),
			tracker: true, wiki: true, walk: "walkNativeSkills"},
		{name: "a data node running the tracker alone", native: own(true, false),
			tracker: true},
		{name: "a data node running the knowledge base alone", native: own(false, true),
			wiki: true, walk: "walkNativeSkills"},
		{name: "a data node on vendors for both", native: own(false, false)},
		{name: "a node without data running both halves",
			remote:  &remoteNative{tracker: true, wiki: true},
			tracker: true, wiki: true, walk: "walkRemoteSkills"},
		{name: "a node without data running the tracker alone",
			remote: &remoteNative{tracker: true}, tracker: true},
		{name: "a node without data running the knowledge base alone",
			remote: &remoteNative{wiki: true}, wiki: true, walk: "walkRemoteSkills"},
		{name: "a node without data on vendors for both", remote: &remoteNative{}},
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
			var routed []string
			parsers, prompts := e.nativeParsers(company)
			for _, p := range parsers {
				routed = append(routed, p.Source())
			}
			var want []string
			if tc.tracker {
				want = append(want, tracker.Source)
			}
			if tc.wiki {
				want = append(want, pages.Source)
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
					"on a node that runs no native knowledge base",
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
