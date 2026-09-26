package prefetch_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/knowledge"
)

// truncating is a backend that cut its answer short of its own ranking.
type truncating struct{ *searcher }

func (s truncating) Search(ctx context.Context, q knowledge.Query) knowledge.Answer {
	answer := s.searcher.Search(ctx, q)
	answer.Truncated = true
	return answer
}

// A TRUNCATED ANSWER SAYS SO, in the block as in the tool.
//
// A backend that read its ranking to a depth, and whose exclusions took places
// among what it read, answers fewer pages than it was asked for while it ranks
// more — and a seat reading the short list as everything that matched
// concludes a page it was not shown does not exist. So the block carries the
// sentence whether or not it found anything, and a whole answer carries none.
func TestATruncatedKnowledgeAnswerSaysSo(t *testing.T) {
	t.Parallel()
	for _, found := range [][]knowledge.Hit{{{Title: "Staging runbook"}}, nil} {
		got := fetch(t, prefetch.Sources{
			Knowledge: truncating{&searcher{hits: found}},
			Models:    models{provider: &aux{answers: []string{"staging proxy"}}},
		}, request(t)).RelevantKnowledge
		if !strings.Contains(got, prefetch.TruncatedKnowledgeNote) {
			t.Errorf("%d hit(s): the block does not say the answer was cut short:\n%s", len(found), got)
		}
	}
	whole := fetch(t, prefetch.Sources{
		Knowledge: &searcher{hits: []knowledge.Hit{{Title: "Staging runbook"}}},
		Models:    models{provider: &aux{answers: []string{"staging proxy"}}},
	}, request(t)).RelevantKnowledge
	if strings.Contains(whole, prefetch.TruncatedKnowledge) {
		t.Errorf("a whole answer's block says it was cut short:\n%s", whole)
	}
}

// AN ANSWER BOTH PARTIAL AND TRUNCATED SAYS BOTH: they are two facts about
// what its hits do not cover, and each sends a seat to its own remedy.
func TestAnAnswerBothPartialAndTruncatedSaysBoth(t *testing.T) {
	t.Parallel()
	partial := &knowledge.Partial{BucketsAnswered: 64, SemanticSkipped: true}
	note := prefetch.KnowledgeAnswerNote(knowledge.Answer{Partial: partial, Truncated: true})
	for _, want := range []string{prefetch.PartialKnowledgeNote(partial), prefetch.TruncatedKnowledgeNote} {
		if !strings.Contains(note, want) {
			t.Errorf("the note %q does not carry %q", note, want)
		}
	}
	if note := prefetch.KnowledgeAnswerNote(knowledge.Answer{}); note != "" {
		t.Errorf("a whole answer's note is %q", note)
	}
}
