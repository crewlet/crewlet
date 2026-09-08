package integration_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/mattermost"
)

// EVERY VENDOR READS A REFUSAL THE SAME WAY.
//
// The rule about which statuses mean "your credential is no good" lives once,
// in [integration.Reject]. What each third-party app contributes is the number, dug
// out of its own error type by its own Status accessor. That split only holds
// if every third-party app actually has the accessor: the three with an explicit auth
// probe grew a private `rejected` helper instead, which was the same six
// lines written three more times and left two shapes in the tree for one job.
//
// Pinned by CALLING each accessor, so a third-party app that drops it or renames it
// fails to compile here rather than quietly going back to a private copy.
func TestEveryVendorReportsARefusalStatus(t *testing.T) {
	accessors := map[string]func(error) int{
		"jira":       jira.Status,
		"confluence": confluence.Status,
		"github":     github.Status,
		"gitlab":     gitlab.Status,
		"mattermost": mattermost.Status,
		// The two this loop was written without. Atlassian is where an
		// agent's account is CREATED and Datadog is where its alerts come
		// from, and both grew a Status accessor of their own — so "every
		// vendor" meant five of seven, and the two newest were exactly the
		// ones nothing held to the rule.
		"atlassian": atlassian.Status,
		"datadog":   datadog.Status,
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

// AND THE POSITIVE HALF, which is what the accessor is FOR.
//
// The negative case above passes for an accessor that always answers 0 — a
// stub, or one whose errors.As no longer matches its own error type after a
// rename. Then every refusal reads as a transport fault, the surface reports
// "the engine is working on it" for ever, and the one action that would fix it
// is never asked for.
func TestEveryVendorReadsItsOwnRefusal(t *testing.T) {
	refusals := map[string]struct {
		status func(error) int
		err    error
	}{
		"jira":       {jira.Status, &jira.APIError{Status: 401}},
		"confluence": {confluence.Status, &confluence.APIError{Status: 401}},
		"github":     {github.Status, &github.APIError{Status: 401}},
		"gitlab":     {gitlab.Status, &gitlab.APIError{Status: 401}},
		"mattermost": {mattermost.Status, &mattermost.Error{Status: 401}},
		"atlassian":  {atlassian.Status, &atlassian.APIError{Status: 401}},
		"datadog":    {datadog.Status, &datadog.APIError{Status: 401}},
	}
	for name, tc := range refusals {
		if got := tc.status(tc.err); got != 401 {
			t.Errorf("%s: its own 401 read as status %d", name, got)
		}
		// WRAPPED, because that is how one arrives: every caller adds
		// context with %w before this is asked.
		wrapped := fmt.Errorf("reading the project: %w", tc.err)
		if got := tc.status(wrapped); got != 401 {
			t.Errorf("%s: a wrapped 401 read as status %d", name, got)
		}
		if !errors.Is(
			integration.Reject(wrapped, tc.status(wrapped)),
			integration.ErrCredentialRejected,
		) {
			t.Errorf("%s: a 401 was not classified as a refused credential", name)
		}
	}
}
