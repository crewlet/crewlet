package appstoreconnect

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// App is one app a connection's keys reach, as the agents' app chooser and the
// connection's card name it.
type App struct {
	ID       string
	Name     string `untrusted:"name"`
	BundleID string
}

// TeamReach is what one connection's keys reach, as this node last read it.
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
	for _, t := range r {
		for _, a := range t.Apps {
			if !slices.ContainsFunc(out, func(o App) bool { return o.ID == a.ID }) {
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

// noListingKey is a team's Unread where no connected key may list its apps:
// the Admin key never reads (doc.go).
const noListingKey = "Crewlet lists a team's apps with a key other than Admin, which it never reads with: " +
	"add a Marketing, Finance or Sales key to list them here"

// Reach is every connection's apps, each team's read with its least-privileged
// key that may list them and reused for reachTTL; fresh reads every team
// again, as a pass does. A team whose list cannot be read says why, and the
// rest are still read. An error is a block that is off or whose keys do not
// parse, which reaches nothing.
func (p *Product) Reach(ctx context.Context, fresh bool) (Reach, error) {
	teams, err := p.teamsNow()
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		// AN UNREAD TEAM IS ITS OWN ANSWER, recorded on it: the rest are read.
		_, _ = p.readApps(ctx, t, fresh)
	}
	return reachOf(teams), nil
}

// HeldReach is what this node last read of each connection's apps, without
// asking Apple: a connection's card answers from it, and a GET never waits on
// Apple. A team not read yet answers Read false; a block that is off or whose
// keys do not parse reaches nothing.
func (p *Product) HeldReach() Reach {
	teams, err := p.teamsNow()
	if err != nil {
		return nil
	}
	return reachOf(teams)
}

// SeatReads is [Product.Reach] as the agents' access session reads it, nil
// where the block reaches nothing.
func (p *Product) SeatReads(ctx context.Context, fresh bool) seataccess.PassReads {
	r, err := p.Reach(ctx, fresh)
	if err != nil {
		return nil
	}
	return r
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

// unreadWords is why a team's apps could not be listed, as a person acts on it.
func unreadWords(err error) string {
	if errors.Is(err, ErrNoKey) {
		return noListingKey
	}
	return "Apple did not list the team's apps: " + err.Error()
}

// Connections is each connection of block as its card shows it: the issuer it
// signs in with, the Apple role of each key it holds (the slot a key was added
// under), and the apps it reaches as held reads them. Where the company names
// apps, a team reaches those of them it lists, or every named app while its
// list is unread; where it names none, every app its keys list, and Open.
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
		team := block.Connections[id]
		c := growth.Connection{ID: id, Identity: team.IssuerID, RolesHeld: []string{}, Accounts: []growth.Account{},
			Open: len(block.Apps) == 0}
		for _, s := range team.Keys.Slots() {
			c.RolesHeld = append(c.RolesHeld, slotRole[s.Name])
		}
		r := byTeam[id]
		c.Unread = r.Unread
		switch {
		case c.Open:
			for _, a := range r.Apps {
				c.Accounts = append(c.Accounts, appAccount(id, a))
			}
		case r.Read:
			for _, a := range r.Apps {
				if slices.Contains(block.Apps, a.ID) {
					c.Accounts = append(c.Accounts, appAccount(id, a))
				}
			}
		default:
			for _, app := range block.Apps {
				c.Accounts = append(c.Accounts, appAccount(id, App{ID: app}))
			}
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
