package queries_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/store"
)

// memorySandbox is a pending-run store with nothing in it: this sweep is
// about which names exist, not what they answer.
type memorySandbox struct{}

func (memorySandbox) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	return nil, nil
}

// dashboardTree is the room source this sweep reads. Relative, because the
// package it certifies is the one that serves those rooms.
const dashboardTree = "../../../static/dashboard/js"

// roomQueries scans the dashboard for every query kind a room asks for.
//
// FROM THE ROOMS' OWN SOURCE, never a list kept here: a hand-maintained one
// is exactly what drifts, and it would drift towards claiming the server
// answers more than it does.
func roomQueries(t *testing.T) map[string][]string {
	t.Helper()
	calls := regexp.MustCompile(`\bquery\("([a-z_]+)"`)
	out := map[string][]string{}
	err := filepath.WalkDir(dashboardTree, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
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
		t.Skipf("the dashboard tree is not in this checkout: %v", err)
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
		State:         &livestate.LiveState{},
		Events:        &store.EventLog{},
		Health:        func() any { return nil },
		Company:       func() *config.Company { return cfg },
		Coord:         coordmemory.New(),
		Plane:         &store.ConfigPlane{},
		Runs:          fakeRuns{},
		Conversations: ledgerstore.NewMemoryConversations(),
		Diary:         &learning.Diary{},
		Episodes:      &learning.Episodes{},
		Budget:        &store.Budgets{},
		Sandbox:       memorySandbox{},
		Config:        surface,
	}
}

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
