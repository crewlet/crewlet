package config

import (
	"hash/fnv"
	"slices"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// RoleAvatar is how an AGENT seat is drawn wherever the dashboard shows who
// it is: one of the Crewlet characters, in one of the six seat colours, both
// chosen by whoever authors the seat.
//
// PRESENTATION ONLY. Nothing that runs a seat reads it: no prompt, no
// routing, no placement. It travels from the document to the org projection
// and stops there, which is why it is checked by ADMISSION rules rather than
// runnable ones (see [Company.validateAvatars]) and is never carried onto
// [org.Role].
//
// CHOSEN, OR PICKED FROM THE HANDLE. A part left out is picked from the
// seat's handle ([DefaultAvatar]), which is minted once and never changes
// (ADR-0013): a rename keeps the face, and agents that chose nothing are
// still told apart rather than all drawn as one look.
type RoleAvatar struct {
	// Character is which Crewlet the seat is drawn as.
	Character AvatarCharacter `yaml:"character,omitempty" json:"character,omitempty" js:"enum=crewlet|hexlet|peaklet|prismlet|towerlet|bricklet|gemlet|coglet|pluslet|shieldlet|stacklet|sparklet|cloudlet|foxlet|rocketlet|crownlet|cactlet|wisplet|heartlet|flasklet|chiplet|duolet|chatlet|pagelet|archlet|dashlet|octlet|conelet|hivelet|folderlet" desc:"The Crewlet character this agent seat is drawn as. Absent = one picked from the seat's handle."`

	// Color is the colour the character and the seat's card are drawn in.
	Color AvatarColor `yaml:"color,omitempty" json:"color,omitempty" js:"enum=purple|cyan|green|amber|rose|blue" desc:"The colour this agent seat is drawn in. Absent = one picked from the seat's handle."`
}

// AvatarCharacter is one of the Crewlet characters a seat can be drawn as.
type AvatarCharacter string

// AvatarCharacters is the closed set, in the order a picker offers them.
//
// THE DESIGN SYSTEM'S SET, character for character: `@crewlethq/icons` draws
// these and nothing else (`CREWLET_CHARACTERS`). The dashboard keeps the same
// list in `dashboard/src/contract/avatar.ts`, which a dashboard suite holds to
// the installed package and `TestTheAvatarSetsAreTheDashboards` holds to this
// one, so an id the engine admits is always an id the dashboard can draw.
var AvatarCharacters = []AvatarCharacter{
	"crewlet", "hexlet", "peaklet", "prismlet", "towerlet", "bricklet",
	"gemlet", "coglet", "pluslet", "shieldlet", "stacklet", "sparklet",
	"cloudlet", "foxlet", "rocketlet", "crownlet", "cactlet", "wisplet",
	"heartlet", "flasklet", "chiplet", "duolet", "chatlet", "pagelet",
	"archlet", "dashlet", "octlet", "conelet", "hivelet", "folderlet",
}

// AvatarColor is one of the six colours a seat can be drawn in.
type AvatarColor string

// The six colours, each named for the hue the design system draws it in.
const (
	AvatarPurple AvatarColor = "purple"
	AvatarCyan   AvatarColor = "cyan"
	AvatarGreen  AvatarColor = "green"
	AvatarAmber  AvatarColor = "amber"
	AvatarRose   AvatarColor = "rose"
	AvatarBlue   AvatarColor = "blue"
)

// AvatarColors is the closed set: the design system's six node hues
// (`NODE_HUES` in `@crewlethq/ui`), held to the dashboard's copy as
// [AvatarCharacters] is.
var AvatarColors = []AvatarColor{AvatarPurple, AvatarCyan, AvatarGreen, AvatarAmber, AvatarRose, AvatarBlue}

// Valid reports whether the character is one this build draws.
func (c AvatarCharacter) Valid() bool { return slices.Contains(AvatarCharacters, c) }

// Valid reports whether the colour is one of the six.
func (c AvatarColor) Valid() bool { return slices.Contains(AvatarColors, c) }

// DefaultAvatar is the look an agent seat that chose none is drawn with: a
// character and a colour picked from an FNV-1a hash of its handle, mixed by
// murmur3's finalizer so handles a letter apart land far apart. The one
// implementation: every read that serves a seat serves it resolved.
func DefaultAvatar(handle string) RoleAvatar {
	h := fnv.New32a()
	_, _ = h.Write([]byte(handle))
	n := h.Sum32()
	n ^= n >> 16
	n *= 0x85ebca6b
	n ^= n >> 13
	n *= 0xc2b2ae35
	n ^= n >> 16
	chars, colors := uint32(len(AvatarCharacters)), uint32(len(AvatarColors))
	return RoleAvatar{Character: AvatarCharacters[n%chars], Color: AvatarColors[n/chars%colors]}
}

// Resolved is the avatar the seat with this handle is drawn with: each part
// as written, and its [DefaultAvatar] part where it is absent.
func (a *RoleAvatar) Resolved(handle string) RoleAvatar {
	out := DefaultAvatar(handle)
	if a == nil {
		return out
	}
	if a.Character != "" {
		out.Character = a.Character
	}
	if a.Color != "" {
		out.Color = a.Color
	}
	return out
}

// validateAvatars refuses an avatar the dashboard could not draw, and one on
// a seat that is never drawn as a character.
//
// AN ADMISSION RULE rather than a runnable one, because nothing about running
// depends on it: no path that runs a seat reads its avatar. That matters
// most for the closed sets. The design system will grow characters, and a
// revision a newer node admitted with one this build does not know must still
// APPLY here during a rolling upgrade; refused as a runnable rule, it would
// split the fleet over a picture.
//
// A HUMAN SEAT IS DRAWN AS A PERSON: the circle and the person's initials,
// never a character, so an avatar there reads as a setting and does nothing,
// which is the silence the human-seat rules exist to end.
func (c *Company) validateAvatars() error {
	var p problems
	for role, path := range c.EachRole() {
		a := role.Avatar
		if a == nil {
			continue
		}
		if role.Kind == org.KindHuman {
			p.add(at(path, "avatar"), ErrConflict,
				"a human seat is drawn as the person it is, never as a Crewlet "+
					"character. Remove this block, or make the seat an agent seat")
			continue
		}
		if a.Character != "" && !a.Character.Valid() {
			p.add(at(at(path, "avatar"), "character"), ErrUnknownValue,
				"%q (want %s)", a.Character, words.Or(AvatarCharacters))
		}
		if a.Color != "" && !a.Color.Valid() {
			p.add(at(at(path, "avatar"), "color"), ErrUnknownValue,
				"%q (want %s)", a.Color, words.Or(AvatarColors))
		}
	}
	return p.err()
}
