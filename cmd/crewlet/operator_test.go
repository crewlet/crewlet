package main

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE OPERATOR'S SURFACES ARE HANDED THE ESTATE ROUTER'S SEAMS, never this
// node's own copy: the board and every other REST read, a project's file rows,
// the purge, and every seam of the operator's own MCP. Read straight off the
// node's copy, a data node whose copy was out of service — wrong rather than
// behind, or its file shut — sent its seats' calls to a peer's copy and went
// on answering its operator from the one it had stopped serving. The router's
// facade is the only type that routes, so a seam of any other type here is a
// surface reading one node's copy whatever its state.
func TestTheOperatorsSurfacesAreHandedTheRouter(t *testing.T) {
	t.Parallel()
	e := testEngine(t)

	if _, ok := nativeWork(e).(estate.Work); !ok {
		t.Errorf("the REST reads are handed %T, want the router's estate.Work", nativeWork(e))
	}
	if _, ok := nativeFiles(e).(estate.Work); !ok {
		t.Errorf("the file rows are handed %T, want the router's estate.Work", nativeFiles(e))
	}
	if _, ok := nativePages(e).(estate.Pages); !ok {
		t.Errorf("the knowledge reads are handed %T, want the router's estate.Pages", nativePages(e))
	}
	if _, ok := nativeWorkSearch(e).(estate.Work); !ok {
		t.Errorf("the ranked search is handed %T, want the router's estate.Work", nativeWorkSearch(e))
	}
	if _, ok := nativePurger(e).(purgeAdapter); !ok {
		t.Errorf("the purge is handed %T, want the router's writer", nativePurger(e))
	}

	opts := api.EngineOperatorOptions(e)
	work := opts.Work
	if work.Reader == nil || opts.Pages.Reader == nil {
		t.Fatalf("the premise: a company on the native tracker and knowledge base hands "+
			"the operator's MCP neither (work %v, pages %v)", work.Reader, opts.Pages.Reader)
	}
	operator := builtin.Actor{Handle: "founder", Kind: tracker.AuthorOperator, OperatorID: "founder"}
	for _, seam := range []struct {
		name string
		got  any
	}{
		{"Reader", work.Reader},
		{"Files", work.Files},
		{"Inbox", work.Inbox},
		{"Writer", work.Writer(operator)},
		{"Dependencies", work.Dependencies(operator)},
		{"Merges", work.Merges(operator)},
		{"Moves", work.Moves(operator)},
		{"FileWriter", work.FileWriter(operator)},
		{"ViewWriter", work.ViewWriter(operator)},
		{"CatalogueWriter", work.CatalogueWriter(operator)},
		{"PersonWriter", work.PersonWriter(operator)},
		{"TrashWriter", work.TrashWriter(operator)},
		{"Placer", work.Placer(operator)},
		{"ProjectWriter", work.ProjectWriter(operator)},
		{"Pages.Reader", opts.Pages.Reader},
		{"Pages.Writer", opts.Pages.Writer},
	} {
		switch seam.got.(type) {
		case estate.Work, estate.WorkWriter, estate.Pages:
		default:
			t.Errorf("the operator's MCP seam %s is %T, want the router's", seam.name, seam.got)
		}
	}
}
