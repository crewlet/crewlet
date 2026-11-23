package appstoreconnect

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/es256"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// Who signs a seat's call: its own key alone (package doc, "Every call is an
// agent's own").

// seatKeys holds each seat's own-key client, rebuilt when its key changes.
type seatKeys struct {
	mu      sync.Mutex
	clients map[string]seatClient
}

type seatClient struct {
	keyID, team, issuer string
	client              *Client
}

// agentKey is a calling agent's own verified key: the team it is in, its
// Apple role and the client that signs with it.
type agentKey struct {
	team   *team
	role   string
	client *Client
	// detail is the vendor number handed in with a Sales key, "" for none.
	detail string
}

// setUpIdentity is what a person does to give an agent a working key.
const setUpIdentity = "A person hands one in with Set up identity on the integration's Agents tab."

// keyOf is handle's own verified key, or the refusal of every call of its:
// Crewlet holds no key of the company's to sign with instead.
func (p *Product) keyOf(ctx context.Context, handle string) (agentKey, error) {
	var signer seataccess.Signer
	if p.deps.Identity != nil {
		var err error
		if signer, err = p.deps.Identity(ctx, handle); err != nil {
			return agentKey{}, growth.Refuse(growth.RefusedUnavailable, "Crewlet could not read which key signs "+
				"this agent's calls. No call was made.")
		}
	}
	rec := signer.Record
	if !signer.Own || !rec.Kept() {
		return agentKey{}, growth.Refuse(growth.RefusedForbidden, "this agent has no App Store Connect key of its "+
			"own yet. "+setUpIdentity+" No call was made.")
	}
	if status := rec.Status(); status != seataccess.IdentityReady && status != seataccess.IdentityReplacing {
		said := ""
		if rec.Check != nil && rec.Check.Said != "" {
			said = " " + strings.TrimSpace(rec.Check.Said)
		}
		return agentKey{}, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("this agent's own key %s is %s.%s "+
			"A person hands in a working one with Set up identity on the integration's Agents tab. No call was made.",
			rec.ID, strings.ToLower(status.Label()), said))
	}
	t, err := p.teamByID(rec.Check.Team)
	if err != nil {
		return agentKey{}, err
	}
	if t == nil {
		return agentKey{}, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("this agent's key is the %s team's, "+
			"which the company no longer connects. %s No call was made.", rec.Check.Team, setUpIdentity))
	}
	c, err := p.seatClient(handle, t, rec)
	if err != nil {
		return agentKey{}, err
	}
	return agentKey{team: t, role: roleID(rec.Check.Role), client: c, detail: rec.Detail}, nil
}

// sign holds m to what the agent's key may make: its role makes it, or the
// call is refused before Apple is asked.
func sign(m *made, level growth.Level) error {
	if growth.Permits(m.op, []string{m.key.role}) {
		return nil
	}
	others := slices.DeleteFunc(slices.Clone(m.op.Roles), func(r string) bool { return r == RoleAdmin })
	if len(rolesNames(others)) == 0 {
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("only the Admin role may make %s, and Crewlet "+
			"holds no Admin key: a person makes it in App Store Connect. No call was made.", m.op.ID))
	}
	fitting := slices.DeleteFunc(slices.Clone(others), func(r string) bool { fits, _ := roleFit(r, level); return !fits })
	if len(fitting) == 0 {
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("this agent's key holds the %s role, and only "+
			"the %s role may make %s, which an agent's key at this level never holds: a person makes it in App Store "+
			"Connect. No call was made.", roleName(m.key.role), words.Or(rolesNames(others)), m.op.ID))
	}
	return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("this agent's key holds the %s role, which cannot "+
		"make %s: it needs a key with the %s role. %s No call was made.", roleName(m.key.role), m.op.ID,
		words.Or(rolesNames(fitting)), setUpIdentity))
}

// roleID is the Apple role named name, or name where it is already one.
func roleID(name string) string {
	for _, r := range roles() {
		if r.Name == name || r.ID == name {
			return r.ID
		}
	}
	return name
}

// via is the access log's word for role: Apple's role ID, lowercase.
func via(role string) string { return strings.ToLower(role) }

// seatClient is the client signing with handle's own key in t, read from the
// sealed store once per key.
func (p *Product) seatClient(handle string, t *team, rec *seataccess.IdentityRecord) (*Client, error) {
	p.seats.mu.Lock()
	defer p.seats.mu.Unlock()
	if c, ok := p.seats.clients[handle]; ok && c.keyID == rec.ID && c.team == t.id && c.issuer == t.issuer {
		return c.client, nil
	}
	name := seataccess.SinkName(integration.KindAppStoreConnect, identityField, handle)
	if rec.Credential != nil && rec.Credential.Variable != "" {
		name = rec.Credential.Variable
	}
	key, err := es256.ParseKey(p.sealed(name))
	if err != nil {
		return nil, growth.Refuse(growth.RefusedUnavailable, "this agent's own key is not sealed on this node "+
			"yet, or does not read as a .p8 key. No call was made.")
	}
	c := p.ownClient(t, Key{ID: rec.ID, Key: key})
	if p.seats.clients == nil {
		p.seats.clients = map[string]seatClient{}
	}
	p.seats.clients[handle] = seatClient{keyID: rec.ID, team: t.id, issuer: t.issuer, client: c}
	return c, nil
}

// do makes req as the agent's key.
func (m made) do(ctx context.Context, req Request) (Response, error) {
	return m.key.client.Do(ctx, ownSlot, req)
}
