package iam

import (
	"slices"
	"testing"
	"unicode"
)

// THE VOCABULARY IS ADMIN AND ONE PERMISSION PER APP, AND ADMIN IS NO APP.
//
// [AllGrants] is exactly admin followed by [Apps], in that order: an app added
// to the vocabulary and not to the list of apps would be a permission no
// picker ticks under Admin and no administrator holds through [Expand], and
// admin listed among the apps would make every agent seat — which holds the
// apps — an administrator. Mutation: put admin in Apps and both assertions go
// red; drop an app from AllGrants and the first does.
func TestEveryAppIsAGrantAndAdminIsNot(t *testing.T) {
	t.Parallel()
	want := append([]Grant{GrantAdmin}, Apps...)
	if !slices.Equal(AllGrants, want) {
		t.Errorf("AllGrants = %v, want admin followed by every app: %v", AllGrants, want)
	}
	if slices.Contains(Apps, GrantAdmin) || GrantAdmin.IsApp() {
		t.Error("admin is listed as an app, so every seat that holds the apps " +
			"runs the company")
	}
	if len(Apps) == 0 {
		t.Fatal("no app is declared, so every assertion about apps certifies nothing")
	}
	for _, app := range Apps {
		if !app.Valid() || !app.IsApp() {
			t.Errorf("%q is declared an app and does not validate as one", app)
		}
	}
	if Grant("swarm").IsApp() || Grant("").IsApp() {
		t.Error("a name this build does not know reads as an app")
	}
}

// EXPAND ADDS EVERY APP EXACTLY WHEN ADMIN IS HELD, AND NOTHING ELSE.
//
// It is the one rule every comparison of permissions goes through, so each of
// its promises is a property somebody relies on: the answer never aliases the
// row it was handed (a caller appending to it must not edit the row), an
// unknown name rides through untouched (it is kept in the row and ignored), an
// app already held is not added twice, nothing is added without admin, and a
// second expansion changes nothing. Mutation: skip one app in the loop and the
// admin case goes red; append even without admin and the control does.
func TestExpandAddsEveryAppExactlyWhenAdminIsHeld(t *testing.T) {
	t.Parallel()
	const future = Grant("swarm")
	for _, c := range []struct {
		name string
		in   []Grant
		want []Grant
	}{
		{"nothing", nil, []Grant{}},
		{"admin alone", []Grant{GrantAdmin},
			append([]Grant{GrantAdmin}, Apps...)},
		{"admin beside an app it already includes",
			[]Grant{GrantDoclet, GrantAdmin},
			[]Grant{GrantDoclet, GrantAdmin, GrantWorklet, GrantArtifacts, GrantChatlet}},
		{"an unknown name is carried through", []Grant{future, GrantAdmin},
			[]Grant{future, GrantAdmin, GrantWorklet, GrantDoclet, GrantArtifacts, GrantChatlet}},
		{"duplicates in the row stay as given", []Grant{GrantWorklet, GrantWorklet},
			[]Grant{GrantWorklet, GrantWorklet}},
		// THE CONTROL: no admin, nothing added.
		{"an app alone", []Grant{GrantWorklet}, []Grant{GrantWorklet}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := slices.Clone(c.in)
			got := Expand(in)
			if !slices.Equal(got, c.want) || got == nil {
				t.Fatalf("Expand(%v) = %#v, want %#v", c.in, got, c.want)
			}
			if !slices.Equal(in, c.in) {
				t.Errorf("Expand edited the row it was handed: %v, was %v", in, c.in)
			}
			if again := Expand(got); !slices.Equal(again, got) {
				t.Errorf("Expand is not idempotent: %v, then %v", got, again)
			}
			if len(in) > 0 && len(got) > 0 {
				got[0] = "edited"
				if in[0] == "edited" {
					t.Error("the answer aliases the row: a caller appending to it " +
						"edits what it read")
				}
			}
		})
	}
}

// EVERY PERMISSION IS ONE LOWERCASE WORD.
//
// These strings land in rows, in Tier A files and on the dashboard's picker,
// so a stray capital or separator is a permission nobody can type and nobody
// can see is missing — and a `domain:verb` spelling would be the eleven-grant
// vocabulary creeping back one name at a time.
func TestEveryGrantIsOneLowercaseWord(t *testing.T) {
	t.Parallel()
	for _, g := range AllGrants {
		if g == "" {
			t.Error("the zero is declared as a permission")
			continue
		}
		for _, r := range string(g) {
			if !unicode.IsLower(r) {
				t.Errorf("%q is not one lowercase word", g)
				break
			}
		}
	}
}
