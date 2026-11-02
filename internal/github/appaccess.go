package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The app endpoints a seat's access is applied through.
//
// # What the app's own key can do, and what it cannot
//
// The engine holds every seat app's private key, so as the app it can read
// its own registration, suspend and unsuspend its installation, and uninstall
// it. Those are the off switch, the back-on switch and the remove the Agents
// tab offers.
//
// What it cannot do is change the registration. GitHub has no endpoint that
// edits an app's permissions or deletes an app: both are a person's acts on
// the app's settings pages ([PermissionsURL], [ManageURL]). So a raise past
// what the app is registered with waits for its owner, and a removal ends
// with a link (D5).

// Registration is what an app is registered with at GitHub: what it asks
// every installation for.
type Registration struct {
	ID          int64             `json:"id"`
	Slug        string            `json:"slug"`
	Permissions map[string]string `json:"permissions"`
	Events      []string          `json:"events"`
	Owner       struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
}

// Registration reads the app's own registration.
//
// THE OTHER HALF OF A RAISE. An installation's permissions are what the
// organization ACCEPTED; the registration's are what the app ASKS for. A
// permission the registration lacks is its owner's to add on the permissions
// page; one the registration holds and the installation lacks is a request
// the organization has still to accept. The two call for different people,
// so a waiting step has to know which it is (D5).
//
// A 404 is the app gone, as [AppClient.Exists] explains, and is returned as
// the [APIError] for the caller to read.
func (c *AppClient) Registration(ctx context.Context) (*Registration, error) {
	out := new(Registration)
	if err := c.call(ctx, http.MethodGet, "/app", nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Suspend switches an installation off. Its tokens are refused from then on,
// and an installation token already minted stops working at once, which is
// what makes it the off switch (D1): the app, its key and its installation
// all stay, so turning the seat back on needs no person.
//
// IDEMPOTENT: suspending a suspended installation is what was asked for.
func (c *AppClient) Suspend(ctx context.Context, installationID int64) error {
	return c.call(ctx, http.MethodPut, installationPath(installationID)+"/suspended", nil, nil)
}

// Unsuspend switches an installation back on.
//
// THE CALLER DECIDES WHETHER IT MAY. GitHub lets the app lift any suspension,
// including one an organization owner placed by hand during an incident, and
// D3 forbids lifting that one automatically. This method is the mechanism;
// the account ledger is what says whose suspension it is.
func (c *AppClient) Unsuspend(ctx context.Context, installationID int64) error {
	return c.call(ctx, http.MethodDelete, installationPath(installationID)+"/suspended", nil, nil)
}

// InstallationSettingsURL is an installation's settings page, where an
// organization owner accepts a permission request and chooses which
// repositories it covers.
//
// GitHub's own `html_url` is preferred where an installation was read
// ([Installation.HTMLURL]); this builds the same address for an installation
// the pass could not read, and answers "" with no id, because a broken link
// costs the trip to find out.
func InstallationSettingsURL(webBase, org string, installationID int64) string {
	if installationID == 0 {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(webBase), "/")
	if base == "" {
		base = defaultWebBase
	}
	if owner := strings.TrimSpace(org); owner != "" {
		base += "/organizations/" + url.PathEscape(owner)
	}
	return base + "/settings/installations/" + strconv.FormatInt(installationID, 10)
}

// repoPageSize is GitHub's per_page maximum for the repository listing.
const repoPageSize = 100

// repoWalkCeiling stops a repository walk that is not converging. Generous,
// since an organization with thousands of repositories is ordinary; a walk
// past it is GitHub answering pages that never end.
const repoWalkCeiling = 50_000

// InstallationRepositories lists the repositories an installation covers, as
// `owner/name`.
//
// AN INSTALLATION'S OWN READ. GitHub serves this list only to an
// installation token, not to the app's assertion, so a token carrying
// nothing but Metadata (Read-only) is minted for it, used for the walk and
// revoked straight after: it is never handed to anything else, and leaving it
// live for its hour would be a credential nobody holds a reason for.
//
// The revoke is reported in the error when the listing itself succeeded,
// because a token this engine minted and could not withdraw is worth
// knowing about; the listing is still returned with it.
func (c *AppClient) InstallationRepositories(ctx context.Context, installationID int64) ([]string, error) {
	token, err := c.mint(ctx, installationID, Permissions{}.Token(), nil)
	if err != nil {
		return nil, err
	}
	repos, listErr := c.listRepositories(ctx, token.Token)
	revokeErr := c.send(ctx, token.Token, http.MethodDelete, "/installation/token", nil, nil)
	if listErr != nil {
		return nil, errors.Join(listErr, revokeErr)
	}
	if revokeErr != nil {
		return repos, fmt.Errorf("github: the listing token could not be revoked: %w", revokeErr)
	}
	return repos, nil
}

// listRepositories walks GET /installation/repositories to exhaustion.
func (c *AppClient) listRepositories(ctx context.Context, token string) ([]string, error) {
	var out []string
	for page := 1; ; page++ {
		var body struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		path := "/installation/repositories?per_page=" + strconv.Itoa(repoPageSize) +
			"&page=" + strconv.Itoa(page)
		if err := c.send(ctx, token, http.MethodGet, path, nil, &body); err != nil {
			return nil, err
		}
		for _, repo := range body.Repositories {
			if name := strings.TrimSpace(repo.FullName); name != "" {
				out = append(out, name)
			}
		}
		if len(body.Repositories) < repoPageSize || len(out) >= body.TotalCount {
			return out, nil
		}
		if len(out) > repoWalkCeiling {
			return nil, fmt.Errorf(
				"github: the installation listed more than %d repositories without "+
					"reaching the total GitHub reported", repoWalkCeiling)
		}
	}
}
