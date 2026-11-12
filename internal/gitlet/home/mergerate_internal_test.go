package home

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A DRAINED QUEUE FORGETS ONLY ITS OWN REPOSITORY'S TARGETS: the merge rate
// is held by repository, then by target, so a turn that read all of one
// repository's queue work drops that repository's targets it did not meet
// and no other repository's — and a repository whose last target went, by a
// turn or by a reading a window on, is dropped with it rather than kept as an
// empty entry for the node's life.
//
// Mutation: drop every repository's unmet targets in seen → repo-b's release
// is forgotten by repo-a's turn, red; keep a repository whose last target a
// turn dropped → repo-a is still held, red; keep one whose last target a
// reading forgot → repo-b is still held, red.
func TestADrainedQueueForgetsOnlyItsOwnRepositorysTargets(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	var r mergeRate
	for _, repo := range []string{"repo-a", "repo-b"} {
		for _, target := range []string{"refs/heads/main", "refs/heads/release"} {
			r.built(repo, target, "train-1", at)
		}
	}
	held := func() []string {
		r.mu.Lock()
		defer r.mu.Unlock()
		var out []string
		for repo, targets := range r.targets {
			for target := range targets {
				out = append(out, repo+" "+target)
			}
			if len(targets) == 0 {
				out = append(out, repo+" (empty)")
			}
		}
		slices.Sort(out)
		return out
	}

	r.seen("repo-a", []string{"refs/heads/main"})
	if got, want := held(), []string{"repo-a refs/heads/main", "repo-b refs/heads/main",
		"repo-b refs/heads/release"}; !slices.Equal(got, want) {
		t.Fatalf("repo-a's turn meeting only main leaves %q, want %q: repo-b's targets are no turn of repo-a's",
			got, want)
	}
	r.seen("repo-a", nil)
	if got, want := held(), []string{"repo-b refs/heads/main", "repo-b refs/heads/release"}; !slices.Equal(got,
		want) {
		t.Fatalf("repo-a drained leaves %q, want %q: a repository with no target left is dropped", got, want)
	}
	var out statelog.Reading
	r.reading(at.Add(gitlet.MergeRateWindow+time.Minute), &out)
	if got := held(); len(got) != 0 {
		t.Fatalf("a reading a window on leaves %q, want nothing: repo-b's trains lapsed and it is dropped", got)
	}
}
