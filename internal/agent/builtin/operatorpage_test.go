package builtin_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/doclet"
)

// A PAGE THAT NAMES NO SPACE LANDS IN THE WRITER'S TEAM SPACE, asked of the
// handle the write is recorded under.
//
// A person writes AS their seat whichever credential carried the write, so a
// person's page lands where their seat's own pages do. Without a default on
// the operator surface, the dashboard's "Save as a page" was refused for every
// person who used it. A credential nobody is bound through writes under its
// login, which no seat matches, so its write is refused naming the argument it
// left out rather than filed somewhere guessed for it.
func TestAPageWithNoSpaceLandsInTheWritersTeamSpace(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		actor  doclet.Actor
		asked  string
		filed  string
		refuse bool
	}{
		"a person, through their own token": {
			actor: doclet.Actor{Handle: "jane", Kind: doclet.AuthorHuman, OperatorID: "pat:4c1e"},
			asked: "jane", filed: "HOME",
		},
		"a credential nobody is bound through": {
			actor: doclet.Actor{Handle: "token:ops", Kind: doclet.AuthorOperator,
				OperatorID: "token:ops"},
			asked: "token:ops", refuse: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kb := newFakeKB()
			var asked []string
			reg := kbRegistry(t, builtin.PageDeps{
				Reader: kb, Writer: kb,
				Actor: func(context.Context, *turnctx.Turn) (doclet.Actor, error) {
					return tc.actor, nil
				},
				DefaultContainer: func(handle string) string {
					asked = append(asked, handle)
					if handle == "jane" {
						return "HOME"
					}
					return ""
				},
			})
			got := callWork(t, reg, builtin.WritePageTool, map[string]any{
				"title": "Notes from the call", "body": "prose",
			})
			if !slices.Equal(asked, []string{tc.asked}) {
				t.Fatalf("the default was asked for %v, want %q: the handle the "+
					"write is recorded under", asked, tc.asked)
			}
			if tc.refuse {
				if !got.Failed || !strings.Contains(got.Output, "needs a `container`") {
					t.Fatalf("a write with no space and no team was answered %q, want "+
						"the refusal naming the container argument", got.Output)
				}
				if len(kb.created) != 0 {
					t.Fatalf("a refused write still created %+v", kb.created)
				}
				return
			}
			if got.Failed {
				t.Fatalf("the person's page was refused: %s", got.Output)
			}
			if kb.created[0].Container != tc.filed {
				t.Fatalf("the page was filed in %q, want the person's %s",
					kb.created[0].Container, tc.filed)
			}
		})
	}
}
