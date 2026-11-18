package host_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/host"
)

// A RENAMED REPOSITORY'S OLD NAME SERVES A CLONE, AND TELLS A PUSH ITS NEW
// NAME, for gitlet.RedirectWindow:
//   - a fetch's advertisement is 302 Found to its path and query under the
//     new name. A client sending its credential up front (http.extraHeader,
//     as CI does) rebases and says `redirecting to`; a person's git,
//     redirected only on its challenged retry, which git follows WITHOUT
//     rebasing, POSTs its fetch to the old name, and that POST is served;
//   - a push is refused at its advertisement, in the 404 git prints, naming
//     the new name and the redirect's end; nothing is pushed.
//
// Mutations: refuse the old name's fetch POST (the person's clone fails),
// redirect a push's advertisement (git prints no reason), or admit the push.
func TestARenamedRepositorysOldNameRedirectsAClone(t *testing.T) {
	until := time.Now().Add(gitlet.RedirectWindow).UTC().Truncate(time.Second)
	e := &recording{resolve: func(ctx context.Context, inner host.Estate, q gitlet.ResolveQuery) (gitlet.RepoRef, error) {
		if q.Name != "old-name" {
			return inner.Resolve(ctx, q)
		}
		// THE OLD NAME, AS THE ROWS ANSWER IT: the repository under its
		// current name, and the redirect's end.
		q.Name = gitlettest.RepoName
		ref, err := inner.Resolve(ctx, q)
		ref.Redirect, ref.RedirectUntil = gitlettest.RepoName, until
		return ref, err
	}}
	c := newCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: e.wrap}}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	n := c.Fleet.Nodes()[0]
	old := n.URL() + "/git/old-name.git"

	// THE ADVERTISEMENT, its redirect not followed.
	client := *n.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodGet, old+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("crewlet", p.Token())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET the old name's advertisement: %v", err)
	}
	_ = resp.Body.Close()
	want := "/git/" + gitlettest.RepoName + ".git/info/refs?service=git-upload-pack"
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
		t.Fatalf("the old name's advertisement answered %d to %q, want 302 to %q", resp.StatusCode,
			resp.Header.Get("Location"), want)
	}

	// A PERSON'S CLONE OF THE OLD URL: challenged first, redirected on the
	// retry, its fetch POSTed to the old name.
	if got := p.Head(p.Clone(old)); got != head {
		t.Fatalf("the person's clone of the old name checked out %s, want %s", got, head)
	}

	// A CLONE PRESENTING ITS CREDENTIAL UP FRONT is rebased onto the new
	// name, and git says so.
	dir := filepath.Join(t.TempDir(), "ci")
	basic := base64.StdEncoding.EncodeToString([]byte("crewlet:" + p.Token()))
	if _, stderr, err := p.Git().Try(t, "", nil, "-c", "http.extraHeader=Authorization: Basic "+basic,
		"clone", old, dir); err != nil {
		t.Fatalf("a clone of the old name with its credential up front: %v\n%s", err, stderr)
	} else if !strings.Contains(stderr, "redirecting to "+n.URL()+"/git/"+gitlettest.RepoName+".git/") {
		t.Fatalf("a clone with its credential up front was not rebased onto the new name:\n%s", stderr)
	}
	if got := p.Head(dir); got != head {
		t.Fatalf("the up-front clone of the old name checked out %s, want %s", got, head)
	}

	// A PUSH OF THE OLD URL is told the new name, as git prints it, and
	// pushes nothing.
	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	pushed := p.Push(c.Ctx, work, old, "main")
	if pushed.Err == nil {
		t.Fatalf("a push to the old name went through: %s", pushed)
	}
	if !strings.Contains(pushed.Stderr, "remote: \"old-name\" was renamed to \""+gitlettest.RepoName+"\"") ||
		!strings.Contains(pushed.Stderr, until.Format(time.RFC3339)) {
		t.Fatalf("a push to the old name was not told the new name and the redirect's end: %s", pushed)
	}
	if got := p.LsRemote(c.URL(0), 0)["refs/heads/main"]; got != head {
		t.Fatalf("after a push to the old name, main is %s, want %s unmoved", got, head)
	}

	// A PUSH'S POST at the old name, which no git sends after its
	// advertisement was refused: the same sentence.
	req, err = http.NewRequestWithContext(c.Ctx, http.MethodPost, old+"/git-receive-pack", strings.NewReader("0000"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", host.ReceivePackRequestType)
	req.SetBasicAuth("crewlet", p.Token())
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST a push to the old name: %v", err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(raw), "renamed to \""+gitlettest.RepoName+"\"") {
		t.Fatalf("a push's POST to the old name answered %d %q, want 404 naming %s", resp.StatusCode, raw,
			gitlettest.RepoName)
	}
}
