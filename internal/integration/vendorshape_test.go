package integration_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/mattermost"
)

// EVERY VENDOR READS A REFUSAL THE SAME WAY.
//
// The rule about which statuses mean "your credential is no good" lives once,
// in [integration.Reject]. What each vendor contributes is the number, dug
// out of its own error type by its own Status accessor. That split only holds
// if every vendor actually has the accessor: the three with an explicit auth
// probe grew a private `rejected` helper instead, which was the same six
// lines written three more times and left two shapes in the tree for one job.
//
// Pinned by CALLING each accessor, so a vendor that drops it or renames it
// fails to compile here rather than quietly going back to a private copy.
func TestEveryVendorReportsARefusalStatus(t *testing.T) {
	accessors := map[string]func(error) int{
		"jira":       jira.Status,
		"confluence": confluence.Status,
		"github":     github.Status,
		"gitlab":     gitlab.Status,
		"mattermost": mattermost.Status,
	}
	for name, status := range accessors {
		// A non-API error is 0, which Reject leaves alone: a dial timeout
		// is a wait, and calling it a refusal would send an operator to
		// rotate a working credential.
		plain := errors.New("dial tcp: i/o timeout")
		if got := status(plain); got != 0 {
			t.Errorf("%s: a non-API error reported status %d, want 0", name, got)
		}
		if errors.Is(integration.Reject(plain, status(plain)), integration.ErrCredentialRejected) {
			t.Errorf("%s: a transport fault was classified as a refusal", name)
		}
	}
}
