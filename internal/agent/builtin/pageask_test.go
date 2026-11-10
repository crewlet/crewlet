package builtin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/tools"
)

// AN AGENT'S PAGE COMMENT THAT ASKS A PERSON IT NAMES IS REFUSED (ADR-0100):
// a page comment carries no decision, so the question goes on a work item.
// Only mentioning a person, asking an agent, naming a team, or a person's own
// comment are posted as before.
//
// Mutation: return no persons from askedInWords, and the agent's request
// that names a person on a page is posted.
func TestAnAgentsPageCommentThatAsksAPersonIsRefused(t *testing.T) {
	t.Parallel()
	roster := func() []colleague.Seat {
		return []colleague.Seat{
			{Handle: "pm", Name: "Pat Manager", Kind: "agent"},
			{Handle: "ana", Name: "Ana", Kind: "human"},
		}
	}
	person := func(context.Context, *turnctx.Turn) (doclet.Actor, error) {
		return doclet.Actor{Handle: "jane", Kind: doclet.AuthorHuman}, nil
	}
	for _, c := range []struct {
		name     string
		mentions fakeMentions
		actor    func(context.Context, *turnctx.Turn) (doclet.Actor, error)
		asks     bool
		refused  bool
	}{
		{"an agent asks a named person", fakeMentions{"ana"}, nil, true, true},
		{"an agent only mentions a person", fakeMentions{"ana"}, nil, false, false},
		{"an agent asks a named agent", fakeMentions{"pm"}, nil, true, false},
		{"an agent asks and names a team", fakeMentions{"eng"}, nil, true, false},
		{"a person asks a named person", fakeMentions{"ana"}, person, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			kb := newFakeKB()
			reg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb, Mentions: c.mentions,
				Seats: roster, Actor: c.actor})
			got := callWork(t, reg, builtin.CommentOnPageTool, map[string]any{"page": "p1",
				"body": "Can you sign this off?", "asks": c.asks})
			switch {
			case !c.refused && (got.Failed || len(kb.comments) != 1):
				t.Fatalf("answered %q and wrote %d comments, want it posted", got.Output, len(kb.comments))
			case !c.refused:
			case !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid || len(kb.comments) != 0:
				t.Fatalf("answered %+v and wrote %d comments, want it refused as invalid", got, len(kb.comments))
			case !strings.Contains(got.Output, builtin.CommentOnWorkTool):
				t.Errorf("the refusal %q does not say where to ask", got.Output)
			}
		})
	}
}
