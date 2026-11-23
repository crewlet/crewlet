package metaads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/words"
)

// The check of a seat's own token (package doc, "Seat identities"). The app
// check is the business check too: Meta installs an app only on its own
// business's system users.

// What a check found wrong, as the access answer names it.
const (
	ProblemRevoked   = "revoked"
	ProblemWrongTeam = "wrong_team"
	ProblemWrongRole = "wrong_role"
)

// SeatProblem is a seat's own token failing its check on something a person
// fixes at Meta.
type SeatProblem struct {
	// Kind is one of the Problem constants.
	Kind string
	// Said is what is wrong and Remedy what to do, as a person reads them.
	Said, Remedy string
	// Held and Needs are what the system user holds and what the seat's level
	// needs, for a wrong role.
	Held, Needs string
	// At is the read that found it: [readToken], [readUser] or [readAccounts].
	At int
}

// The check's reads, in order.
const (
	readToken = iota
	readUser
	readAccounts
)

func (e *SeatProblem) Error() string { return e.Said }

// SeatRead is what Meta says of a seat's own token.
type SeatRead struct {
	UserID, Name string
	Scopes       []string
	Expires      time.Time
	// Tasks are its tasks on each ad account it reaches.
	Tasks map[string][]string
	// Role is its role as the business lists it, "" where no admin token
	// reads the list.
	Role string
	// Warnings are what the check allows and a person should know, and
	// Broader whether one is a task beyond what the level needs.
	Warnings []string
	Broader  bool
}

// systemUsersPage is where a person makes and assigns a system user.
const systemUsersPage = businessSettings

// check reads value as Meta answers it and holds it to level and accounts.
func (g *graph) check(ctx context.Context, value string, level growth.Level, accounts []string) (SeatRead, error) {
	d, err := g.debug(ctx, value)
	if err != nil {
		return SeatRead{}, err
	}
	out := SeatRead{UserID: d.Data.UserID, Scopes: d.Data.Scopes, Tasks: map[string][]string{}}
	if d.Data.ExpiresAt > 0 {
		out.Expires = time.Unix(d.Data.ExpiresAt, 0).UTC()
	}
	switch {
	case !d.Data.IsValid:
		return out, &SeatProblem{Kind: ProblemRevoked, At: readToken, Said: "Meta no longer accepts this token (" +
			validityWords(d.Data.Error.Subcode) + ").", Remedy: "Give the agent a new identity."}
	case d.Data.AppID != g.appID:
		return out, &SeatProblem{Kind: ProblemWrongTeam, At: readToken, Said: "This token is another Meta app's, so its system user " +
			"belongs to another business.", Remedy: "Generate the token from app " + g.appID + " in the company's business."}
	case d.Data.Type != "SYSTEM_USER":
		return out, &SeatProblem{Kind: ProblemWrongRole, At: readToken, Said: "This is a " + strings.ToLower(d.Data.Type) +
			" token, not a system user's.", Remedy: "Generate the token on the agent's system user."}
	}
	if why := seatPolicy(level, out.Scopes); why != "" {
		return out, &SeatProblem{Kind: ProblemWrongRole, At: readToken, Said: "The token is refused: " + why + ".",
			Remedy: "Generate a new token with " + words.And(seatBase(level)) + "."}
	}
	resp, err := g.client.Do(ctx, value, Request{Method: http.MethodGet, Path: "/me",
		Params: url.Values{"fields": {"id,name"}}, Read: true, Reserved: true})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == 190 {
			return out, &SeatProblem{Kind: ProblemRevoked, At: readUser, Said: "Meta no longer accepts this token.",
				Remedy: "Give the agent a new identity."}
		}
		return out, fmt.Errorf("metaads: read the seat's system user: %w", err)
	}
	var me struct {
		ID   string `json:"id"`
		Name string `json:"name" untrusted:"name"`
	}
	if json.Unmarshal(resp.Body, &me) != nil || me.ID == "" {
		return out, fmt.Errorf("%w: /me names no system user", errAnswer)
	}
	out.Name = me.Name
	if out.UserID == "" {
		out.UserID = me.ID
	}
	if g.admin != "" && g.business != "" {
		users, err := g.systemUsers(ctx)
		if err != nil {
			return out, fmt.Errorf("metaads: list the business's system users: %w", err)
		}
		// BY ID, OR BY NAME where Meta scopes the IDs it lists apart from the
		// token's: the app is the business's own, so a name it answers is one
		// of the business's system users.
		i := slices.IndexFunc(users, func(u systemUser) bool { return u.ID == out.UserID || u.ID == me.ID })
		if i < 0 {
			i = slices.IndexFunc(users, func(u systemUser) bool { return u.Name == me.Name })
		}
		if i < 0 {
			return out, &SeatProblem{Kind: ProblemWrongTeam, At: readUser, Said: "System user " + me.Name +
				" is not one of business " + g.business + "'s.", Remedy: "Make the system user in business " +
				g.business + "."}
		}
		out.Role = users[i].Role
	} else {
		out.Warnings = append(out.Warnings, "Crewlet cannot read the system user's role without the admin system "+
			"user token: make sure it is an Employee, never an Admin.")
	}
	if out.Role == roleAdmin {
		return out, &SeatProblem{Kind: ProblemWrongRole, At: readUser, Said: "System user " + out.Name + " is an Admin system user, " +
			"which reaches every asset of the business.", Held: "Admin", Needs: "Employee",
			Remedy: "Make an Employee system user for the agent."}
	}
	if err := g.readTasks(ctx, value, &out); err != nil {
		return out, err
	}
	need := seatTasks(level)
	var short []words.Said
	for _, a := range accounts {
		held, ok := out.Tasks[a]
		switch {
		case !ok:
			short = append(short, words.Said(a+" is not assigned"))
		case len(missing(held, need)) > 0:
			short = append(short, words.Said(fmt.Sprintf("%s holds %s", a, tasksText(held))))
		}
	}
	if len(short) > 0 {
		return out, &SeatProblem{Kind: ProblemWrongRole, At: readAccounts, Held: words.And(short), Needs: TasksWords(level),
			Said: fmt.Sprintf("System user %s is short of what %s needs: %s.", out.Name, levelName(level),
				words.And(short)),
			Remedy: fmt.Sprintf("In Business settings, give it %s on %s, then check again.", TasksWords(level),
				words.And(accounts))}
	}
	var wide, beyond []string
	for a, held := range out.Tasks {
		switch {
		case !slices.Contains(accounts, a):
			beyond = append(beyond, a)
		case level != growth.LevelAdminister && slices.Contains(held, taskManage):
			wide = append(wide, a)
		}
	}
	slices.Sort(wide)
	slices.Sort(beyond)
	if len(wide) > 0 {
		out.Broader = true
		out.Warnings = append(out.Warnings, fmt.Sprintf("It holds Manage ad account on %s, more than %s needs; Crewlet "+
			"still holds every call to %s.", words.And(wide), levelName(level), levelName(level)))
	}
	if len(beyond) > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("It also reaches %s, which Crewlet keeps this agent from.",
			words.And(beyond)))
	}
	return out, nil
}

// debug is what debug_token says of value, asked with the app's own token.
func (g *graph) debug(ctx context.Context, value string) (debugAnswer, error) {
	g.client.hold(value)
	resp, err := g.client.Do(ctx, g.appToken(), Request{Method: http.MethodGet, Path: "/debug_token",
		Params: url.Values{"input_token": {value}}, Read: true, Reserved: true})
	if err != nil {
		return debugAnswer{}, fmt.Errorf("metaads: read a token with debug_token: %w", err)
	}
	var d debugAnswer
	if err := json.Unmarshal(resp.Body, &d); err != nil {
		return debugAnswer{}, fmt.Errorf("%w: debug_token: %w", errAnswer, err)
	}
	return d, nil
}

// scopesOf are the permissions value holds while Meta accepts it.
func (g *graph) scopesOf(ctx context.Context, value string) ([]string, error) {
	d, err := g.debug(ctx, value)
	if err != nil || !d.Data.IsValid || d.Data.AppID != g.appID {
		return nil, err
	}
	return d.Data.Scopes, nil
}

// userOf is the user value is a token of while Meta accepts it.
func (g *graph) userOf(ctx context.Context, value string) (string, error) {
	d, err := g.debug(ctx, value)
	if err != nil || !d.Data.IsValid {
		return "", err
	}
	return d.Data.UserID, nil
}

// accountsOf are the ad accounts value's system user reaches, none where Meta
// does not answer: what a remake or a drop takes from it.
func (g *graph) accountsOf(ctx context.Context, value string) []string {
	read := SeatRead{Tasks: map[string][]string{}}
	if g.readTasks(ctx, value, &read) != nil {
		return nil
	}
	out := slices.Collect(maps.Keys(read.Tasks))
	slices.Sort(out)
	return out
}

// readTasks reads the tasks the token's system user holds on each ad account
// it reaches.
func (g *graph) readTasks(ctx context.Context, value string, out *SeatRead) error {
	err := pages(ctx, g.client, value, "/me/adaccounts", url.Values{"fields": {"id,user_tasks"}, "limit": {"100"}},
		"", true, maxAccountPages, func(raw json.RawMessage) error {
			var a struct {
				ID    string   `json:"id"`
				Tasks []string `json:"user_tasks"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return fmt.Errorf("metaads: read an ad account's tasks: %w", err)
			}
			out.Tasks[a.ID] = a.Tasks
			return nil
		})
	if err != nil {
		return fmt.Errorf("metaads: read the seat's ad accounts: %w", err)
	}
	return nil
}

// tasksText are tasks as Business settings labels them, broadest first, "no
// task" for none.
func tasksText(tasks []string) string {
	var out []words.Said
	for _, t := range []string{taskManage, taskAdvertise, taskAnalyze} {
		if slices.Contains(tasks, t) {
			out = append(out, words.Said(taskWords[t]))
		}
	}
	if len(out) == 0 {
		return "no task"
	}
	return words.And(out)
}

// proveAdmin is what is wrong with the admin system user's token, nil where it
// can make agents' system users: alive, the company's app, business_management,
// an Admin of the business, and not about to end.
func (g *graph) proveAdmin(ctx context.Context, now time.Time) (*integration.Finding, error) {
	const subject = "integrations." + Kind + ".admin_system_user_token"
	refused := func(why string) *integration.Finding {
		return &integration.Finding{Kind: integration.FindingCredentialRejected, Subject: subject,
			Detail: "the admin system user token " + why + ", so Crewlet cannot make agents' own system users; " +
				"generate the business's Admin system user's token from the app with business_management and paste it"}
	}
	d, err := g.debug(ctx, g.admin)
	if err != nil {
		return nil, err
	}
	switch {
	case !d.Data.IsValid:
		return refused("is one Meta no longer accepts (" + validityWords(d.Data.Error.Subcode) + ")"), nil
	case d.Data.AppID != g.appID:
		return refused("was issued by another Meta app than app " + g.appID), nil
	case !slices.Contains(d.Data.Scopes, "business_management"):
		return refused("lacks business_management"), nil
	}
	users, err := g.systemUsers(ctx)
	var apiErr *APIError
	if errors.As(err, &apiErr) && !apiErr.Throttled() && !apiErr.Transient {
		return refused("cannot list business " + g.business + "'s system users (" + apiErr.Error() + ")"), nil
	}
	if err != nil {
		return nil, fmt.Errorf("metaads: list the business's system users: %w", err)
	}
	if i := slices.IndexFunc(users, func(u systemUser) bool { return u.ID == d.Data.UserID }); i < 0 ||
		users[i].Role != roleAdmin {
		return refused("is not an Admin system user's of business " + g.business + ", and only one makes system users"), nil
	}
	if d.Data.ExpiresAt > 0 {
		if ends := time.Unix(d.Data.ExpiresAt, 0).UTC(); ends.Sub(now) <= renewAt {
			return &integration.Finding{Kind: integration.FindingCredentialExpiring, Subject: subject, ExpiresAt: ends,
				Detail: fmt.Sprintf("the admin system user token expires on %s; generate a new one, or one that never "+
					"expires, and paste it", ends.Format("2 January 2006"))}, nil
		}
	}
	return nil, nil
}
