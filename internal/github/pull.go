package github

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// A pull request read for its preview: what a link to one in chat is drawn
// with (its number, title, state, whether it is a draft, and its size) and
// nothing else, its body, comments and author never decoded, read with the same
// credential a thread's participants are, scoped to what the company's own
// apps may read.

// PullRequest is one pull request as its preview draws it.
type PullRequest struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Merged    bool   `json:"merged"`
	Draft     bool   `json:"draft"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// PullRequest reads one pull request.
func (c *Client) PullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	var out PullRequest
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls/" +
		strconv.Itoa(number)
	if err := c.get(ctx, path, nil, &out); err != nil {
		return PullRequest{}, err
	}
	return out, nil
}

// PullRef is a pull request a web address names on a code host: its owner,
// its repository and its number.
type PullRef struct {
	Owner  string
	Repo   string
	Number int
}

// ParsePullURL reads a pull request's web address on the host at web
// (`https://github.com`, or an Enterprise Server's base): its own page or any
// tab of it (`/files`, `/commits`). False for any other address, an address
// on another host included.
func ParsePullURL(web, raw string) (PullRef, bool) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(web), "/"))
	if err != nil || base.Host == "" {
		return PullRef{}, false
	}
	link, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(link.Scheme, base.Scheme) ||
		!strings.EqualFold(link.Host, base.Host) {
		return PullRef{}, false
	}
	rest, ok := strings.CutPrefix(link.Path, base.Path+"/")
	if !ok {
		return PullRef{}, false
	}
	parts := strings.Split(strings.TrimRight(rest, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" || parts[0] == "" || parts[1] == "" {
		return PullRef{}, false
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return PullRef{}, false
	}
	return PullRef{Owner: parts[0], Repo: parts[1], Number: number}, true
}

// Canonical is the pull request's own page on the host at web.
func (r PullRef) Canonical(web string) string {
	return strings.TrimRight(web, "/") + "/" + url.PathEscape(r.Owner) + "/" +
		url.PathEscape(r.Repo) + "/pull/" + strconv.Itoa(r.Number)
}

// PullRequest reads one pull request through the first agent's app that can,
// as [SeatLookup.Of] reads a thread's participants: an app the repository is
// not installed on answers 404, and the next is asked.
func (l *SeatLookup) PullRequest(ctx context.Context, ref PullRef) (PullRequest, error) {
	var out PullRequest
	err := l.read(ctx, func(client *Client) error {
		pr, err := client.PullRequest(ctx, ref.Owner, ref.Repo, ref.Number)
		if err == nil {
			out = pr
		}
		return err
	})
	if err != nil {
		return PullRequest{}, fmt.Errorf("github: read %s/%s#%d: %w", ref.Owner, ref.Repo,
			ref.Number, err)
	}
	return out, nil
}
