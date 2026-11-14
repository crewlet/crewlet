package slack

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
)

// Slack's half of the atlas (ADR-0139): the workspace's own id, name and
// address, on the account the settings already name, asked once a read with
// a bot token one of the agents' apps already holds. It needs no scope.
//
// THE WORKSPACE, AND NOBODY IN IT: the answer also says which bot user asked,
// which is not decoded, and no member, channel or message is ever listed.

func init() { collect.Register(atlasSource{}) }

// atlasSource is Slack's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindSlack }

// partWorkspace is the one part of Slack a read can be refused.
const partWorkspace = "workspace"

// atlasSeat is one agent's app the read may ask through.
type atlasSeat struct{ handle, token string }

// atlasSeats is every agent seat whose Slack app is switched on and whose bot
// token resolves, by handle. A seat switched off is not asked through: its
// token is revoked on purpose.
func atlasSeats(in collect.Input) ([]atlasSeat, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("slack: %w: no company document", integration.ErrNotConfigured)
	}
	if in.Config.Integrations.Slack == nil {
		return nil, fmt.Errorf("slack: %w: there is no integrations.slack block", integration.ErrNotConfigured)
	}
	var out []atlasSeat
	for role := range in.Config.EachRole() {
		seat := role.Seat()
		if seat == nil || !seat.IsAgent() || !role.SeatAccessEnabled(string(integration.KindSlack)) || in.Env == nil {
			continue
		}
		ref := strings.TrimSpace(role.Integrations.Slack.BotToken)
		if ref == "" {
			ref = "${" + SeatVar(seat.Handle(), FieldBotToken) + "}"
		}
		if token := strings.TrimSpace(in.Env.Value(ref)); token != "" {
			out = append(out, atlasSeat{handle: seat.Handle(), token: token})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("slack: %w: no agent has a Slack app installed to ask through",
			integration.ErrNotConfigured)
	}
	slices.SortFunc(out, func(a, z atlasSeat) int { return cmp.Compare(a.handle, z.handle) })
	return out, nil
}

// workspace is what auth.test says of the workspace a token belongs to.
// CLOSED FIELDS: the answer also names the bot user and its ids.
type workspace struct {
	ID   string `json:"team_id"`
	Name string `json:"team"`
	URL  string `json:"url"`
}

// workspaceOf asks Slack which workspace the client's token belongs to,
// waiting out a short rate limit as Slack asks.
func workspaceOf(ctx context.Context, c *Client) (workspace, error) {
	for attempt := 0; ; attempt++ {
		var out workspace
		err := call(ctx, c.http, "auth.test", c.token, map[string]any{}, &out)
		limited, ok := errors.AsType[*RateLimited](err)
		if !ok {
			return out, err
		}
		if unwaited := httpx.VendorRateLimit().WaitOut(ctx, attempt, limited.RetryAfter); unwaited != nil {
			return workspace{}, fmt.Errorf("%w (%w)", err, unwaited)
		}
	}
}

// Collect asks which workspace the agents' apps are installed in and reports
// it as claims in the integration's voice. It writes nothing at Slack.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	seats, err := atlasSeats(in)
	if err != nil {
		return collect.Read{}, err
	}
	base := in.HTTP
	if base == nil {
		base = httpx.Client(ClientTimeout)
	}
	counted, calls := collect.Counted(base)
	b := collect.NewBuilder(integration.KindSlack, in.Now)
	// THE KEY THE SETTINGS GIVE THE ACCOUNT, so the two name one entry.
	account, ok := b.Ref("workspace", atlas.KindAccount, string(atlas.VendorSlack))
	if !ok {
		return collect.Read{}, errors.New("slack: the workspace is not an entry the atlas can name")
	}
	answered := false
	for _, seat := range seats {
		client, cerr := NewClient(seat.token, counted)
		if cerr != nil {
			return collect.Read{}, cerr
		}
		ws, werr := workspaceOf(ctx, client)
		if TokenDead(werr) {
			// AN APP SOMEBODY UNINSTALLED says nothing of the workspace:
			// the next agent's is asked.
			continue
		}
		if werr != nil {
			// ANYTHING ELSE IS A READ THAT DID NOT FINISH, Slack's rate
			// limit included, and fails whole.
			return collect.Read{}, fmt.Errorf("slack: ask which workspace the apps are in: %w", werr)
		}
		b.Says("workspace.team_id", account, atlas.FactAccountRef, ws.ID)
		b.Says("workspace.team", account, atlas.FactAccountName, collect.PlainLabel(ws.Name))
		b.Says("workspace.url", account, atlas.FactSite, siteOf(ws.URL))
		answered = true
		break
	}
	if !answered {
		b.Unread(collect.Unread{Part: partWorkspace, Holds: func(atlas.Claim) bool { return true },
			Detail: "The workspace was not read: Slack refused the bot token of every agent's app. Install " +
				"an agent's app in the workspace again."})
	}
	b.Calls(calls())
	return b.Read(), nil
}

// siteOf is a workspace's address as the atlas keeps it: its scheme and host
// and nothing else, "" where it names no host. The host is as Slack wrote
// it, for the builder to judge before the grammar lowers it.
func siteOf(address string) string {
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil || u.Hostname() == "" || u.Scheme == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
