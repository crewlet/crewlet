package appstoreconnect

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// App is one app of a connection's team, as the agents' app chooser and the
// connection's card name it.
type App struct {
	ID       string
	Name     string `untrusted:"name"`
	BundleID string
}

// TeamReach is one connection's apps, as this node last read them.
type TeamReach struct {
	// Connection is the team's connection id.
	Connection string
	// Apps are the team's apps as Apple listed them, by name; meaningful
	// only where Read.
	Apps []App
	// Read is whether Apps was read; false before the first read.
	Read bool
	// Unread is why the last read failed, in words a person acts on; empty
	// where it did not. A list read before stays in Apps.
	Unread string
}

// Reach is every connection's reach, in the order a call tries the teams. It
// is what the agents' access session offers apps from
// ([seataccess.OpenInput.Reads]).
type Reach []TeamReach

// ReadFor implements [seataccess.PassReads].
func (Reach) ReadFor() integration.Kind { return integration.KindAppStoreConnect }

// Apps is every app r reaches, each once, by name.
func (r Reach) Apps() []App {
	var out []App
	seen := map[string]bool{}
	for _, t := range r {
		for _, a := range t.Apps {
			if !seen[a.ID] {
				seen[a.ID] = true
				out = append(out, a)
			}
		}
	}
	sortApps(out)
	return out
}

// sortApps orders apps by name, then Apple ID: how a person looks one up.
func sortApps(apps []App) {
	slices.SortFunc(apps, func(a, b App) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.ID, b.ID))
	})
}

// noAgentKey is a team's Unread before any agent's key in it was checked:
// Crewlet lists a team's apps with an agent's own key alone.
const noAgentKey = "App names appear once an agent's key in this team is checked: set up an agent's identity " +
	"on the Agents tab"

// Reach is every connection's apps, each team's listed with a verified agent
// key in it, or the key a check just read, and reused for reachTTL; fresh
// reads every team again, as a pass does. A team whose list cannot be read
// says why, and the rest are still read. An error is a block that is off.
func (p *Product) Reach(ctx context.Context, fresh bool) (Reach, error) {
	teams, err := p.teamsNow()
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		t.mu.Lock()
		current := t.current(p.deps.Now())
		t.mu.Unlock()
		if current && !fresh {
			continue
		}
		c := p.agentClientIn(ctx, t)
		if c == nil {
			t.mu.Lock()
			if t.appsAt.IsZero() {
				t.unread = noAgentKey
			}
			t.mu.Unlock()
			continue
		}
		// AN UNREAD TEAM IS ITS OWN ANSWER, recorded on it: the rest are read.
		_, _ = p.readApps(ctx, t, c, fresh)
	}
	return reachOf(teams), nil
}

// agentClientIn is the client of the first agent, by handle, whose own key
// works in t; nil where none does yet.
func (p *Product) agentClientIn(ctx context.Context, t *team) *Client {
	c := p.deps.Company()
	var handles []string
	for role := range c.EachRole() {
		if c.AppStoreConnectSeat(role.IdentityKey()) != nil {
			handles = append(handles, role.IdentityKey())
		}
	}
	slices.Sort(handles)
	for _, h := range handles {
		if key, err := p.keyOf(ctx, h); err == nil && key.team == t {
			return key.client
		}
	}
	return nil
}

// HeldReach is what this node last read of each connection's apps, without
// asking Apple: a connection's card answers from it, and a GET never waits on
// Apple. A team not read yet answers Read false; a block that is off reaches
// nothing.
func (p *Product) HeldReach() Reach {
	teams, err := p.teamsNow()
	if err != nil {
		return nil
	}
	return reachOf(teams)
}

// Reads is what a pass hands the agents' access session: the teams' apps,
// and the product, which checks a seat's own key with Apple.
type Reads struct {
	Reach   Reach
	product *Product
}

// ReadFor implements [seataccess.PassReads].
func (Reads) ReadFor() integration.Kind { return integration.KindAppStoreConnect }

// SeatReads is [Product.Reach] as the agents' access session reads it, with
// the product; nil where the block reaches nothing.
func (p *Product) SeatReads(ctx context.Context, fresh bool) seataccess.PassReads {
	r, err := p.Reach(ctx, fresh)
	if err != nil {
		return nil
	}
	return Reads{Reach: r, product: p}
}

// reachOf is each team's held list.
func reachOf(teams []*team) Reach {
	out := make(Reach, 0, len(teams))
	for _, t := range teams {
		t.mu.Lock()
		r := TeamReach{Connection: t.id, Read: !t.appsAt.IsZero(), Unread: t.unread}
		for _, a := range t.apps {
			r.Apps = append(r.Apps, App{ID: a.ID, Name: a.Name, BundleID: a.BundleID})
		}
		t.mu.Unlock()
		sortApps(r.Apps)
		out = append(out, r)
	}
	return out
}

// unreadWords is why a team's apps could not be listed, as a person acts on
// it, in Crewlet's words alone: never Apple's text, which is not ours to show.
func unreadWords(err error) string {
	var apiErr *APIError
	switch {
	case errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden):
		return fmt.Sprintf("Apple refused the agent's key that listed the team's apps (HTTP %d): the next "+
			"check reads the agents' keys again", apiErr.Status)
	case errors.As(err, &apiErr):
		// A RATE LIMIT OR AN OUTAGE IS APPLE'S, never a reason to replace a key.
		return fmt.Sprintf("Apple did not list the team's apps (HTTP %d); Crewlet tries again on the next "+
			"check", apiErr.Status)
	default:
		return "Crewlet could not reach Apple to list the team's apps; it tries again on the next check"
	}
}

// Connections is each connection of block as its card shows it: the issuer
// its agents' keys sign in with, and every app of its team as held reads list
// them. The company holds no key, so no connection holds a role.
func Connections(block *config.AppStoreConnect, held Reach) []growth.Connection {
	out := []growth.Connection{}
	if block == nil {
		return out
	}
	byTeam := map[string]TeamReach{}
	for _, r := range held {
		byTeam[r.Connection] = r
	}
	for _, id := range block.ConnectionIDs() {
		r := byTeam[id]
		c := growth.Connection{ID: id, Identity: block.Connections[id].IssuerID, RolesHeld: []string{},
			Accounts: []growth.Account{}, Open: true, Unread: r.Unread}
		for _, a := range r.Apps {
			c.Accounts = append(c.Accounts, appAccount(id, a))
		}
		out = append(out, c)
	}
	return out
}

// appAccount is an app as a connection's card lists it: an app has no serving
// state Crewlet reads, so it carries none.
func appAccount(connection string, a App) growth.Account {
	return growth.Account{Kind: Kind, Connection: connection, ID: a.ID, Name: a.Name, Detail: a.BundleID}
}
