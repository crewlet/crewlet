package github_test

import (
	"context"
	"crypto/sha1" //nolint:gosec // a git object's id is a SHA-1 of its header and bytes, which the fake lists a tree by
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/codescan"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlas/collect/collecttest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
)

// gitBlob is the id git gives a blob of body.
func gitBlob(body string) string {
	h := sha1.New() //nolint:gosec // see the import
	fmt.Fprintf(h, "blob %d\x00", len(body))
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// tree is the repository's tree as GitHub lists it: every directory, every
// file with its size and every symbolic link, by path.
func (r *ghRepo) tree() []map[string]any {
	var rows []map[string]any
	dirs := map[string]bool{}
	add := func(path, mode string, body string) {
		for i := range len(path) {
			if dir := path[:i]; path[i] == '/' && !dirs[dir] {
				dirs[dir] = true
				rows = append(rows, map[string]any{"path": dir, "mode": "040000", "type": "tree",
					"sha": gitBlob("tree " + dir), "url": canaryText})
			}
		}
		rows = append(rows, map[string]any{"path": path, "mode": mode, "type": "blob", "sha": gitBlob(body),
			"size": len(body), "url": canaryText})
	}
	for path, body := range r.files {
		add(path, "100644", body)
	}
	for path, target := range r.links {
		add(path, "120000", target)
	}
	slices.SortFunc(rows, func(a, z map[string]any) int { return strings.Compare(a["path"].(string), z["path"].(string)) })
	return rows
}

const (
	codeHead   = "1111111111111111111111111111111111111111"
	codeCanary = "atlas-canary-never-read"
)

// withCode gives the fixture's repositories their code: one with listed
// files beside files the atlas never reads, one with a Procfile alone, and
// the archived one a manifest nothing may ask for.
func withCode(f *forge) *forge {
	api, web, old := f.repos["Acme/API"], f.repos["acme/web"], f.repos["acme/old"]
	api.head, web.head, old.head = codeHead, codeHead, codeHead
	// An older Enterprise Server's listing may not say when a repository was
	// pushed: the second repository's does not.
	api.row["pushed_at"] = "2026-11-12T06:00:00Z"
	api.files = map[string]string{
		"compose.yaml": "services:\n  web:\n    build: .\n    environment:\n      TOKEN: " + codeCanary +
			"\n  db:\n    image: postgres:16.2\n",
		"go.mod":                       "module acme.example/api\n\nrequire github.com/stripe/stripe-go/v79 v79.1.0\n",
		"Dockerfile":                   "FROM alpine\nENV TOKEN=" + codeCanary + "\n",
		".env":                         "TOKEN=" + codeCanary + "\n",
		"chart/values.yaml":            "password: " + codeCanary + "\n",
		"infra/prod.tfvars":            "token = \"" + codeCanary + "\"\n",
		"k8s/secrets.yaml":             "kind: Secret\nstringData:\n  token: " + codeCanary + "\n",
		".github/workflows/deploy.yml": "env:\n  TOKEN: " + codeCanary + "\n",
		"main.go":                      "package main\n",
	}
	// A symbolic link named for a listed file points wherever it likes.
	api.links = map[string]string{"deploy/compose.yaml": "../.env"}
	web.files = map[string]string{"Procfile": "web: run --token=" + codeCanary + "\n"}
	old.files = map[string]string{"fly.toml": "app = \"old\"\n"}
	return f
}

func githubCode(t *testing.T) collect.CodeHost {
	t.Helper()
	host, ok := githubSource(t).(collect.CodeHost)
	if !ok {
		t.Fatal("the GitHub source hosts no code")
	}
	return host
}

// codeRig is the forges' reader over the fake GitHub.
type codeRig struct {
	t      *testing.T
	f      *forge
	in     collect.Input
	log    *collecttest.Log
	store  *codescan.ForgeStore
	forges *codescan.Forges
	now    time.Time
}

func newCodeRig(t *testing.T, f *forge, roles ...config.Role) *codeRig {
	t.Helper()
	r := &codeRig{t: t, f: f, in: f.input(t, roles...), log: collecttest.NewLog(), now: atlasNow}
	store, err := codescan.NewForgeStore(memory.NewFleet())
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	r.forges, err = codescan.NewForges(codescan.ForgeOptions{Log: r.log, Store: store,
		Mirrored: func(context.Context) (map[string]bool, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *codeRig) pass() codescan.ForgeStatus {
	r.t.Helper()
	r.forges.Pass(r.t.Context(), githubCode(r.t), r.in, r.now)
	fs, _, err := r.store.Load(r.t.Context(), integration.KindGitHub)
	if err != nil {
		r.t.Fatal(err)
	}
	return fs
}

func (f *forge) asked(part string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.Contains(c, part) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out
}

func (f *forge) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

const apiVoice = "github.com:acme:api"

// THE FILES OF A GITHUB REPOSITORY GITLET DOES NOT MIRROR ARE READ THROUGH
// GITHUB, BY THE SAME CLOSED LIST: under a token minted for the pass with
// Read-only of Metadata and Contents and revoked after it, the listed files
// are read by the object the tree lists, a Dockerfile says what it says by
// being there, and an `.env` file, a values file, a variable file, a manifest
// named for secrets, a workflow and a symbolic link are never asked for. An
// archived repository is not read, and one the listing says nothing was
// pushed to is asked nothing.
//
// Mutation: list a symbolic link as a file, or serve every file the tree
// holds to the parsers, and GitHub is asked for an object this test plants a
// canary in.
func TestTheFilesOfAGitHubRepositoryAreReadByTheClosedList(t *testing.T) {
	t.Parallel()
	f := withCode(acme())
	r := newCodeRig(t, f)
	// The token is gone with its revoke, so a twin that cannot revoke keeps
	// what the pass asked for.
	twin := withCode(acme())
	twin.refuse["/installation/token"] = http.StatusInternalServerError
	newCodeRig(t, twin).pass()
	if got, want := twin.minted["installation-token-7"], map[string]string{"metadata": "read", "contents": "read"}; !mapsEqual(got, want) {
		t.Errorf("the pass's token carries %v, want %v", got, want)
	}

	fs := r.pass()
	api := f.repos["Acme/API"]
	want := []string{
		"GET /repos/Acme/API/git/blobs/" + gitBlob(api.files["compose.yaml"]),
		"GET /repos/Acme/API/git/blobs/" + gitBlob(api.files["go.mod"]),
	}
	slices.Sort(want)
	if got := f.asked("/git/blobs/"); !slices.Equal(got, want) {
		t.Fatalf("GitHub was asked for %v, want only the two listed files that are read", got)
	}
	if len(f.asked("/repos/acme/old/")) != 0 {
		t.Errorf("an archived repository was asked for %v", f.asked("/repos/acme/old/"))
	}
	for _, in := range r.log.Reads {
		if text := fmt.Sprintf("%+v", in); strings.Contains(text, codeCanary) || strings.Contains(text, canaryText) {
			t.Errorf("a value the atlas never reads reached it: %s", text)
		}
	}
	if got := r.log.Entries(atlas.CodeSource(apiVoice)); !slices.Equal(got,
		[]string{"datastore:api/db", "service:api", "service:api-web", "vendor_account:other/stripe"}) {
		t.Errorf("the files of the repository declare %v", got)
	}
	if got := r.log.Entries(atlas.CodeSource("github.com:acme:web")); !slices.Equal(got, []string{"service:web"}) {
		t.Errorf("a Procfile's repository declares %v", got)
	}
	// The installation, its token, its repositories and the revoke; a ref
	// and a tree for each of the two repositories; and the two files.
	if !fs.Completed || !fs.Whole || fs.Repositories != 2 || fs.Calls != 4+4+2 || fs.Reads != 2 ||
		len(f.revoked) != 1 || len(f.minted) != 0 {
		t.Errorf("the pass's standing: %+v, revoked %v, calls %v", fs, f.revoked, f.calls)
	}

	// Nothing was pushed: the listing says so of one repository, which is
	// asked nothing, and says nothing of the other, which is asked where its
	// default branch is.
	f.forget()
	r.now = r.now.Add(time.Hour)
	if fs = r.pass(); !slices.Equal(f.asked("/repos/"), []string{"GET /repos/acme/web/git/ref/heads/main"}) || fs.Calls != 5 {
		t.Errorf("a pass with nothing pushed asked %v", f.asked("/repos/"))
	}
	// A push moves the stamp and the default branch with it.
	api.row["pushed_at"] = "2026-11-12T09:00:00Z"
	api.head = "2222222222222222222222222222222222222222"
	api.files["go.mod"] = "module acme.example/api\n\nrequire github.com/DataDog/datadog-go/v5 v5.5.0\n"
	f.forget()
	r.now = r.now.Add(time.Hour)
	r.pass()
	if got := f.asked("/repos/Acme/API/"); len(got) != 3 || len(f.asked("/repos/acme/web/")) != 1 {
		t.Errorf("a push to one repository asked %v, want its ref, its tree and the changed file", got)
	}
	if got := r.log.Entries(atlas.CodeSource(apiVoice)); !slices.Contains(got, "vendor_account:datadog") || slices.Contains(got, "vendor_account:other/stripe") {
		t.Errorf("after the push the files declare %v", got)
	}
}

// AN INSTALLATION WITHOUT CONTENTS SERVES NO FILE, AND WHAT GITHUB REFUSES
// OR CUTS SHORT IS NEVER READ AS EMPTY: an installation that holds no
// Contents is not read through and makes the listing partial, so nothing is
// released; a repository with no commit declares nothing; a tree GitHub
// truncates has none of its files read; a refused tree leaves the repository
// as it was; and the rate limit is a fault that still revokes the token.
//
// Mutation: read a truncated tree as whole, and a manifest GitHub left out
// withdraws everything it declared.
func TestWhatGitHubRefusesOrCutsShortIsNeverReadAsEmpty(t *testing.T) {
	t.Parallel()
	f := withCode(acme())
	r := newCodeRig(t, f)
	r.pass()
	before := r.log.Entries(atlas.CodeSource(apiVoice))

	// The installation loses Contents: not read through, nothing released.
	delete(f.installs[7].perms, "contents")
	f.forget()
	r.now = r.now.Add(time.Hour)
	fs := r.pass()
	if fs.Whole || fs.Released != 0 || fs.Repositories != 0 || f.sent("POST /app/installations/7/access_tokens") != 0 ||
		!slices.Equal(r.log.Entries(atlas.CodeSource(apiVoice)), before) {
		t.Fatalf("an installation without Contents: %+v, calls %v", fs, f.calls)
	}
	f.installs[7].perms["contents"] = "read"

	// A truncated tree, a refused tree and an empty repository.
	api, web := f.repos["Acme/API"], f.repos["acme/web"]
	api.head, api.truncated, api.row["pushed_at"] = "2222222222222222222222222222222222222222", true, "p2"
	web.head, web.row["pushed_at"] = "", "p2"
	r.now = r.now.Add(time.Hour)
	fs = r.pass()
	_, repos, _ := r.store.Load(t.Context(), integration.KindGitHub)
	if st := repos[apiVoice]; fs.TooLarge != 1 || !st.TooLarge || len(r.log.Entries(atlas.CodeSource(apiVoice))) != 0 {
		t.Errorf("a truncated tree: %+v and %+v", fs, st)
	}
	if got := r.log.Entries(atlas.CodeSource("github.com:acme:web")); len(got) != 0 || fs.LastError != "" {
		t.Errorf("a repository with no commit declares %v: %+v", got, fs)
	}
	api.truncated, api.head, api.row["pushed_at"] = false, "3333333333333333333333333333333333333333", "p3"
	r.now = r.now.Add(time.Hour)
	r.pass()
	restored := r.log.Entries(atlas.CodeSource(apiVoice))
	api.head, api.row["pushed_at"] = "4444444444444444444444444444444444444444", "p4"
	f.refuse["/repos/Acme/API/git/trees/4444444444444444444444444444444444444444"] = http.StatusNotFound
	r.now = r.now.Add(time.Hour)
	if fs = r.pass(); fs.Refused != 1 || fs.LastError != "" || len(restored) == 0 ||
		!slices.Equal(r.log.Entries(atlas.CodeSource(apiVoice)), restored) {
		t.Errorf("a refused tree: %+v, %v", fs, r.log.Entries(atlas.CodeSource(apiVoice)))
	}

	// The rate limit: a fault, nothing withdrawn, and the token revoked.
	f.refuse = map[string]int{}
	f.limit = "/repos/Acme/API/git/trees/4444444444444444444444444444444444444444"
	revoked := len(f.revoked)
	r.now = r.now.Add(time.Hour)
	if fs = r.pass(); !strings.Contains(fs.LastError, "403") || len(f.revoked) != revoked+1 ||
		!slices.Equal(r.log.Entries(atlas.CodeSource(apiVoice)), restored) {
		t.Errorf("a rate-limited tree: %+v, revoked %d", fs, len(f.revoked)-revoked)
	}
	if _, err := githubCode(t).OpenCode(t.Context(), collect.Input{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no document: %v, want not configured", err)
	}
}
