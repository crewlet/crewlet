package iam

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestFromIsThreeValued is the arm that keeps a store blip from reading as a
// bad password. "This is who it is", "this is definitively nobody" and "this
// node could not tell" are three facts, and a caller has to be able to tell
// the last two apart without guessing.
func TestFromIsThreeValued(t *testing.T) {
	resolved := Principal{
		ID: uuid.New(), Login: "jane.doe", Kind: KindPerson,
		Stage: StageActive, Grants: []Grant{GrantStateRead},
	}
	storeDown := errors.New("coordination store unreachable")

	cases := []struct {
		name       string
		ctx        context.Context
		want       Resolution
		wantWho    Principal
		wantReason error
	}{{
		name: "a resolver that named somebody",
		ctx:  WithPrincipal(context.Background(), resolved),
		want: Resolved, wantWho: resolved,
	}, {
		name: "a resolver that ran and found nobody",
		ctx:  WithAnonymous(context.Background()),
		want: Anonymous,
	}, {
		name: "a resolver that could not answer",
		ctx:  WithUnresolved(context.Background(), storeDown),
		want: Unknown, wantReason: storeDown,
	}, {
		// SILENCE IS NOT ANONYMOUS. A middleware that did not run and a
		// handler nobody wired through the guard both look exactly like
		// this, and reading either as "definitively nobody" is how a
		// surface decides it is safe to serve because nothing told it
		// otherwise.
		name: "a context no resolver ever touched",
		ctx:  context.Background(),
		want: Unknown, wantReason: ErrUnresolved,
	}, {
		name: "an unresolved with no reason given",
		ctx:  WithUnresolved(context.Background(), nil),
		want: Unknown, wantReason: ErrUnresolved,
	}, {
		name: "no context at all",
		ctx:  nil,
		want: Unknown, wantReason: ErrUnresolved,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			who, how := From(tc.ctx)
			if how != tc.want {
				t.Fatalf("From = %q, want %q", how, tc.want)
			}
			if who.ID != tc.wantWho.ID || who.Login != tc.wantWho.Login {
				t.Fatalf("From principal = %+v, want %+v", who, tc.wantWho)
			}
			if got := Reason(tc.ctx); !errors.Is(got, tc.wantReason) {
				t.Fatalf("Reason = %v, want %v", got, tc.wantReason)
			}
		})
	}

	// THE POINT, STATED AS ITS OWN ASSERTION: the two refusing answers are
	// different values, so a gate can answer "ask again" to one and "you
	// are not who you say" to the other. A From that collapsed them would
	// satisfy every anonymous case above by calling it unknown, or the
	// reverse.
	_, anon := From(WithAnonymous(context.Background()))
	_, unknown := From(WithUnresolved(context.Background(), storeDown))
	if anon == unknown {
		t.Fatalf("anonymous and unknown are the same answer (%q) — "+
			"a store blip is indistinguishable from a caller with no credential", anon)
	}
	if Reason(WithAnonymous(context.Background())) != nil {
		t.Error("an anonymous resolution carries a reason, so a caller cannot use nil to tell them apart")
	}

	// A principal that reached the gate carries nothing on the two
	// refusing answers, so a caller that ignores the resolution refuses
	// rather than admits.
	for _, ctx := range []context.Context{
		WithAnonymous(context.Background()),
		WithUnresolved(context.Background(), storeDown),
		context.Background(),
	} {
		who, _ := From(ctx)
		if who.Can(GrantStateRead) || len(who.Grants) != 0 {
			t.Errorf("an unresolved principal carries grants: %+v", who)
		}
	}
}

// TestAResolutionCannotBeForgedFromOutside pins that the context key is this
// package's own: a value stored under any other key is not a resolution, so no
// other package can attach one without going through the three constructors.
func TestAResolutionCannotBeForgedFromOutside(t *testing.T) {
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, resolution{
		principal: Principal{ID: uuid.New(), Grants: []Grant{GrantSecretRead}},
		how:       Resolved,
	})
	who, how := From(ctx)
	if how != Unknown || who.Can(GrantSecretRead) {
		t.Fatalf("From = (%+v, %q) for a value stored under a foreign key, want an unknown with no grants", who, how)
	}
}

// TestAnUnreadableResolutionIsUnknown covers the value a rolling upgrade could
// put in the context: a resolution this build cannot classify is not a reason
// to believe whatever principal came with it.
func TestAnUnreadableResolutionIsUnknown(t *testing.T) {
	ctx := context.WithValue(context.Background(), principalKey{}, resolution{
		principal: Principal{ID: uuid.New(), Grants: []Grant{GrantFleetOperate}},
		how:       Resolution("attested"),
	})
	who, how := From(ctx)
	if how != Unknown {
		t.Fatalf("From = %q for an unreadable resolution, want %q", how, Unknown)
	}
	if who.Can(GrantFleetOperate) {
		t.Fatal("an unreadable resolution handed its grants through")
	}
	if !errors.Is(Reason(ctx), ErrUnresolved) {
		t.Fatalf("Reason = %v, want %v", Reason(ctx), ErrUnresolved)
	}
}
