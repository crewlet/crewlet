package expo

import (
	"context"
	"fmt"
)

// What the atlas reads at Expo: the account's projects and the latest build
// of each on each platform. THE QUERY NAMES EVERY FIELD IT IS SENT, so a
// build's credentials, its environment and its artifacts are never asked for.

// The bounds of the listing: a page of projects, and the pages a listing may
// run to, which ten thousand projects of one account do not reach.
const (
	projectPage     = 50
	maxProjectPages = 200
)

// The two platforms Expo builds for, as its AppPlatform enum spells them.
const (
	PlatformAndroid = "ANDROID"
	PlatformIOS     = "IOS"
)

// Project is one Expo project: its slug and name, the GitHub repository
// linked to it, and its latest build on each platform.
type Project struct {
	ID   string
	Slug string
	Name string

	// RepoOwner and RepoName are the linked GitHub repository, "" for none.
	RepoOwner, RepoName string

	// Builds is the latest build of each platform that has one.
	Builds []Build
}

// Build is one build: its platform and where it stands.
type Build struct {
	Platform string `json:"platform"`
	Status   string `json:"status"`
}

// Projects is the account's own name and every project of it, each with its
// latest build per platform. A robot with no role on the account answers a
// 403 or a 404.
func (c *Client) Projects(ctx context.Context, account string) (string, []Project, error) {
	type node struct {
		ID         string `json:"id"`
		Slug       string `json:"slug"`
		Name       string `json:"name"`
		Repository *struct {
			Metadata struct {
				Owner string `json:"githubRepoOwnerName"`
				Name  string `json:"githubRepoName"`
			} `json:"metadata"`
		} `json:"githubRepository"`
		Android []Build `json:"android"`
		IOS     []Build `json:"ios"`
	}
	const q = `query CrewletAtlasProjects($name: String!, $first: Int!, $after: String) { account { ` +
		`byName(accountName: $name) { name appsPaginated(first: $first, after: $after) { ` +
		`edges { node { id slug name githubRepository { metadata { githubRepoOwnerName githubRepoName } } ` +
		`android: builds(limit: 1, offset: 0, platform: ANDROID) { platform status } ` +
		`ios: builds(limit: 1, offset: 0, platform: IOS) { platform status } } } ` +
		`pageInfo { hasNextPage endCursor } } } } }`
	var (
		name  string
		out   []Project
		after any
	)
	for range maxProjectPages {
		var data struct {
			Account struct {
				ByName struct {
					Name string `json:"name"`
					Apps struct {
						Edges []struct {
							Node node `json:"node"`
						} `json:"edges"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"appsPaginated"`
				} `json:"byName"`
			} `json:"account"`
		}
		vars := map[string]any{"name": account, "first": projectPage, "after": after}
		if err := c.do(ctx, "CrewletAtlasProjects", q, vars, &data); err != nil {
			return "", nil, err
		}
		page := data.Account.ByName
		name = page.Name
		for _, e := range page.Apps.Edges {
			p := Project{ID: e.Node.ID, Slug: e.Node.Slug, Name: e.Node.Name,
				Builds: append(e.Node.Android, e.Node.IOS...)}
			if e.Node.Repository != nil {
				p.RepoOwner, p.RepoName = e.Node.Repository.Metadata.Owner, e.Node.Repository.Metadata.Name
			}
			out = append(out, p)
		}
		if !page.Apps.PageInfo.HasNextPage || page.Apps.PageInfo.EndCursor == "" {
			return name, out, nil
		}
		after = page.Apps.PageInfo.EndCursor
	}
	return "", nil, fmt.Errorf("expo: the projects of %q did not end within %d pages", account, maxProjectPages)
}
