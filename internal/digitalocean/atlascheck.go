package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// AtlasTokenName is the name a person gives the atlas's token, so the team's
// token list says whose it is. It labels the token and never identifies it.
const AtlasTokenName = "crewlet-atlas"

// atlasScopes is every scope the atlas's token holds: account:read, which
// every check starts with, and one read per listing the atlas makes. No
// wildcard and no write. spaces:read is absent on purpose: it guards no
// listing, and a bucket is named among its project's resources.
var atlasScopes = []string{
	"account:read", "droplet:read", "kubernetes:read", "database:read", "load_balancer:read",
	"domain:read", "app:read", "project:read",
}

// AtlasScopes is every scope the atlas's token holds.
func AtlasScopes() []string { return slices.Clone(atlasScopes) }

// atlasReadChecks is the read check of each scope the atlas reads with.
func atlasReadChecks() []check {
	var out []check
	for _, ch := range readChecks {
		if slices.Contains(atlasScopes, ch.scope) {
			out = append(out, ch)
		}
	}
	return out
}

// AtlasInput is the atlas's token and the team it must act in.
type AtlasInput struct {
	// Token is the token as pasted or sealed. Never logged or echoed.
	Token string
	// Team is the team's name the company connected.
	Team string
}

// AtlasChecked is what the atlas's token proved.
type AtlasChecked struct {
	Team Team
	// Lacks is every scope of [AtlasScopes] the token does not hold, whose
	// listing is left unread.
	Lacks []string
}

// CheckAtlas proves a token fit to read the team for the atlas, with requests
// that cannot have an effect: it is a personal access token of the company's
// team, and it holds no scope the checks can prove that changes anything or
// reads a secret. A refusal is a [*Refusal]; DigitalOcean not answering is any
// other error. A token cannot read its own scopes, so a write no check proves
// is not seen.
func CheckAtlas(ctx context.Context, c *Client, in AtlasInput) (AtlasChecked, error) {
	if refusal := formatRefusal(in.Token); refusal != nil {
		return AtlasChecked{}, refusal
	}
	team, err := c.Team(ctx)
	switch Status(err) {
	case http.StatusUnauthorized:
		return AtlasChecked{}, &Refusal{dead: true, Detail: "DigitalOcean did not accept this token.",
			Hint: "It may have expired or been deleted. Create a new token for the atlas and paste it."}
	case http.StatusForbidden:
		return AtlasChecked{}, &Refusal{
			Detail: "This token lacks account:read, which Crewlet needs to see which team it reads.",
			Hint:   remake("account:read ticked")}
	}
	if err != nil {
		return AtlasChecked{}, err
	}
	name := strings.TrimSpace(team.Name)
	switch {
	case team.Locked:
		return AtlasChecked{}, &Refusal{Detail: "This DigitalOcean account is locked.",
			Hint: "Resolve it at DigitalOcean, then paste the token again."}
	case name != strings.TrimSpace(in.Team):
		made := "no team"
		if name != "" {
			made = "team " + name
		}
		return AtlasChecked{}, &Refusal{
			Detail: fmt.Sprintf("This token was made in %s, and Crewlet is connected to team %s.", made, in.Team),
			Hint: fmt.Sprintf("Switch to %s in the control panel's team menu, create the token there, "+
				"and delete this one.", in.Team)}
	}
	reads := atlasReadChecks()
	held, err := prove(ctx, c, forbiddenChecks, separatorChecks, administerChecks, reads)
	if errors.Is(err, errTokenRefused) {
		return AtlasChecked{}, &Refusal{dead: true,
			Detail: "DigitalOcean stopped accepting this token while it was checked.",
			Hint:   "Create a new token for the atlas and paste it."}
	}
	if err != nil {
		return AtlasChecked{}, err
	}
	for _, set := range [][]check{forbiddenChecks, separatorChecks, administerChecks} {
		if ch, ok := held.first(set); ok {
			return AtlasChecked{}, &Refusal{
				Detail: fmt.Sprintf("This token can %s (%s). The atlas reads with a token that changes "+
					"nothing and reads no secret.", ch.can, ch.scope),
				Hint: remake("only the read scopes the atlas step lists")}
		}
	}
	out := AtlasChecked{Team: team}
	for _, ch := range reads {
		if !held[ch.scope] {
			out.Lacks = append(out.Lacks, ch.scope)
		}
	}
	return out, nil
}
