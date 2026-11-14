package mattermost

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Mattermost's half of the atlas (ADR-0139): the id and the display name of
// the team the agents' bots belong to, on the account the settings already
// name, read with one GET under a credential the engine already holds.
//
// THE TEAM, AND NOBODY IN IT: no member, channel or post is ever listed, and
// a team's row is decoded for its id and its display name alone.

func init() { collect.Register(atlasSource{}) }

// atlasSource is Mattermost's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindMattermost }

// partTeam is the one part of Mattermost a read can be refused.
const partTeam = "team"

// atlasAccess is what a read of the team is made with.
type atlasAccess struct {
	url  string
	team string
	// key is the account's key, as the settings give it.
	key string
	// tokens is every credential the read may ask with, the narrowest first:
	// each agent's bot by handle, then the provisioning token.
	tokens []string
}

// openAtlas resolves the access a read is made with, and answers
// [integration.ErrNotConfigured] where the company gave none.
func openAtlas(in collect.Input) (atlasAccess, error) {
	if in.Config == nil {
		return atlasAccess{}, fmt.Errorf("mattermost: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Mattermost
	if cfg == nil || !cfg.Enabled || in.Env == nil {
		return atlasAccess{}, fmt.Errorf("mattermost: %w: integrations.mattermost is off",
			integration.ErrNotConfigured)
	}
	a := atlasAccess{url: strings.TrimSpace(in.Env.Value(cfg.URL)), team: strings.TrimSpace(in.Env.Value(cfg.Team)),
		key: string(atlas.VendorMattermost)}
	if a.url == "" || a.team == "" {
		return atlasAccess{}, fmt.Errorf("mattermost: %w: integrations.mattermost.url and team did not both resolve",
			integration.ErrNotConfigured)
	}
	// THE KEY THE SETTINGS GIVE THE ACCOUNT: the host of the address as the
	// document writes it, so the two name one entry.
	if u, err := url.Parse(strings.TrimSpace(cfg.URL)); err == nil && u.Hostname() != "" {
		a.key += "/" + strings.ToLower(u.Hostname())
	}
	type seatToken struct{ handle, token string }
	var seats []seatToken
	for role := range in.Config.EachRole() {
		seat := role.Seat()
		if seat == nil || !seat.IsAgent() || !role.SeatAccessEnabled(string(integration.KindMattermost)) {
			continue
		}
		ref := strings.TrimSpace(role.Integrations.Mattermost.BotToken)
		if ref == "" {
			ref = "${" + seataccess.SinkName(integration.KindMattermost, BotTokenField, seat.Handle()) + "}"
		}
		if token := strings.TrimSpace(in.Env.Value(ref)); token != "" {
			seats = append(seats, seatToken{seat.Handle(), token})
		}
	}
	slices.SortFunc(seats, func(x, z seatToken) int { return cmp.Compare(x.handle, z.handle) })
	for _, s := range seats {
		a.tokens = append(a.tokens, s.token)
	}
	if cfg.Provisioning != nil {
		if admin := strings.TrimSpace(in.Env.Value(cfg.Provisioning.AdminToken)); admin != "" {
			a.tokens = append(a.tokens, admin)
		}
	}
	if len(a.tokens) == 0 {
		return atlasAccess{}, fmt.Errorf("mattermost: %w: no agent's bot token and no provisioning token "+
			"resolved, so there is nothing to read with", integration.ErrNotConfigured)
	}
	return a, nil
}

// teamIdentity is a team's row as the read decodes it. CLOSED FIELDS: a row
// also carries the team's contact address and its invite id.
type teamIdentity struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// teamIdentityOf reads the team with this slug.
func (c *Client) teamIdentityOf(ctx context.Context, slug string) (teamIdentity, error) {
	var out teamIdentity
	_, err := c.request(ctx, http.MethodGet, "/teams/name/"+url.PathEscape(slug), nil, &out, true)
	return out, err
}

// refusedTeam reports Mattermost refusing one credential the team, as opposed
// to failing to answer: a token it no longer knows, or a bot that is not a
// member of the team. Never its rate limit, which the client waits out.
func refusedTeam(err error) bool {
	status := Status(err)
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound
}

// Collect reads the team's identity and reports it as claims in the
// integration's voice. It writes nothing at Mattermost.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	access, err := openAtlas(in)
	if err != nil {
		return collect.Read{}, err
	}
	base := in.HTTP
	if base == nil {
		base = httpx.Client(DefaultTimeout)
	}
	counted, calls := collect.Counted(base)
	b := collect.NewBuilder(integration.KindMattermost, in.Now)
	account, ok := b.Ref("team", atlas.KindAccount, access.key)
	if !ok {
		return collect.Read{}, fmt.Errorf("mattermost: %w: integrations.mattermost.url names no host the atlas "+
			"can key the server by", integration.ErrNotConfigured)
	}
	answered := false
	for _, token := range access.tokens {
		client, cerr := NewClient(ClientOptions{URL: access.url, Token: token, HTTP: counted})
		if cerr != nil {
			return collect.Read{}, fmt.Errorf("%w: %w", integration.ErrNotConfigured, cerr)
		}
		team, terr := client.teamIdentityOf(ctx, access.team)
		if refusedTeam(terr) {
			// ONE CREDENTIAL'S REFUSAL says nothing of the team: a bot
			// somebody deactivated, or one that never joined it.
			continue
		}
		if terr != nil {
			// ANYTHING ELSE IS A READ THAT DID NOT FINISH, and fails whole.
			return collect.Read{}, fmt.Errorf("mattermost: read the team: %w", terr)
		}
		b.Says("team.id", account, atlas.FactAccountRef, team.ID)
		b.Says("team.display_name", account, atlas.FactAccountName, collect.PlainLabel(team.DisplayName))
		answered = true
		break
	}
	if !answered {
		b.Unread(collect.Unread{Part: partTeam, Holds: func(atlas.Claim) bool { return true },
			Detail: "The team was not read: Mattermost showed integrations.mattermost.team to none of the " +
				"credentials the engine holds. Check the team's name, and that the agents' bots are its members."})
	}
	b.Calls(calls())
	return b.Read(), nil
}
