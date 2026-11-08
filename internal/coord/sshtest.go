package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// AN SSH TEST is what the latest Test connection of one server found with one
// certificate authority: each step of the server setup proven, in order.
//
// # Who has to agree on it
//
// The whole company, now. A test is pressed through one node, shown on the
// server's row to every administrator whichever node serves them, and read
// by a rotation's promote, which is refused unless every server's latest test
// with the staged authority passed. So it is one record per server and
// authority in the coordination store, the CURRENT VALUE ONLY: a later test
// replaces it, and removing the server deletes its records.
//
// # Bounded
//
// A record names at most [maxSSHTestSteps] steps, each sentence cut to
// [MaxSSHTestDetailBytes] by the writer ([SSHTest.Validate] refuses one it
// did not). Nothing a server printed is kept beyond a step's sentence.

// SSHTest is one server's latest test with one authority.
type SSHTest struct {
	// Host is the server's id; Authority the SHA-256 fingerprint of the
	// authority the test's certificates were signed with.
	Host      string `json:"host"`
	Authority string `json:"authority"`

	// At is when the test was pressed; By, ByKind and OperatorID who
	// pressed it, as [ModelRetry] records them.
	At         time.Time     `json:"at"`
	By         string        `json:"by"`
	ByKind     iam.ActorKind `json:"by_kind"`
	OperatorID string        `json:"operator_id,omitempty"`

	// OK is whether every step passed.
	OK bool `json:"ok"`

	// Steps are the steps taken, in order.
	Steps []SSHTestStep `json:"steps"`

	// Version is the store's version of the record as it was read. Opaque.
	Version uint64 `json:"-"`
}

// SSHTestStep is one step's id, verdict and sentence.
type SSHTestStep struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// MaxSSHTestDetailBytes bounds one step's sentence: a refusal's text and a
// remedy fit in a few hundred bytes, and the rest is never a person's to read.
const MaxSSHTestDetailBytes = 1 << 10

// maxSSHTestSteps bounds a test's steps: the most Test connection takes.
const maxSSHTestSteps = 8

// Validate reports why a test cannot be written.
func (s SSHTest) Validate() error {
	switch {
	case s.Host == "" || strings.ContainsAny(s.Host, "/@."):
		return errors.New("coord: an SSH test needs the id of the server it tested")
	case !strings.HasPrefix(s.Authority, "SHA256:") || strings.ContainsAny(s.Authority, " \t\r\n"):
		return errors.New("coord: an SSH test needs the fingerprint of the authority it tested")
	case s.At.IsZero():
		return errors.New("coord: an SSH test needs the instant it was pressed")
	case s.By == "":
		return errors.New("coord: an SSH test needs who pressed it")
	case !s.ByKind.Valid():
		return errors.New("coord: an SSH test needs what sort of author pressed it " +
			"(iam.ActorFor's kind): agent, human, operator or system")
	case len(s.Steps) == 0 || len(s.Steps) > maxSSHTestSteps:
		return fmt.Errorf("coord: an SSH test names %d steps, where a test takes 1 to %d", len(s.Steps), maxSSHTestSteps)
	}
	for _, step := range s.Steps {
		if step.ID == "" || len(step.Detail) > MaxSSHTestDetailBytes {
			return fmt.Errorf("coord: an SSH test's step needs an id and a sentence of at most %d bytes",
				MaxSSHTestDetailBytes)
		}
	}
	return nil
}

// SSHTestClass is the key class a test is filed under in the positions
// register.
const SSHTestClass = "ssh_test"

// SSHTestEntry is the entry a server's test with an authority is filed under:
// the host, a slash, then the fingerprint. A host id holds no slash.
func SSHTestEntry(host, authority string) string { return host + "/" + authority }

// SSHTestOf splits an entry into its host and authority.
func SSHTestOf(entry string) (host, authority string, ok bool) {
	host, authority, ok = strings.Cut(entry, "/")
	return host, authority, ok && host != "" && authority != ""
}

// SSHTestKey is an entry's key in the register.
func SSHTestKey(entry string) string {
	host, authority, _ := SSHTestOf(entry)
	return DocumentKey(SSHTestClass, host, authority)
}

// SSHTestEntryOf recovers the entry from a key of the test class, reporting
// false for a key of another class or one this grammar did not write.
func SSHTestEntryOf(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 3 || segments[0] != SSHTestClass || segments[1] == "" || segments[2] == "" {
		return "", false
	}
	return SSHTestEntry(segments[1], segments[2]), true
}

// SSHTests is the fleet's record of each server's latest test with each
// authority.
//
// Every read RAISES rather than answering empty when the store cannot be read:
// "never tested" and "could not read" are two facts, and a promote that read
// the first for the second would act on a test it never saw.
type SSHTests interface {
	// SSHTest reads one server's latest test with one authority.
	SSHTest(ctx context.Context, host, authority string) (SSHTest, bool, error)

	// ListSSHTests returns every latest test, ordered by host, then authority.
	ListSSHTests(ctx context.Context) ([]SSHTest, error)

	// PutSSHTest records a test, keeping whichever of it and the stored one is
	// LATER, and returns the record held after the call.
	PutSSHTest(ctx context.Context, s SSHTest) (SSHTest, error)

	// DeleteSSHTest removes a test at a version, reporting whether that
	// version still held. A version of zero never holds.
	DeleteSSHTest(ctx context.Context, host, authority string, version uint64) (bool, error)
}
