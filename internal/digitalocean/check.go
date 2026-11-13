package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// tokenPattern is a personal access token: dop_v1_ and 64 hexadecimal digits.
// DigitalOcean's OAuth access and refresh tokens begin doo_v1_ and dor_v1_.
// Compiled at first use, not at init: Gitlet's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go);
// the bounded repeat compiles to a program holding every count, about 21 KB.
var tokenPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^dop_v1_[0-9a-f]{64}$`)
})

// CheckInput is one pasted token and what it must prove for its seat.
type CheckInput struct {
	// Token is the value the person pasted. Never logged or echoed.
	Token string

	// Team is the team's name the company connected, and TeamUUID the team
	// another seat's token already proved, "" before the first.
	Team, TeamUUID string

	// Level is the seat's level, whose scopes the token must hold.
	Level string

	// HeldBy answers the seat already holding the same token, or "".
	HeldBy func(token string) string
}

// Checked is what a token proved.
type Checked struct {
	Account Account
	Level   string
}

// Refusal is a pasted token the check turned down: the sentence a person reads
// beside the field and what to do about it. It never carries the token.
type Refusal struct {
	Detail string
	Hint   string

	// dead marks a token DigitalOcean no longer accepts at all.
	dead bool
}

func (r *Refusal) Error() string { return "digitalocean: " + r.Detail }

// IsRefusal reports a check that turned the token down, as opposed to
// DigitalOcean not answering.
func IsRefusal(err error) (*Refusal, bool) { return errors.AsType[*Refusal](err) }

// remake is the hint for a token whose scopes are wrong: DigitalOcean cannot
// edit a token's scopes, so the remedy is always a new one.
func remake(what string) string {
	return "Delete this token at DigitalOcean, then create a new one with Custom Scopes and " + what + "."
}

// Check proves a pasted token is a personal access token of the company's
// team, holding exactly its level's scopes as far as they can be proven and no
// refused scope, and no other seat's. A refusal is a [*Refusal]; DigitalOcean
// not answering is any other error.
func Check(ctx context.Context, c *Client, in CheckInput) (Checked, error) {
	if _, _, ok := find(in.Level); !ok {
		return Checked{}, &Refusal{Detail: fmt.Sprintf("%q is not a DigitalOcean level this engine offers.", in.Level),
			Hint: "Choose a level for this agent on the Agents tab."}
	}
	if refusal := formatRefusal(in.Token); refusal != nil {
		return Checked{}, refusal
	}
	if in.HeldBy != nil {
		if seat := in.HeldBy(in.Token); seat != "" {
			return Checked{}, &Refusal{Detail: fmt.Sprintf("This token is already %s's.", seat),
				Hint: "Each agent needs its own token: create one for this agent."}
		}
	}
	acct, err := c.Account(ctx)
	switch Status(err) {
	case http.StatusUnauthorized:
		return Checked{}, &Refusal{Detail: "DigitalOcean did not accept this token.",
			Hint: "It may have expired or been deleted. Create a new token and paste it."}
	case http.StatusForbidden:
		return Checked{}, &Refusal{
			Detail: "This token lacks account:read, which Crewlet needs to see whose token it is.",
			Hint:   remake("account:read ticked")}
	}
	if err != nil {
		return Checked{}, err
	}
	if refusal := accountRefusal(acct, in); refusal != nil {
		return Checked{}, refusal
	}
	held, err := prove(ctx, c, forbiddenChecks, readChecks, separatorChecks, administerChecks)
	if errors.Is(err, errTokenRefused) {
		return Checked{}, &Refusal{Detail: "DigitalOcean stopped accepting this token while it was checked.",
			Hint: "Create a new token and paste it."}
	}
	if err != nil {
		return Checked{}, err
	}
	if refusal := scopeRefusal(held, in.Level); refusal != nil {
		return Checked{}, refusal
	}
	return Checked{Account: acct, Level: in.Level}, nil
}

// formatRefusal is why value is not a personal access token, or nil.
func formatRefusal(value string) *Refusal {
	switch {
	case tokenPattern().MatchString(value):
		return nil
	case strings.HasPrefix(value, "doo_v1_"), strings.HasPrefix(value, "dor_v1_"):
		return &Refusal{Detail: "This is an OAuth token, which expires within hours or days.",
			Hint: "Create a personal access token on the token page and paste it."}
	default:
		return &Refusal{Detail: "This is not a DigitalOcean personal access token.",
			Hint: "Copy the token that starts with dop_v1_, which DigitalOcean shows once when you generate it."}
	}
}

// accountRefusal is why the token's account or team is not the company's, or
// nil.
func accountRefusal(acct Account, in CheckInput) *Refusal {
	team := strings.TrimSpace(acct.Team.Name)
	switch {
	case acct.Locked():
		return &Refusal{Detail: "This DigitalOcean account is locked.",
			Hint: "Resolve it at DigitalOcean, then paste the token again."}
	case team != strings.TrimSpace(in.Team):
		made := "no team"
		if team != "" {
			made = "team " + team
		}
		return &Refusal{
			Detail: fmt.Sprintf("This token was made in %s, and Crewlet is connected to team %s.", made, in.Team),
			Hint: fmt.Sprintf("Switch to %s in the control panel's team menu, create the token there, "+
				"and delete this one.", in.Team)}
	case in.TeamUUID != "" && acct.Team.UUID != in.TeamUUID:
		return &Refusal{
			Detail: fmt.Sprintf("This token was made in a team named %s whose Team ID is %s, and the "+
				"other agents' tokens are in Team ID %s.", team, acct.Team.UUID, in.TeamUUID),
			Hint: "Create the token in the team the other agents' tokens are in (Settings, Team shows " +
				"its Team ID), and delete this one."}
	default:
		return nil
	}
}

// scopeRefusal is why the proven scopes are not level's, or nil. A refused
// scope answers first, whatever else the token holds.
func scopeRefusal(held Held, level string) *Refusal {
	if ch, ok := held.first(forbiddenChecks); ok {
		detail := fmt.Sprintf("This token can %s (%s). Agents never hold it.", ch.can, ch.scope)
		if ch.scope == "billing:read" {
			detail = "This token can read billing (billing:read): it is Full Access or Read Only, or " +
				"billing was ticked. Agents never hold it."
		}
		return &Refusal{Detail: detail, Hint: remake(ch.scope + " unticked")}
	}
	name := LevelName(level)
	if ch, ok := held.missing(readChecks); ok {
		return &Refusal{Detail: fmt.Sprintf("This token lacks %s, which %s needs.", ch.scope, name),
			Hint: remake("every scope the list names")}
	}
	switch level {
	case levels[0].id:
		if ch, ok := held.first(separatorChecks); ok {
			return &Refusal{
				Detail: fmt.Sprintf("This token can %s (%s), and you chose Read.", ch.can, ch.scope),
				Hint:   "Delete it at DigitalOcean and create one with the Read scopes only, or choose Operate here."}
		}
	default:
		if ch, ok := held.missing(separatorChecks); ok {
			return &Refusal{Detail: fmt.Sprintf("This token lacks %s, which %s needs.", ch.scope, name),
				Hint: remake("every scope the list names")}
		}
	}
	if level == levels[2].id {
		if ch, ok := held.missing(administerChecks); ok {
			return &Refusal{Detail: fmt.Sprintf("This token lacks %s, which Administer needs.", ch.scope),
				Hint: remake("every scope the list names")}
		}
		return nil
	}
	if ch, ok := held.first(administerChecks); ok {
		return &Refusal{
			Detail: fmt.Sprintf("This token can %s (%s), which only Administer grants.", ch.can, ch.scope),
			Hint:   "Delete it at DigitalOcean and create one without it, or choose Administer here."}
	}
	return nil
}
