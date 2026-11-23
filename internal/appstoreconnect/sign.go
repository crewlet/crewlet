package appstoreconnect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/es256"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// Who signs a seat's call: package doc, "A seat's own key".

// seatKeys holds each seat's own-key client, rebuilt when its key changes.
type seatKeys struct {
	mu      sync.Mutex
	clients map[string]seatClient
}

type seatClient struct {
	keyID, team string
	client      *Client
}

// signer is who signs handle's calls, or the refusal of every call.
func (p *Product) signer(ctx context.Context, handle string) (seataccess.Signer, error) {
	if p.deps.Identity == nil {
		return seataccess.Signer{}, nil
	}
	signer, err := p.deps.Identity(ctx, handle)
	if refused, ok := errors.AsType[*seataccess.IdentityRefusal](err); ok {
		return signer, growth.Refuse(growth.RefusedForbidden, refused.Error()+" No call was made.")
	}
	if err != nil {
		return signer, growth.Refuse(growth.RefusedUnavailable, "Crewlet could not read which identity signs "+
			"this agent's calls. No call was made.")
	}
	return signer, nil
}

// sign decides who signs m for handle: the client, the key slot the access
// log names, and whose identity it is.
func (p *Product) sign(ctx context.Context, handle string, m *made) error {
	signer, err := p.signer(ctx, handle)
	if err != nil {
		return err
	}
	admin := m.class == growth.ClassAdmin
	read := m.op.Method == "GET"
	company := func() error {
		slot, ok := keyFor(m.op, m.team.slots, admin)
		if !ok {
			return noKey(m.op, m.team.slots)
		}
		m.slot, m.signer = slot, growth.SignerCompany
		return nil
	}
	if !signer.Own || signer.Record == nil {
		return company()
	}
	rec := signer.Record
	var team, role string
	if rec.Check != nil {
		team, role = rec.Check.Team, roleID(rec.Check.Role)
	}
	switch {
	case team != m.team.id && read:
		return company()
	case team != m.team.id:
		return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("this agent's own key is the %s team's, and %s "+
			"acts in the %s team: its changes go through its own key only. No call was made.", team, m.op.ID,
			m.team.id))
	case growth.Permits(m.op, []string{role}):
		c, err := p.seatClient(handle, m.team, rec)
		if err != nil {
			return err
		}
		m.own, m.slot, m.signer = c, slotOf(role), growth.SignerOwn
		return nil
	case read:
		return company()
	case admin && !growth.Permits(m.op, []string{RoleAppManager}):
		// ONLY ADMIN MAY MAKE IT, and an administrator allowed it: the
		// company's Admin key, which no seat ever holds.
		return company()
	}
	return growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("this agent's own key holds the %s role, which "+
		"cannot make %s, and its changes go through its own key only. A person replaces it with a key at the "+
		"%s role on the integration's Agents tab. No call was made.", roleName(role), m.op.ID,
		words.Or(rolesNames(slices.DeleteFunc(slices.Clone(m.op.Roles), func(r string) bool { return r == RoleAdmin })))))
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

// slotOf is the document slot of an Apple role, which the access log names.
func slotOf(role string) string {
	for slot, r := range slotRole {
		if r == role {
			return slot
		}
	}
	return ""
}

// seatClient is the client signing with handle's own key in t, read from the
// sealed store once per key.
func (p *Product) seatClient(handle string, t *team, rec *seataccess.IdentityRecord) (*Client, error) {
	p.seats.mu.Lock()
	defer p.seats.mu.Unlock()
	if c, ok := p.seats.clients[handle]; ok && c.keyID == rec.ID && c.team == t.id {
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
	p.seats.clients[handle] = seatClient{keyID: rec.ID, team: t.id, client: c}
	return c, nil
}

// do makes req as whoever signs m.
func (m made) do(ctx context.Context, req Request) (Response, error) {
	if m.own != nil {
		return m.own.Do(ctx, ownSlot, req)
	}
	return m.team.client.Do(ctx, m.slot, req)
}
