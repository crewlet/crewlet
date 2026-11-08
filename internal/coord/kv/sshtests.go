package kv

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- SSH tests ----------------------------------------------------------- //

// One record per server and authority, its latest Test connection, filed in
// the positions register under a class of its own: see internal/coord's
// sshtest.go. The MCP checks' verbs (mcpchecks.go), over the entry.

var sshTestClass = entryClass[coord.SSHTest]{
	name:   "the SSH tests",
	filter: coord.DocumentFilter(coord.SSHTestClass),
	key:    coord.SSHTestKey,
	entry:  coord.SSHTestEntryOf,
	stored: func(s coord.SSHTest, entry string, version uint64) coord.SSHTest {
		// A RECORD DESCRIBES THE SERVER AND AUTHORITY IT IS FILED UNDER,
		// whatever its body says.
		s.Host, s.Authority, _ = coord.SSHTestOf(entry)
		s.At, s.Version = s.At.UTC(), version
		return s
	},
}

// SSHTest reads one server's latest test with one authority.
func (f *FleetStore) SSHTest(ctx context.Context, host, authority string) (coord.SSHTest, bool, error) {
	return sshTestClass.read(ctx, f, coord.SSHTestEntry(host, authority))
}

// ListSSHTests returns every latest test, ordered by host, then authority.
func (f *FleetStore) ListSSHTests(ctx context.Context) ([]coord.SSHTest, error) {
	return sshTestClass.list(ctx, f)
}

// PutSSHTest records a test, keeping the later stamp, as [FleetStore.PutMCPCheck]
// does and bounded by the same [fleetCASAttempts].
func (f *FleetStore) PutSSHTest(ctx context.Context, s coord.SSHTest) (coord.SSHTest, error) {
	if err := s.Validate(); err != nil {
		return coord.SSHTest{}, err
	}
	entry := coord.SSHTestEntry(s.Host, s.Authority)
	var stored coord.SSHTest
	settled, err := casRetry(ctx, &f.positionTrips, "record the SSH test of "+s.Host, func() (bool, error) {
		held, found, err := sshTestClass.read(ctx, f, entry)
		if err != nil {
			return false, err
		}
		if found && !s.At.After(held.At) {
			stored = held
			return true, nil
		}
		var wrote bool
		if found {
			stored, wrote, err = sshTestClass.update(ctx, f, entry, s, held.Version)
		} else {
			stored, wrote, err = sshTestClass.create(ctx, f, entry, s)
		}
		return wrote, err
	})
	switch {
	case err != nil:
		return coord.SSHTest{}, err
	case !settled:
		return coord.SSHTest{}, contended("PutSSHTest", entry)
	}
	return stored, nil
}

// DeleteSSHTest removes a test at a version.
func (f *FleetStore) DeleteSSHTest(ctx context.Context, host, authority string, version uint64) (bool, error) {
	return sshTestClass.remove(ctx, f, coord.SSHTestEntry(host, authority), version)
}
