package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/capability"
)

// AgentCapability is one agent seat's access as its next turn would be told
// it: what it may do at each vendor and server, and the boxes it runs in.
type AgentCapability struct {
	Handle, Name string
	Access       capability.Snapshot
}

// AgentCapabilities is every agent seat's [AgentCapability] on this node's
// company, in the chart's order, for an administrator's go-live review.
func (e *Engine) AgentCapabilities(ctx context.Context) ([]AgentCapability, error) {
	company := e.Company()
	if company == nil || company.Org == nil || company.Config == nil {
		return []AgentCapability{}, nil
	}
	out := []AgentCapability{}
	for role := range company.Org.AllRoles() {
		if !role.IsAgent() {
			continue
		}
		in, err := e.capabilityInput(ctx, company, role.Handle())
		if err != nil {
			return nil, fmt.Errorf("engine: read %s's access: %w", role.Handle(), err)
		}
		out = append(out, AgentCapability{Handle: role.Handle(), Name: role.Name, Access: capability.Take(in)})
	}
	return out, nil
}
