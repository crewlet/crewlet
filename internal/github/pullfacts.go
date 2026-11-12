package github

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// PullFacts is one pull request as a merge decision reads it.
//
// # Every fact is about one commit
//
// A delegate's "allow" stands only on facts that hold at decision time, and
// the one that moves under them is the head: a push after the reviews were
// read would leave an approval of a commit nobody is merging. So the head is
// read once, here, and everything else is read AGAINST it: an approval counts
// only when it reviewed [PullFacts.HeadSHA], and the checks are that commit's.
// The merge then names the same commit ([Client.MergePull]), and GitHub
// refuses it if the head has moved since.
//
// # A partial answer is refused, never returned
//
// Each list is read whole ([ErrListTooLong]) and each read either answers or
// fails the call: a reviewer's later change request on page two, or a failing
// check past a cut, would make a partial answer a wrong one. What GitHub
// refuses to show the credential is the one exception, said per check
// ([CheckUnread]).
type PullFacts struct {
	Ref PullRef `json:"ref"`

	Title string `json:"title" untrusted:"name"`

	// Author is the opener's login, folded ([NormalizeLogin]).
	Author string `json:"author"`

	// State is GitHub's: open or closed. A merged pull request is closed.
	State          string     `json:"state"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at,omitempty"`
	MergeCommitSHA string     `json:"merge_commit_sha,omitempty"`
	Draft          bool       `json:"draft"`

	HeadSHA string `json:"head_sha"`
	HeadRef string `json:"head_ref" untrusted:"name"`
	BaseRef string `json:"base_ref" untrusted:"name"`

	// Mergeable is nil while GitHub is still computing it, which it does
	// in the background after a push or a first read.
	Mergeable *bool `json:"mergeable,omitempty"`

	// MergeableState is GitHub's word for it (clean, blocked, behind,
	// dirty, unstable, draft, unknown, ...), as GitHub says it.
	MergeableState string `json:"mergeable_state,omitempty"`

	// Labels are the labels' names, as written.
	Labels []string `json:"labels" untrusted:"name"`

	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`

	// Approvals are the logins, folded and sorted, whose latest review
	// approved HeadSHA, the author excluded ([approvalsOf]).
	Approvals []string `json:"approvals"`

	// Required is every check the base branch requires
	// ([Client.RequiredChecks]) and every one the company declared for it
	// ([WithDeclared]), and Checks how each stands on HeadSHA, in the same
	// order.
	Required []RequiredCheck `json:"required"`
	Checks   []CheckResult   `json:"checks"`
}

// pullWire is a pull request as GitHub's pull endpoint answers it.
type pullWire struct {
	Title          string     `json:"title"`
	State          string     `json:"state"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	Draft          bool       `json:"draft"`
	User           user       `json:"user"`
	Head           struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Labels         []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
}

// reviewWire is one review on a pull request.
type reviewWire struct {
	User     user   `json:"user"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
}

// PullFacts reads one pull request, its reviews, its base branch's required
// checks beside those declared names for it (nil for none), and how each
// stands on its head, counting no report untrusted made.
func (c *Client) PullFacts(ctx context.Context, owner, repo string, number int,
	declared func(base string) []string, untrusted Reporters) (PullFacts, error) {
	if number <= 0 {
		return PullFacts{}, fmt.Errorf("github: %d is not a pull request number", number)
	}
	pull := repoPath(owner, repo) + "/pulls/" + strconv.Itoa(number)
	name := fmt.Sprintf("%s/%s#%d", owner, repo, number)

	var wire pullWire
	if readErr := c.get(ctx, pull, nil, &wire); readErr != nil {
		return PullFacts{}, fmt.Errorf("github: read %s: %w", name, readErr)
	}
	if !fullSHA(wire.Head.SHA) {
		return PullFacts{}, fmt.Errorf("github: GitHub named no head commit for %s", name)
	}
	facts := PullFacts{
		Ref:   PullRef{Owner: owner, Repo: repo, Number: number},
		Title: wire.Title, Author: NormalizeLogin(wire.User.Login),
		State: wire.State, Merged: wire.Merged, MergedAt: wire.MergedAt,
		MergeCommitSHA: wire.MergeCommitSHA, Draft: wire.Draft,
		HeadSHA: wire.Head.SHA, HeadRef: wire.Head.Ref, BaseRef: wire.Base.Ref,
		Mergeable: wire.Mergeable, MergeableState: wire.MergeableState,
		Labels:    make([]string, 0, len(wire.Labels)),
		Additions: wire.Additions, Deletions: wire.Deletions, ChangedFiles: wire.ChangedFiles,
	}
	for _, label := range wire.Labels {
		facts.Labels = append(facts.Labels, label.Name)
	}

	reviews, err := walk(ctx, c, pull+"/reviews", nil,
		func(rows []reviewWire) []reviewWire { return rows })
	if err != nil {
		return PullFacts{}, fmt.Errorf("github: read the reviews of %s: %w", name, err)
	}
	facts.Approvals = approvalsOf(reviews, facts.Author, facts.HeadSHA)

	if facts.Required, err = c.RequiredChecks(ctx, owner, repo, facts.BaseRef); err != nil {
		return PullFacts{}, err
	}
	if declared != nil {
		facts.Required = WithDeclared(facts.Required, declared(facts.BaseRef))
	}
	if facts.Checks, err = c.CommitChecks(ctx, owner, repo, facts.HeadSHA, facts.Required, untrusted); err != nil {
		return PullFacts{}, err
	}
	return facts, nil
}

// approvalsOf is the logins whose latest deciding review approved head,
// author excluded, folded and sorted.
//
// LATEST BY SUBMISSION ORDER, which is the order GitHub lists reviews in. A
// change request or a dismissal after an approval withdraws it; a comment
// decides nothing and leaves it standing, and so does a pending review, which
// only its author can see. An approval of an earlier commit approved code
// that is no longer the head, so it does not count. The author's own approval
// is excluded because it is nobody else's judgment.
func approvalsOf(reviews []reviewWire, author, head string) []string {
	latest := map[string]reviewWire{}
	for _, review := range reviews {
		login := NormalizeLogin(review.User.Login)
		if login != "" && slices.Contains(deciding, strings.ToUpper(review.State)) {
			latest[login] = review
		}
	}
	out := []string{}
	for login, review := range latest {
		if login != author && strings.EqualFold(review.State, "APPROVED") &&
			strings.EqualFold(review.CommitID, head) {
			out = append(out, login)
		}
	}
	slices.Sort(out)
	return out
}

// deciding is the review states that set where a reviewer stands.
var deciding = []string{"APPROVED", "CHANGES_REQUESTED", "DISMISSED"}

// pullFilesPageLimit is how far GitHub lists a pull request's files: 3000
// files, thirty pages of a hundred. Past it GitHub lists nothing more, so the
// cap is GitHub's rather than a bound this engine chose.
const pullFilesPageLimit = 30

// PullFiles is every path the pull request number touches, a rename's old
// path beside its new one, in GitHub's order.
//
// complete is false when GitHub's cap was reached: GitHub lists at most 3000
// files, so a pull request that fills the cap may touch paths no read can
// learn, and a caller judging the paths must not read the list as all of
// them.
func (c *Client) PullFiles(ctx context.Context, owner, repo string, number int) (paths []string, complete bool, err error) {
	if number <= 0 {
		return nil, false, fmt.Errorf("github: %d is not a pull request number", number)
	}
	path := repoPath(owner, repo) + "/pulls/" + strconv.Itoa(number) + "/files"
	type fileWire struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename"`
	}
	paths = []string{}
	for page := 1; page <= pullFilesPageLimit; page++ {
		var rows []fileWire
		params := url.Values{
			"per_page": {strconv.Itoa(factPageSize)}, "page": {strconv.Itoa(page)},
		}
		if err = c.get(ctx, path, params, &rows); err != nil {
			return nil, false, fmt.Errorf("github: read the files of %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, row := range rows {
			// BOTH PATHS OF A RENAME: the change removes the old one as
			// surely as it adds the new one.
			if row.PreviousFilename != "" {
				paths = append(paths, row.PreviousFilename)
			}
			paths = append(paths, row.Filename)
		}
		if len(rows) < factPageSize {
			return paths, true, nil
		}
	}
	return paths, false, nil
}
