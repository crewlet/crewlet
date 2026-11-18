package engine

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet/host"
)

// A CHECK'S BOX READS ITS COMMIT WHERE THE API SERVES IT: the base the engine
// gives the check runner (checkCodeBase, from CREWLET_BOX_GIT_URL), with a
// repository's name and a read after it, is a path under auth.BoxGitletPrefix,
// the route auth leaves public for the box, that the API's three box reads
// answer (host.PatternBoxGitletTree, Blob and Diff, routed here by a mux of
// those patterns), on the listener the box clones from under
// auth.BoxGitPrefix, whatever path that listener sits at behind a balancer. A
// base that does not end in the box git route gives none, which checkrun.New
// refuses.
func TestACheckBoxReadsItsCommitWhereTheAPIServesIt(t *testing.T) {
	t.Parallel()
	reads := http.NewServeMux()
	for _, pattern := range []string{host.PatternBoxGitletTree, host.PatternBoxGitletBlob, host.PatternBoxGitletDiff} {
		reads.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	}
	gitRoute := strings.TrimSuffix(auth.BoxGitPrefix, "/")
	for _, listener := range []string{"https://code.example.com", "http://172.17.0.1:8000",
		"https://lb.example.com/crewlet"} {

		mount, err := url.Parse(listener)
		if err != nil {
			t.Fatal(err)
		}
		gitBase := listener + gitRoute
		clone, err := url.Parse(gitBase + "/web.git")
		if err != nil || clone.Path != mount.Path+auth.BoxGitPrefix+"web.git" {
			t.Fatalf("the clone URL %s/web.git is not under %s (%v)", gitBase, auth.BoxGitPrefix, err)
		}
		base := checkCodeBase(gitBase)
		for _, kind := range []string{"tree", "blob", "diff"} {
			read, err := url.Parse(base + "/web/" + kind)
			if err != nil {
				t.Fatalf("checkCodeBase(%q) = %q, which does not parse with a read after it: %v", gitBase, base, err)
			}
			want := mount.Path + auth.BoxGitletPrefix + "web/" + kind
			if read.Scheme != mount.Scheme || read.Host != mount.Host || read.Path != want {
				t.Errorf("checkCodeBase(%q) = %q: a check's read of web's %s goes to %s, want %s://%s%s, under "+
					"the route the API serves it at (%s)", gitBase, base, kind, read, mount.Scheme, mount.Host,
					want, auth.BoxGitletPrefix)
			}
			// What the API sees: the path behind the balancer's own.
			served := strings.TrimPrefix(read.Path, mount.Path)
			if !auth.Public(served) {
				t.Errorf("a check's read %s is no route auth leaves public for its box", served)
			}
			if _, pattern := reads.Handler(httptest.NewRequest(http.MethodGet, served, nil)); pattern == "" ||
				!strings.HasSuffix(pattern, "/"+kind) {
				t.Errorf("a check's read %s of web's %s is routed to %q, none of the box's reads %s, %s and %s",
					served, kind, pattern, host.PatternBoxGitletTree, host.PatternBoxGitletBlob, host.PatternBoxGitletDiff)
			}
		}
	}
	for _, gitBase := range []string{"", "https://code.example.com", "https://code.example.com/git",
		gitRoute} {
		if base := checkCodeBase(gitBase); base != "" {
			t.Errorf("checkCodeBase(%q) = %q, want none: it names no listener's box git route", gitBase, base)
		}
	}
}

// AN ENDING IS SENT AGAIN WITHOUT ITS COUNTS ONLY WHERE NO DATA NODE RAN IT: unreadAnywhere is true
// for the unknown keys refused over a whole walk, wrapped, and false for a walk in which a node went
// silent (which may have run it), an answer this build could not read (whose request ran), an
// outcome unknown, and a walk no node served for another reason.
//
// Mutations: drop the walk's test (the answer's refusal reads as one, red); drop the silent test
// (the silent walk reads as unread, red).
func TestAnEndingIsSentAgainWithoutItsCountsOnlyWhereNoDataNodeRanIt(t *testing.T) {
	t.Parallel()
	walk := &estate.ErrUnserved{Detail: "data-a: its build does not declare args.Finished.log_lines"}
	silent := &estate.ErrUnserved{Detail: "data-a: its build does not declare args.Finished.log_lines; data-b: silent",
		NodeSilent: true}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"unknown keys over a walk": {fmt.Errorf("record the ending: %w", &estate.ErrUnknownKeys{Op: "code.checks",
			Keys: []string{"args.Finished.log_lines"}, Nodes: []string{"data-a"}, Unserved: walk}), true},
		"unknown keys over a walk a node went silent in": {&estate.ErrUnknownKeys{Op: "code.checks",
			Keys: []string{"args.Finished.log_lines"}, Nodes: []string{"data-a"}, Unserved: silent}, false},
		"unknown keys in an answer": {&estate.ErrUnknownKeys{Op: "code.checks", Keys: []string{"result.novel"},
			Nodes: []string{"data-a"}, Answer: true}, false},
		"an outcome unknown": {estate.ErrOutcomeUnknown, false},
		"a walk unserved":    {walk, false},
		"another error":      {errors.New("busy"), false},
	} {
		if got := unreadAnywhere(tc.err); got != tc.want {
			t.Errorf("%s: unreadAnywhere = %v, want %v", name, got, tc.want)
		}
	}
}
