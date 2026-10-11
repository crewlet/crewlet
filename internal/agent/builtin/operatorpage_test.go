package builtin_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/pages"
)

// A PAGE THAT NAMES NO SPACE LANDS IN THE PERSON'S TEAM SPACE, never the
// token's: an operator's token names no seat, and the seat it is bound to does.
// Asked for the token, the default found no team and the dashboard's "Save as a
// page" was refused for every person who used it.
func TestAnOperatorsPageWithNoSpaceLandsInThePersonsTeamSpace(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	var asked []string
	reg := kbRegistry(t, builtin.PageDeps{
		Reader: kb, Writer: kb,
		Actor: func(context.Context, *turnctx.Turn) (pages.Actor, error) {
			return pages.Actor{Handle: "founder", Kind: pages.AuthorOperator,
				OperatorID: "founder", Seat: "jane"}, nil
		},
		DefaultContainer: func(handle string) string {
			asked = append(asked, handle)
			if handle == "jane" {
				return "HOME"
			}
			return ""
		},
	})
	if got := callWork(t, reg, builtin.WritePageTool, map[string]any{
		"title": "Notes from the call", "body": "prose",
	}); got.Failed {
		t.Fatalf("the person's page was refused: %s", got.Output)
	}
	if !slices.Equal(asked, []string{"jane"}) || kb.created[0].Container != "HOME" {
		t.Fatalf("the default was asked for %v and the page filed in %q, want the person's HOME",
			asked, kb.created[0].Container)
	}
}
