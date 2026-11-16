package queries_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/research"
)

// readerOf answers every listed page, and records the hosts it was handed.
type readerOf struct{ hosts *[]string }

func (r readerOf) Read(_ context.Context, hosts []string, url string, from int) (research.Page, error) {
	*r.hosts = hosts
	if _, err := research.Allowed(hosts, url); err != nil {
		return research.Page{}, err
	}
	return research.Page{URL: url, From: from, Text: "Words."}, nil
}

// A page is read only from the hosts the company lists: none listed is research
// off, refused naming the setting, and a host off the list is refused.
func TestAPageIsReadOnlyFromTheHostsTheCompanyLists(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc, url string
		ok       bool
	}{
		"off":      {"name: Acme\n", "https://docs.example.com/a", false},
		"listed":   {"name: Acme\nassistant:\n  research:\n    hosts: [docs.example.com]\n", "https://docs.example.com/a", true},
		"unlisted": {"name: Acme\nassistant:\n  research:\n    hosts: [docs.example.com]\n", "https://collector.example/x", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.ParseCompany([]byte(tc.doc))
			if err != nil {
				t.Fatal(err)
			}
			var hosts []string
			s := viewerSources(t, &stubWork{})
			s.Company, s.Research = companySource(t, cfg), readerOf{&hosts}
			got, err := askAsSeat(t, s, "ana", "research_page", map[string]any{"url": tc.url})
			switch {
			case tc.ok && (err != nil || got.(research.Page).Text != "Words."):
				t.Errorf("a listed page answered %v, %v", got, err)
			case !tc.ok && !errors.Is(err, queries.ErrBadParams):
				t.Errorf("%s answered %v, %v; want a refusal", tc.url, got, err)
			}
		})
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

// A search is refused naming the setting while none is configured, a query
// out of bounds is refused, and a server that fails is unavailable.
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
			got, err := askAsSeat(t, s, "ana", "research_search", map[string]any{"q": "origin rules"})
			switch {
			case tc.want == nil && (err != nil || got.(research.Results).Query != "origin rules"):
				t.Errorf("the search answered %v, %v", got, err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("the search answered %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
