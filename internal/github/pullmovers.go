package github

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// PullMovers is a pull request a task waits on, as the engine reads whether
// the wait can end: whether it has settled and, while it is open, whether
// anybody or anything is moving it at its current head (ADR-0128).
type PullMovers struct {
	State    string     `json:"state"`
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	Draft    bool       `json:"draft"`

	// Head is the head commit, lower-cased, and Author the opener's login,
	// folded ([NormalizeLogin]).
	Head   string `json:"head"`
	Author string `json:"author"`

	// Requested is every reviewer asked for and not yet heard from: users by
	// their folded login, teams as `team:<slug>`, sorted.
	Requested []string `json:"requested"`

	// Reviewed is every login whose review was submitted on Head, the
	// author's own excluded, folded and sorted.
	Reviewed []string `json:"reviewed"`

	// AutoMerge is a merge armed to happen once the branch's rules allow it.
	AutoMerge bool `json:"auto_merge"`
}

// Open reports a pull request that has neither merged nor closed.
func (m PullMovers) Open() bool { return m.State == "open" && !m.Merged }

// Moving reports an open pull request that somebody or something will move:
// a reviewer asked for, a review of its head, or a merge armed.
func (m PullMovers) Moving() bool {
	return len(m.Requested) > 0 || len(m.Reviewed) > 0 || m.AutoMerge
}

// moversWire is the pull endpoint's answer as a wait reads it.
type moversWire struct {
	State     string     `json:"state"`
	Merged    bool       `json:"merged"`
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	Draft     bool       `json:"draft"`
	User      user       `json:"user"`
	Reviewers []user     `json:"requested_reviewers"`
	Teams     []struct {
		Slug string `json:"slug"`
	} `json:"requested_teams"`
	AutoMerge *struct{} `json:"auto_merge"`
	Head      struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// submitted is the review states a reviewer has spoken in; a pending review
// is seen by its author alone, and a dismissed one was taken back.
var submitted = []string{"APPROVED", "CHANGES_REQUESTED", "COMMENTED"}

// PullMovers reads one pull request and, while it is open, its reviews.
func (c *Client) PullMovers(ctx context.Context, owner, repo string, number int) (PullMovers, error) {
	if number <= 0 {
		return PullMovers{}, fmt.Errorf("github: %d is not a pull request number", number)
	}
	pull := repoPath(owner, repo) + "/pulls/" + strconv.Itoa(number)
	name := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	var wire moversWire
	if err := c.get(ctx, pull, nil, &wire); err != nil {
		return PullMovers{}, fmt.Errorf("github: read %s: %w", name, err)
	}
	out := PullMovers{State: wire.State, Merged: wire.Merged, MergedAt: wire.MergedAt,
		ClosedAt: wire.ClosedAt, Draft: wire.Draft, Head: strings.ToLower(wire.Head.SHA),
		Author: NormalizeLogin(wire.User.Login), AutoMerge: wire.AutoMerge != nil,
		Requested: []string{}, Reviewed: []string{}}
	if !out.Open() {
		return out, nil
	}
	if !fullSHA(out.Head) {
		return PullMovers{}, fmt.Errorf("github: GitHub named no head commit for %s", name)
	}
	for _, reviewer := range wire.Reviewers {
		if login := NormalizeLogin(reviewer.Login); login != "" {
			out.Requested = append(out.Requested, login)
		}
	}
	for _, team := range wire.Teams {
		if slug := strings.ToLower(strings.TrimSpace(team.Slug)); slug != "" {
			out.Requested = append(out.Requested, "team:"+slug)
		}
	}
	slices.Sort(out.Requested)
	reviews, err := walk(ctx, c, pull+"/reviews", nil, func(rows []reviewWire) []reviewWire { return rows })
	if err != nil {
		return PullMovers{}, fmt.Errorf("github: read the reviews of %s: %w", name, err)
	}
	out.Reviewed = reviewedAt(reviews, out.Author, out.Head)
	return out, nil
}

// reviewedAt is the logins that submitted a review of head, author excluded,
// folded, sorted and each once.
func reviewedAt(reviews []reviewWire, author, head string) []string {
	out := []string{}
	for _, review := range reviews {
		login := NormalizeLogin(review.User.Login)
		if login == "" || login == author || !strings.EqualFold(review.CommitID, head) ||
			!slices.Contains(submitted, strings.ToUpper(review.State)) {
			continue
		}
		out = append(out, login)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// PullMovers reads one pull request through the first agent's app that can,
// as [SeatLookup.PullRequest] does.
func (l *SeatLookup) PullMovers(ctx context.Context, ref PullRef) (PullMovers, error) {
	var out PullMovers
	err := l.read(ctx, func(client *Client) error {
		m, err := client.PullMovers(ctx, ref.Owner, ref.Repo, ref.Number)
		if err == nil {
			out = m
		}
		return err
	})
	if err != nil {
		return PullMovers{}, fmt.Errorf("github: read %s/%s#%d: %w", ref.Owner, ref.Repo, ref.Number, err)
	}
	return out, nil
}
