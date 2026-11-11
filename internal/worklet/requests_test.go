package worklet_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/scenario"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN AGENT'S WORDS THAT ASK A PERSON WITHOUT AN ASK ARE A REQUEST, AND WORDS
// THAT ONLY INFORM ARE NOT (ADR-0122): the one predicate the comment tools
// refuse by and the advisor counts by. A person named in words that ask
// (`asks: true`) outside the comment's `ask` is requested of; one named in
// words that ask nothing, the person the `ask` is put to, an agent, a
// person's own words and an older build's record with no statement are not.
//
// Mutation: drop the statement from the predicate, and every mention of a
// person, a status note included, reads as a request.
func TestOnlyWordsThatAskAPersonWithoutAnAskAreARequest(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	person := func(h string) bool { return h == "ana" || h == "lee" }
	for _, c := range []struct {
		name  string
		agent bool
		asks  *bool
		ask   string
		named []string
		want  []string
	}{
		{"an agent asks a named person in words", true, &yes, "", []string{"ana"}, []string{"ana"}},
		{"an agent asks an agent and names a person", true, &yes, "pm", []string{"pm", "ana"}, []string{"ana"}},
		{"an agent asks the named person with ask", true, &yes, "ana", []string{"ana"}, nil},
		{"an agent informs a named person", true, &no, "", []string{"ana"}, nil},
		{"an agent asks a named agent", true, &yes, "", []string{"pm"}, nil},
		{"a person asks a named person", false, &yes, "", []string{"ana"}, nil},
		{"an older build's record", true, nil, "", []string{"ana"}, nil},
		{"two persons, named twice", true, &yes, "", []string{"lee", "ana", "lee"}, []string{"lee", "ana"}},
	} {
		if got := worklet.RequestedWithoutAsk(c.agent, c.asks, c.ask, c.named, person); !slices.Equal(got, c.want) {
			t.Errorf("%s: requested of %v, want %v", c.name, got, c.want)
		}
	}
}

// THE ADVISOR COUNTS WHAT THE TOOL REFUSES, AND ONLY THAT: of an agent's seven
// comments naming a person, the five that ask in words are counted as requests
// and the two status notes, which only inform, are not, while every one is
// still told.
//
// Mutation: count a person's requests by `told`, and the status notes are read
// as requests the comment tool never refuses.
func TestTheAdvisorCountsTheRequestsTheToolRefuses(t *testing.T) {
	t.Parallel()
	tl, err := scenario.Named("person_requests_without_ask")
	if err != nil {
		t.Fatal(err)
	}
	d := playTimeline(t, tl)
	work, err := d.r.reader.WorkPatterns(t.Context(), worklet.PatternsQuery{
		Since: d.start, Until: time.Now().UTC().Add(time.Microsecond), Level: statelog.ReadStale,
		QuietSince: d.quietSince(), People: []string{"owner"}})
	if err != nil {
		t.Fatalf("WorkPatterns: %v", err)
	}
	i := slices.IndexFunc(work.Named, func(n worklet.NamedWithoutAsk) bool { return n.Handle == "owner" })
	if i < 0 {
		t.Fatalf("the owner is not among the named: %+v", work.Named)
	}
	if owner := work.Named[i]; owner.Requested != 5 || owner.Told != 7 || len(owner.RequestedComments) != 5 {
		t.Errorf("the owner was requested of %d times (%d named) and told %d, want 5 of 7",
			owner.Requested, len(owner.RequestedComments), owner.Told)
	}
}
