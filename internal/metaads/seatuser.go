package metaads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// A seat's own system user at Meta (package doc, "Seat identities"): made,
// assigned, minted for and revoked with the business's admin system user's
// token, and checked with its own.

// Meta's ad account tasks.
const (
	taskManage    = "MANAGE"
	taskAdvertise = "ADVERTISE"
	taskAnalyze   = "ANALYZE"
)

// taskWords are the tasks as Business settings labels them.
var taskWords = map[string]string{taskManage: "Manage ad account", taskAdvertise: "Manage campaigns",
	taskAnalyze: "View performance"}

// seatTasks are the tasks a level's own system user holds on each of its ad
// accounts: Meta's analyst, advertiser and admin task sets, the least each
// level's operations need.
func seatTasks(level growth.Level) []string {
	switch level {
	case growth.LevelAdminister:
		return []string{taskManage, taskAdvertise, taskAnalyze}
	case growth.LevelOperate:
		return []string{taskAdvertise, taskAnalyze}
	}
	return []string{taskAnalyze}
}

// TasksWords are a level's tasks as Business settings labels them.
func TasksWords(level growth.Level) string { return tasksText(seatTasks(level)) }

// seatBase are the permissions a level's own token must hold. Only an
// Administer seat's holds business_management, for the account changes an
// administrator allows it.
func seatBase(level growth.Level) []string {
	switch level {
	case growth.LevelAdminister:
		return []string{"ads_management", "ads_read", "business_management"}
	case growth.LevelOperate:
		return []string{"ads_management", "ads_read"}
	}
	return []string{"ads_read"}
}

// seatScopes are what Crewlet mints a level's own token with: its base, and
// what the company's tokens of the slots the level uses prove the app holds,
// within what the level may hold.
func seatScopes(level growth.Level, company map[string][]string) []string {
	out := slices.Clone(seatBase(level))
	add := func(scopes []string) {
		for _, s := range scopes {
			if !slices.Contains(out, s) && seatPolicy(level, append(slices.Clone(out), s)) == "" {
				out = append(out, s)
			}
		}
	}
	add(company[slotRead])
	if level != growth.LevelRead {
		add(company[slotOperate])
	}
	if level == growth.LevelAdminister {
		add(company[slotAdmin])
	}
	slices.Sort(out)
	return out
}

// seatPolicy is why a seat's own token is refused for level, "" where it is
// not: it holds the level's base, nothing that writes at Read, no
// business_management below Administer, and no permission no operation needs.
func seatPolicy(level growth.Level, scopes []string) string {
	known := func(p string) bool {
		return slices.Contains(readOnly, p) || slices.ContainsFunc(permissions, func(k growth.Permission) bool { return k.ID == p })
	}
	var wide, writes []string
	for _, s := range scopes {
		if !known(s) {
			wide = append(wide, s)
		}
		if !slices.Contains(readOnly, s) {
			writes = append(writes, s)
		}
	}
	switch {
	case len(wide) > 0:
		return fmt.Sprintf("it holds %s, which no Meta Ads operation needs; generate a token without it", words.And(wide))
	case len(missing(scopes, seatBase(level))) > 0:
		return fmt.Sprintf("it lacks %s, which %s needs", words.And(missing(scopes, seatBase(level))), levelName(level))
	case level == growth.LevelRead && len(writes) > 0:
		return fmt.Sprintf("it holds %s, which writes, and this agent only reads; generate it with read permissions alone",
			words.And(writes))
	case level != growth.LevelAdminister && slices.Contains(scopes, "business_management"):
		return "it holds business_management, which can change the business portfolio and only an Administer agent's " +
			"token holds; generate it without"
	}
	return ""
}

// levelName is a level as the Agents tab names it.
func levelName(level growth.Level) string {
	switch level {
	case growth.LevelAdminister:
		return "Administer"
	case growth.LevelOperate:
		return "Operate"
	}
	return "Read"
}

// graph is the company's Meta app as a seat's identity reaches it.
type graph struct {
	client   *Client
	appID    string
	business string
	// admin is the business's admin system user's token, "" where none is
	// connected: then a person makes each seat's system user by hand.
	admin string
}

// appToken is the app's own token, which debug_token takes.
func (g *graph) appToken() string { return g.appID + "|" + g.client.secret }

// systemUser is one system user of the business, as Meta lists it.
type systemUser struct {
	ID   string `json:"id"`
	Name string `json:"name" untrusted:"name"`
	Role string `json:"role"`
}

// roleAdmin is an admin system user's role, which reaches every asset of the
// business and is refused for a seat.
const roleAdmin = "ADMIN"

// maxSystemUserPages bounds the business's system user list: Meta allows a
// business 11 at most (10 and its admin system user), within one page of 100.
const maxSystemUserPages = 2

// systemUsers are the business's system users, read with the admin token.
func (g *graph) systemUsers(ctx context.Context) ([]systemUser, error) {
	var out []systemUser
	err := pages(ctx, g.client, g.admin, "/"+g.business+"/system_users",
		url.Values{"fields": {"id,name,role"}, "limit": {"100"}}, "", true, maxSystemUserPages,
		func(raw json.RawMessage) error {
			var u systemUser
			if err := json.Unmarshal(raw, &u); err != nil {
				return fmt.Errorf("metaads: read a system user: %w", err)
			}
			out = append(out, u)
			return nil
		})
	return out, err
}

// SystemUserCap is how many system users Meta lets a business make besides its
// admin system user: 10 at Full access, 1 at Limited. A tier not heard yet is
// read as Full, and Meta's own refusal (3949) then says otherwise.
func SystemUserCap(tier string) int {
	if Limited(tier) {
		return 1
	}
	return 10
}

// CapError is Meta refusing another system user: the business holds its cap.
type CapError struct{ Used, Cap int }

func (e *CapError) Error() string {
	return fmt.Sprintf("Meta allows this business %d system users and it has %d", e.Cap, e.Used)
}

// errCodeCap is Meta's "maximum number of system user limit" refusal.
const errCodeCap = 3949

// used counts the system users the cap counts: every one but the admin
// system user.
func used(users []systemUser) int {
	n := 0
	for _, u := range users {
		if u.Role != roleAdmin {
			n++
		}
	}
	return n
}

// hearTier reads account once with the admin token, so the client hears the
// app's access tier, which Meta says only on an answer about an ad account.
func (g *graph) hearTier(ctx context.Context, account string) error {
	if g.client.Tier() != "" || account == "" {
		return nil
	}
	_, err := g.client.Do(ctx, g.admin, Request{Method: http.MethodGet, Path: "/" + account,
		Params: url.Values{"fields": {"id"}}, Object: objectOf(account), Read: true, Reserved: true})
	if err != nil {
		return fmt.Errorf("metaads: read ad account %s for the app's access tier: %w", account, err)
	}
	return nil
}

// ensure is the business's system user named name (Meta deletes none, so a
// returning seat reuses it), or a new Employee one, refused at the cap before
// Meta is asked; account is read for the app's tier.
func (g *graph) ensure(ctx context.Context, name, account string) (systemUser, error) {
	if err := g.hearTier(ctx, account); err != nil {
		return systemUser{}, err
	}
	users, err := g.systemUsers(ctx)
	if err != nil {
		return systemUser{}, fmt.Errorf("metaads: list the business's system users: %w", err)
	}
	if i := slices.IndexFunc(users, func(u systemUser) bool { return u.Name == name }); i >= 0 {
		if users[i].Role == roleAdmin {
			return systemUser{}, fmt.Errorf("%w: %s is an admin system user, which reaches every asset of the business",
				errSeatUser, name)
		}
		return users[i], nil
	}
	limit := SystemUserCap(g.client.Tier())
	if n := used(users); n >= limit {
		return systemUser{}, &CapError{Used: n, Cap: limit}
	}
	resp, err := g.client.Do(ctx, g.admin, Request{Method: http.MethodPost, Path: "/" + g.business + "/system_users",
		Params: url.Values{"name": {name}, "role": {"EMPLOYEE"}}, Reserved: true})
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Code == errCodeCap || apiErr.Subcode == errCodeCap) {
		return systemUser{}, &CapError{Used: used(users), Cap: used(users)}
	}
	if err != nil {
		return systemUser{}, fmt.Errorf("metaads: make system user %s: %w", name, err)
	}
	var made struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(resp.Body, &made) != nil || made.ID == "" {
		return systemUser{}, fmt.Errorf("%w: Meta's answer to making %s names no ID", errAnswer, name)
	}
	return systemUser{ID: made.ID, Name: name, Role: "EMPLOYEE"}, nil
}

// errSeatUser is a system user Crewlet may not use for a seat.
var errSeatUser = errors.New("metaads: the system user cannot be a seat's")

// install gives the system user the company's app, which Meta needs before it
// mints the system user a token for it.
func (g *graph) install(ctx context.Context, user string) error {
	_, err := g.client.Do(ctx, g.admin, Request{Method: http.MethodPost, Path: "/" + user + "/applications",
		Params: url.Values{"business_app": {g.appID}}, Reserved: true})
	if err != nil {
		return fmt.Errorf("metaads: give system user %s the app: %w", user, err)
	}
	return nil
}

// assign gives the system user account with tasks; it also changes the tasks
// of an assignment already made.
func (g *graph) assign(ctx context.Context, account, user string, tasks []string) error {
	list, _ := json.Marshal(tasks)
	_, err := g.client.Do(ctx, g.admin, Request{Method: http.MethodPost, Path: "/" + account + "/assigned_users",
		Params: url.Values{"user": {user}, "tasks": {string(list)}, "business": {g.business}},
		Object: objectOf(account), Reserved: true})
	if err != nil {
		return fmt.Errorf("metaads: assign ad account %s to system user %s: %w", account, user, err)
	}
	return nil
}

// unassign takes account from the system user.
func (g *graph) unassign(ctx context.Context, account, user string) error {
	_, err := g.client.Do(ctx, g.admin, Request{Method: http.MethodDelete, Path: "/" + account + "/assigned_users",
		Params: url.Values{"user": {user}, "business": {g.business}}, Object: objectOf(account), Reserved: true})
	if err != nil {
		return fmt.Errorf("metaads: take ad account %s from system user %s: %w", account, user, err)
	}
	return nil
}

// mint is a new sixty-day token of the system user holding scopes.
func (g *graph) mint(ctx context.Context, user string, scopes []string) (string, error) {
	resp, err := g.client.Do(ctx, g.admin, Request{Method: http.MethodPost, Path: "/" + user + "/access_tokens",
		Params: url.Values{"business_app": {g.appID}, "scope": {strings.Join(scopes, ",")},
			"set_token_expires_in_60_days": {"true"}}, Reserved: true})
	if err != nil {
		return "", fmt.Errorf("metaads: mint system user %s's token: %w", user, err)
	}
	var doc struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(resp.Body, &doc) != nil || doc.AccessToken == "" {
		return "", fmt.Errorf("%w: Meta's answer to minting system user %s's token holds none", errAnswer, user)
	}
	g.client.hold(doc.AccessToken)
	return doc.AccessToken, nil
}

// revokeAll revokes every token of the system user.
func (g *graph) revokeAll(ctx context.Context, user string) error {
	_, err := g.client.Do(ctx, g.admin, Request{Method: http.MethodDelete, Path: "/" + user + "/access_tokens",
		Reserved: true})
	if err != nil {
		return fmt.Errorf("metaads: revoke system user %s's tokens: %w", user, err)
	}
	return nil
}

// revoke revokes one token, called as caller (a token of the same app), every
// secret in the POST form body.
func (g *graph) revoke(ctx context.Context, value, caller string) error {
	_, err := g.client.Do(ctx, caller, Request{Method: http.MethodPost, Path: "/oauth/revoke",
		Params:   url.Values{"client_id": {g.appID}, "client_secret": {g.client.secret}, "revoke_token": {value}},
		Reserved: true})
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == 190 {
		// ALREADY DEAD: a token Meta no longer accepts needs no revoking.
		return nil
	}
	if err != nil {
		return fmt.Errorf("metaads: revoke a seat's token: %w", err)
	}
	return nil
}

// SeatMade is a seat's system user as [graph.make] left it.
type SeatMade struct {
	UserID, Name string
	// Token is the minted token. Never logged.
	Token string `json:"-"`
}

// make is the seat's system user named name, holding exactly accounts at
// level's tasks, with a new token of scopes ([graph.remake]); a failure takes
// back what it assigned.
func (g *graph) make(ctx context.Context, name string, level growth.Level, accounts, had, scopes []string) (SeatMade, error) {
	first := ""
	if len(accounts) > 0 {
		first = accounts[0]
	}
	u, err := g.ensure(ctx, name, first)
	if err != nil {
		return SeatMade{}, err
	}
	// THE APP MAY BE INSTALLED ALREADY on a system user Crewlet reuses; the
	// mint says whether it is, and an install that failed is named beside it.
	installErr := g.install(ctx, u.ID)
	made, err := g.remake(ctx, u, level, accounts, had, scopes)
	if err != nil {
		return SeatMade{}, errors.Join(err, installErr)
	}
	return made, nil
}

// remake brings system user u to exactly accounts at level's tasks, taking
// what it held and the seat no longer names, and mints it a new token of
// scopes. A failure takes back what it assigned.
func (g *graph) remake(ctx context.Context, u systemUser, level growth.Level, accounts, had,
	scopes []string) (SeatMade, error) {
	var assigned []string
	undo := func() {
		cleanup := context.WithoutCancel(ctx)
		for _, a := range assigned {
			if !slices.Contains(had, a) {
				_ = g.unassign(cleanup, a, u.ID)
			}
		}
	}
	for _, a := range accounts {
		if err := g.assign(ctx, a, u.ID, seatTasks(level)); err != nil {
			undo()
			return SeatMade{}, err
		}
		assigned = append(assigned, a)
	}
	for _, a := range had {
		if !slices.Contains(accounts, a) {
			if err := g.unassign(ctx, a, u.ID); err != nil {
				undo()
				return SeatMade{}, err
			}
		}
	}
	token, err := g.mint(ctx, u.ID, scopes)
	if err != nil {
		undo()
		return SeatMade{}, err
	}
	return SeatMade{UserID: u.ID, Name: u.Name, Token: token}, nil
}

// retire revokes sealed (which must hold), then every other token and the
// accounts where Meta answers: a system user with no token acts for nobody.
// The system user stays, since Meta deletes none.
func (g *graph) retire(ctx context.Context, user, sealed string, accounts []string) error {
	if sealed != "" {
		if err := g.revoke(ctx, sealed, g.admin); err != nil {
			return err
		}
	}
	if err := g.revokeAll(ctx, user); err != nil && sealed == "" {
		return err
	}
	for _, a := range accounts {
		_ = g.unassign(ctx, a, user)
	}
	return nil
}

// renewal is a new token for a seat's own system user: minted with the admin
// token where one is connected, else the old token exchanged for one living
// sixty days. The old token is revoked once the new one is sealed.
func (g *graph) renewal(ctx context.Context, user, old string, scopes []string) (string, error) {
	if g.admin != "" {
		return g.mint(ctx, user, scopes)
	}
	fresh, _, err := exchange(ctx, g.client, g.appID, old)
	return fresh, err
}
