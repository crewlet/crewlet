package builtin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// shortCoverage is an answer one of whose three partitions did not answer.
var shortCoverage = statelog.Coverage{
	Addressed: 3, Answered: []string{"tracker.000", "tracker.002"},
	Missing: []statelog.MissingPartition{
		{Partition: "tracker.001", Reason: statelog.MissingUnreachable, Detail: "data-b: no answer"},
	},
}

// EVERY TOOL THAT RENDERS A GATHERED LIST RENDERS A MISSING PARTITION AS THE
// ONE SENTENCE, and never as the shorter list alone — an empty list included,
// which must not read "nothing matches" when part of the company was not read.
// And a list that covered everything says nothing about partitions at all, nor
// spends the turn's context on the coverage a model can do nothing with.
func TestEveryGatheredListSaysWhatDidNotAnswer(t *testing.T) {
	t.Parallel()
	notice := shortCoverage.Notice()
	for _, missing := range []bool{true, false} {
		cov := statelog.Coverage{Addressed: 3,
			Answered: []string{"tracker.000", "tracker.001", "tracker.002"}}
		if missing {
			cov = shortCoverage
		}
		trk := newFakeTracker()
		trk.coverage = cov
		trk.answer = &tracker.Answer{Complete: true, Coverage: cov}
		trk.myWork = tracker.MyWork{Handle: "eng", Complete: true, Coverage: cov}
		trk.activity = tracker.ActivityAnswer{Complete: true, Coverage: cov}
		trk.projects = tracker.ProjectListing{Complete: true, Coverage: cov}
		trk.ranked = []tracker.Ranked{{ID: "id-2", Key: "ENG-2", Rank: 1}}
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Inbox: trk,
			Search: trk})
		kb := newFakeKB()
		kb.listCoverage, kb.listEmpty = cov, true
		pagesReg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})

		for _, call := range []struct {
			name   string
			output string
		}{
			{builtin.ListWorkItemsTool, callWork(t, reg, builtin.ListWorkItemsTool, nil).Output},
			{tracker.MyWorkTool, callWork(t, reg, tracker.MyWorkTool, nil).Output},
			{tracker.TaskActivityTool, callWork(t, reg, tracker.TaskActivityTool, nil).Output},
			{tracker.ListProjectsTool, callWork(t, reg, tracker.ListProjectsTool, nil).Output},
			{tracker.WorkInboxTool, callPlain(t, personSurface(t, trk, &personSpy{},
				func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
					return builtin.Actor{Handle: "alice", Kind: tracker.AuthorHuman}, nil
				}, nil), tracker.WorkInboxTool, map[string]any{"handle": "eng"}).Output},
			{tracker.SearchWorkItemsTool, callWork(t, reg, tracker.SearchWorkItemsTool,
				map[string]any{"text": "retry backoff"}).Output},
			{builtin.ListPagesTool, callWork(t, pagesReg, builtin.ListPagesTool, nil).Output},
		} {
			switch {
			case missing && !strings.Contains(call.output, notice):
				t.Errorf("%s with a partition missing rendered:\n%s\nwant %q", call.name,
					call.output, notice)
			case missing && strings.Contains(call.output, "match that filter"):
				t.Errorf("%s with a partition missing said nothing matches:\n%s", call.name,
					call.output)
			case !missing && strings.Contains(call.output, "did not answer"):
				t.Errorf("%s with every partition answering spoke of one that did not:\n%s",
					call.name, call.output)
			case strings.Contains(call.output, "tracker.002"):
				t.Errorf("%s put the coverage itself in front of the model:\n%s", call.name,
					call.output)
			}
		}
	}
}
