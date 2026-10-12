package config

import (
	"slices"

	"github.com/crewlet/crewlet/internal/org"
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
// CHOSEN, NEVER DERIVED. A colour hashed from a seat's name named nothing:
// rename the seat and its colour changed. A character and a colour somebody
// picked survive a rename, because neither was made from one.
//
// EITHER PART MAY BE LEFT OUT, and that is a value rather than a gap: an
// absent character is the original Crewlet and an absent colour is purple,
// which is how every agent seat was drawn before it could choose
// ([RoleAvatar.Resolved]). So a seat that says nothing keeps the look it had.
type RoleAvatar struct {
	// Character is which Crewlet the seat is drawn as.
	Character AvatarCharacter `yaml:"character,omitempty" json:"character,omitempty" js:"enum=crewlet|hexlet|peaklet|prismlet|towerlet|bricklet|gemlet|coglet|pluslet|shieldlet|stacklet|sparklet|cloudlet|foxlet|rocketlet|crownlet|cactlet|wisplet|heartlet|flasklet|chiplet|duolet|chatlet|pagelet|archlet|dashlet|octlet|conelet|hivelet|folderlet" desc:"The Crewlet character this agent seat is drawn as. Absent = crewlet, the original."`

	// Color is the colour the character and the seat's card are drawn in.
	Color AvatarColor `yaml:"color,omitempty" json:"color,omitempty" js:"enum=purple|cyan|green|amber|rose|blue" desc:"The colour this agent seat is drawn in. Absent = purple."`
}

// AvatarCharacter is one of the Crewlet characters a seat can be drawn as.
type AvatarCharacter string

// DefaultAvatarCharacter is the original Crewlet: the mark itself, which every
// agent seat was drawn as before a seat could choose.
const DefaultAvatarCharacter AvatarCharacter = "crewlet"

// AvatarCharacters is the closed set, in the order a picker offers them.
//
// THE DESIGN SYSTEM'S SET, character for character: `@crewlethq/icons` draws
// these and nothing else (`CREWLET_CHARACTERS`). The dashboard keeps the same
// list in `dashboard/src/contract/avatar.ts`, which a dashboard suite holds to
// the installed package and `TestTheAvatarSetsAreTheDashboards` holds to this
// one, so an id the engine admits is always an id the dashboard can draw.
var AvatarCharacters = []AvatarCharacter{
	DefaultAvatarCharacter,
	"hexlet", "peaklet", "prismlet", "towerlet", "bricklet",
	"gemlet", "coglet", "pluslet", "shieldlet", "stacklet", "sparklet",
	"cloudlet", "foxlet", "rocketlet", "crownlet", "cactlet", "wisplet",
	"heartlet", "flasklet", "chiplet", "duolet", "chatlet", "pagelet",
	"archlet", "dashlet", "octlet", "conelet", "hivelet", "folderlet",
}

// AvatarColor is one of the six colours a seat can be drawn in.
type AvatarColor string

// The six colours, each named for the hue the design system draws it in.
const (
	// AvatarPurple is the DEFAULT: the brand's own hue, which every agent
	// seat was drawn in before a seat could choose.
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

// Resolved is the avatar the seat is drawn with: each part as written, and
// the default where it is absent. A nil avatar resolves to both defaults.
func (a *RoleAvatar) Resolved() RoleAvatar {
	out := RoleAvatar{Character: DefaultAvatarCharacter, Color: AvatarPurple}
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
				"%q (want %s)", a.Character, names(AvatarCharacters))
		}
		if a.Color != "" && !a.Color.Valid() {
			p.add(at(at(path, "avatar"), "color"), ErrUnknownValue,
				"%q (want %s)", a.Color, names(AvatarColors))
		}
	}
	return p.err()
}
