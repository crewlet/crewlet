package engine

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/gitlet/host"
)

// A CHECK'S BOX READS ITS COMMIT WHERE THE API SERVES IT: the base the engine
// gives the check runner (checkCodeBase, from CREWLET_BOX_GIT_URL), with a
// repository's name and a read after it, is a path under auth.BoxGitletPrefix —
// the route auth leaves public for the box — that the API's three box reads
// are mounted to answer (host.PatternBoxGitletTree, Blob and Diff, routed here
// by a mux of those very patterns), on the very listener the box clones from
// under auth.BoxGitPrefix, whatever path that listener sits at behind a
// balancer. A base that does not end in the box git route gives none, which
// checkrun.New refuses. The runner spelled its own "/box/gitlet" before, held
// to the route by nothing.
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
