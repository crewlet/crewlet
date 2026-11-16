package catalog_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/iam"
)

// The docs tools are offered with no app; a search answers sections by path, a
// read answers one within a docs answer's bound, and a missing page is refused.
func TestCrewletsOwnDocumentationIsEverybodysToRead(t *testing.T) {
	t.Parallel()
	nobody := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson, Stage: iam.StageActive}
	offered := catalog.Offered(nobody, nil)
	for _, name := range []string{catalog.SearchEngineDocsTool, catalog.ReadEngineDocTool} {
		if !slices.ContainsFunc(offered, func(t catalog.Tool) bool { return t.Name == name }) {
			t.Errorf("%s is not offered to a person holding no app", name)
		}
	}
	search := mustTool(t, catalog.SearchEngineDocsTool)
	found, err := search.Docs(catalog.Args{"query": "request_access grant hours"})
	if err != nil {
		t.Fatal(err)
	}
	first := ""
	for _, line := range strings.Split(found, "\n") {
		if strings.HasPrefix(line, "1. ") {
			first = strings.TrimPrefix(line, "1. ")
		}
	}
	if !strings.HasPrefix(first, "docs/") && !strings.HasPrefix(first, "adr/") {
		t.Fatalf("the search answered no section by its path:\n%s", found)
	}
	read := mustTool(t, catalog.ReadEngineDocTool)
	text, err := read.Docs(catalog.Args{"path": first})
	if err != nil || !strings.HasPrefix(text, first) || len(text) > 15<<10 {
		t.Errorf("reading %s answered %d bytes beginning %.120q, %v", first, len(text), text, err)
	}
	if _, err := read.Docs(catalog.Args{"path": "docs/nothing-here.md"}); !errors.Is(err,
		catalog.ErrInvalidArguments) || !strings.Contains(err.Error(), "search") {
		t.Errorf("a page the tree lacks answered %v, want a refusal saying to search", err)
	}
	whole, err := read.Docs(catalog.Args{"path": "docs/reference/api-endpoints.md"})
	if err != nil || len(whole) > 15<<10 || !strings.Contains(whole, "Sections of this page") {
		t.Errorf("the reference's top reads %d bytes (%v), want its top and its sections "+
			"within what a docs answer holds", len(whole), err)
	}
}
