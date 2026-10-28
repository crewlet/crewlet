package queries_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE ANSWER EVERY READER GETS CARRIES HOSTS AND NOTHING ELSE.
//
// `link_hosts` is everybody's because it is the least of the integrations a
// reader needs to link a reference, and the moment it carries one more thing
// it is a second `integrations` answer with the administrator's gate taken
// off. So the fixture configures every app with everything a block can hold
// that a reader must not read (each credential, the organization, group,
// repositories, projects and team), and with a credential, a query and a
// fragment written into an instance URL, and the case fails on any of them
// reaching the wire, on any field beside the two, and on any kind outside the
// closed set.
func TestTheLinkHostsAnswerCarriesHostsAndNothingElse(t *testing.T) {
	t.Parallel()
	const secret = "SECRETMARK"
	cfg := company(t)
	cfg.Integrations.GitHub = &config.GitHub{
		Enabled: true, URL: "${GHE_URL}", WebhookSecret: secret + "-gh-hook", Token: secret + "-gh-token",
		Provisioning: &config.GitHubProvisioning{Org: "ORGMARK", Repos: []string{"ORGMARK/REPOMARK"}},
	}
	cfg.Integrations.GitLab = &config.GitLab{
		Enabled: true, URL: "https://deploy:" + secret + "@GitLab.Example.com/scm/?private_token=" + secret + "#frag",
		SigningSecret: secret + "-gl-sign", Token: secret + "-gl-token",
		Provisioning: &config.GitLabProvisioning{
			Group: "GROUPMARK", AdminToken: secret + "-gl-admin", Projects: []string{"GROUPMARK/PROJECTMARK"},
		},
	}
	cfg.Integrations.Mattermost = &config.Mattermost{Enabled: true, URL: "https://chat.example.com/", Team: "TEAMMARK"}
	cfg.Integrations.Jira = &config.Jira{
		CloudID: "CLOUDMARK", SiteURL: "https://acme.atlassian.net", Token: secret + "-jira", Email: "EMAILMARK@example.com",
		WebhookSecret: secret + "-jira-hook",
	}
	cfg.Integrations.Confluence = &config.Confluence{
		URL: "https://wiki.example.com/confluence", Token: secret + "-wiki", WebhookSecret: secret + "-wiki-hook",
	}
	env := map[string]string{"GHE_URL": "https://github.example.com/api/v3"}
	body := asMap(t, answer(t, queries.Sources{
		Company: companySource(t, cfg),
		Env:     func(name string) (string, bool) { v, ok := env[name]; return v, ok },
	}, "link_hosts", nil))

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, mark := range []string{secret, "ORGMARK", "REPOMARK", "GROUPMARK", "PROJECTMARK", "TEAMMARK", "CLOUDMARK", "EMAILMARK", "frag", "private_token", "deploy", "${"} {
		if strings.Contains(string(raw), mark) {
			t.Errorf("the answer every reader gets carries %q: %s", mark, raw)
		}
	}
	rows := rowsOf(t, body["hosts"])
	holdShape(t, "LinkHostsAnswer", []map[string]any{body}, false)
	holdShape(t, "LinkHost", rows, false)

	var got []string
	for _, row := range rows {
		if len(row) != 2 {
			t.Errorf("a host row carries %d fields, want kind and url alone: %v", len(row), row)
		}
		kind, _ := row["kind"].(string)
		if !queries.LinkHostKind(kind).Valid() {
			t.Errorf("a host row names kind %q, outside the closed set", kind)
		}
		got = append(got, kind+" "+row["url"].(string))
	}
	want := []string{
		// RESOLVED, and the browser's address rather than the REST base.
		"github https://github.example.com",
		// The credential, query and fragment gone; the host lowercased and
		// the path prefix kept, since a GitLab can live under one.
		"gitlab https://gitlab.example.com/scm",
		"mattermost https://chat.example.com",
		// A cloud id's site, never its API gateway.
		"jira https://acme.atlassian.net",
		"confluence https://wiki.example.com/confluence",
	}
	if !slices.Equal(got, want) {
		t.Errorf("hosts:\n got %q\nwant %q", got, want)
	}
}

// AN ADDRESS A BROWSER CANNOT OPEN IS NO HOST: a cloud id with no site URL
// has only an API gateway, and a URL with no http scheme is not a link a
// reader should be handed. Neither adds a row, and a company with no apps
// answers an empty list rather than nothing.
func TestALinkHostIsOnlyAnAddressABrowserOpens(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func(*config.Integrations)
		want []string
	}{
		{"no apps", func(*config.Integrations) {}, nil},
		{"github.com when no url is given", func(in *config.Integrations) {
			in.GitHub = &config.GitHub{Enabled: true}
		}, []string{"github https://github.com"}},
		{"a cloud id with no site", func(in *config.Integrations) {
			in.Jira = &config.Jira{CloudID: "abc"}
		}, nil},
		{"a scheme no browser follows", func(in *config.Integrations) {
			in.Mattermost = &config.Mattermost{URL: "javascript:alert(1)"}
			in.GitLab = &config.GitLab{URL: "ftp://gitlab.example.com"}
		}, nil},
		{"a reference nothing resolves", func(in *config.Integrations) {
			in.GitLab = &config.GitLab{URL: "${UNSET_GITLAB_URL}"}
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := company(t)
			// FROM NOTHING: the shared fixture configures apps of its own.
			cfg.Integrations = config.Integrations{}
			tc.set(&cfg.Integrations)
			body := asMap(t, answer(t, queries.Sources{
				Company: companySource(t, cfg),
				Env:     func(string) (string, bool) { return "", false },
			}, "link_hosts", nil))
			var got []string
			for _, row := range rowsOf(t, body["hosts"]) {
				got = append(got, row["kind"].(string)+" "+row["url"].(string))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("hosts = %q, want %q", got, tc.want)
			}
		})
	}
}

// THE DASHBOARD KNOWS EXACTLY THE KINDS THIS ANSWER SENDS: a kind it has not
// heard of is a host it draws no mark for, and one it declares that the
// engine never sends is a branch nothing reaches.
func TestTheDashboardKnowsExactlyTheLinkHostKinds(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Union(clientsource.Tree(t), "LinkHostKind")
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the client's declaration", err)
	}
	want := make([]string, 0, len(queries.LinkHostKinds))
	for _, k := range queries.LinkHostKinds {
		want = append(want, string(k))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's LinkHostKind is %v; the engine sends %v", got, want)
	}
}

// NOBODY ASKING IS REFUSED, AND AN UNREADABLE IDENTITY ESTATE IS A RETRY: the
// registry's two unresolved arms, held for this question on the production
// registration rather than on a stand-in, since "everybody" is the posture
// most easily read as "anybody".
func TestTheLinkHostsAreAnsweredOnlyToSomebody(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Company: companySource(t, company(t))})

	_, anonymous := r.Answer(iam.WithAnonymous(t.Context()), "link_hosts", nil)
	if !errors.Is(anonymous, queries.ErrUnauthenticated) {
		t.Errorf("a caller presenting nothing was not refused as nobody: %v", anonymous)
	}
	_, unread := r.Answer(iam.WithUnresolved(t.Context(), errors.New("estate behind")), "link_hosts", nil)
	if !errors.Is(unread, queries.ErrUnavailable) || errors.Is(unread, queries.ErrUnauthorized) {
		t.Errorf("an unreadable identity estate was not a retry: %v", unread)
	}
	// A PERSON HOLDING NOTHING is somebody, and the hosts are theirs.
	nothing := iam.WithPrincipal(t.Context(), iam.Principal{
		ID: uuid.New(), Login: "jane", Kind: iam.KindPerson, Stage: iam.StageActive,
	})
	if _, err := r.Answer(nothing, "link_hosts", nil); err != nil {
		t.Errorf("a person holding no app was refused the hosts: %v", err)
	}
}
