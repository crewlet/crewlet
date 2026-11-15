package queries_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
)

// ruleGitHub is a company's GitHub as a rule's form reads it: acme/api reads
// whole, acme/locked answers its branches and refuses its check runs, and
// any other repository no agent's app covers; noApps is a company whose
// agents have no app installed.
type ruleGitHub struct{ off, noApps bool }

func (g ruleGitHub) GitHubRepositories(context.Context) ([]github.InventoryRepo, error) {
	switch {
	case g.off:
		return nil, fmt.Errorf("engine: %w", integration.ErrNotConfigured)
	case g.noApps:
		return nil, github.ErrNoSeatCanRead
	}
	return []github.InventoryRepo{{FullName: "acme/api", DefaultBranch: "main"},
		{FullName: "acme/old", DefaultBranch: "master", Archived: true}}, nil
}

func (g ruleGitHub) GitHubBranches(_ context.Context, owner, repo string) ([]string, bool, error) {
	switch {
	case owner != "acme":
		return nil, false, fmt.Errorf("%w: %s", github.ErrOutsideCompany, owner)
	case repo == "api" || repo == "locked":
		return []string{"main", "release"}, true, nil
	}
	return nil, false, github.ErrNoSeatCanRead
}

func (g ruleGitHub) GitHubCheckNames(_ context.Context, _, repo, _ string) ([]string, error) {
	if repo == "locked" {
		return nil, fmt.Errorf("%w: %w", github.ErrNoSeatCanRead, &github.APIError{Status: 403})
	}
	return []string{"lint", "test"}, nil
}

// askRuleGitHub asks one of the form's questions as an administrator.
func askRuleGitHub(t *testing.T, g ruleGitHub, what string, params map[string]any) (map[string]any, error) {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{DelegatedGitHub: g})
	raw, err := r.Answer(asSeat(t, "admin", iam.GrantAdmin), what, params)
	if err != nil {
		return nil, err
	}
	return asMap(t, raw), nil
}

// WHY NOTHING COULD BE READ IS SAID IN WORDS a person can act on: install an
// app, give it Checks, or the owner is not the company's, and typing the name
// is always left open.
func TestTheRuleFormSaysWhyAListCouldNotBeRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, what string
		params     map[string]any
		want       string
	}{
		{"not covered", "delegated_branches", map[string]any{"repo": "acme/gone"}, "install an agent's app"},
		{"not the company's", "delegated_branches", map[string]any{"repo": "other/api"}, "not one the company's GitHub names"},
		{"no Checks", "delegated_checks", map[string]any{"repo": "acme/locked", "branch": "main"}, "Checks (Read-only)"},
	} {
		body, err := askRuleGitHub(t, ruleGitHub{}, tc.what, tc.params)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if unread, _ := body["unread"].(string); !strings.Contains(unread, tc.want) {
			t.Errorf("%s: unread is %q, want it to say %q", tc.name, unread, tc.want)
		}
	}
	body, err := askRuleGitHub(t, ruleGitHub{off: true}, "delegated_repositories", nil)
	if err != nil || body["connected"] != false || body["unread"] != nil {
		t.Errorf("a company with no GitHub answered %v (%v), want not connected and nothing unread", body, err)
	}
}

// A REPOSITORY IS OWNER/NAME AND A BRANCH A BRANCH NAME, refused as the
// parameter it is, before GitHub is asked.
func TestTheRuleFormsQuestionsRefuseBadParameters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		what   string
		params map[string]any
	}{
		{"delegated_branches", nil},
		{"delegated_branches", map[string]any{"repo": "api"}},
		{"delegated_branches", map[string]any{"repo": "acme/api/main"}},
		{"delegated_checks", map[string]any{"repo": "acme/api"}},
		{"delegated_checks", map[string]any{"repo": "acme/api", "branch": "bad..name"}},
	} {
		if _, err := askRuleGitHub(t, ruleGitHub{}, tc.what, tc.params); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s %v answered %v, want bad_params", tc.what, tc.params, err)
		}
	}
}
