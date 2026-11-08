package memory

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- SSH tests ----------------------------------------------------------- //

// The MCP checks' machinery (mcpchecks.go), keyed by the server and authority's
// entry ([coord.SSHTestEntry]): one record each, its latest test.

func newSSHTestRecords() entryRecords[coord.SSHTest, coord.SSHTest] {
	return entryRecords[coord.SSHTest, coord.SSHTest]{
		stamp: func(v coord.SSHTest, version uint64) coord.SSHTest {
			v.At, v.Version = v.At.UTC(), version
			return v
		},
		update: func(v *coord.SSHTest, _ string, _ bool) coord.SSHTest {
			if v == nil {
				return coord.SSHTest{}
			}
			return *v
		},
	}
}

var errNoSSHTestHost = errors.New("coord/memory: an SSH test needs the server's id and the authority's fingerprint")

// SSHTest reads one server's latest test with one authority.
func (f *Fleet) SSHTest(_ context.Context, host, authority string) (coord.SSHTest, bool, error) {
	if host == "" || authority == "" {
		return coord.SSHTest{}, false, errNoSSHTestHost
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sshTests.records[coord.SSHTestEntry(host, authority)]
	return s, ok, nil
}

// ListSSHTests returns every latest test, ordered by host, then authority.
func (f *Fleet) ListSSHTests(context.Context) ([]coord.SSHTest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sshTests.sortedLocked(), nil
}

// PutSSHTest records a test, keeping the later stamp.
func (f *Fleet) PutSSHTest(_ context.Context, s coord.SSHTest) (coord.SSHTest, error) {
	if err := s.Validate(); err != nil {
		return coord.SSHTest{}, err
	}
	entry := coord.SSHTestEntry(s.Host, s.Authority)
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.sshTests.records[entry]; ok && !s.At.After(held.At) {
		return held, nil
	}
	return f.sshTests.storeLocked(f, entry, s), nil
}

// DeleteSSHTest removes a test at a version.
func (f *Fleet) DeleteSSHTest(_ context.Context, host, authority string, version uint64) (bool, error) {
	if host == "" || authority == "" {
		return false, errNoSSHTestHost
	}
	entry := coord.SSHTestEntry(host, authority)
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.sshTests.records[entry]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.sshTests.removeLocked(entry)
	return true, nil
}
