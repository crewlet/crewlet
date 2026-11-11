package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// MergeMethod is how a pull request is merged, as GitHub's API spells it.
type MergeMethod string

// The three merge methods: a merge commit, one squashed commit, or the head's
// commits replayed onto the base.
const (
	MergeMethodMerge  MergeMethod = "merge"
	MergeMethodSquash MergeMethod = "squash"
	MergeMethodRebase MergeMethod = "rebase"
)

// Valid reports one of the three methods GitHub offers.
func (m MergeMethod) Valid() bool {
	switch m {
	case MergeMethodMerge, MergeMethodSquash, MergeMethodRebase:
		return true
	}
	return false
}

// MergeResult is GitHub's answer to a merge it made.
type MergeResult struct {
	Merged bool `json:"merged"`

	// SHA is the commit the merge left on the base branch.
	SHA string `json:"sha"`

	Message string `json:"message"`
}

// GitHub's refusals of a merge, each wrapped with the [APIError] that carries
// GitHub's own message (which names the missing check or review), so a caller
// words the kind with errors.Is and quotes the detail with errors.As.
var (
	// ErrNotMergeable is GitHub's 405: the pull request cannot be merged as
	// it stands (a required check or review is missing, it conflicts, it is
	// a draft, or the repository does not allow this merge method).
	ErrNotMergeable = errors.New(
		"github: GitHub will not merge this pull request as it stands")

	// ErrHeadMoved is GitHub's 409: the head is no longer the commit the
	// decision was made on, so nothing was merged. The new head needs a
	// decision of its own.
	ErrHeadMoved = errors.New(
		"github: the pull request's head moved after it was decided on, so nothing " +
			"was merged: the new head needs its own decision")

	// ErrMergeInvalid is GitHub's 422: it refused the request itself.
	ErrMergeInvalid = errors.New("github: GitHub refused the merge request as invalid")

	// ErrMergeForbidden is GitHub's 403 that is not its rate limit: the app
	// may not merge here. A merge needs Contents: Read and write on the
	// repository, and branch protection or a ruleset can refuse an app that
	// holds it.
	ErrMergeForbidden = errors.New(
		"github: this agent's app may not merge this pull request: it needs Contents: " +
			"Read and write on the repository, and the base branch's protection or " +
			"rulesets must let it merge")
)

// MergePull merges the pull request number at exactly sha, by method.
//
// THE SHA IS REQUIRED. GitHub merges whatever the head is when the sha is
// left out, so a push between the decision and the merge would land code
// nobody decided on; named, GitHub refuses the merge once the head has moved
// ([ErrHeadMoved]).
func (c *Client) MergePull(
	ctx context.Context, owner, repo string, number int, sha string, method MergeMethod,
) (MergeResult, error) {
	switch {
	case number <= 0:
		return MergeResult{}, fmt.Errorf("github: %d is not a pull request number", number)
	case !fullSHA(sha):
		return MergeResult{}, fmt.Errorf("github: %q is not a full commit id, and a merge "+
			"names the exact head it was decided on", sha)
	case !method.Valid():
		return MergeResult{}, fmt.Errorf("github: %q is not a merge method: the choices "+
			"are merge, squash and rebase", string(method))
	}
	body := struct {
		SHA         string      `json:"sha"`
		MergeMethod MergeMethod `json:"merge_method"`
	}{sha, method}
	var out MergeResult
	path := repoPath(owner, repo) + "/pulls/" + strconv.Itoa(number) + "/merge"
	if err := c.do(ctx, http.MethodPut, path, nil, body, &out); err != nil {
		return MergeResult{}, mergeRefusal(err)
	}
	if !out.Merged {
		// A 200 that did not merge is a shape GitHub does not document;
		// read as merged, it would report code landed that did not.
		return out, fmt.Errorf("%w: GitHub answered without merging: %s", ErrNotMergeable, out.Message)
	}
	return out, nil
}

// mergeRefusal wraps GitHub's refusal of a merge in the sentinel for its
// kind, keeping the [APIError] beneath it. A refusal of another kind (a 404,
// a rate limit, a 5xx) is returned as it came.
func mergeRefusal(err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch {
	case apiErr.Status == http.StatusMethodNotAllowed:
		return fmt.Errorf("%w: %w", ErrNotMergeable, err)
	case apiErr.Status == http.StatusConflict:
		return fmt.Errorf("%w: %w", ErrHeadMoved, err)
	case apiErr.Status == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %w", ErrMergeInvalid, err)
	case apiErr.Forbidden():
		return fmt.Errorf("%w: %w", ErrMergeForbidden, err)
	}
	return err
}

// Contains reports whether ancestor is reachable from commit: whether a
// deploy of commit carries ancestor. Both are full commit ids, since a branch
// moves between the read and whatever acts on it.
//
// Read from GitHub's comparison of ancestor...commit: commit ahead of ancestor,
// or the same commit, contains it; behind or diverged does not.
func (c *Client) Contains(ctx context.Context, owner, repo, commit, ancestor string) (bool, error) {
	for _, sha := range []string{commit, ancestor} {
		if !fullSHA(sha) {
			return false, fmt.Errorf("github: %q is not a full commit id", sha)
		}
	}
	var out struct {
		Status string `json:"status"`
	}
	path := repoPath(owner, repo) + "/compare/" + url.PathEscape(ancestor+"..."+commit)
	// ONE COMMIT PER PAGE: only the status is read, and the comparison
	// otherwise lists up to 250 commits a long way ahead.
	if err := c.get(ctx, path, url.Values{"per_page": {"1"}}, &out); err != nil {
		return false, fmt.Errorf("github: compare %s/%s %s...%s: %w", owner, repo, ancestor, commit, err)
	}
	switch out.Status {
	case "ahead", "identical":
		return true, nil
	case "behind", "diverged":
		return false, nil
	}
	return false, fmt.Errorf("github: comparing %s/%s %s...%s answered %q, which is none of "+
		"ahead, identical, behind or diverged", owner, repo, ancestor, commit, out.Status)
}
