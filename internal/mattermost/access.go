package mattermost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// What an agent's bot may do at Mattermost, in Mattermost's own terms.
//
// A BOT ACCOUNT'S ROLE AND TWO OPTIONAL PERMISSIONS, which is the whole of
// what Mattermost offers per account: the System Console's bot page names
// them Member or System Admin, plus post:all and post:channels. The API
// spells them as system role names on the user, and those names are what a
// seat's document stores, so the picker and the vendor agree by construction
// rather than through a Crewlet word for each.

// The system roles a bot account can hold, as Mattermost's API spells them
// (`PUT /api/v4/users/{id}/roles`).
const (
	// RoleSystemUser is every account's base role. Mattermost labels it
	// Member.
	RoleSystemUser = "system_user"
	// RolePostAllPublic is the post:channels permission: post in every
	// public channel, joined or not.
	RolePostAllPublic = "system_post_all_public"
	// RolePostAll is the post:all permission: post in every channel,
	// private ones included.
	RolePostAll = "system_post_all"
	// RoleSystemAdmin is System Admin, which grants every other system
	// administration privilege with it.
	RoleSystemAdmin = "system_admin"
)

// RoleOption is one access a seat may hold, as the Agents tab offers it.
type RoleOption struct {
	// Roles is the role list the seat's document stores and the vendor
	// is set to, in canonical order.
	Roles []string
	// Label is Mattermost's own name for the option.
	Label string
	// Hint is the one line a picker puts under the label.
	Hint string
	// Administration marks the option D10 steps up: a raise to it needs a
	// fresh step-up and carries a warning in the review.
	Administration bool
}

// roleOptions are the four accesses, IN MATTERMOST'S OWN ORDER of what they
// grant, which is the only order a "raise" is derived from.
//
//nolint:gochecknoglobals // an immutable table, not state
var roleOptions = []RoleOption{
	{
		Roles: []string{RoleSystemUser},
		Label: "Member",
		Hint:  "Reads and posts in the channels it has joined.",
	},
	{
		Roles: []string{RoleSystemUser, RolePostAllPublic},
		Label: "Member with post:channels",
		Hint:  "Also posts in every public channel without joining it.",
	},
	{
		Roles: []string{RoleSystemUser, RolePostAll},
		Label: "Member with post:all",
		Hint:  "Also posts in every channel, private ones included, without joining it.",
	},
	{
		Roles:          []string{RoleSystemUser, RoleSystemAdmin},
		Label:          "System Admin",
		Hint:           "Every System Admin privilege, including users, teams and settings.",
		Administration: true,
	},
}

// RoleOptions is every access a seat may hold, in Mattermost's order.
func RoleOptions() []RoleOption {
	out := make([]RoleOption, len(roleOptions))
	for i, option := range roleOptions {
		option.Roles = slices.Clone(option.Roles)
		out[i] = option
	}
	return out
}

// DefaultRoles is what a seat that names no roles is given: Member, the
// least a bot can hold and still read the channels it is in.
func DefaultRoles() []string { return slices.Clone(roleOptions[0].Roles) }

// ErrUnknownRoles is a role list that is not one of the four Mattermost
// options.
var ErrUnknownRoles = errors.New("mattermost: not a role list Mattermost offers a bot")

// CanonicalRoles checks a role list against the four options and returns it
// in canonical order.
//
// THE STATIC CHECK the document validator makes (design section 2): the set
// is fixed by Mattermost, so a list outside it is refused on submit rather
// than discovered by a pass. Order and duplicates do not matter; an empty
// list is [DefaultRoles].
func CanonicalRoles(roles []string) ([]string, error) {
	option, ok := optionOf(roles)
	if !ok {
		return nil, fmt.Errorf("%w: %q is none of %s", ErrUnknownRoles,
			strings.Join(normalizeRoles(roles), " "), optionList())
	}
	return slices.Clone(option.Roles), nil
}

// RolesLabel is Mattermost's name for a role list, or the list itself when
// it is not one of the four (an account changed by hand to something this
// engine does not offer, which is shown as the vendor spells it).
func RolesLabel(roles []string) string {
	if option, ok := optionOf(roles); ok {
		return option.Label
	}
	return strings.Join(normalizeRoles(roles), " ")
}

// AdministrationRoles reports whether a role list is the administration
// class (D10): System Admin.
func AdministrationRoles(roles []string) bool {
	return slices.Contains(normalizeRoles(roles), RoleSystemAdmin)
}

// RolesRank is a role list's place in Mattermost's order, or -1 for a list
// that is not one of the options. A raise is a move to a higher rank.
func RolesRank(roles []string) int {
	want := normalizeRoles(roles)
	for i, option := range roleOptions {
		if sameSet(option.Roles, want) {
			return i
		}
	}
	return -1
}

// SameRoles reports whether two role lists hold the same roles, whatever
// their order. It is the drift test: a bot whose roles differ from its
// seat's document is restored (D2).
func SameRoles(a, b []string) bool { return sameSet(normalizeRoles(a), normalizeRoles(b)) }

// RolesOf is the role list an account holds, from the space-separated string
// Mattermost serves on the user.
func RolesOf(user User) []string { return normalizeRoles(strings.Fields(user.Roles)) }

func optionOf(roles []string) (RoleOption, bool) {
	want := normalizeRoles(roles)
	if len(want) == 0 {
		return roleOptions[0], true
	}
	for _, option := range roleOptions {
		if sameSet(option.Roles, want) {
			return option, true
		}
	}
	return RoleOption{}, false
}

// normalizeRoles trims, lowercases, drops empties and duplicates, and sorts,
// so two spellings of one set compare equal.
func normalizeRoles(roles []string) []string {
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		if role = strings.ToLower(strings.TrimSpace(role)); role != "" {
			out = append(out, role)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func sameSet(a, b []string) bool {
	return slices.Equal(normalizeRoles(a), normalizeRoles(b))
}

func optionList() string {
	names := make([]string, 0, len(roleOptions))
	for _, option := range roleOptions {
		names = append(names, fmt.Sprintf("[%s] (%s)",
			strings.Join(option.Roles, " "), option.Label))
	}
	return strings.Join(names, ", ")
}

// UserByID reads one account by id, reporting whether it exists.
//
// THREE-VALUED, as [Client.BotByUsername] is: the ledger reaches an account
// by the id it recorded, including a seat that has left the document, and an
// account that is gone is a ledger row to close (design 5.4) where a read
// that failed is not.
func (c *Client) UserByID(ctx context.Context, userID string) (User, bool, error) {
	if strings.TrimSpace(userID) == "" {
		return User{}, false, errors.New("mattermost: a user read needs an id")
	}
	var out User
	_, err := c.request(ctx, http.MethodGet, "/users/"+url.PathEscape(userID), nil, &out, false)
	switch {
	case isStatus(err, http.StatusNotFound):
		return User{}, false, nil
	case err != nil:
		return User{}, false, err
	}
	return out, true, nil
}

// UpdateRoles sets the system roles an account holds, replacing what it had.
//
// THE WHOLE LIST, which is Mattermost's own shape for the call: the server
// stores exactly the string it is sent, so a partial write would drop the
// base role and leave a bot that can read nothing. The list is checked
// against the four options first, so this engine never writes a role set
// it does not offer. Needs the provisioning token's system_admin.
func (c *Client) UpdateRoles(ctx context.Context, userID string, roles []string) error {
	if strings.TrimSpace(userID) == "" {
		return errors.New("mattermost: a role update needs a user")
	}
	canonical, err := CanonicalRoles(roles)
	if err != nil {
		return err
	}
	_, err = c.request(ctx, http.MethodPut, "/users/"+url.PathEscape(userID)+"/roles",
		map[string]string{"roles": strings.Join(canonical, " ")}, nil, true)
	return err
}

// RemoveChannelMember takes an account out of a channel.
//
// THE OTHER HALF OF A SCOPE CHANGE: a channel removed from a seat's list is
// one its bot must stop hearing, and a bot hears everything in a channel it
// is a member of. A 404 is success, for the reason [Client.RevokeToken]'s
// is: the account already out is the state this call asks for, and a retry
// after a lost answer must not fail on the first attempt's work.
func (c *Client) RemoveChannelMember(ctx context.Context, channelID, userID string) error {
	if channelID == "" || userID == "" {
		return errors.New("mattermost: leaving a channel needs a channel and a user")
	}
	_, err := c.request(ctx, http.MethodDelete, "/channels/"+url.PathEscape(channelID)+
		"/members/"+url.PathEscape(userID), nil, nil, true)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
