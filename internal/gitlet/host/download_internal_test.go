package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/iam"
)

// downloadServer is a Server enough for a download's first steps, recording
// every process handed to the pool and answering each with run.
func downloadServer(t *testing.T, run func(c gitproc.Cmd) (gitproc.Result, error)) (*Server, *[]gitproc.Cmd) {
	t.Helper()
	var ran []gitproc.Cmd
	s := &Server{log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), now: time.Now}
	s.life, s.end = context.WithCancel(context.Background())
	t.Cleanup(s.end)
	s.runGit = func(_ context.Context, c gitproc.Cmd) (gitproc.Result, error) {
		ran = append(ran, c)
		return run(c)
	}
	return s, &ran
}

// A DOWNLOAD IS JUDGED BY THE GRANT admission read: one that reads nothing is
// refused 403 in its own words, and no git runs.
//
// Mutation: drop the grant's check, and the lookup runs.
func TestADownloadIsJudgedByTheGrant(t *testing.T) {
	t.Parallel()
	s, ran := downloadServer(t, func(gitproc.Cmd) (gitproc.Result, error) {
		return gitproc.Result{}, fmt.Errorf("no git runs here")
	})
	grant := gitlet.Grant{RepoName: "acme"}
	admit := func(context.Context, http.ResponseWriter, *http.Request, auth.GitOp) (admission, bool) {
		return admission{name: "acme", repo: gitlet.RepoRef{ID: "r1", Name: "acme",
			Status: gitlet.RepoStatusActive}, grant: grant}, true
	}
	for _, kind := range []string{types.FetchedRaw, types.FetchedArchive} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/git/acme/-/raw?rev="+strings.Repeat("a", 40)+"&path=a", nil)
		if kind == types.FetchedArchive {
			r = httptest.NewRequest(http.MethodGet, "/git/acme/-/archive.zip?rev="+strings.Repeat("a", 40), nil)
		}
		s.download(w, r, admit, kind)
		if w.Code != http.StatusForbidden || w.Body.String() != grant.ReadRefusal()+"\n" {
			t.Errorf("a %s under a grant that reads nothing answered %d %q, want 403 %q", kind, w.Code,
				w.Body.String(), grant.ReadRefusal())
		}
	}
	if len(*ran) != 0 {
		t.Errorf("a refused download ran git %q", (*ran)[0].Args)
	}
}

// AN ANONYMOUS DOWNLOAD IS NOT CHALLENGED by admission either: a request
// reaching the host with nobody resolved is 401 with no WWW-Authenticate,
// while git's own routes still carry git's Basic challenge.
//
// Mutation: challenge every anonymous request alike.
func TestAnAnonymousDownloadIsNotChallenged(t *testing.T) {
	t.Parallel()
	s := &Server{}
	for op, challenged := range map[auth.GitOp]bool{auth.GitDownload: false, auth.GitFetch: true} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/git/acme/-/raw", nil)
		r.SetPathValue(PathRepo, "acme")
		if _, ok := s.admit(iam.WithAnonymous(r.Context()), w, r, op); ok {
			t.Fatalf("an anonymous %s was admitted", op)
		}
		if w.Code != http.StatusUnauthorized || (w.Header().Get("WWW-Authenticate") != "") != challenged {
			t.Errorf("an anonymous %s answered %d with WWW-Authenticate %q, want 401 challenged %v", op, w.Code,
				w.Header().Get("WWW-Authenticate"), challenged)
		}
	}
}

// A PRINCIPAL IS MARKED BEFORE IT WAITS: its second ZIP is refused at once,
// whether its first waits for a place or holds one, and the mark and the
// place are each given back once, however often their release is called.
//
// Mutation: mark the principal only once it holds a place.
func TestAPrincipalMakesOneZipAtATime(t *testing.T) {
	t.Parallel()
	p := newArchivePlaces(1, time.Hour)
	ceo := gitproc.Share{Class: gitproc.SharePerson, Principal: "ceo"}
	leave, err := p.enter(ceo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.enter(ceo); !errors.Is(err, errArchiving) {
		t.Errorf("a principal's second ZIP entered (%v), want errArchiving", err)
	}
	if _, err := p.enter(gitproc.Share{Class: gitproc.SharePerson, Principal: "dev"}); err != nil {
		t.Errorf("another principal's ZIP was refused: %v", err)
	}
	release, err := p.take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	leave()
	leave()
	if held, places := p.inUse(); held != 0 || places != 1 {
		t.Errorf("after one release of each, %d of %d places are held", held, places)
	}
	if _, err := p.enter(ceo); err != nil {
		t.Errorf("a principal whose ZIP ended was refused its next: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	hold, err := p.take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	cancel()
	if _, err := p.take(ctx); err == nil {
		t.Errorf("a wait for a place outlived its request")
	}
}
