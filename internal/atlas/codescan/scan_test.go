package codescan_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/codescan"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/deployfiles"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeRepo is one repository as Gitlet's rows and its default tip's tree hold
// it: every file of the tree, on the closed list or not.
type fakeRepo struct {
	id, name   string
	visibility gitlet.Visibility
	head       string
	files      map[string]string
}

// fakeCode is Gitlet as the scan reads it, answering the two engine reads the
// way the Gitlet node does: the listing filtered by the closed list, and a
// file only where the list admits it.
type fakeCode struct {
	mu     sync.Mutex
	repos  []*fakeRepo
	gone   map[string]bool
	reads  []string
	lists  int
	refuse error
}

func (c *fakeCode) repo(id string) *fakeRepo {
	for _, r := range c.repos {
		if r.id == id && !c.gone[id] {
			return r
		}
	}
	return nil
}

func (c *fakeCode) Read(_ context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if q.Kind != gitlet.ReadDefaultTips {
		return gitlet.ReadAnswer{}, fmt.Errorf("the scan asked a %s read", q.Kind)
	}
	var out gitlet.ReadAnswer
	for _, r := range c.repos {
		if c.gone[r.id] || (q.DefaultTips != nil && q.DefaultTips.RepoID != r.id) {
			continue
		}
		out.DefaultTips = append(out.DefaultTips, gitlet.DefaultTip{ID: r.id, Name: r.name, Visibility: r.visibility,
			Head: r.head})
	}
	return out, nil
}

// blob is a stand-in for a blob's id: the same bytes are the same blob.
func blob(body string) string { return fmt.Sprintf("%040x", len(body)*7919+int(sumBytes(body))) }

func sumBytes(s string) (n uint32) {
	for i := range len(s) {
		n = n*31 + uint32(s[i])
	}
	return n
}

func (c *fakeCode) GitRead(_ context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := q.Validate(); err != nil {
		return gitlet.GitAnswer{}, err
	}
	if c.refuse != nil {
		return gitlet.GitAnswer{}, c.refuse
	}
	r := c.repo(q.RepoID)
	if q.Kind != gitlet.GitCodeFiles || r == nil || q.CodeFiles.Commit != r.head {
		return gitlet.GitAnswer{}, fmt.Errorf("the scan asked %+v", q)
	}
	ans := &gitlet.CodeFilesAnswer{Commit: r.head}
	if q.CodeFiles.File != "" {
		c.reads = append(c.reads, r.name+":"+q.CodeFiles.File)
		body, ok := r.files[q.CodeFiles.File]
		ans.File = &gitlet.CodeFile{Path: q.CodeFiles.File}
		if ok {
			ans.File.OID, ans.File.Size, ans.Raw = blob(body), int64(len(body)), []byte(body)
		}
		return gitlet.GitAnswer{Kind: q.Kind, CodeFiles: ans}, nil
	}
	c.lists++
	for path, body := range r.files {
		if _, ok := deployfiles.Classify(path); ok {
			ans.Files = append(ans.Files, gitlet.CodeFile{Path: path, OID: blob(body), Size: int64(len(body))})
		}
	}
	slices.SortFunc(ans.Files, func(a, b gitlet.CodeFile) int { return strings.Compare(a.Path, b.Path) })
	if len(ans.Files) > deployfiles.MaxFiles {
		ans.More, ans.Files = len(ans.Files)-deployfiles.MaxFiles, ans.Files[:deployfiles.MaxFiles]
	}
	return gitlet.GitAnswer{Kind: q.Kind, CodeFiles: ans}, nil
}

// fakeLog is the atlas log as the scan writes it: every voice's live claims.
type fakeLog struct {
	mu      sync.Mutex
	live    map[atlas.Source]map[atlas.Ref]map[string]atlas.Claim
	reads   []atlasdomain.ReadInput
	callers []iam.Actor
	purged  map[atlas.Ref]bool
	refuse  error
}

func newFakeLog() *fakeLog {
	return &fakeLog{live: map[atlas.Source]map[atlas.Ref]map[string]atlas.Claim{}, purged: map[atlas.Ref]bool{}}
}

func (l *fakeLog) LiveOf(_ context.Context, src atlas.Source, _ statelog.Freshness) (atlasdomain.Live, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := atlasdomain.Live{Complete: true}
	for _, slots := range l.live[src] {
		for _, c := range slots {
			out.Claims = append(out.Claims, c)
		}
	}
	return out, nil
}

func (l *fakeLog) Read(_ context.Context, c atlasdomain.Call, in atlasdomain.ReadInput) (statelog.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.refuse != nil:
		return statelog.Result{}, l.refuse
	case l.purged[in.Entry]:
		return statelog.Result{}, atlasdomain.ErrPurged
	}
	l.callers = append(l.callers, c.By)
	l.reads = append(l.reads, in)
	voice := l.live[in.Source]
	if voice == nil {
		voice = map[atlas.Ref]map[string]atlas.Claim{}
		l.live[in.Source] = voice
	}
	slots := voice[in.Entry]
	if slots == nil {
		slots = map[string]atlas.Claim{}
		voice[in.Entry] = slots
	}
	for _, claim := range in.Claims {
		claim.Source, claim.ObservedAt = in.Source, in.ObservedAt
		if err := claim.Validate(); err != nil {
			return statelog.Result{}, err
		}
		slots[claim.Slot()] = claim
	}
	for _, slot := range in.Withdrawn {
		delete(slots, slot)
	}
	if len(slots) == 0 {
		delete(voice, in.Entry)
	}
	return statelog.Result{}, nil
}

// entries is the entries the repository's voice holds, in order.
func (l *fakeLog) entries(repo string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for r := range l.live[atlas.CodeSource(repo)] {
		out = append(out, r.String())
	}
	sort.Strings(out)
	return out
}

func (l *fakeLog) written() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.reads)
}

type rig struct {
	t       *testing.T
	code    *fakeCode
	log     *fakeLog
	store   *codescan.CoordStore
	scanner *codescan.Scanner
	now     time.Time
}

func newRig(t *testing.T, repos ...*fakeRepo) *rig {
	t.Helper()
	r := &rig{t: t, code: &fakeCode{repos: repos, gone: map[string]bool{}}, log: newFakeLog(), now: scanAt}
	store, err := codescan.NewCoordStore(memory.NewFleet())
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	r.scanner, err = codescan.New(codescan.Options{Code: r.code, Log: r.log, Store: store,
		Now: func() time.Time { return r.now }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// due is every repository the listing says owes a pass.
func (r *rig) due() []string {
	r.t.Helper()
	var out []string
	for cursor := ""; ; {
		ids, next, err := r.scanner.Due(r.t.Context(), statelog.Position{}, cursor)
		if err != nil {
			r.t.Fatal(err)
		}
		out = append(out, ids...)
		if cursor = next; cursor == "" {
			sort.Strings(out)
			return out
		}
	}
}

// pass works every due repository once, as one backstop pass does.
func (r *rig) pass() {
	r.t.Helper()
	for _, id := range r.due() {
		if err := r.scanner.Work(r.t.Context(), id, statelog.Position{}); err != nil {
			r.t.Fatalf("work %s: %v", id, err)
		}
	}
}

func (r *rig) status(id string) codescan.Status {
	r.t.Helper()
	all, err := r.store.Load(r.t.Context())
	if err != nil {
		r.t.Fatal(err)
	}
	return all[id]
}

const (
	shopID  = "0192f3a0-7c1e-7b3a-9d2e-000000000001"
	adminID = "0192f3a0-7c1e-7b3a-9d2e-000000000002"
	tip1    = "1111111111111111111111111111111111111111"
	tip2    = "2222222222222222222222222222222222222222"
	tip3    = "3333333333333333333333333333333333333333"
)

func shop() *fakeRepo {
	return &fakeRepo{id: shopID, name: "shop", visibility: gitlet.VisibilityCompany, head: tip1, files: map[string]string{
		"Dockerfile":   "FROM scratch\nENV TOKEN=never-read\n",
		"compose.yaml": "services:\n  api:\n    build: .\n    depends_on: [db]\n  db:\n    image: postgres:16\n",
		"package.json": `{"dependencies":{"stripe":"14"}}`,
		".env":         "TOKEN=never-read\n",
		"main.go":      "package main\n",
	}}
}

// A REPOSITORY IS READ WHEN ITS DEFAULT BRANCH MOVES, AND ONLY WHAT CHANGED:
// the first pass reads every listed file that is read at all, a pass at the
// same tip reads and writes nothing, and a tip that changed one file reads
// that file alone and records only what it changed.
//
// Mutation: drop the blob comparison in Scanner.scan, and the second tip
// reads every file again.
func TestARepositoryIsReadWhenItsDefaultBranchMovesAndOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	repo := shop()
	r := newRig(t, repo)
	r.pass()
	// The Dockerfile is listed by its presence and never read; .env and
	// main.go are not on the list, so they are neither listed nor read.
	if want := []string{"shop:compose.yaml", "shop:package.json"}; !slices.Equal(r.code.reads, want) {
		t.Fatalf("the first pass read %v, want %v", r.code.reads, want)
	}
	want := []string{"datastore:shop/db", "service:shop", "service:shop-api", "vendor_account:other/stripe"}
	if got := r.log.entries("shop"); !slices.Equal(got, want) {
		t.Fatalf("the files declared %v, want %v", got, want)
	}
	st := r.status(shopID)
	if !st.Completed || st.Tip != tip1 || st.Listed != 3 || st.Reads != 2 || st.Calls != 3 || st.Entries != 4 ||
		st.Name != "shop" || st.Rules != codescan.Rules {
		t.Fatalf("the standing after the first pass: %+v", st)
	}
	for _, by := range r.log.callers {
		if by.Kind != iam.ActorSystem || by.Name != codescan.Name {
			t.Errorf("a record was written as %+v, want the engine's own worker", by)
		}
	}

	// NOTHING MOVED: not due, and a nudge (a check finished, another branch
	// was pushed) lists nothing, reads nothing and writes nothing.
	lists, reads, written := r.code.lists, len(r.code.reads), r.log.written()
	if due := r.due(); len(due) != 0 {
		t.Fatalf("a repository whose tip was read whole is due: %v", due)
	}
	if err := r.scanner.Work(t.Context(), shopID, statelog.Position{}); err != nil {
		t.Fatal(err)
	}
	if r.code.lists != lists || len(r.code.reads) != reads || r.log.written() != written {
		t.Fatalf("a nudge that moved no tip listed %d, read %d and wrote %d", r.code.lists-lists,
			len(r.code.reads)-reads, r.log.written()-written)
	}

	// THE TIP MOVES, one file changed and a file off the list changed.
	repo.head = tip2
	repo.files["compose.yaml"] = "services:\n  api:\n    build: .\n    depends_on: [cache]\n  cache:\n    image: redis:7\n"
	repo.files["main.go"] = "package main // changed\n"
	r.pass()
	if got := r.code.reads[reads:]; !slices.Equal(got, []string{"shop:compose.yaml"}) {
		t.Fatalf("the second tip read %v, want the one file that changed", got)
	}
	want = []string{"datastore:shop/cache", "service:shop", "service:shop-api", "vendor_account:other/stripe"}
	if got := r.log.entries("shop"); !slices.Equal(got, want) {
		t.Fatalf("after the move the files declare %v, want %v", got, want)
	}
	// The cache is new, the database is withdrawn, and the api's relation
	// moved: three records, and none for the entries nothing changed about.
	if got := r.log.written() - written; got != 3 {
		t.Errorf("the move wrote %d records, want 3", got)
	}

	// A TIP THAT TOUCHES NO LISTED FILE costs the listing and nothing else.
	reads, written = len(r.code.reads), r.log.written()
	repo.head = tip3
	repo.files["main.go"] = "package main // again\n"
	r.pass()
	if len(r.code.reads) != reads || r.log.written() != written || r.status(shopID).Tip != tip3 {
		t.Errorf("a tip that changed no listed file read %d files and wrote %d records; standing %+v",
			len(r.code.reads)-reads, r.log.written()-written, r.status(shopID))
	}
}

// A FILE OFF THE CLOSED LIST IS NEVER READ, by the scan or for it: the scan
// asks for no such file, and the read's own validation refuses one before a
// Gitlet node is asked.
func TestAFileOffTheClosedListIsNeverRead(t *testing.T) {
	t.Parallel()
	r := newRig(t, shop())
	r.pass()
	for _, read := range r.code.reads {
		if strings.Contains(read, ".env") || strings.Contains(read, "Dockerfile") || strings.Contains(read, "main.go") {
			t.Errorf("the scan read %s", read)
		}
	}
	for _, path := range []string{".env", "services/api/.env", "Dockerfile", "Procfile", "main.go", "k8s/secrets.yaml",
		"charts/api/values.yaml", "infra/prod.tfvars", "../compose.yaml"} {
		q := gitlet.GitQuery{Kind: gitlet.GitCodeFiles, RepoID: shopID,
			CodeFiles: &gitlet.CodeFilesQuery{Commit: tip1, File: path}}
		if err := q.Validate(); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("a read of %s: %v, want it refused as invalid", path, err)
		}
	}
}

// A PASS READS A BOUNDED NUMBER OF FILES AND NEVER PUBLISHES HALF A COMMIT:
// a repository with more changed files than one pass reads is read over
// several, nothing is recorded until the last, and each file is read once.
func TestAPassReadsABoundedNumberOfFilesAndPublishesAWholeCommit(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{id: shopID, name: "mono", visibility: gitlet.VisibilityCompany, head: tip1,
		files: map[string]string{}}
	const services = codescan.MaxReadsPerPass*2 + 5
	for i := range services {
		repo.files[fmt.Sprintf("services/s%03d/fly.toml", i)] = fmt.Sprintf("app = \"svc-%03d\"\n", i)
	}
	r := newRig(t, repo)
	r.pass()
	st := r.status(shopID)
	if st.Completed || st.Reads != codescan.MaxReadsPerPass || st.Pending != services-codescan.MaxReadsPerPass ||
		r.log.written() != 0 {
		t.Fatalf("after one pass: %+v, with %d records written; want the bound read and nothing published",
			st, r.log.written())
	}
	r.pass()
	if st = r.status(shopID); st.Completed || r.log.written() != 0 {
		t.Fatalf("after two passes: %+v, with %d records written", st, r.log.written())
	}
	r.pass()
	st = r.status(shopID)
	if !st.Completed || st.Pending != 0 || st.Reads != 5 || len(r.code.reads) != services {
		t.Fatalf("after three passes: %+v, %d reads in all; want every file read once", st, len(r.code.reads))
	}
	// Every service, and the one platform account they all run on.
	if got := len(r.log.entries("mono")); got != services+1 {
		t.Errorf("%d entries declared, want %d", got, services+1)
	}
}

// WHAT A FILE REFUSES IS COUNTED ON THE STANDING: a file past the byte bound
// is not read, a file that is not its format declares nothing, files past the
// list's bound are not listed, and a refused value is counted.
func TestWhatTheScanRefusesIsCountedOnTheStanding(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{id: shopID, name: "shop", visibility: gitlet.VisibilityCompany, head: tip1, files: map[string]string{
		"compose.yaml": "services:\n  Not_A_Slug!:\n    image: nginx\n  web:\n    image: nginx\n",
		"k8s/big.yaml": strings.Repeat("# padding\n", deployfiles.MaxFileBytes/10+1),
		"package.json": "{",
	}}
	for i := range deployfiles.MaxFiles {
		repo.files[fmt.Sprintf("z/s%03d/go.mod", i)] = "module example.com/x\n"
	}
	r := newRig(t, repo)
	for range 6 {
		r.pass()
	}
	st := r.status(shopID)
	if !st.Completed || st.Listed != deployfiles.MaxFiles || st.Unlisted != 3 || st.Oversize != 1 ||
		st.Unparsed != 1 || st.Withheld != 1 {
		t.Fatalf("the standing: %+v; want %d listed, 3 unlisted, 1 oversize, 1 unparsed, 1 withheld", st,
			deployfiles.MaxFiles)
	}
	for _, read := range r.code.reads {
		if strings.HasSuffix(read, "big.yaml") {
			t.Error("a file past the byte bound was read")
		}
	}
	if got := r.log.entries("shop"); !slices.Equal(got, []string{"service:shop-web"}) {
		t.Errorf("declared %v, want the one service whose name is one", got)
	}
}

// A REPOSITORY ONLY ITS UNIT MAY READ IS NEVER SCANNED, and one that stops
// being scanned (made unit-only, archived or deleted, renamed) has everything
// its files said withdrawn.
//
// Mutation: scan a unit-only repository, and what its files declare is on the
// log every holder of the atlas reads.
func TestAUnitOnlyRepositoryIsNeverScannedAndAnUnscannedOneIsWithdrawn(t *testing.T) {
	t.Parallel()
	private := &fakeRepo{id: adminID, name: "payroll", visibility: gitlet.VisibilityUnit, head: tip1,
		files: map[string]string{"fly.toml": "app = \"payroll\"\n"}}
	repo := shop()
	r := newRig(t, repo, private)
	r.pass()
	if len(r.log.entries("payroll")) != 0 || r.status(adminID).Repo != "" {
		t.Fatalf("a unit-only repository was scanned: %v, %+v", r.log.entries("payroll"), r.status(adminID))
	}
	for _, read := range r.code.reads {
		if strings.HasPrefix(read, "payroll:") {
			t.Errorf("a file of a unit-only repository was read: %s", read)
		}
	}
	if len(r.log.entries("shop")) == 0 {
		t.Fatal("the company's repository declared nothing")
	}

	// RENAMED: the old name's voice is withdrawn and the new one speaks.
	repo.name = "store"
	r.pass()
	if got := r.log.entries("shop"); len(got) != 0 {
		t.Errorf("after the rename the old voice still holds %v", got)
	}
	if got := r.log.entries("store"); !slices.Contains(got, "service:store") || r.status(shopID).Name != "store" {
		t.Errorf("after the rename the new voice holds %v, standing %+v", got, r.status(shopID))
	}

	// MADE UNIT-ONLY: withdrawn, and its standing forgotten.
	repo.visibility = gitlet.VisibilityUnit
	r.pass()
	if got := r.log.entries("store"); len(got) != 0 || r.status(shopID).Repo != "" {
		t.Errorf("a repository made unit-only still holds %v, standing %+v", got, r.status(shopID))
	}

	// AND GONE FROM THE LISTING (deleted or archived) is withdrawn the same.
	repo.visibility = gitlet.VisibilityCompany
	r.pass()
	if len(r.log.entries("store")) == 0 {
		t.Fatal("a repository made company-wide again declared nothing")
	}
	r.code.gone[shopID] = true
	r.pass()
	if got := r.log.entries("store"); len(got) != 0 || r.status(shopID).Repo != "" {
		t.Errorf("a repository gone from the listing still holds %v, standing %+v", got, r.status(shopID))
	}
}

// A PASS THAT FAILED RECORDS ITS FAULT AND WAITS, a purged entry is counted
// and never said, and an empty repository withdraws what it declared.
func TestAFailedPassWaitsAndAPurgedEntryIsCounted(t *testing.T) {
	t.Parallel()
	repo := shop()
	r := newRig(t, repo)
	r.code.refuse = errors.New("the Gitlet node is not ready")
	if err := r.scanner.Work(t.Context(), shopID, statelog.Position{}); err == nil {
		t.Fatal("a pass whose listing failed returned no error")
	}
	st := r.status(shopID)
	if st.Completed || !strings.Contains(st.LastError, "not ready") || r.log.written() != 0 {
		t.Fatalf("the standing after a failed pass: %+v", st)
	}
	r.code.refuse = nil
	if due := r.due(); len(due) != 0 {
		t.Fatalf("a repository whose pass just failed is due at once: %v", due)
	}
	r.now = r.now.Add(codescan.RetryAfter)
	r.log.purged[atlas.MustRef(atlas.KindAccount, "other/stripe")] = true
	r.pass()
	st = r.status(shopID)
	if !st.Completed || st.LastError != "" || st.Purged != 1 || st.Recorded != 3 {
		t.Fatalf("the standing after the retry: %+v; want it complete with one purged record counted", st)
	}
	if slices.Contains(r.log.entries("shop"), "vendor_account:other/stripe") {
		t.Error("a purged entry was recorded again")
	}
	repo.head, repo.files = "", map[string]string{}
	r.pass()
	if got := r.log.entries("shop"); len(got) != 0 || !r.status(shopID).Completed {
		t.Errorf("an emptied repository still declares %v", got)
	}
}
