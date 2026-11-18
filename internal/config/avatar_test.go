package config_test

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
)

// AN ABSENT PART IS PICKED FROM THE SEAT'S HANDLE, part by part: a seat that
// names one part keeps it and takes the other from its handle.
func TestAnAbsentAvatarPartIsPickedFromTheHandle(t *testing.T) {
	t.Parallel()
	const handle = "qa-engineer"
	picked := config.RoleAvatar{Character: "flasklet", Color: config.AvatarAmber}
	for _, tc := range []struct {
		name   string
		avatar *config.RoleAvatar
		want   config.RoleAvatar
	}{
		{"no avatar", nil, picked},
		{"an empty one", &config.RoleAvatar{}, picked},
		{"a character alone", &config.RoleAvatar{Character: "hexlet"}, config.RoleAvatar{Character: "hexlet", Color: picked.Color}},
		{"a colour alone", &config.RoleAvatar{Color: "rose"}, config.RoleAvatar{Character: picked.Character, Color: "rose"}},
		{"both", &config.RoleAvatar{Character: "foxlet", Color: "amber"}, config.RoleAvatar{Character: "foxlet", Color: "amber"}},
	} {
		if got := tc.avatar.Resolved(handle); got != tc.want {
			t.Errorf("%s: Resolved(%q) = %+v, want %+v", tc.name, handle, got, tc.want)
		}
	}
}

// THE PICK IS PINNED: a change to it redraws every agent that chose nothing.
func TestTheDefaultLookIsPinned(t *testing.T) {
	t.Parallel()
	for handle, want := range map[string]config.RoleAvatar{
		"product-manager":           {Character: "wisplet", Color: config.AvatarGreen},
		"senior-backend-engineer":   {Character: "sparklet", Color: config.AvatarRose},
		"qa-engineer":               {Character: "flasklet", Color: config.AvatarAmber},
		"site-reliability-engineer": {Character: "heartlet", Color: config.AvatarAmber},
		"security-engineer":         {Character: "heartlet", Color: config.AvatarPurple},
		"release-manager":           {Character: "flasklet", Color: config.AvatarGreen},
	} {
		if got := config.DefaultAvatar(handle); got != want {
			t.Errorf("DefaultAvatar(%q) = %+v, want %+v", handle, got, want)
		}
	}
}

// AGENTS THAT CHOSE NOTHING ARE TOLD APART: over a company's worth of handles
// the pick reaches most of the family rather than drawing them as one look.
func TestTheDefaultLookSpreadsAcrossTheFamily(t *testing.T) {
	t.Parallel()
	characters := map[config.AvatarCharacter]bool{}
	colors := map[config.AvatarColor]bool{}
	for i := range 60 {
		look := config.DefaultAvatar("seat-" + strconv.Itoa(i))
		characters[look.Character] = true
		colors[look.Color] = true
		if !look.Character.Valid() || !look.Color.Valid() {
			t.Fatalf("DefaultAvatar picked %+v, outside the sets", look)
		}
	}
	if len(characters) < 20 || len(colors) != len(config.AvatarColors) {
		t.Errorf("60 handles drew %d characters and %d colours, want at least 20 and all %d", len(characters), len(colors), len(config.AvatarColors))
	}
}

// THE CLOSED SETS ARE THE DESIGN SYSTEM'S, each value once.
func TestTheAvatarSetsAreClosed(t *testing.T) {
	t.Parallel()
	if len(config.AvatarCharacters) != 30 {
		t.Errorf("%d characters, want the design system's thirty", len(config.AvatarCharacters))
	}
	for _, set := range [][]string{strs(config.AvatarCharacters), strs(config.AvatarColors)} {
		sorted := slices.Clone(set)
		slices.Sort(sorted)
		if len(slices.Compact(sorted)) != len(set) {
			t.Errorf("a set lists one value twice: %v", set)
		}
	}
}

// AN AVATAR THE DASHBOARD CANNOT DRAW IS REFUSED ON A WRITE, where it was
// written, and APPLIED from a stored revision: it is an admission rule, so a
// character a newer build admitted never splits a fleet over a picture.
func TestAnAvatarTheDashboardCannotDrawIsAnAdmissionRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, avatar, path string
	}{
		{"an unknown character", "{character: starfish}", "roles[0].avatar.character"},
		{"an unknown colour", "{character: hexlet, color: teal}", "roles[0].avatar.color"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := parsed(t, "name: Acme\nroles:\n  - name: Dev\n    avatar: "+tc.avatar+"\n")
			if err := cfg.ValidateRunnable(); err != nil {
				t.Errorf("ValidateRunnable() = %v, want nil: nothing about running reads an avatar", err)
			}
			problems := config.Problems(cfg.ValidateAdmission())
			var got []located
			for _, p := range problems {
				got = append(got, located{p.Path, p.Kind, p.Seat, p.Unit})
			}
			if want := []located{{tc.path, "unknown_value", "dev", ""}}; !reflect.DeepEqual(got, want) {
				t.Errorf("admission problems = %+v, want %+v", got, want)
			}
			if len(problems) == 1 && !strings.Contains(problems[0].Message, "want ") {
				t.Errorf("the refusal does not say what is accepted: %s", problems[0].Message)
			}
		})
	}
}

// AN AGENT SEAT MAY NAME EITHER PART, BOTH OR NEITHER, and every one of the
// thirty characters in every one of the six colours is accepted.
func TestEveryCharacterInEveryColourIsAccepted(t *testing.T) {
	t.Parallel()
	var doc strings.Builder
	doc.WriteString("name: Acme\nroles:\n")
	n := 0
	for _, character := range config.AvatarCharacters {
		for _, color := range config.AvatarColors {
			doc.WriteString("  - name: Seat " + string(rune('A'+n/26)) + string(rune('A'+n%26)) + "\n")
			doc.WriteString("    avatar: {character: " + string(character) + ", color: " + string(color) + "}\n")
			n++
		}
	}
	doc.WriteString("  - name: Partial\n    avatar: {color: green}\n  - name: Plain\n")
	if err := parsed(t, doc.String()).Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

// THE SCHEMA, THE ENGINE AND THE DASHBOARD NAME ONE SET EACH. The dashboard
// keeps the lists in its contract (`dashboard/src/contract/avatar.ts`), where a
// suite of its own holds them to the design system's packages; this holds them
// to the engine's. A character the engine admits and the dashboard cannot draw
// is a seat drawn as nothing, and one the dashboard offers that the engine
// refuses is a pick whose every save fails.
func TestTheAvatarSetsAreTheDashboards(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"AVATAR_CHARACTERS", strs(config.AvatarCharacters)},
		{"AVATAR_COLORS", strs(config.AvatarColors)},
	} {
		body, err := clientsource.Literal(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		got := clientsource.Strings(body)
		if len(got) == 0 {
			t.Fatalf("the dashboard's %s names nothing, so this gate certifies nothing", tc.name)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("the dashboard's %s is %v, want the engine's set, in its order: %v", tc.name, got, tc.want)
		}
	}
}

// strs is a closed set as plain strings.
func strs[T ~string](set []T) []string {
	out := make([]string, len(set))
	for i, v := range set {
		out[i] = string(v)
	}
	return out
}
