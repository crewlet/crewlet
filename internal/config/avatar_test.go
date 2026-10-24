package config_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
)

// AN UNSET AVATAR IS THE LOOK EVERY AGENT SEAT HAD BEFORE IT COULD CHOOSE, part
// by part: the original Crewlet, in purple. A seat that names one part keeps
// the other's default rather than losing it.
func TestAnAbsentAvatarPartIsItsDefault(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		avatar *config.RoleAvatar
		want   config.RoleAvatar
	}{
		{"no avatar", nil, config.RoleAvatar{Character: "crewlet", Color: "purple"}},
		{"an empty one", &config.RoleAvatar{}, config.RoleAvatar{Character: "crewlet", Color: "purple"}},
		{"a character alone", &config.RoleAvatar{Character: "hexlet"}, config.RoleAvatar{Character: "hexlet", Color: "purple"}},
		{"a colour alone", &config.RoleAvatar{Color: "rose"}, config.RoleAvatar{Character: "crewlet", Color: "rose"}},
		{"both", &config.RoleAvatar{Character: "foxlet", Color: "amber"}, config.RoleAvatar{Character: "foxlet", Color: "amber"}},
	} {
		if got := tc.avatar.Resolved(); got != tc.want {
			t.Errorf("%s: Resolved() = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// THE CLOSED SETS ARE THE DESIGN SYSTEM'S, and their FIRST entries are the
// defaults: the dashboard reads an absent part as the first of its list, so
// the two answers for "what does an unset avatar look like" are one.
func TestTheAvatarSetsLeadWithTheirDefaults(t *testing.T) {
	t.Parallel()
	if len(config.AvatarCharacters) != 30 {
		t.Errorf("%d characters, want the design system's thirty", len(config.AvatarCharacters))
	}
	if config.AvatarCharacters[0] != config.DefaultAvatarCharacter {
		t.Errorf("the first character is %q, want the default %q", config.AvatarCharacters[0], config.DefaultAvatarCharacter)
	}
	if config.AvatarColors[0] != config.AvatarPurple {
		t.Errorf("the first colour is %q, want the default %q", config.AvatarColors[0], config.AvatarPurple)
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
