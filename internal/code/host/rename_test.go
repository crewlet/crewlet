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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/host"
)

// A RENAMED REPOSITORY'S OLD NAME SERVES A CLONE, AND TELLS A PUSH ITS NEW
// NAME. A rename leaves the old name redirecting to the repository for
// code.RedirectWindow:
//
//   - a fetch's advertisement is 302 Found to the same path and query under
//     the new name. A client presenting its credential up front
//     (http.extraHeader, a CI checkout's shape) meets it on its first
//     request, so git rebases onto the new name and says so (`redirecting
//     to`);
//   - a person's git, whose credential helper answers the challenge its
//     anonymous first request meets, is redirected only on the retry — which
//     git follows WITHOUT rebasing — so it POSTs its fetch to the old name,
//     and that POST is served: the clone works either way;
//   - a push is refused at its advertisement, in the 404 git prints, naming
//     the new name and when the old one stops serving clones, and nothing is
//     pushed; a push's POST at the old name, which no git sends after that
//     refusal, is the same 404.
//
// Mutations: refuse a fetch's POST at the old name (a redirect-only old name)
// — the person's clone fails `RPC failed; HTTP 404`, red; redirect a push's
// advertisement like a fetch's — the person's push is redirected and POSTs to
// the old name, where git prints no reason, red; admit a push at the old name
// — it lands, red.
func TestARenamedRepositorysOldNameRedirectsAClone(t *testing.T) {
	until := time.Now().Add(code.RedirectWindow).UTC().Truncate(time.Second)
	e := &recording{resolve: func(ctx context.Context, inner host.Estate, q code.ResolveQuery) (code.RepoRef, error) {
		if q.Name != "old-name" {
			return inner.Resolve(ctx, q)
		}
		// THE OLD NAME, AS THE ROWS ANSWER IT: the repository under its
		// current name, and the redirect's end.
		q.Name = codetest.RepoName
		ref, err := inner.Resolve(ctx, q)
		ref.Redirect, ref.RedirectUntil = codetest.RepoName, until
		return ref, err
	}}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
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
	want := "/git/" + codetest.RepoName + ".git/info/refs?service=git-upload-pack"
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
	} else if !strings.Contains(stderr, "redirecting to "+n.URL()+"/git/"+codetest.RepoName+".git/") {
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
	if !strings.Contains(pushed.Stderr, "remote: \"old-name\" was renamed to \""+codetest.RepoName+"\"") ||
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
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(raw), "renamed to \""+codetest.RepoName+"\"") {
		t.Fatalf("a push's POST to the old name answered %d %q, want 404 naming %s", resp.StatusCode, raw,
			codetest.RepoName)
	}
}
