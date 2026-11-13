package gitlab_test

import (
	"context"
	"crypto/sha1" //nolint:gosec // a git object's id is a SHA-1 of its header and bytes, which the fake lists a tree by
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/codescan"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlas/collect/collecttest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/deployfiles"
	"github.com/crewlet/crewlet/internal/integration"
)

// gitBlob is the id git gives a blob of body.
func gitBlob(body string) string {
	h := sha1.New() //nolint:gosec // see the import
	fmt.Fprintf(h, "blob %d\x00", len(body))
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// tree is the project's tree as GitLab lists it: every directory, every file
// and every symbolic link, by path, with no size.
func (p *glProject) tree() []map[string]any {
	var rows []map[string]any
	dirs := map[string]bool{}
	add := func(path, mode, kind, id string) {
		for i := range len(path) {
			if dir := path[:i]; path[i] == '/' && !dirs[dir] {
				dirs[dir] = true
				rows = append(rows, map[string]any{"id": gitBlob("tree " + dir), "name": dir, "type": "tree",
					"path": dir, "mode": "040000"})
			}
		}
		rows = append(rows, map[string]any{"id": id, "name": path, "type": kind, "path": path, "mode": mode})
	}
	for path, body := range p.files {
		add(path, "100644", "blob", gitBlob(body))
	}
	for path, target := range p.links {
		add(path, "120000", "blob", gitBlob(target))
	}
	slices.SortFunc(rows, func(a, z map[string]any) int { return strings.Compare(a["path"].(string), z["path"].(string)) })
	return rows
}

const (
	codeHead   = "1111111111111111111111111111111111111111"
	codeCanary = "atlas-canary-never-read"
)

// withCode gives the fixture's first two projects a repository: one with
// listed files beside files the atlas never reads, one with a Procfile alone.
func withCode(h *hub) *hub {
	api, web := h.projects[0], h.projects[1]
	api.head, web.head = codeHead, codeHead
	api.files = map[string]string{
		"compose.yaml": "services:\n  web:\n    build: .\n    environment:\n      TOKEN: " + codeCanary +
			"\n  db:\n    image: postgres:16.2\n",
		"go.mod":            "module acme.example/api\n\nrequire github.com/stripe/stripe-go/v79 v79.1.0\n",
		"Dockerfile":        "FROM alpine\nENV TOKEN=" + codeCanary + "\n",
		".env":              "TOKEN=" + codeCanary + "\n",
		"chart/values.yaml": "password: " + codeCanary + "\n",
		"infra/prod.tfvars": "token = \"" + codeCanary + "\"\n",
		"k8s/secrets.yaml":  "kind: Secret\nstringData:\n  token: " + codeCanary + "\n",
		".gitlab-ci.yml":    "variables:\n  TOKEN: " + codeCanary + "\n",
		"main.go":           "package main\n",
	}
	// A symbolic link named for a listed file points wherever it likes.
	api.links = map[string]string{"deploy/compose.yaml": "../.env"}
	web.files = map[string]string{"Procfile": "web: run --token=" + codeCanary + "\n"}
	return h
}

func gitlabCode(t *testing.T) collect.CodeHost {
	t.Helper()
	host, ok := gitlabSource(t).(collect.CodeHost)
	if !ok {
		t.Fatal("the GitLab source hosts no code")
	}
	return host
}

// codeRig is the forges' reader over the fake GitLab.
type codeRig struct {
	t        *testing.T
	h        *hub
	in       collect.Input
	log      *collecttest.Log
	store    *codescan.ForgeStore
	forges   *codescan.Forges
	mirrored map[string]bool
	now      time.Time
}

func newCodeRig(t *testing.T, h *hub, edit ...func(*config.Company)) *codeRig {
	t.Helper()
	r := &codeRig{t: t, h: h, in: h.input(t, edit...), log: collecttest.NewLog(), mirrored: map[string]bool{}, now: atlasNow}
	store, err := codescan.NewForgeStore(memory.NewFleet())
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	r.forges, err = codescan.NewForges(codescan.ForgeOptions{Log: r.log, Store: store,
		Mirrored: func(context.Context) (map[string]bool, error) { return r.mirrored, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *codeRig) pass() codescan.ForgeStatus {
	r.t.Helper()
	r.forges.Pass(r.t.Context(), gitlabCode(r.t), r.in, r.now)
	fs, _, err := r.store.Load(r.t.Context(), integration.KindGitLab)
	if err != nil {
		r.t.Fatal(err)
	}
	return fs
}

func (h *hub) blobsAsked() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.calls {
		if strings.Contains(c, "/repository/blobs/") {
			out = append(out, c)
		}
	}
	return out
}

// THE FILES OF A GITLAB PROJECT GITLET DOES NOT MIRROR ARE READ THROUGH
// GITLAB, BY THE SAME CLOSED LIST: the listed files are read by the object
// the tree lists, a Dockerfile says what it says by being there, and an
// `.env` file, a values file, a variable file, a manifest named for secrets,
// the CI configuration and a symbolic link are never asked for. Every
// request is a GET under the read credential, and the pass records its calls.
//
// Mutation: list a symbolic link as a file, or serve every file the tree
// holds to the parsers, and GitLab is asked for an object this test plants a
// canary in.
func TestTheFilesOfAGitLabProjectAreReadByTheClosedList(t *testing.T) {
	t.Parallel()
	h := withCode(acme())
	r := newCodeRig(t, h)
	fs := r.pass()
	api := h.projects[0]
	want := []string{
		"GET /api/v4/projects/1/repository/blobs/" + gitBlob(api.files["compose.yaml"]) + "/raw",
		"GET /api/v4/projects/1/repository/blobs/" + gitBlob(api.files["go.mod"]) + "/raw",
	}
	got := h.blobsAsked()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("GitLab was asked for %v, want only the two listed files that are read", got)
	}
	for _, in := range r.log.Reads {
		if text := fmt.Sprintf("%+v", in); strings.Contains(text, codeCanary) || strings.Contains(text, canaryEmail) {
			t.Errorf("a value the atlas never reads reached it: %s", text)
		}
	}
	const voice = "gitlab.com:acme:api"
	if got := r.log.Entries(atlas.CodeSource(voice)); !slices.Equal(got,
		[]string{"datastore:api/db", "service:api", "service:api-web", "vendor_account:other/stripe"}) {
		t.Errorf("the files of the project declare %v", got)
	}
	if got := r.log.Entries(atlas.CodeSource("gitlab.com:acme:platform:web")); !slices.Equal(got, []string{"service:web"}) {
		t.Errorf("a Procfile's project declares %v", got)
	}
	// The group and its projects; a branch for each of the three projects
	// that may hold code and a tree for the two whose branch holds a commit;
	// and the two files. The archived project and the empty one are not asked.
	if !fs.Completed || fs.Repositories != 3 || fs.Calls != 2+3+2+2 || fs.Reads != 2 || fs.Refused != 0 {
		t.Errorf("the pass's standing: %+v, calls %v", fs, h.calls)
	}
	_, repos, _ := r.store.Load(t.Context(), integration.KindGitLab)
	if st := repos[voice]; st.Tip != codeHead || st.Listed != 3 || st.Calls != 4 || st.Bytes == 0 {
		t.Errorf("the project's standing: %+v", st)
	}

	// A pass over a branch that did not move asks for one branch a project.
	h.mu.Lock()
	h.calls = nil
	h.mu.Unlock()
	r.now = r.now.Add(time.Hour)
	if r.pass(); len(h.blobsAsked()) != 0 || h.sent("GET /api/v4/projects/1/repository/tree") != 0 ||
		h.sent("GET /api/v4/projects/1/repository/branches/main") != 1 {
		t.Errorf("an unmoved project cost %v", h.calls)
	}
	// A project Gitlet mirrors is left to Gitlet's own scan.
	r.mirrored["gitlab.com/acme/api"] = true
	r.now = r.now.Add(time.Hour)
	if fs = r.pass(); fs.Mirrored != 1 || fs.Released != 1 || len(r.log.Entries(atlas.CodeSource(voice))) != 0 {
		t.Errorf("a project that gained a mirror: %+v, %v", fs, r.log.Entries(atlas.CodeSource(voice)))
	}
}

// A TREE IS WALKED BY KEYSET TO ITS END OR NOT READ AT ALL, and a file's
// size is learned by reading it: a tree past the first page is listed whole,
// a file past the size bound is counted and not parsed, and a tree past what
// one read walks has none of its files read.
//
// Mutation: stop at the first page, and a manifest that sorts after the
// hundredth entry is never found.
func TestAGitLabTreeIsWalkedToItsEndOrNotAtAll(t *testing.T) {
	t.Parallel()
	h := withCode(acme())
	api := h.projects[0]
	for i := range 250 {
		api.files[fmt.Sprintf("src/f%03d.go", i)] = "package src // " + strconv.Itoa(i) + "\n"
	}
	api.files["zeta/fly.toml"] = "app = \"edge\"\n"
	api.files["package.json"] = "{\"name\": \"" + strings.Repeat("x", deployfiles.MaxFileBytes) + "\"}"
	r := newCodeRig(t, h)
	r.pass()
	const voice = "gitlab.com:acme:api"
	if got := r.log.Entries(atlas.CodeSource(voice)); !slices.Contains(got, "service:edge") {
		t.Fatalf("a file past the tree's first page was not found: %v", got)
	}
	_, repos, _ := r.store.Load(t.Context(), integration.KindGitLab)
	if st := repos[voice]; st.Oversize != 1 || st.Listed != 5 || h.sent("GET /api/v4/projects/1/repository/tree") != 3 {
		t.Errorf("the project's standing: %+v after %d pages", st, h.sent("GET /api/v4/projects/1/repository/tree"))
	}

	// Past what one read walks.
	for i := range 10000 {
		api.files[fmt.Sprintf("gen/g%05d.txt", i)] = "g" + strconv.Itoa(i)
	}
	api.head = "2222222222222222222222222222222222222222"
	r.now = r.now.Add(time.Hour)
	fs := r.pass()
	_, repos, _ = r.store.Load(t.Context(), integration.KindGitLab)
	if st := repos[voice]; fs.TooLarge != 1 || !st.TooLarge || len(r.log.Entries(atlas.CodeSource(voice))) != 0 {
		t.Errorf("a tree past what one read walks: %+v and %+v", fs, st)
	}
}

// A PROJECT WHOSE CODE GITLAB REFUSES IS LEFT AS IT WAS, AND A RATE LIMIT
// STOPS THE PASS: a 403 or a 404 on one project's branch or blob withdraws
// nothing and the rest are read, a rejected credential and a rate limit are
// faults, and a group or a named project GitLab does not show makes the
// listing partial, which releases nothing.
//
// Mutation: read a refused branch as an empty one, and a project whose
// credential's owner lost a role has everything its files declared withdrawn.
func TestAGitLabProjectThatRefusesItsCodeIsLeftAsItWas(t *testing.T) {
	t.Parallel()
	whole := newCodeRig(t, withCode(acme()))
	whole.pass()
	const voice = "gitlab.com:acme:api"
	before := whole.log.Entries(atlas.CodeSource(voice))

	whole.h.projects[0].head = "2222222222222222222222222222222222222222"
	whole.h.refuse["/api/v4/projects/1/repository/branches/main"] = http.StatusForbidden
	whole.now = whole.now.Add(time.Hour)
	fs := whole.pass()
	if fs.Refused != 1 || fs.LastError != "" || !slices.Equal(whole.log.Entries(atlas.CodeSource(voice)), before) {
		t.Fatalf("a project that refused its branch: %+v, %v", fs, whole.log.Entries(atlas.CodeSource(voice)))
	}
	delete(whole.h.refuse, "/api/v4/projects/1/repository/branches/main")
	whole.h.refuse["/api/v4/projects/1/repository/tree"] = http.StatusTooManyRequests
	whole.now = whole.now.Add(time.Hour)
	if fs = whole.pass(); !strings.Contains(fs.LastError, "429") || !slices.Equal(whole.log.Entries(atlas.CodeSource(voice)), before) {
		t.Fatalf("a rate-limited tree: %+v", fs)
	}

	// A group GitLab does not show: nothing listed, nothing released.
	whole.h.group = "elsewhere"
	whole.now = whole.now.Add(time.Hour)
	if fs = whole.pass(); fs.Whole || fs.Released != 0 || !slices.Equal(whole.log.Entries(atlas.CodeSource(voice)), before) {
		t.Fatalf("a group GitLab does not show: %+v", fs)
	}
	// A rejected credential is a fault of the pass, named as one.
	dead := withCode(acme())
	dead.token = "another"
	r := newCodeRig(t, dead)
	if fs = r.pass(); !strings.Contains(fs.LastError, "401") {
		t.Errorf("a rejected credential: %+v", fs)
	}
	// And a company that does not connect GitLab has nothing to open.
	if _, err := gitlabCode(t).OpenCode(t.Context(), collect.Input{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no document: %v, want not configured", err)
	}
}
