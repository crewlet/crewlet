package codescan_test

import (
	"context"
	"crypto/sha1" //nolint:gosec // a git object's id is a SHA-1 of its header and bytes, which the fake forge lists by
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/codescan"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/deployfiles"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/statelog"
)

// gitBlob is the id git gives a blob of body.
func gitBlob(body string) string {
	h := sha1.New() //nolint:gosec // see the import
	fmt.Fprintf(h, "blob %d\x00", len(body))
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// forgeRepo is one repository at the fake forge: every file of its default
// branch's tree, on the closed list or not.
type forgeRepo struct {
	key   string
	tip   string
	stamp string
	files map[string]string
	// sizeless lists the files with no size, as a forge that does not say.
	sizeless bool
}

// fakeForge is a forge as the pass reads it. It answers whatever it is asked
// and records it, so a test sees exactly which objects the pass asked for.
type fakeForge struct {
	kind integration.Kind

	mu       sync.Mutex
	repos    []*forgeRepo
	whole    bool
	off      bool
	opens    int
	closes   int
	calls    int
	tips     []string
	trees    []string
	blobs    []string
	refuse   map[string]error
	tooLarge map[string]bool
	// lie answers a blob's bytes as another object's.
	lie map[string]string
}

func newForge(repos ...*forgeRepo) *fakeForge {
	return &fakeForge{kind: integration.KindGitHub, repos: repos, whole: true, refuse: map[string]error{},
		tooLarge: map[string]bool{}, lie: map[string]string{}}
}

func (f *fakeForge) Kind() integration.Kind { return f.kind }

func (f *fakeForge) Collect(context.Context, collect.Input) (collect.Read, error) {
	return collect.Read{}, nil
}

func (f *fakeForge) OpenCode(context.Context, collect.Input) (collect.CodeSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.off {
		return nil, fmt.Errorf("forge: %w", integration.ErrNotConfigured)
	}
	f.opens++
	f.calls = 0
	return forgeSession{f}, nil
}

type forgeSession struct{ f *fakeForge }

func (s forgeSession) Calls() int {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return s.f.calls
}

func (s forgeSession) Close(context.Context) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.closes++
}

func (s forgeSession) Repositories(context.Context) ([]collect.CodeRepository, bool, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.calls++
	if err := s.f.refuse["repositories"]; err != nil {
		return nil, false, err
	}
	var out []collect.CodeRepository
	for _, r := range s.f.repos {
		out = append(out, collect.CodeRepository{Key: r.key, ID: r.key, Branch: "main", Stamp: r.stamp})
	}
	return out, s.f.whole, nil
}

func (s forgeSession) repo(key string) *forgeRepo {
	for _, r := range s.f.repos {
		if r.key == key {
			return r
		}
	}
	return nil
}

func (s forgeSession) Tip(_ context.Context, repo collect.CodeRepository) (string, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.calls++
	s.f.tips = append(s.f.tips, repo.Key)
	if err := s.f.refuse["tip:"+repo.Key]; err != nil {
		return "", err
	}
	return s.repo(repo.ID).tip, nil
}

func (s forgeSession) Files(_ context.Context, repo collect.CodeRepository, tip string) ([]collect.CodeFile, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.calls++
	s.f.trees = append(s.f.trees, repo.Key)
	r := s.repo(repo.ID)
	switch {
	case r.tip != tip:
		return nil, fmt.Errorf("the pass listed %s at %s, which is not its tip", repo.Key, tip)
	case s.f.tooLarge[repo.Key]:
		return nil, collect.ErrCodeTooLarge
	}
	var out []collect.CodeFile
	for path, body := range r.files {
		f := collect.CodeFile{Path: path, OID: gitBlob(body), Size: int64(len(body))}
		if r.sizeless {
			f.Size = -1
		}
		out = append(out, f)
	}
	return out, nil
}

func (s forgeSession) Blob(_ context.Context, repo collect.CodeRepository, oid string, limit int64) ([]byte, bool, error) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.calls++
	for path, body := range s.repo(repo.ID).files {
		if gitBlob(body) != oid {
			continue
		}
		s.f.blobs = append(s.f.blobs, repo.Key+":"+path)
		if err := s.f.refuse["blob:"+repo.Key+":"+path]; err != nil {
			return nil, false, err
		}
		if other, ok := s.f.lie[path]; ok {
			body = other
		}
		if int64(len(body)) > limit {
			return nil, true, nil
		}
		return []byte(body), false, nil
	}
	return nil, false, fmt.Errorf("the pass asked %s for %s, which it did not list", repo.Key, oid)
}

type forgeRig struct {
	t        *testing.T
	forge    *fakeForge
	log      *fakeLog
	scans    *memory.Fleet
	store    *codescan.ForgeStore
	forges   *codescan.Forges
	mirrored map[string]bool
	mirrors  error
	now      time.Time
}

func newForgeRig(t *testing.T, repos ...*forgeRepo) *forgeRig {
	t.Helper()
	r := &forgeRig{t: t, forge: newForge(repos...), log: newFakeLog(), scans: memory.NewFleet(),
		mirrored: map[string]bool{}, now: scanAt}
	store, err := codescan.NewForgeStore(r.scans)
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	r.forges, err = codescan.NewForges(codescan.ForgeOptions{Log: r.log, Store: store,
		Mirrored: func(context.Context) (map[string]bool, error) { return r.mirrored, r.mirrors }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// pass runs one pass where one is due at the rig's instant, and reports
// whether one was.
func (r *forgeRig) pass() bool {
	r.t.Helper()
	if !r.forges.Due(r.t.Context(), r.forge.kind, time.Hour, r.now) {
		return false
	}
	r.forges.Pass(r.t.Context(), r.forge, collect.Input{Now: r.now}, r.now)
	return true
}

func (r *forgeRig) standing() (codescan.ForgeStatus, map[string]codescan.Status) {
	r.t.Helper()
	fs, repos, err := r.store.Load(r.t.Context(), r.forge.kind)
	if err != nil {
		r.t.Fatal(err)
	}
	return fs, repos
}

// asked is what the forge was asked since the last call, and forgets it.
func (f *fakeForge) asked() (tips, trees, blobs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tips, trees, blobs = f.tips, f.trees, f.blobs
	f.tips, f.trees, f.blobs = nil, nil, nil
	slices.Sort(blobs)
	return tips, trees, blobs
}

const (
	forgeAPI   = "github.com/acme/api"
	forgeVoice = "github.com:acme:api"
)

func apiRepo() *forgeRepo {
	return &forgeRepo{key: forgeAPI, tip: tip1, stamp: "s1", files: map[string]string{
		"compose.yaml": "services:\n  web:\n    build: .\n  db:\n    image: postgres:16.2\n",
		"go.mod":       "module acme.example/api\n\nrequire github.com/stripe/stripe-go/v79 v79.1.0\n",
		"main.go":      "package main\n",
	}}
}

// A FORGE REPOSITORY IS READ WHEN ITS DEFAULT BRANCH MOVES, AND ONLY WHAT
// CHANGED: the first pass lists its tree once and reads its listed files, in
// the repository's own voice and built from the forge's repository; a pass
// while the listing's stamp is unchanged asks nothing of it; a stamp that
// moved with the branch where it was costs one question; and a branch that
// moved reads only the file whose object changed.
//
// Mutation: drop the tip from the standing, and every pass reads every file.
func TestAForgeRepositoryIsReadWhenItsDefaultBranchMovesAndOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	api := apiRepo()
	r := newForgeRig(t, api)
	if !r.pass() {
		t.Fatal("a forge never read owes no pass")
	}
	tips, trees, blobs := r.forge.asked()
	if len(tips) != 1 || len(trees) != 1 ||
		!slices.Equal(blobs, []string{forgeAPI + ":compose.yaml", forgeAPI + ":go.mod"}) {
		t.Fatalf("the first pass asked for tips %v, trees %v and blobs %v", tips, trees, blobs)
	}
	want := []string{"datastore:api/db", "service:api-web", "vendor_account:other/stripe"}
	if got := r.log.entries(forgeVoice); !slices.Equal(got, want) {
		t.Fatalf("the files of %s declare %v, want %v", forgeAPI, got, want)
	}
	var built bool
	for _, in := range r.log.Reads {
		for _, c := range in.Claims {
			if c.Facet.Kind == atlas.FacetRelation && c.Facet.Relation == atlas.RelationBuiltFrom {
				built = c.Facet.Target.Entry == atlas.MustRef(atlas.KindRepository, forgeAPI)
			}
			if c.Facet.Kind == atlas.FacetExists && c.Subject.Kind == atlas.KindRepository {
				t.Errorf("the files claim the repository %s exists, which its forge's read says", c.Subject)
			}
		}
	}
	if !built {
		t.Error("the service a compose file builds is not built from the forge's repository")
	}
	fs, repos := r.standing()
	st := repos[forgeVoice]
	if !fs.Completed || fs.Repositories != 1 || fs.Calls != 5 || fs.Trees != 1 || fs.Reads != 2 || fs.Bytes == 0 ||
		!st.Completed || st.Tip != tip1 || st.Calls != 4 || st.Reads != 2 || st.Listed != 2 {
		t.Fatalf("the standings after the first pass: %+v and %+v", fs, st)
	}
	if r.forge.closes != 1 {
		t.Errorf("the session was closed %d times, want once", r.forge.closes)
	}

	// Not due inside the interval, and a pass at it asks nothing of a
	// repository whose stamp did not move.
	r.now = r.now.Add(30 * time.Minute)
	if r.pass() {
		t.Fatal("a forge read whole half an hour ago owes a pass")
	}
	r.now = r.now.Add(time.Hour)
	written := r.log.written()
	r.pass()
	if tips, trees, blobs := r.forge.asked(); len(tips)+len(trees)+len(blobs) != 0 {
		t.Errorf("an unmoved repository was asked for tips %v, trees %v and blobs %v", tips, trees, blobs)
	}
	// A push to another branch moves the stamp and not the default branch.
	api.stamp = "s2"
	r.now = r.now.Add(time.Hour)
	r.pass()
	if tips, trees, blobs := r.forge.asked(); len(tips) != 1 || len(trees)+len(blobs) != 0 {
		t.Errorf("a stamp that moved alone cost tips %v, trees %v and blobs %v", tips, trees, blobs)
	}
	r.now = r.now.Add(time.Hour)
	r.pass()
	if tips, _, _ := r.forge.asked(); len(tips) != 0 {
		t.Errorf("the new stamp was not kept: the repository was asked for its tip again")
	}
	// The default branch moves, and one listed file with it.
	api.tip, api.stamp = tip2, "s3"
	api.files["go.mod"] = "module acme.example/api\n\nrequire github.com/DataDog/datadog-go/v5 v5.5.0\n"
	api.files["main.go"] = "package main\n\nfunc main() {}\n"
	r.now = r.now.Add(time.Hour)
	r.pass()
	if _, trees, blobs := r.forge.asked(); len(trees) != 1 || !slices.Equal(blobs, []string{forgeAPI + ":go.mod"}) {
		t.Errorf("a moved branch cost trees %v and blobs %v, want one tree and the changed file", trees, blobs)
	}
	want = []string{"datastore:api/db", "service:api-web", "vendor_account:datadog"}
	if got := r.log.entries(forgeVoice); !slices.Equal(got, want) || r.log.written() == written {
		t.Errorf("after the move the files declare %v, want %v", got, want)
	}
}

// NOTHING OFF THE CLOSED LIST IS EVER ASKED OF A FORGE, AND NOTHING LISTED BY
// ITS PRESENCE IS READ: an `.env` file, a values file, a variable file, a
// manifest named for its secrets and a file under a dot directory are never
// asked for, a Dockerfile says what it says by being there, and what a listed
// file holds under `environment` or as a command reaches no claim.
//
// Mutation: read every file the forge lists, or read a Dockerfile, and the
// forge is asked for an object this test plants a canary in.
func TestNothingOffTheClosedListIsEverAskedOfAForge(t *testing.T) {
	t.Parallel()
	const canary = "atlas-canary-never-read"
	planted := map[string]string{
		".env":                            "DATABASE_URL=postgres://app:" + canary + "@db/app\n",
		".env.production":                 "STRIPE_KEY=" + canary + "\n",
		"deploy/.env":                     "TOKEN=" + canary + "\n",
		"chart/values.yaml":               "postgresql:\n  auth:\n    password: " + canary + "\n",
		"chart/values-prod.yaml":          "secret: " + canary + "\n",
		"infra/prod.tfvars":               "db_password = \"" + canary + "\"\n",
		"infra/terraform.tfstate":         "{\"secret\": \"" + canary + "\"}\n",
		"k8s/db-secret.yaml":              "kind: Secret\nstringData:\n  password: " + canary + "\n",
		"k8s/credentials.yaml":            "kind: ConfigMap\ndata:\n  token: " + canary + "\n",
		".github/workflows/deploy.yml":    "env:\n  TOKEN: " + canary + "\n",
		"vendor/acme/compose.yaml":        "services:\n  " + canary + ":\n    image: redis\n",
		"node_modules/x/package.json":     "{\"dependencies\": {\"stripe\": \"" + canary + "\"}}\n",
		"README.md":                       canary + "\n",
		"scripts/deploy.sh":               "export TOKEN=" + canary + "\n",
		"chart/templates/deployment.yaml": "env: " + canary + "\n",
		"Dockerfile":                      "FROM alpine\nENV TOKEN=" + canary + "\nARG KEY=" + canary + "\nCMD [\"run\", \"--token=" + canary + "\"]\n",
		"Procfile":                        "web: run --token=" + canary + "\n",
	}
	listed := map[string]string{
		"compose.yaml": "services:\n  web:\n    image: ghcr.io/acme/web:1.2\n    command: run --token=" + canary +
			"\n    environment:\n      TOKEN: " + canary + "\n      DATABASE_URL: postgres://app:" + canary +
			"@db/app\n    env_file: [.env]\n    labels:\n      note: " + canary + "\n",
		"k8s/web.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  annotations:\n    note: " +
			canary + "\nspec:\n  template:\n    spec:\n      containers:\n        - name: web\n          image: acme/web\n" +
			"          args: [\"--token=" + canary + "\"]\n          env:\n            - name: TOKEN\n              value: " +
			canary + "\n",
		"chart/Chart.yaml": "apiVersion: v2\nname: web\ndescription: " + canary + "\n",
		"package.json": "{\"name\": \"web\", \"scripts\": {\"deploy\": \"deploy --token=" + canary +
			"\"}, \"dependencies\": {\"stripe\": \"" + canary + "\"}}\n",
	}
	repo := &forgeRepo{key: forgeAPI, tip: tip1, files: map[string]string{}}
	for path, body := range planted {
		repo.files[path] = body
	}
	for path, body := range listed {
		repo.files[path] = body
	}
	// Both ways a forge lists a file: with its size, and without.
	for _, sizeless := range []bool{false, true} {
		repo.sizeless = sizeless
		r := newForgeRig(t, repo)
		r.pass()
		_, _, blobs := r.forge.asked()
		var want []string
		for path := range listed {
			want = append(want, forgeAPI+":"+path)
		}
		slices.Sort(want)
		if !slices.Equal(blobs, want) {
			t.Fatalf("sizeless %v: the forge was asked for %v, want only the listed files %v", sizeless, blobs, want)
		}
		for _, in := range r.log.Reads {
			if text := fmt.Sprintf("%+v", in); strings.Contains(text, canary) {
				t.Errorf("sizeless %v: a value no parser has a field for reached the atlas: %s", sizeless, text)
			}
		}
		// What the listed files do declare is still read, and the two files
		// listed by their presence say the repository builds a service.
		got := r.log.entries(forgeVoice)
		for _, entry := range []string{"service:api", "service:api-web", "service:web", "vendor_account:other/stripe"} {
			if !slices.Contains(got, entry) {
				t.Errorf("sizeless %v: the files declare %v, without %s", sizeless, got, entry)
			}
		}
		if _, repos := r.standing(); repos[forgeVoice].Listed != len(listed)+2 {
			t.Errorf("sizeless %v: %d files listed, want the four read and the two by presence", sizeless,
				repos[forgeVoice].Listed)
		}
	}
}

// A PASS OVER A FORGE IS BOUNDED IN FILES, BYTES AND TREES, AND RECORDS WHAT
// IT COST: a pass reads at most fifty files and starts none once it has read
// a mebibyte, lists at most a hundred trees, and leaves the rest to the pass
// five minutes later. A repository whose files were not all read publishes
// nothing until they are.
//
// Mutation: drop the byte bound, and a pass over seventeen files at the size
// bound reads them all.
func TestAPassOverAForgeIsBoundedInFilesBytesAndTrees(t *testing.T) {
	t.Parallel()
	many := &forgeRepo{key: "github.com/acme/many", tip: tip1, files: map[string]string{}}
	for i := range 70 {
		many.files[fmt.Sprintf("svc%02d/package.json", i)] = fmt.Sprintf("{\"name\": \"svc%02d\", \"dependencies\": {\"stripe\": \"1\"}}\n", i)
	}
	r := newForgeRig(t, many)
	r.pass()
	fs, repos := r.standing()
	st := repos["github.com:acme:many"]
	if fs.Completed || fs.Waiting != 1 || fs.Reads != codescan.MaxReadsPerPass || st.Completed || st.Pending != 20 ||
		r.log.written() != 0 {
		t.Fatalf("a pass past its file bound: %+v and %+v, %d records", fs, st, r.log.written())
	}
	// Owed again after the retry, not the interval, and it reads the rest.
	r.now = r.now.Add(codescan.RetryAfter - time.Second)
	if r.pass() {
		t.Fatal("a pass that left work behind is owed before the retry")
	}
	r.now = r.now.Add(time.Second)
	r.forge.asked()
	r.pass()
	_, _, blobs := r.forge.asked()
	fs, repos = r.standing()
	if st = repos["github.com:acme:many"]; len(blobs) != 20 || !fs.Completed || !st.Completed || st.Entries == 0 {
		t.Fatalf("the second pass read %d files: %+v and %+v", len(blobs), fs, st)
	}

	// Twenty files a little under the size bound: the seventeenth takes the
	// pass past a mebibyte, and no eighteenth is started.
	large := &forgeRepo{key: "github.com/acme/large", tip: tip1, files: map[string]string{}}
	body := "{\"name\": \"x\", \"description\": \"" + strings.Repeat("x", deployfiles.MaxFileBytes-200) + "\"}\n"
	for i := range 20 {
		large.files[fmt.Sprintf("p%02d/package.json", i)] = strings.Replace(body, "\"x\"", fmt.Sprintf("\"p%02d\"", i), 1)
	}
	r = newForgeRig(t, large)
	r.pass()
	if fs, _ = r.standing(); fs.Reads != 17 || fs.Bytes < codescan.MaxForgeBytesPerPass || fs.Waiting != 1 || r.log.written() != 0 {
		t.Fatalf("a pass past its byte bound read %d files and %d bytes: %+v", fs.Reads, fs.Bytes, fs)
	}

	// A hundred and twenty repositories: a hundred trees a pass.
	var repositories []*forgeRepo
	for i := range 120 {
		repositories = append(repositories, &forgeRepo{key: fmt.Sprintf("github.com/acme/r%03d", i), tip: tip1,
			files: map[string]string{"Procfile": "web: run\n"}})
	}
	r = newForgeRig(t, repositories...)
	r.pass()
	fs, repos = r.standing()
	if _, trees, _ := r.forge.asked(); len(trees) != codescan.MaxForgeTreesPerPass || fs.Waiting != 20 ||
		fs.Trees != codescan.MaxForgeTreesPerPass || len(repos) != codescan.MaxForgeTreesPerPass {
		t.Fatalf("a pass listed %d trees: %+v", len(trees), fs)
	}
	r.now = r.now.Add(codescan.RetryAfter)
	r.pass()
	if fs, repos = r.standing(); !fs.Completed || len(repos) != 120 {
		t.Fatalf("the second pass left %d repositories read: %+v", len(repos), fs)
	}
}

// A REPOSITORY GITLET MIRRORS IS NOT READ THROUGH ITS FORGE: Gitlet's own
// scan reads it, under the mirror's name and the mirror's visibility. One
// that gains a mirror has what this reader said of it withdrawn, and a pass
// that cannot learn the mirrors reads nothing.
//
// Mutation: read a mirrored repository, and everything its files declare is
// said twice, once in a voice a unit-only mirror was meant to keep.
func TestARepositoryGitletMirrorsIsNotReadThroughItsForge(t *testing.T) {
	t.Parallel()
	web := &forgeRepo{key: "github.com/acme/web", tip: tip1, files: map[string]string{"Procfile": "web: run\n"}}
	r := newForgeRig(t, apiRepo(), web)
	r.mirrored["github.com/acme/web"] = true
	r.pass()
	if tips, _, _ := r.forge.asked(); !slices.Equal(tips, []string{forgeAPI}) {
		t.Fatalf("the pass asked for the tips of %v, want the unmirrored repository's alone", tips)
	}
	fs, repos := r.standing()
	if fs.Mirrored != 1 || len(repos) != 1 || len(r.log.entries("github.com:acme:web")) != 0 {
		t.Fatalf("a mirrored repository: %+v, standings %v", fs, repos)
	}
	// The other gains a mirror: withdrawn at the next pass.
	r.mirrored[forgeAPI] = true
	r.now = r.now.Add(time.Hour)
	r.pass()
	fs, repos = r.standing()
	if fs.Released != 1 || len(repos) != 0 || len(r.log.entries(forgeVoice)) != 0 {
		t.Fatalf("a repository that gained a mirror: %+v, standings %v, entries %v", fs, repos, r.log.entries(forgeVoice))
	}
	// Gitlet cannot say what it mirrors: nothing is read, and the pass says so.
	r.mirrored, r.mirrors = nil, errors.New("gitlet is not answering")
	r.now = r.now.Add(time.Hour)
	r.forge.asked()
	r.pass()
	if tips, _, _ := r.forge.asked(); len(tips) != 0 {
		t.Errorf("a pass that could not learn the mirrors asked for tips %v", tips)
	}
	if fs, _ = r.standing(); !strings.Contains(fs.LastError, "gitlet is not answering") || r.forge.closes != r.forge.opens {
		t.Errorf("the pass that could not learn the mirrors: %+v, %d of %d sessions closed", fs, r.forge.closes, r.forge.opens)
	}
}

// A REPOSITORY THE LISTING DOES NOT HOLD IS RELEASED ONLY WHERE THE LISTING
// IS WHOLE: one a forge stopped listing has what its files declared
// withdrawn, and a listing that is missing a part of the forge withdraws
// nothing, since that part says nothing of what it holds.
//
// Mutation: release on any listing, and a suspended installation withdraws
// every suggestion from the repositories it covered.
func TestARepositoryTheListingDoesNotHoldIsReleasedOnlyWhereTheListingIsWhole(t *testing.T) {
	t.Parallel()
	r := newForgeRig(t, apiRepo())
	r.pass()
	r.forge.repos, r.forge.whole = nil, false
	r.now = r.now.Add(time.Hour)
	r.pass()
	if _, repos := r.standing(); len(repos) != 1 || len(r.log.entries(forgeVoice)) == 0 {
		t.Fatalf("a listing missing a part released %v", repos)
	}
	r.forge.whole = true
	r.now = r.now.Add(time.Hour)
	r.pass()
	if fs, repos := r.standing(); fs.Released != 1 || len(repos) != 0 || len(r.log.entries(forgeVoice)) != 0 {
		t.Fatalf("a whole listing without the repository: %+v, standings %v", fs, repos)
	}
	// A forge the company no longer connects: everything is withdrawn.
	r = newForgeRig(t, apiRepo())
	r.pass()
	r.forge.off = true
	r.now = r.now.Add(time.Hour)
	r.pass()
	// And a forge there is nothing to read with is not asked again at once.
	r.now = r.now.Add(time.Minute)
	if r.pass() {
		t.Error("a forge the company does not connect owes a pass a minute after one found that out")
	}
	left, err := r.scans.AtlasCodeScans(t.Context())
	if _, repos := r.standing(); err != nil || len(repos) != 0 || len(r.log.entries(forgeVoice)) != 0 || len(left) != 0 {
		t.Fatalf("a disconnected forge kept %v, %v and %d standings (%v)", repos, r.log.entries(forgeVoice), len(left), err)
	}
}

// WHAT A FORGE ANSWERS IS HELD TO WHAT IT LISTED: bytes that are not the
// object listed are no reading of it, a file past the size bound is counted
// and not read whichever way the forge says so, and a tree the forge does
// not list whole has none of its files read and what an earlier tip declared
// withdrawn.
//
// Mutation: skip the hash, and a forge that answers another file's bytes for
// a listed object is read as the listed file.
func TestWhatAForgeAnswersIsHeldToWhatItListed(t *testing.T) {
	t.Parallel()
	api := apiRepo()
	r := newForgeRig(t, api)
	r.forge.lie["go.mod"] = "module acme.example/api\n\nrequire github.com/DataDog/datadog-go/v5 v5.5.0\n"
	r.pass()
	fs, repos := r.standing()
	if st := repos[forgeVoice]; !strings.Contains(fs.LastError, "another object") || st.Completed ||
		!strings.Contains(st.LastError, "another object") || r.log.written() != 0 {
		t.Fatalf("a forge that answered another object: %+v and %+v", fs, st)
	}

	// Past the size bound, said by the listing and said only by the read.
	for _, sizeless := range []bool{false, true} {
		big := apiRepo()
		big.sizeless = sizeless
		big.files["package.json"] = "{\"name\": \"" + strings.Repeat("x", deployfiles.MaxFileBytes) + "\"}"
		r = newForgeRig(t, big)
		r.pass()
		_, _, blobs := r.forge.asked()
		_, repos = r.standing()
		if st := repos[forgeVoice]; st.Oversize != 1 || !st.Completed || slices.Contains(blobs, forgeAPI+":package.json") != sizeless {
			t.Errorf("sizeless %v: a file past the size bound: %+v, asked for %v", sizeless, st, blobs)
		}
	}

	// A tree the forge does not list whole.
	r = newForgeRig(t, api)
	r.pass()
	api.tip, api.stamp = tip2, "s2"
	r.forge.lie = map[string]string{}
	r.forge.tooLarge[forgeAPI] = true
	r.now = r.now.Add(time.Hour)
	r.pass()
	fs, repos = r.standing()
	if st := repos[forgeVoice]; fs.TooLarge != 1 || !st.TooLarge || !st.Completed || st.Tip != tip2 ||
		len(r.log.entries(forgeVoice)) != 0 {
		t.Fatalf("a tree past what the forge lists: %+v and %+v", fs, st)
	}
}

// A REPOSITORY A FORGE REFUSES IS LEFT AS IT WAS, AND A FORGE THAT FAILS
// SEVERAL STOPS THE PASS: a refusal of one repository's code withdraws
// nothing and the rest are read; one repository's fault is recorded and the
// rest are read; three faults end the pass, which is owed again at the retry.
//
// Mutation: end the pass on a refusal, and one repository an installation
// lost starves every repository after it.
func TestARefusedRepositoryIsLeftAsItWasAndAFailingForgeStopsThePass(t *testing.T) {
	t.Parallel()
	var repos []*forgeRepo
	for i := range 6 {
		repos = append(repos, &forgeRepo{key: fmt.Sprintf("github.com/acme/r%d", i), tip: tip1,
			files: map[string]string{"Procfile": "web: run\n"}})
	}
	r := newForgeRig(t, repos...)
	r.pass()
	for _, repo := range repos {
		repo.tip = tip2
		repo.files["fly.toml"] = "app = \"edge\"\n"
	}
	r.forge.refuse["tip:github.com/acme/r0"] = fmt.Errorf("%w: 404", collect.ErrCodeRefused)
	r.forge.refuse["blob:github.com/acme/r1:fly.toml"] = fmt.Errorf("%w: 403", collect.ErrCodeRefused)
	r.forge.refuse["tip:github.com/acme/r2"] = errors.New("502 from the forge")
	r.now = r.now.Add(time.Hour)
	r.pass()
	fs, standings := r.standing()
	if fs.Refused != 2 || !strings.Contains(fs.LastError, "502") || fs.Completed {
		t.Fatalf("a pass with two refusals and a fault: %+v", fs)
	}
	for i, wantTip := range []string{tip1, tip1, tip1, tip2, tip2, tip2} {
		voice := fmt.Sprintf("github.com:acme:r%d", i)
		if st := standings[voice]; st.Tip != wantTip || len(r.log.entries(voice)) == 0 {
			t.Errorf("%s is at %s with %v, want %s and its claims kept", voice, st.Tip, r.log.entries(voice), wantTip)
		}
	}
	// Three faults: the pass stops, and the rest wait for the retry.
	r.forge.asked()
	for _, repo := range repos {
		repo.tip = tip1
	}
	for i := range 3 {
		r.forge.refuse[fmt.Sprintf("tip:github.com/acme/r%d", i)] = errors.New("429 from the forge")
	}
	r.now = r.now.Add(codescan.RetryAfter - time.Second)
	if r.pass() {
		t.Fatal("a pass that faulted is owed before the retry")
	}
	r.now = r.now.Add(time.Second)
	r.pass()
	if tips, _, _ := r.forge.asked(); len(tips) != 3 {
		t.Errorf("a forge that failed three repositories was asked for %d tips, want it left alone after three", len(tips))
	}
	if r.forge.closes != r.forge.opens {
		t.Errorf("%d of %d sessions were closed", r.forge.closes, r.forge.opens)
	}
}

// THE TWO SCANS KEEP THEIR STANDINGS APART: a forge's standings sit in the
// bucket Gitlet's scan keeps its own in, and neither reader takes the other's
// keys for a repository of its own.
//
// Mutation: read every key as a Gitlet repository's, and Gitlet's scan
// releases every forge repository as one Gitlet no longer lists.
func TestTheTwoScansKeepTheirStandingsApart(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	gitletStore, err := codescan.NewCoordStore(fleet)
	if err != nil {
		t.Fatal(err)
	}
	forgeStore, err := codescan.NewForgeStore(fleet)
	if err != nil {
		t.Fatal(err)
	}
	log := newFakeLog()
	forges, err := codescan.NewForges(codescan.ForgeOptions{Log: log, Store: forgeStore,
		Mirrored: func(context.Context) (map[string]bool, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	forge := newForge(apiRepo())
	forges.Pass(t.Context(), forge, collect.Input{Now: scanAt}, scanAt)
	code := &fakeCode{repos: []*fakeRepo{shop()}, gone: map[string]bool{}}
	scanner, err := codescan.New(codescan.Options{Code: code, Log: log, Store: gitletStore,
		Now: func() time.Time { return scanAt }})
	if err != nil {
		t.Fatal(err)
	}
	due, _, err := scanner.Due(t.Context(), statelog.Position{}, "")
	if err != nil || !slices.Equal(due, []string{shopID}) {
		t.Fatalf("Gitlet's scan owes %v, %v, want its own repository alone", due, err)
	}
	if err := scanner.Work(t.Context(), shopID, statelog.Position{}); err != nil {
		t.Fatal(err)
	}
	if got, err := gitletStore.Load(t.Context()); err != nil || len(got) != 1 {
		t.Errorf("Gitlet's scan reads %d standings, %v, want its own", len(got), err)
	}
	if _, repos, err := forgeStore.Load(t.Context(), integration.KindGitHub); err != nil || len(repos) != 1 {
		t.Errorf("the forges' reader reads %d standings, %v, want its own", len(repos), err)
	}
	if _, repos, _ := forgeStore.Load(t.Context(), integration.KindGitLab); len(repos) != 0 {
		t.Errorf("one forge reads another's standings: %v", repos)
	}
	if len(log.entries(forgeVoice)) == 0 || len(log.entries("shop")) == 0 {
		t.Errorf("the two voices hold %v and %v", log.entries(forgeVoice), log.entries("shop"))
	}
}

// A FORGE REPOSITORY'S VOICE IS ITS KEY, EXACT BOTH WAYS, and never a Gitlet
// repository's name: a key that cannot be a source's name is not read.
func TestAForgeRepositorysVoiceIsItsKey(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]codescan.Repo{
		"github.com/acme/api":              {Voice: "github.com:acme:api", Name: "api", Key: "github.com/acme/api"},
		"git.acme.example/gitlab/a/b/tool": {Voice: "git.acme.example:gitlab:a:b:tool", Name: "tool", Key: "git.acme.example/gitlab/a/b/tool"},
	} {
		if got, ok := codescan.ForgeRepo(key); !ok || got != want {
			t.Errorf("%s is %+v, %v, want %+v", key, got, ok, want)
		}
	}
	for _, key := range []string{"", "gitlet/api", "api", "GitHub.com/Acme/API", "github.com/acme/" + strings.Repeat("x", 130)} {
		if got, ok := codescan.ForgeRepo(key); ok {
			t.Errorf("%q is read as %+v, want no repository", key, got)
		}
	}
	if got := codescan.GitletRepo("shop"); got != (codescan.Repo{Voice: "shop", Name: "shop", Key: "gitlet/shop"}) {
		t.Errorf("a Gitlet repository is %+v", got)
	}
}
