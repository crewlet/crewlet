package expo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// AtlasRobotName is the name a person gives the atlas's robot at Expo, so
// the account's robot list says whose it is. It labels the robot and never
// identifies it.
const AtlasRobotName = "crewlet-atlas"

// CheckAtlas proves a token fit to read account for the atlas, with reads
// only: it is a robot's, and the robot holds exactly the Viewer role on the
// account, the one role that changes nothing. A refusal is a [*Refusal]; Expo
// not answering is any other error.
func CheckAtlas(ctx context.Context, c *Client, account string) (Actor, error) {
	me, err := c.Viewer(ctx)
	if Status(err) == http.StatusUnauthorized {
		return Actor{}, &Refusal{dead: true, Detail: "Expo did not accept this token.",
			Hint: "Create a new token on the atlas's robot at Expo and paste it."}
	}
	if err != nil {
		return Actor{}, err
	}
	if !me.Robot() {
		return Actor{}, &Refusal{
			Detail: fmt.Sprintf("This token belongs to the person %s. The atlas reads as a robot of its "+
				"own, so its reads are nobody else's.", nameOf(me)),
			Hint: "Add a robot named " + AtlasRobotName + " with the Viewer role and create the token on its row."}
	}
	role, err := c.RoleOn(ctx, account)
	if s := Status(err); s == http.StatusForbidden || s == http.StatusNotFound {
		return Actor{}, &Refusal{
			Detail: fmt.Sprintf("Robot %s holds no role on %s.", nameOf(me), account),
			Hint:   fmt.Sprintf("Add the robot on %s's access tokens page with the Viewer role.", account)}
	}
	if errors.Is(err, ErrRoleUnsaid) {
		return Actor{}, &Refusal{Detail: "Expo did not say this robot's role.",
			Hint: "Nothing is read. Check the robot's role at Expo and paste the token again; if this " +
				"persists, Expo has retired the role field Crewlet reads."}
	}
	if err != nil {
		return Actor{}, err
	}
	if role != RoleViewOnly {
		return Actor{}, &Refusal{
			Detail: fmt.Sprintf("Robot %s holds the %s role on %s. The atlas reads as a Viewer, which "+
				"changes nothing.", nameOf(me), roleName(role), account),
			Hint: "Change its role to Viewer with Edit robot at Expo, then paste the token again."}
	}
	return me, nil
}
