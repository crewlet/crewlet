package memory

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP checks ---------------------------------------------------------- //

// The model retries' class machinery (modelrecords.go), keyed by the server's
// name: one record per server, its latest test. Nothing watches it, so the
// update shape is the record itself.

func newCheckRecords() entryRecords[coord.MCPCheck, coord.MCPCheck] {
	return entryRecords[coord.MCPCheck, coord.MCPCheck]{
		stamp: func(v coord.MCPCheck, version uint64) coord.MCPCheck {
			v.At, v.Version = v.At.UTC(), version
			return v
		},
		update: func(v *coord.MCPCheck, _ string, _ bool) coord.MCPCheck {
			if v == nil {
				return coord.MCPCheck{}
			}
			return *v
		},
	}
}

var errNoCheckServer = errors.New("coord/memory: an MCP check needs the server's name")

// MCPCheck reads one server's latest check.
func (f *Fleet) MCPCheck(_ context.Context, server string) (coord.MCPCheck, bool, error) {
	if server == "" {
		return coord.MCPCheck{}, false, errNoCheckServer
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.checks.records[server]
	return c, ok, nil
}

// ListMCPChecks returns every server's latest check, ordered by server.
func (f *Fleet) ListMCPChecks(context.Context) ([]coord.MCPCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks.sortedLocked(), nil
}

// PutMCPCheck records a check, keeping the later stamp.
func (f *Fleet) PutMCPCheck(_ context.Context, c coord.MCPCheck) (coord.MCPCheck, error) {
	if err := c.Validate(); err != nil {
		return coord.MCPCheck{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.checks.records[c.Server]; ok && !c.At.After(held.At) {
		return held, nil
	}
	return f.checks.storeLocked(f, c.Server, c), nil
}

// DeleteMCPCheck removes a server's check at a version.
func (f *Fleet) DeleteMCPCheck(_ context.Context, server string, version uint64) (bool, error) {
	if server == "" {
		return false, errNoCheckServer
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.checks.records[server]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.checks.removeLocked(server)
	return true, nil
}
