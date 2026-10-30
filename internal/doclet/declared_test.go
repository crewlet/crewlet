package doclet_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/doclet"
)

// A PAGE IS CREATED ONLY IN A SPACE THE COMPANY DECLARES — by a person exactly
// as by an agent.
//
// A container is a space the org chart names: who leads it, who is woken by
// what happens in it, whose knowledge it is. A page filed under a key nothing
// declares belongs to nobody, and a typo of a real key splits a team's
// knowledge in two with nothing to say so — which is a fact about the KEY, so
// unlike the reserved containers this binds every actor. The refusal names the
// key, says how a space is declared and lists the spaces there are, because
// that is everything the caller needs to file the page where it belongs.
func TestAPageIsCreatedOnlyInADeclaredSpace(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for name, actor := range map[string]doclet.Actor{
		"a person": author("ana"), "an agent": agent("pm"),
	} {
		_, err := r.store.Create(t.Context(), actor, doclet.NewPage{
			Container: "engg", Title: "Deploy notes", Body: "x",
		})
		if !errors.Is(err, doclet.ErrInvalid) {
			t.Fatalf("%s's page in an undeclared space gave %v, want ErrInvalid",
				name, err)
		}
		said := doclet.Sentence(err)
		// THE SET IS OFFERED AS ONE, quoted like the key the caller typed,
		// rather than as a column of names after "one of:".
		for _, want := range []string{`"ENGG" isn't a space this company declares`,
			"`space:`", "knowledge.root_space",
			`write in "ENG", "HOME", "OPS", "PROD", "SUITE" or "TS"`} {
			if !strings.Contains(said, want) {
				t.Errorf("%s's refusal reads %q, which does not say %q", name, said, want)
			}
		}
		// AND A DECLARED ONE TAKES IT, under any spelling of its key.
		if _, err := r.store.Create(t.Context(), actor, doclet.NewPage{
			Container: " eng ", Title: "Deploy notes by " + actor.Handle, Body: "x",
		}); err != nil {
			t.Fatalf("%s could not create a page in a declared space: %v", name, err)
		}
		r.drain()
	}
}

// A STORE THAT DECLARES NOTHING CREATES NO PAGE, and still makes containers.
//
// The meaningful zero: a store built with no reserved seam at all, and one
// whose seam declares no space, both refuse every create — for a person and an
// agent — because a seam whose absence meant "anywhere" would be how a surface
// wired later filed pages under keys nobody named. Only a create asks, so the
// chart's own container apply is untouched by it.
func TestAStoreThatDeclaresNothingCreatesNoPage(t *testing.T) {
	t.Parallel()
	for name, reserved := range map[string]func() doclet.Reserved{
		"no seam":            nil,
		"an empty declaring": func() doclet.Reserved { return doclet.Reserved{} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTripReserving(t, openLog(t), openNodeStore(t, "node.db"),
				"node-a", reserved)
			if _, _, err := r.store.EnsureContainer(t.Context(), activation(0),
				"ENG", "Engineering", ""); err != nil {
				t.Fatalf("the chart's container apply was refused: %v", err)
			}
			r.drain()
			for who, actor := range map[string]doclet.Actor{
				"a person": author("ana"), "an agent": agent("pm"),
			} {
				_, err := r.store.Create(t.Context(), actor, doclet.NewPage{
					Container: "ENG", Title: "Deploy notes", Body: "x",
				})
				if !errors.Is(err, doclet.ErrInvalid) {
					t.Fatalf("%s created a page on a store that declares nothing: %v",
						who, err)
				}
				if said := doclet.Sentence(err); !strings.Contains(said,
					"declares none yet") {
					t.Errorf("%s's refusal reads %q, want it to say the company "+
						"declares no space", who, said)
				}
			}
		})
	}
}
