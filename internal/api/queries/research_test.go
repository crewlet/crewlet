package queries_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/research"
)

// readerOf answers every public https page.
type readerOf struct{}

func (readerOf) Read(_ context.Context, url string, from int) (research.Page, error) {
	if _, err := research.Public(url); err != nil {
		return research.Page{}, err
	}
	return research.Page{URL: url, From: from, Text: "Words."}, nil
}

// TestAPageIsReadAtAnyPublicHttpsAddress: research_page answers any public
// https page with no list to configure, and refuses an address that is not
// one as the caller's to change. Mutation: hold the address to a list again.
func TestAPageIsReadAtAnyPublicHttpsAddress(t *testing.T) {
	t.Parallel()
	for url, ok := range map[string]bool{
		"https://docs.example.com/a": true, "https://collector.example/x": true,
		"http://docs.example.com/a": false, "https://docs.example.com:8443/a": false,
	} {
		s := viewerSources(t, &stubWork{})
		s.Research = readerOf{}
		got, err := askAsSeat(t, s, "ana", "research_page", map[string]any{"url": url})
		switch {
		case ok && (err != nil || got.(research.Page).Text != "Words."):
			t.Errorf("%s answered %v, %v", url, got, err)
		case !ok && !errors.Is(err, queries.ErrBadParams):
			t.Errorf("%s answered %v, %v; want a refusal", url, got, err)
		}
	}
}

// searcherOf answers a search as err, or with one result.
type searcherOf struct{ err error }

func (s searcherOf) ResearchSearch(_ context.Context, q string) (research.Results, error) {
	if s.err != nil {
		return research.Results{}, s.err
	}
	return research.Results{Query: q, Results: []research.Result{{Title: "Rules"}}}, nil
}

// TestASearchAnswersOrSaysWhyNot: a search is answered through Ask Crewlet's
// grant alone, refused where the model has no search run or the query is out
// of bounds, and unavailable where the run fails. Asked at the keyboard it is
// refused on authority, since only a turn is charged its spend.
func TestASearchAnswersOrSaysWhyNot(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err, want error
	}{
		"found":       {nil, nil},
		"off":         {research.ErrSearchOff, queries.ErrBadParams},
		"long query":  {research.ErrQuery, queries.ErrBadParams},
		"server down": {research.ErrSearch, queries.ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := viewerSources(t, &stubWork{})
			s.Searcher = searcherOf{tc.err}
			r := queries.NewRegistry()
			queries.Register(r, s)
			person := iam.Principal{ID: uuid.New(), Login: "ana", Kind: iam.KindPerson,
				Seat: "ana", Stage: iam.StageActive}
			got, err := r.Answer(through(person), "research_search", map[string]any{"q": "origin rules"})
			if _, kept := r.Answer(iam.WithPrincipal(t.Context(), person), "research_search",
				map[string]any{"q": "origin rules"}); !errors.Is(kept, queries.ErrUnauthorized) {
				t.Errorf("a search asked at the keyboard answered %v", kept)
			}
			switch {
			case tc.want == nil && (err != nil || got.(research.Results).Query != "origin rules"):
				t.Errorf("the search answered %v, %v", got, err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("the search answered %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
