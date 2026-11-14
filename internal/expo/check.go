package expo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// CheckInput is one pasted token and what it must prove for its seat.
type CheckInput struct {
	// Token is the value the person pasted. Never logged or echoed.
	Token string

	// Account is the account name the robot must hold its role on.
	Account string

	// Level is the seat's level, whose role the robot must hold.
	Level string

	// Seat names the seat for a sentence; Robot is the robot the seat already
	// has, by ID, which a new token must belong to ("" for a first token).
	Seat  string
	Robot string

	// OwnerOf answers the seat that already holds robot id, or "".
	OwnerOf func(robotID string) string
}

// Checked is the robot a token proved, which the ledger records.
type Checked struct {
	Robot  Actor
	Role   Role
	Prefix string
}

// Refusal is a pasted token the check turned down: the sentence a person
// reads beside the field and what to do about it. It never carries the token.
type Refusal struct {
	Detail string
	Hint   string

	// dead marks a token Expo no longer accepts at all.
	dead bool
}

func (r *Refusal) Error() string { return "expo: " + r.Detail }

// Check proves a pasted token is a robot of the account at the seat's level
// and no other seat's, with reads only. A refusal is a [*Refusal]; Expo not
// answering is any other error.
func Check(ctx context.Context, c *Client, in CheckInput) (Checked, error) {
	me, err := c.Viewer(ctx)
	if Status(err) == http.StatusUnauthorized {
		return Checked{}, &Refusal{Detail: "Expo did not accept this token.",
			Hint: "Create a new token on the robot's row at Expo and paste it."}
	}
	if err != nil {
		return Checked{}, err
	}
	if !me.Robot() {
		return Checked{}, &Refusal{
			Detail: fmt.Sprintf("This token belongs to the person %s. Each agent needs its own "+
				"robot, so its builds and updates are attributable to it.", nameOf(me)),
			Hint: "Create the token on the robot's row, not under your own account."}
	}
	role, err := c.RoleOn(ctx, in.Account)
	if s := Status(err); s == http.StatusForbidden || s == http.StatusNotFound {
		return Checked{}, &Refusal{
			Detail: fmt.Sprintf("Robot %s holds no role on %s.", nameOf(me), in.Account),
			Hint: fmt.Sprintf("Add the robot on %s's access tokens page, or paste a token of a "+
				"robot on %s.", in.Account, in.Account)}
	}
	if errors.Is(err, ErrRoleUnsaid) {
		return Checked{}, &Refusal{Detail: "Expo did not say this robot's role.",
			Hint: "Nothing was stored. Check the robot's role at Expo and paste the token again; " +
				"if this persists, Expo has retired the role field Crewlet reads."}
	}
	if err != nil {
		return Checked{}, err
	}
	if refusal := roleRefusal(role, in.Level, in.Account); refusal != nil {
		return Checked{}, refusal
	}
	if in.Robot != "" && me.ID != in.Robot {
		return Checked{}, &Refusal{
			Detail: fmt.Sprintf("This token belongs to robot %s, not to %s's robot.", nameOf(me), in.Seat),
			Hint:   "Create the new token on the row of the robot this agent already has."}
	}
	if owner := ownerOf(in, me.ID); owner != "" && owner != in.Seat {
		return Checked{}, &Refusal{
			Detail: fmt.Sprintf("This robot is already %s's.", owner),
			Hint:   "Each agent needs its own robot: add one for this agent at Expo."}
	}
	return Checked{Robot: me, Role: role, Prefix: prefixOf(me, in.Token)}, nil
}

// roleRefusal is why role is not the seat's level, or nil where it is.
func roleRefusal(role Role, level, account string) *Refusal {
	want, ok := RoleOf(level)
	switch {
	case !ok:
		return &Refusal{Detail: fmt.Sprintf("%q is not an Expo level this engine offers.", level),
			Hint: "Choose a level for this agent on the Agents tab."}
	case role.Administers():
		return &Refusal{Detail: "Agents never hold " + roleName(role) + ".",
			Hint: "Change this robot's role at Expo."}
	case role == RoleCustom || levelOf(role) == "":
		return &Refusal{Detail: fmt.Sprintf("This robot holds a %s on %s.", roleName(role), account),
			Hint: "Give it one of Viewer, Runner, Developer or Release Manager at Expo."}
	case role != want:
		return &Refusal{
			Detail: fmt.Sprintf("This robot is a %s on %s, and you chose %s.", roleName(role), account,
				LevelName(level)),
			Hint: fmt.Sprintf("Change its role with Edit robot at Expo, or choose %s here.", roleName(role))}
	default:
		return nil
	}
}

// prefixOf is the prefix Expo shows for the robot's token that value begins
// with, so a later step can name it without the value.
func prefixOf(me Actor, value string) string {
	for _, t := range me.Tokens {
		if t.Prefix != "" && strings.HasPrefix(value, t.Prefix) && !t.Revoked {
			return t.Prefix
		}
	}
	return ""
}

// ownerOf is the seat already holding robot id, or "".
func ownerOf(in CheckInput, id string) string {
	if in.OwnerOf == nil {
		return ""
	}
	return in.OwnerOf(id)
}

// nameOf is an actor's name for a sentence.
func nameOf(a Actor) string {
	if name := strings.TrimSpace(a.Name); name != "" {
		return name
	}
	return a.ID
}

// IsRefusal reports a check that turned the token down, as opposed to Expo
// not answering.
func IsRefusal(err error) (*Refusal, bool) { return errors.AsType[*Refusal](err) }
