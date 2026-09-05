package queries_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/work"
)

// memorySandbox is a pending-run store with nothing in it: this sweep is
// about which names exist, not what they answer.
type memorySandbox struct{}

func (memorySandbox) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	return nil, nil
}

// dashboardTree is the room source this sweep reads. Relative, because the
// package it certifies is the one that serves those rooms.
//
// THE SOURCE, not the build output. It pointed at `static/dashboard/js` — the
// hand-written bundle the React rewrite deleted — so WalkDir failed, the skip
// below fired, and both gates in this file certified nothing for the whole of
// that rewrite while reporting a pass. That is the exact failure they exist to
// catch, one level up.
const dashboardTree = "../../../dashboard/src"

// roomQueries scans the dashboard for every query kind a room asks for.
//
// FROM THE ROOMS' OWN SOURCE, never a list kept here: a hand-maintained one
// is exactly what drifts, and it would drift towards claiming the server
// answers more than it does.
func roomQueries(t *testing.T) map[string][]string {
	t.Helper()
	// Both call shapes: the `useQuery` hook a screen renders from, and the
	// direct `socket.query` a pager or an action uses.
	//
	// DIGITS IN THE NAME. The class was `[a-z_]+`, which cannot match
	// `a2a_channels` — so the one kind whose name carries a number was
	// invisible to a sweep whose whole job is to notice a missing name.
	// `\s*` after the paren: a formatter wraps a call whose arguments do not
	// fit, and `useQuery(\n  "config_diff",` is the same call as the one that
	// fits on a line. Without it the sweep reported a live reader as missing.
	calls := regexp.MustCompile(`\b(?:useQuery|query)\(\s*"([a-z0-9_]+)"`)
	out := map[string][]string{}
	err := filepath.WalkDir(dashboardTree, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() ||
			(!strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx")) ||
			strings.HasSuffix(path, ".test.ts") || strings.HasSuffix(path, ".test.tsx") {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range calls.FindAllStringSubmatch(string(source), -1) {
			room := filepath.Base(path)
			if !slices.Contains(out[m[1]], room) {
				out[m[1]] = append(out[m[1]], room)
			}
		}
		return nil
	})
	if err != nil {
		// FAILS rather than skips. The dashboard source is committed, so it
		// is always in a checkout — and a skip here is indistinguishable
		// from a pass, which is how this gate went quiet the last time the
		// tree moved.
		t.Fatalf("the dashboard source at %s could not be read, so this gate "+
			"certifies nothing: %v", dashboardTree, err)
	}
	if len(out) == 0 {
		t.Fatal("the sweep found no query calls at all, so it certifies nothing")
	}
	return out
}

// everySeam is a Sources with every seam present, which is what makes the
// registry list everything this build can answer.
//
// The seams are only tested for nil by Register, so zero values are enough
// and nothing here is called — this sweep is about WHICH NAMES exist, not
// what they answer. The per-kind tests in this package cover the answers.
func everySeam(t *testing.T) queries.Sources {
	t.Helper()
	surface, _ := configSurface(t, companyDoc)
	cfg := company(t)
	return queries.Sources{
		State:    &livestate.LiveState{},
		Events:   &store.EventLog{},
		Health:   func(context.Context) any { return nil },
		Company:  func() *config.Company { return cfg },
		Coord:    coordmemory.New(),
		Plane:    coordmemory.NewFleet(),
		Runs:     fakeRuns{},
		Diary:    &learning.Diary{},
		Episodes: &learning.Episodes{},
		Skills:   &learning.Skills{},
		Channels: fakeChannels{},
		Budget:   coordmemory.NewFleet(),
		Sandbox:  memorySandbox{},
		Config:   surface,
		Work:     emptyWork{},
		Pages:    emptyPages{},
	}
}

// emptyWork and emptyPages are the native readers with nothing in them, on
// fakeChannels' terms: this sweep is about which NAMES exist.
type emptyWork struct{}

func (emptyWork) List(context.Context, work.Filter) ([]work.Summary, error) { return nil, nil }
func (emptyWork) Get(context.Context, string) (work.Detail, error)          { return work.Detail{}, nil }
func (emptyWork) Counters(context.Context) (map[string]int, error)          { return nil, nil }

type emptyPages struct{}

func (emptyPages) List(context.Context, pages.Filter) ([]pages.Summary, error) { return nil, nil }
func (emptyPages) Get(context.Context, string) (pages.Detail, error)           { return pages.Detail{}, nil }
func (emptyPages) Containers(context.Context) ([]pages.Container, error)       { return nil, nil }

// fakeChannels is an A2A channel reader with nothing in it: this sweep is
// about which names exist, not what they answer.
type fakeChannels struct{}

func (fakeChannels) OpenChannels(context.Context) ([]coord.Channel, error) { return nil, nil }

// EVERY QUERY A ROOM MAKES IS A QUERY THIS SERVER ANSWERS.
//
// Nothing linked the two, and the cost was a whole feature: the Config
// room's entity editor listed a collection with query("config_entities"),
// opened one with {kind, id}, and no answer was ever registered under that
// name — so every list came back unknown_query and the editor was dead from
// the day it shipped. Both sides' tests passed, because each was written
// against its own idea of the other.
//
// This is the cheap half of the gate internal/e2e gives the push protocol,
// and it is deliberately about NAMES rather than fields: a name is checkable
// without standing up a node, and a name that nothing answers is the failure
// that renders as an empty room with no error anywhere.
func TestEveryQueryARoomMakesIsAnswered(t *testing.T) {
	t.Parallel()
	answered := registeredKinds(t)
	for kind, rooms := range roomQueries(t) {
		if !slices.Contains(answered, kind) {
			t.Errorf("%s asks for %q and nothing answers it, so the room renders "+
				"empty with no error anywhere", strings.Join(rooms, " and "), kind)
		}
	}
}

// registeredKinds is every name this build answers, from a Sources with
// every seam present.
//
// The NAMES only: Register gates each kind on its seam being non-nil, so
// this is the complete surface — and nothing here is invoked, because the
// question is which names exist rather than what they answer. The per-kind
// tests in this package cover the answers.
func registeredKinds(t *testing.T) []string {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, everySeam(t))
	names := r.Names()
	if len(names) == 0 {
		t.Fatal("a Sources with every seam registered nothing, so this sweep " +
			"certifies nothing")
	}
	return names
}

// AND EVERY QUERY THIS SERVER ANSWERS IS ONE SOMETHING ASKS FOR.
//
// The other direction, and the one that goes quiet rather than breaking: an
// answer nobody calls is code with tests, no readers, and no way to notice
// it stopped being right. The exceptions are named rather than assumed.
func TestEveryQueryThisServerAnswersHasAReader(t *testing.T) {
	t.Parallel()
	// Read by name from somewhere that is not a room's query() call.
	nonRoom := map[string]string{
		"stream": "the header's health poll reads it through api.js, not a room",
	}

	asked := roomQueries(t)
	for _, kind := range registeredKinds(t) {
		if _, ok := asked[kind]; ok {
			continue
		}
		if why, exempt := nonRoom[kind]; exempt {
			t.Logf("%s: %s", kind, why)
			continue
		}
		t.Errorf("this build answers %q and no room asks for it — either a "+
			"reader was lost, or the answer should go with whatever used to "+
			"call it", kind)
	}
}
