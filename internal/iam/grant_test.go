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
			[]Grant{GrantDoclet, GrantAdmin, GrantWorklet, GrantArtifacts, GrantChatlet, GrantGitlet,
				GrantAtlas}},
		{"an unknown name is carried through", []Grant{future, GrantAdmin},
			[]Grant{future, GrantAdmin, GrantWorklet, GrantDoclet, GrantArtifacts, GrantChatlet,
				GrantGitlet, GrantAtlas}},
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

// ATLAS IS THE LAST APP, AND EVERY APP BEFORE IT KEEPS ITS PLACE.
//
// The order of [Apps] is what [Expand] stores in a row and what every list of
// an administrator's permissions reads in, so an app is appended and never
// inserted (ADR-0127 appended atlas after gitlet, as ADR-0110 appended gitlet
// after chatlet). The prefix is pinned by value rather than read off the list,
// because a list compared with itself cannot move. Mutation: insert atlas
// before gitlet and the order goes red; drop it from AllGrants and the
// vocabulary does.
func TestAtlasIsTheLastApp(t *testing.T) {
	t.Parallel()
	want := []Grant{GrantWorklet, GrantDoclet, GrantArtifacts, GrantChatlet, GrantGitlet, GrantAtlas}
	if !slices.Equal(Apps, want) {
		t.Fatalf("Apps = %v, want %v: an app moved, so every row Expand stored reads in "+
			"another order", Apps, want)
	}
	if GrantAtlas != "atlas" || !GrantAtlas.Valid() || !GrantAtlas.IsApp() {
		t.Errorf("atlas is not a valid app: %q", GrantAtlas)
	}
	if got := AllGrants[len(AllGrants)-1]; got != GrantAtlas {
		t.Errorf("the vocabulary ends with %q, want atlas appended last", got)
	}
}

// ATLAS IS GIVEN ONLY WHEN SOMEBODY CHOOSES TO, AND EVERY OTHER APP BY DEFAULT.
//
// A person invited or created with nothing said holds [DefaultApps]: every app
// but atlas, whose map of servers and addresses is a decision about the
// person (ADR-0127). An administrator still holds it, through [Expand].
// Mutation: drop atlas from OptInApps and a newcomer is given the map; add
// another app and a newcomer loses a tool the company expects them to use.
func TestAtlasIsOptIn(t *testing.T) {
	t.Parallel()
	want := []Grant{GrantWorklet, GrantDoclet, GrantArtifacts, GrantChatlet, GrantGitlet}
	if !slices.Equal(DefaultApps, want) {
		t.Errorf("DefaultApps = %v, want every app but atlas: %v", DefaultApps, want)
	}
	if !slices.Equal(OptInApps, []Grant{GrantAtlas}) {
		t.Errorf("OptInApps = %v, want atlas alone", OptInApps)
	}
	for _, app := range OptInApps {
		if !app.IsApp() {
			t.Errorf("%q is opt-in and is not an app", app)
		}
	}
	if !slices.Contains(Expand([]Grant{GrantAdmin}), GrantAtlas) {
		t.Error("an administrator does not hold atlas")
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

// WHAT A REQUESTER USES IS APPS, AND NEVER ADMIN: admin includes every app, and
// running the company is nothing a turn may do for somebody (ADR-0109). A name
// this build does not know grants nothing, and the answer never aliases the
// row it was read from.
func TestAppsOfIsEveryAppAdminIncludesAndNeverAdmin(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		held []Grant
		want []Grant
	}{
		{nil, []Grant{}},
		{[]Grant{GrantAdmin}, Apps},
		{[]Grant{GrantDoclet, "teleport", GrantWorklet}, []Grant{GrantDoclet, GrantWorklet}},
		{[]Grant{GrantChatlet, GrantAdmin},
			[]Grant{GrantChatlet, GrantWorklet, GrantDoclet, GrantArtifacts, GrantGitlet, GrantAtlas}},
	} {
		held := slices.Clone(c.held)
		got := AppsOf(held)
		if !slices.Equal(got, c.want) {
			t.Errorf("AppsOf(%v) = %v, want %v", c.held, got, c.want)
		}
		if slices.Contains(got, GrantAdmin) {
			t.Errorf("AppsOf(%v) carries admin", c.held)
		}
		if len(got) > 0 {
			got[0] = "edited"
			if !slices.Equal(held, c.held) {
				t.Errorf("AppsOf(%v) aliases the row it read: an edit to the answer "+
					"changed it to %v", c.held, held)
			}
		}
	}
}

// A WRITE RECORDS APPS FOR A CREDENTIAL ACTING AS NO SEAT, AND FOR NOBODY ELSE:
// a seat's or a person's record names the seat, whose holder the node running
// a turn reads, and apps on it would be a second, stale answer to that.
func TestActorAppsAreACredentialsAlone(t *testing.T) {
	t.Parallel()
	every := []Grant{GrantAdmin}
	for _, c := range []struct {
		name string
		p    Principal
		want []Grant
	}{
		{"a Tier A token", Principal{Kind: KindMachine, Login: TokenLogin("ci"),
			Grants: []Grant{GrantWorklet}}, []Grant{GrantWorklet}},
		{"an admin token", Principal{Kind: KindMachine, Login: TokenLogin("ops"), Grants: every}, Apps},
		{"a person on their seat", Principal{Kind: KindPerson, Login: "ana", Seat: "ana",
			Grants: every}, nil},
		{"an agent seat", Principal{Kind: KindSeat, Seat: "swe", Grants: every}, nil},
		{"the engine", Principal{Kind: KindEngine, Login: "node-a", Grants: every}, nil},
	} {
		if got := ActorApps(c.p); !slices.Equal(got, c.want) {
			t.Errorf("%s records apps %v, want %v", c.name, got, c.want)
		}
	}
}
