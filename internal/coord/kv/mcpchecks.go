package kv

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP checks ---------------------------------------------------------- //

// One record per server, its latest test, filed in the positions register
// under a class of their own: see internal/coord's mcpcheck.go. The model
// retries' verbs (modelrecords.go), over the server's name.

var mcpCheckClass = entryClass[coord.MCPCheck]{
	name:   "the MCP checks",
	filter: coord.DocumentFilter(coord.MCPCheckClass),
	key:    coord.MCPCheckKey,
	entry:  coord.MCPCheckServerOf,
	stored: func(c coord.MCPCheck, server string, version uint64) coord.MCPCheck {
		// A RECORD DESCRIBES THE SERVER IT IS FILED UNDER, whatever its body
		// says.
		c.Server, c.At, c.Version = server, c.At.UTC(), version
		return c
	},
}

// MCPCheck reads one server's latest check.
func (f *FleetStore) MCPCheck(ctx context.Context, server string) (coord.MCPCheck, bool, error) {
	return mcpCheckClass.read(ctx, f, server)
}

// ListMCPChecks returns every server's latest check, ordered by server.
func (f *FleetStore) ListMCPChecks(ctx context.Context) ([]coord.MCPCheck, error) {
	return mcpCheckClass.list(ctx, f)
}

// PutMCPCheck records a check, keeping the later stamp: a read, then a create
// or an update at the version read, retried on a lost race, as
// [FleetStore.PutMCPRestart] is.
func (f *FleetStore) PutMCPCheck(ctx context.Context, c coord.MCPCheck) (coord.MCPCheck, error) {
	if err := c.Validate(); err != nil {
		return coord.MCPCheck{}, err
	}
	for range maxRetryRounds {
		held, found, err := mcpCheckClass.read(ctx, f, c.Server)
		if err != nil {
			return coord.MCPCheck{}, err
		}
		if found && !c.At.After(held.At) {
			return held, nil
		}
		var wrote bool
		var stored coord.MCPCheck
		if found {
			stored, wrote, err = mcpCheckClass.update(ctx, f, c.Server, c, held.Version)
		} else {
			stored, wrote, err = mcpCheckClass.create(ctx, f, c.Server, c)
		}
		if err != nil {
			return coord.MCPCheck{}, err
		}
		if wrote {
			return stored, nil
		}
	}
	return coord.MCPCheck{}, contended("PutMCPCheck", c.Server)
}

// DeleteMCPCheck removes a server's check at a version.
func (f *FleetStore) DeleteMCPCheck(ctx context.Context, server string, version uint64) (bool, error) {
	return mcpCheckClass.remove(ctx, f, server, version)
}
