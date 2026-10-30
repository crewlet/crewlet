package mattermost_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// accessChat is a Mattermost instance the access vendor is run against: its
// accounts with their roles and state, one team, its channels and who is in
// them, and each account's tokens. It remembers what it was told, so a case
// asserts what a step DID.
type accessChat struct {
	mu       sync.Mutex
	url      string
	users    map[string]*chatUser
	inTeam   map[string]bool
	channels map[string]string          // name -> id
	members  map[string]map[string]bool // channel id -> user ids
	tokens   map[string][]mattermost.Token
	refuse   map[string]refusal // "METHOD /path" -> answer
	writes   []string
	next     int
}

type chatUser struct {
	id, username, roles, description string
	bot                              bool
	deleteAt                         int64
}

type refusal struct {
	status int
	said   string
}

func newAccessChat(t *testing.T) *accessChat {
	t.Helper()
	c := &accessChat{
		users: map[string]*chatUser{}, inTeam: map[string]bool{},
		channels: map[string]string{
			"general": "c-general", "incidents": "c-incidents", "random": "c-random",
		},
		members: map[string]map[string]bool{}, tokens: map[string][]mattermost.Token{},
		refuse: map[string]refusal{},
	}
	server := httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(server.Close)
	c.url = server.URL
	return c
}

// bot adds a bot this engine made for handle, holding roles and in channels.
func (c *accessChat) bot(handle, roles string, channels ...string) *chatUser {
	c.mu.Lock()
	defer c.mu.Unlock()
	user := c.add("agent-"+handle, roles, true)
	c.inTeam[user.id] = true
	for _, name := range channels {
		c.join(c.channels[name], user.id)
	}
	return user
}

func (c *accessChat) add(username, roles string, bot bool) *chatUser {
	c.next++
	user := &chatUser{id: "u-" + strconv.Itoa(c.next), username: username, roles: roles, bot: bot}
	c.users[user.id] = user
	return user
}

func (c *accessChat) join(channelID, userID string) {
	if c.members[channelID] == nil {
		c.members[channelID] = map[string]bool{}
	}
	c.members[channelID][userID] = true
}

func (c *accessChat) token(userID, description string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	id := "t-" + strconv.Itoa(c.next)
	c.tokens[userID] = append(c.tokens[userID], mattermost.Token{ID: id, Description: description})
	return id
}

func (c *accessChat) tokenIDs(userID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, token := range c.tokens[userID] {
		out = append(out, token.ID)
	}
	return out
}

func (c *accessChat) user(id string) chatUser {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.users[id]
}

func (c *accessChat) channelsOf(userID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for name, id := range c.channels {
		if c.members[id][userID] {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func (c *accessChat) wrote() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.writes)
}

func (c *accessChat) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/v4")
	route := r.Method + " " + path
	if r.Method != http.MethodGet && path != "/users/usernames" {
		c.writes = append(c.writes, route)
	}
	write := func(body any) { _ = json.NewEncoder(w).Encode(body) }
	fail := func(status int, said string) {
		w.WriteHeader(status)
		write(map[string]any{"message": said})
	}
	if refused, ok := c.refuse[route]; ok {
		fail(refused.status, refused.said)
		return
	}
	userRow := func(u *chatUser) map[string]any {
		return map[string]any{"id": u.id, "username": u.username, "roles": u.roles,
			"is_bot": u.bot, "delete_at": u.deleteAt}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case route == "GET /teams/name/nimbus":
		write(map[string]any{"id": "team-1", "name": "nimbus"})
	case route == "POST /users/usernames":
		var names []string
		_ = json.NewDecoder(r.Body).Decode(&names)
		out := []any{}
		for _, u := range c.users {
			if slices.Contains(names, u.username) {
				out = append(out, userRow(u))
			}
		}
		write(out)
	case route == "POST /bots":
		var body struct{ Username, DisplayName string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		user := c.add(body.Username, "system_user", true)
		write(map[string]any{"user_id": user.id, "username": user.username})
	case route == "POST /users/tokens/revoke":
		var body struct {
			TokenID string `json:"token_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for id, held := range c.tokens {
			c.tokens[id] = slices.DeleteFunc(held, func(t mattermost.Token) bool { return t.ID == body.TokenID })
		}
		write(map[string]any{"status": "OK"})
	case len(parts) == 2 && parts[0] == "users" && r.Method == http.MethodGet:
		user, ok := c.users[parts[1]]
		if !ok {
			fail(http.StatusNotFound, "not found")
			return
		}
		write(userRow(user))
	case len(parts) == 3 && parts[0] == "users" && parts[2] == "roles":
		var body struct{ Roles string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.users[parts[1]].roles = body.Roles
		write(map[string]any{"status": "OK"})
	case len(parts) == 3 && parts[0] == "users" && parts[2] == "tokens":
		if r.Method == http.MethodGet {
			write(c.tokens[parts[1]])
			return
		}
		var body struct{ Description string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.next++
		token := mattermost.Token{ID: "t-" + strconv.Itoa(c.next), Description: body.Description,
			Value: "fake-token-" + strconv.Itoa(c.next)}
		c.tokens[parts[1]] = append(c.tokens[parts[1]], mattermost.Token{ID: token.ID,
			Description: token.Description})
		write(token)
	case len(parts) == 5 && parts[0] == "users" && parts[4] == "channels":
		out := []any{map[string]any{"id": "d-1", "name": parts[1] + "__u-9", "type": "D"}}
		for name, id := range c.channels {
			if c.members[id][parts[1]] {
				out = append(out, map[string]any{"id": id, "name": name, "type": "O"})
			}
		}
		write(out)
	case len(parts) == 2 && parts[0] == "bots":
		user := c.users[parts[1]]
		if r.Method == http.MethodPut {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if description, ok := body["description"]; ok {
				user.description = description
			}
		}
		write(map[string]any{"user_id": user.id, "username": user.username,
			"description": user.description})
	case len(parts) == 3 && parts[0] == "bots" && parts[2] == "disable":
		c.users[parts[1]].deleteAt = 1700000000000
		write(map[string]any{})
	case len(parts) == 3 && parts[0] == "bots" && parts[2] == "enable":
		c.users[parts[1]].deleteAt = 0
		write(map[string]any{})
	case len(parts) == 4 && parts[0] == "teams" && parts[2] == "members" && r.Method == http.MethodGet:
		if !c.inTeam[parts[3]] {
			fail(http.StatusNotFound, "not a member")
			return
		}
		write(map[string]any{"team_id": "team-1", "user_id": parts[3]})
	case route == "POST /teams/team-1/members":
		var body struct {
			UserID string `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.inTeam[body.UserID] = true
		write(map[string]any{})
	case len(parts) == 5 && parts[0] == "teams" && parts[2] == "channels" && parts[3] == "name":
		id, ok := c.channels[parts[4]]
		if !ok {
			fail(http.StatusNotFound, "no channel")
			return
		}
		write(map[string]any{"id": id, "name": parts[4], "type": "O"})
	case len(parts) == 3 && parts[0] == "channels" && parts[2] == "members":
		var body struct {
			UserID string `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.join(parts[1], body.UserID)
		write(map[string]any{})
	case len(parts) == 4 && parts[0] == "channels" && parts[2] == "members" &&
		r.Method == http.MethodDelete:
		delete(c.members[parts[1]], parts[3])
		write(map[string]any{"status": "OK"})
	default:
		fail(http.StatusNotFound, "no route "+route)
	}
}

// open is a session against the fake through the registered vendor.
func (c *accessChat) open(t *testing.T, channels ...string) seataccess.Session {
	t.Helper()
	vendor, ok := seataccess.Lookup(integration.KindMattermost)
	if !ok {
		t.Fatal("the mattermost package registered no access vendor")
	}
	company := &config.Company{}
	company.Integrations.Mattermost = &config.Mattermost{
		Enabled: true, URL: c.url, Team: "nimbus",
		Provisioning: &config.MattermostProvisioning{
			UsernamePrefix: "agent-", AdminToken: "${MM_ADMIN}", Channels: channels,
		},
	}
	session, err := vendor.Open(context.Background(), seataccess.OpenInput{
		Config: company,
		Env:    config.NewResolver(config.MapSource{"MM_ADMIN": "fake-admin"}),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return session
}

func sre() seataccess.SeatRef { return seataccess.SeatRef{Handle: "sre", Name: "SRE"} }

func mmStep(t *testing.T, op seataccess.Op, verb seataccess.StepVerb) seataccess.Step {
	t.Helper()
	vendor, _ := seataccess.Lookup(integration.KindMattermost)
	for _, s := range vendor.Steps(sre(), seataccess.LaneMattermost, op,
		seataccess.Desired{Scope: []string{"general"}}, seataccess.Observed{}) {
		if s.Verb == verb {
			return s
		}
	}
	t.Fatalf("no %s step for %s", verb, op)
	return seataccess.Step{}
}

func heldBot(id string) *seataccess.Record {
	return &seataccess.Record{
		Kind: integration.KindMattermost, Handle: "sre",
		Account: &seataccess.Account{ID: id, State: seataccess.AccountActive},
	}
}

func mmApply(t *testing.T, s seataccess.Session, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	t.Helper()
	in.Seat = sre()
	return s.Apply(context.Background(), in)
}

const (
	member     = "system_user"
	postAll    = "system_user system_post_all"
	sysAdmin   = "system_admin system_user"
	postPublic = "system_user system_post_all_public"
)

// THE CATALOG SEALS THE BOT'S TOKEN and binds no tool server: the engine's own
// chat transport reads it.
func TestTheMattermostCatalog(t *testing.T) {
	t.Parallel()
	vendor, _ := seataccess.Lookup(integration.KindMattermost)
	cat := vendor.Catalog()
	if !slices.Equal(cat.Credentials, []string{mattermost.BotTokenField}) ||
		seataccess.ToolFor(seataccess.LaneMattermost) != nil {
		t.Errorf("catalog = %+v, want one token and no tool binding", cat)
	}
	lane, ok := cat.Lane(seataccess.LaneMattermost)
	if !ok || !lane.Levels || lane.LiveLevels || !lane.Scoped || !lane.Rotatable {
		t.Errorf("lane = %+v", lane)
	}
	if got := seataccess.SinkName(integration.KindMattermost, mattermost.BotTokenField, "sre"); got !=
		"MATTERMOST_BOT_TOKEN_SRE" {
		t.Errorf("sink = %q", got)
	}
}

// THE OPTIONS ARE THE DOCUMENT'S FOUR ROLE SETS under Mattermost's names, in
// its order, with System Admin the administration class, and the company's
// channels as the scope.
func TestMattermostOptions(t *testing.T) {
	t.Parallel()
	opts, err := newAccessChat(t).open(t, "incidents", "general").
		Options(context.Background(), seataccess.LaneMattermost)
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	var ids []string
	for i, level := range opts.Levels {
		ids = append(ids, level.ID)
		if want := strings.Join(config.MattermostRoleSets()[i], " "); level.ID != want {
			t.Errorf("level %d = %q, want the document's set %q", i, level.ID, want)
		}
		if level.Administers != (level.ID == sysAdmin) {
			t.Errorf("%s administers = %v", level.Name, level.Administers)
		}
	}
	if !slices.Equal(ids, []string{member, postPublic, postAll, sysAdmin}) {
		t.Errorf("levels = %q", ids)
	}
	if !slices.Equal(opts.Scope, []string{"general", "incidents"}) || opts.ScopeOpen {
		t.Errorf("scope = %q open=%v", opts.Scope, opts.ScopeOpen)
	}
	if !seataccess.NeedsStepUp(opts, seataccess.Desired{Enabled: true, Level: member},
		seataccess.Desired{Enabled: true, Level: sysAdmin}) {
		t.Error("a raise to System Admin does not ask for a step-up")
	}
	open, _ := newAccessChat(t).open(t).Options(context.Background(), seataccess.LaneMattermost)
	if !open.ScopeOpen {
		t.Error("a company with no channel list is not an open scope")
	}
}

// EVERY OP IS DRAWN IN MATTERMOST'S STEPS, and a scope change names the
// channels it joins and leaves.
func TestMattermostStepsPerOp(t *testing.T) {
	t.Parallel()
	vendor, _ := seataccess.Lookup(integration.KindMattermost)
	want := seataccess.Desired{Level: postAll, Scope: []string{"general", "incidents"}}
	have := seataccess.Observed{Scope: []string{"general", "random"}}
	for op, verbs := range map[seataccess.Op][]seataccess.StepVerb{
		seataccess.OpCreate: {seataccess.VerbCreate, seataccess.VerbGrant, seataccess.VerbScope,
			seataccess.VerbMint},
		seataccess.OpEnable:  {seataccess.VerbEnable, seataccess.VerbMint},
		seataccess.OpLevel:   {seataccess.VerbRerole},
		seataccess.OpScope:   {seataccess.VerbScope},
		seataccess.OpMint:    {seataccess.VerbMint},
		seataccess.OpDisable: {seataccess.VerbDisable},
		seataccess.OpRevoke:  {seataccess.VerbRevoke},
		seataccess.OpRemove:  {seataccess.VerbRemove},
		seataccess.OpRotate:  {seataccess.VerbMint, seataccess.VerbRevoke},
	} {
		steps := vendor.Steps(sre(), seataccess.LaneMattermost, op, want, have)
		var got []seataccess.StepVerb
		for i, s := range steps {
			got = append(got, s.Verb)
			if s.Index != i || s.Op != op || strings.Contains(s.Text+s.Note, "—") {
				t.Errorf("%s step %d = %+v", op, i, s)
			}
		}
		if !slices.Equal(got, verbs) {
			t.Errorf("%s verbs = %q, want %q", op, got, verbs)
		}
	}
	scope := vendor.Steps(sre(), seataccess.LaneMattermost, seataccess.OpScope, want, have)[0]
	if scope.Text != "Add it to ~incidents and remove it from ~random" {
		t.Errorf("scope text = %q", scope.Text)
	}
	level := vendor.Steps(sre(), seataccess.LaneMattermost, seataccess.OpLevel, want, have)[0]
	if !strings.Contains(level.Text, "Member with post:all") {
		t.Errorf("level text = %q, want Mattermost's name", level.Text)
	}
}

// EVERY PASS READS EACH BOT BACK: by the ledger's id, adopted by username
// (only a bot, never a person), a departed seat, its roles as the canonical
// level, its channels within the company's list, who deactivated it, and
// whether it holds a token.
func TestMattermostObserve(t *testing.T) {
	t.Parallel()
	chat := newAccessChat(t)
	recorded := chat.bot("sre", "system_post_all system_user", "general", "random")
	chat.token(recorded.id, "crewlet-sre")
	adopted := chat.bot("oncall", "system_user system_custom", "incidents")
	byHand := chat.bot("dba", member)
	byHand.deleteAt = 1
	marked := chat.bot("analyst", member)
	marked.deleteAt, marked.description = 1, mattermost.DisconnectedDescription
	departed := chat.bot("old", member)
	chat.mu.Lock()
	chat.add("agent-person", member, false)
	chat.mu.Unlock()

	obs, err := chat.open(t, "general", "incidents").Observe(context.Background(),
		seataccess.ObserveInput{
			Seats: []seataccess.SeatRef{{Handle: "sre"}, {Handle: "oncall"}, {Handle: "dba"},
				{Handle: "analyst"}, {Handle: "person"}},
			Records: map[string]seataccess.Record{
				"sre": *heldBot(recorded.id),
				"old": {Handle: "old", Account: &seataccess.Account{ID: departed.id}},
			},
		})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	lane := func(h string) seataccess.Observed { return obs[h][seataccess.LaneMattermost] }
	if got := lane("sre"); !got.Exists || got.Level != postAll || got.Account != "@agent-sre" ||
		!slices.Equal(got.Scope, []string{"general"}) {
		t.Errorf("sre = %+v", got)
	}
	if got := lane("oncall"); got.AccountID != adopted.id || got.Level != "system_custom system_user" ||
		!slices.Equal(got.Scope, []string{"incidents"}) {
		t.Errorf("oncall = %+v", got)
	}
	if got := lane("dba"); got.Enabled || got.DisabledBy != seataccess.DisabledByHand {
		t.Errorf("dba = %+v", got)
	}
	if got := lane("analyst"); got.DisabledBy != seataccess.DisabledByCrewlet {
		t.Errorf("analyst = %+v", got)
	}
	if got := lane("old"); !got.Exists {
		t.Errorf("the departed seat was not observed: %+v", got)
	}
	if got := lane("person"); got.Exists {
		t.Errorf("a person's account was adopted: %+v", got)
	}
}

// A CREATE MAKES THE BOT, PUTS IT IN THE TEAM WITH ITS ROLES AND CHANNELS, AND
// MINTS a token whose Revoke takes it back if the sync cannot seal it.
func TestMattermostCreate(t *testing.T) {
	t.Parallel()
	chat := newAccessChat(t)
	session := chat.open(t, "general", "incidents")
	want := seataccess.Desired{Enabled: true, Level: postPublic, Scope: []string{"general"}}

	created, err := mmApply(t, session, seataccess.ApplyInput{
		Step: mmStep(t, seataccess.OpCreate, seataccess.VerbCreate), Want: want,
	})
	if err != nil || created.AccountID == "" || created.Account != "@agent-sre" {
		t.Fatalf("create = %+v, %v", created, err)
	}
	// THE SYNC RECORDS THE BOT AS SOON AS THE CREATE RETURNS, and the
	// create's later steps reach it through that record.
	rec := heldBot(created.AccountID)
	for _, verb := range []seataccess.StepVerb{seataccess.VerbGrant, seataccess.VerbScope} {
		if _, err := mmApply(t, session, seataccess.ApplyInput{
			Step: mmStep(t, seataccess.OpCreate, verb), Want: want, Record: rec,
		}); err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
	}
	got := chat.user(created.AccountID)
	if !mattermost.SameRoles(strings.Fields(got.roles), strings.Fields(postPublic)) {
		t.Errorf("roles = %q", got.roles)
	}
	if channels := chat.channelsOf(created.AccountID); !slices.Equal(channels, []string{"general"}) {
		t.Errorf("channels = %q", channels)
	}
	minted, err := mmApply(t, session, seataccess.ApplyInput{
		Step: mmStep(t, seataccess.OpCreate, seataccess.VerbMint), Want: want, Record: rec,
	})
	if err != nil || len(minted.Minted) != 1 || minted.Minted[0].Field != mattermost.BotTokenField ||
		!strings.HasPrefix(minted.Minted[0].Value, "fake-token-") {
		t.Fatalf("mint = %+v, %v", minted, err)
	}
	if err := minted.Minted[0].Revoke(context.Background()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if tokens := chat.tokenIDs(created.AccountID); len(tokens) != 0 {
		t.Errorf("the unsealed token is still live: %q", tokens)
	}
}

// A LEVEL CHANGE WRITES THE WHOLE ROLE LIST, and a bot already holding the
// seat's roles is not written to.
func TestMattermostLevel(t *testing.T) {
	t.Parallel()
	chat := newAccessChat(t)
	bot := chat.bot("sre", "system_user system_admin")
	session := chat.open(t)
	in := seataccess.ApplyInput{
		Step: mmStep(t, seataccess.OpLevel, seataccess.VerbRerole),
		Want: seataccess.Desired{Enabled: true, Level: member}, Record: heldBot(bot.id),
	}
	if _, err := mmApply(t, session, in); err != nil {
		t.Fatalf("level: %v", err)
	}
	if got := chat.user(bot.id).roles; got != member {
		t.Errorf("roles = %q, want Member", got)
	}
	before := len(chat.wrote())
	if _, err := mmApply(t, session, in); err != nil {
		t.Fatalf("converged level: %v", err)
	}
	if len(chat.wrote()) != before {
		t.Error("a converged bot was written to")
	}
}

// A SCOPE CHANGE JOINS AND LEAVES WITHIN THE COMPANY'S LIST, leaves a room a
// person invited the bot into, and a channel the team does not have is a
// refusal naming it.
func TestMattermostScope(t *testing.T) {
	t.Parallel()
	chat := newAccessChat(t)
	bot := chat.bot("sre", member, "general", "random")
	session := chat.open(t, "general", "incidents", "typo")
	in := seataccess.ApplyInput{
		Step: mmStep(t, seataccess.OpScope, seataccess.VerbScope), Record: heldBot(bot.id),
		Want: seataccess.Desired{Enabled: true, Level: member, Scope: []string{"incidents"}},
	}
	if _, err := mmApply(t, session, in); err != nil {
		t.Fatalf("scope: %v", err)
	}
	if got := chat.channelsOf(bot.id); !slices.Equal(got, []string{"incidents", "random"}) {
		t.Errorf("channels = %q, want incidents joined, general left, random untouched", got)
	}
	in.Want.Scope = []string{"incidents", "typo"}
	_, err := mmApply(t, session, in)
	var refused *seataccess.Refusal
	if !errors.As(err, &refused) || !strings.Contains(refused.Said, "~typo") ||
		refused.Actor != seataccess.ByYou {
		t.Errorf("err = %#v, want a refusal naming the missing channel", err)
	}
}

// OFF MARKS AND DEACTIVATES THE BOT, which Mattermost keeps (D1), and the
// revoke that follows takes this engine's tokens and hands the sync the field
// to clear. Back on reactivates it, clears the marker and mints a fresh token.
func TestMattermostOffAndBackOn(t *testing.T) {
	t.Parallel()
	chat := newAccessChat(t)
	bot := chat.bot("sre", member)
	chat.token(bot.id, "crewlet-sre")
	admins := chat.token(bot.id, "admin debugging")
	session := chat.open(t)
	in := seataccess.ApplyInput{Record: heldBot(bot.id), Want: seataccess.Desired{Level: member}}

	in.Step = mmStep(t, seataccess.OpDisable, seataccess.VerbDisable)
	off, err := mmApply(t, session, in)
	if err != nil || !off.Disabled {
		t.Fatalf("deactivate = %+v, %v", off, err)
	}
	if got := chat.user(bot.id); got.deleteAt == 0 || got.description != mattermost.DisconnectedDescription {
		t.Errorf("bot = %+v, want deactivated and marked", got)
	}
	in.Step = mmStep(t, seataccess.OpRevoke, seataccess.VerbRevoke)
	revoked, err := mmApply(t, session, in)
	if err != nil || !slices.Equal(revoked.Revoked, []string{mattermost.BotTokenField}) {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	if tokens := chat.tokenIDs(bot.id); !slices.Equal(tokens, []string{admins}) {
		t.Errorf("tokens = %q, want only the administrator's own left", tokens)
	}

	in.Step = mmStep(t, seataccess.OpEnable, seataccess.VerbEnable)
	if _, err := mmApply(t, session, in); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := chat.user(bot.id); got.deleteAt != 0 || got.description != "" {
		t.Errorf("bot = %+v, want reactivated with the marker cleared", got)
	}
	in.Step = mmStep(t, seataccess.OpEnable, seataccess.VerbMint)
	if minted, err := mmApply(t, session, in); err != nil || len(minted.Minted) != 1 {
		t.Fatalf("mint = %+v, %v", minted, err)
	}
}

// REMOVE DELETES NOTHING, which would delete every post: a bot already off is
// left exactly as it is.
func TestMattermostRemoveKeepsTheBot(t *testing.T) {
	t.Parallel()
	chat := newAccessChat(t)
	bot := chat.bot("sre", member)
	bot.deleteAt, bot.description = 1, mattermost.DisconnectedDescription
	_, err := mmApply(t, chat.open(t), seataccess.ApplyInput{
		Step: mmStep(t, seataccess.OpRemove, seataccess.VerbRemove), Record: heldBot(bot.id),
	})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if writes := chat.wrote(); len(writes) != 0 {
		t.Errorf("writes = %q, want none", writes)
	}
}

// A ROTATION MINTS, THEN REVOKES EVERY OLDER TOKEN THIS ENGINE MINTED AND
// NEVER THE NEW ONE; without the new one remembered it revokes nothing.
func TestMattermostRotate(t *testing.T) {
	t.Parallel()
	chat := newAccessChat(t)
	bot := chat.bot("sre", member)
	old := chat.token(bot.id, "crewlet-sre")
	admins := chat.token(bot.id, "admin debugging")
	session := chat.open(t)
	in := seataccess.ApplyInput{Record: heldBot(bot.id)}

	in.Step = mmStep(t, seataccess.OpRotate, seataccess.VerbRevoke)
	if _, err := mmApply(t, chat.open(t), in); err == nil {
		t.Error("a retire with nothing minted in its session was taken")
	}
	in.Step = mmStep(t, seataccess.OpRotate, seataccess.VerbMint)
	if _, err := mmApply(t, session, in); err != nil {
		t.Fatalf("mint: %v", err)
	}
	in.Step = mmStep(t, seataccess.OpRotate, seataccess.VerbRevoke)
	if _, err := mmApply(t, session, in); err != nil {
		t.Fatalf("retire: %v", err)
	}
	tokens := chat.tokenIDs(bot.id)
	if slices.Contains(tokens, old) || !slices.Contains(tokens, admins) || len(tokens) != 2 {
		t.Errorf("tokens = %q, want the new token and the administrator's", tokens)
	}
}

// A REFUSAL NAMES MATTERMOST'S REASON, THE REMEDY AND WHO ACTS; a server error
// is a fault the sync retries; a bot gone at Mattermost is a record to close.
func TestMattermostRefusalsFaultsAndGone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  int
		refusal bool
		remedy  string
	}{
		{"forbidden", http.StatusForbidden, true, "System Admin"},
		{"unauthorized", http.StatusUnauthorized, true, "administrator token"},
		{"bad request", http.StatusBadRequest, true, "System Console"},
		{"server error", http.StatusInternalServerError, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chat := newAccessChat(t)
			bot := chat.bot("sre", member)
			chat.refuse["PUT /users/"+bot.id+"/roles"] = refusal{tc.status, "the vendor's reason"}
			_, err := mmApply(t, chat.open(t), seataccess.ApplyInput{
				Step:   mmStep(t, seataccess.OpLevel, seataccess.VerbRerole),
				Want:   seataccess.Desired{Enabled: true, Level: sysAdmin},
				Record: heldBot(bot.id),
			})
			var refused *seataccess.Refusal
			if errors.As(err, &refused) != tc.refusal {
				t.Fatalf("err = %#v, want refusal=%v", err, tc.refusal)
			}
			if tc.refusal && (!strings.Contains(refused.Said, "the vendor's reason") ||
				!strings.Contains(refused.Remedy, tc.remedy) || refused.Actor != seataccess.ByYou ||
				!strings.HasSuffix(refused.ActionURL, "/admin_console/user_management/users")) {
				t.Errorf("refusal = %+v", refused)
			}
		})
	}
	chat := newAccessChat(t)
	gone, err := mmApply(t, chat.open(t), seataccess.ApplyInput{
		Step: mmStep(t, seataccess.OpLevel, seataccess.VerbRerole),
		Want: seataccess.Desired{Level: member}, Record: heldBot("u-deleted"),
	})
	if err != nil || !gone.Gone {
		t.Errorf("a bot deleted at Mattermost = %+v, %v; want Gone", gone, err)
	}
}

// OPEN ANSWERS NOT CONFIGURED for a company that does not provision
// Mattermost, so the loop forgets the lane rather than reporting a fault.
func TestMattermostOpenNotConfigured(t *testing.T) {
	t.Parallel()
	vendor, _ := seataccess.Lookup(integration.KindMattermost)
	base := func() *config.Mattermost {
		return &config.Mattermost{Enabled: true, URL: "https://chat.example.com", Team: "nimbus",
			Provisioning: &config.MattermostProvisioning{AdminToken: "${MM_ADMIN}"}}
	}
	noTeam, noToken := base(), base()
	noTeam.Team = ""
	noToken.Provisioning.AdminToken = ""
	for name, cfg := range map[string]*config.Mattermost{
		"no block": nil, "off": {}, "no team": noTeam, "no admin token": noToken,
	} {
		company := &config.Company{}
		company.Integrations.Mattermost = cfg
		_, err := vendor.Open(context.Background(), seataccess.OpenInput{
			Config: company, Env: config.NewResolver(config.MapSource{"MM_ADMIN": "fake-admin"}),
		})
		if !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: err = %v, want not configured", name, err)
		}
	}
}
