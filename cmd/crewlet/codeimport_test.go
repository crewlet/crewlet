package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/bits"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
)

// importLimits is the code block every import case runs under: a 1 MiB push
// limit — the smallest the schema admits, so a test-sized history crosses the
// step budget — and a 64 KiB blob limit, so a 100 KiB blob is past it.
const importLimits = "  limits:\n    max_push_bytes: 1048576\n    max_blob_bytes: 65536\n"

// importBudget is a step's budget under importLimits: 921.6 KiB.
const importBudget = 1048576 - 1048576/10

// importRepo declares the repository every import case imports into.
const importRepo = "    - name: web\n      owner: {seat: founder}\n"

// importNode is a node declaring importRepo under importLimits.
func importNode(t *testing.T) codeHost {
	t.Helper()
	return codeNodeWith(t, codeDeclaration{repos: importRepo, settings: importLimits}, nil)
}

// sourceHistory is a source repository's history as git fast-import reads it:
// one git process for a history of any length, every commit's time fixed, so a
// case's ids are the same on every run.
type sourceHistory struct {
	stream bytes.Buffer
	marks  int
}

// when is the commit and tag time of mark.
func when(mark int) int64 { return 1767225600 + int64(mark)*60 }

// commit adds a commit to ref — from its first parent (0: a root), merging
// merges — whose tree is its first parent's with files changed ("" deletes
// one), and answers its mark.
func (h *sourceHistory) commit(ref string, from int, files map[string]string, merges ...int) int {
	h.marks++
	msg := fmt.Sprintf("commit %d", h.marks)
	fmt.Fprintf(&h.stream, "commit %s\nmark :%d\ncommitter Crewlet Test <test@example.com> %d +0000\ndata %d\n%s\n",
		ref, h.marks, when(h.marks), len(msg), msg)
	if from > 0 {
		fmt.Fprintf(&h.stream, "from :%d\n", from)
	}
	for _, m := range merges {
		fmt.Fprintf(&h.stream, "merge :%d\n", m)
	}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if files[path] == "" {
			fmt.Fprintf(&h.stream, "D %s\n", path)
			continue
		}
		fmt.Fprintf(&h.stream, "M 100644 inline %s\ndata %d\n%s\n", path, len(files[path]), files[path])
	}
	h.stream.WriteString("\n")
	return h.marks
}

// tag adds an annotated tag at mark. Its data's one optional newline ends it:
// a blank line after would be read as a command, where a commit's is read as
// its end.
func (h *sourceHistory) tag(name string, mark int, message string) {
	fmt.Fprintf(&h.stream, "tag %s\nfrom :%d\ntagger Crewlet Test <test@example.com> %d +0000\ndata %d\n%s\n",
		name, mark, when(mark), len(message), message)
}

// point sets ref — a lightweight tag, a forge's pull ref — to mark.
func (h *sourceHistory) point(ref string, mark int) {
	fmt.Fprintf(&h.stream, "reset %s\nfrom :%d\n\n", ref, mark)
}

// load writes the history into a fresh bare repository whose HEAD is main,
// answering the client that made it, its file:// URL and its path.
func (h *sourceHistory) load(t *testing.T) (*gittest.Client, string, string) {
	t.Helper()
	git := gittest.NewClient(t)
	bare := filepath.Join(t.TempDir(), "source.git")
	git.Run(t, "", "init", "--bare", "-q", bare)
	if _, stderr, err := git.Try(t, "", bytes.NewReader(h.stream.Bytes()), "--git-dir="+bare, "fast-import",
		"--quiet"); err != nil {
		t.Fatalf("the source's history: %v\n%s", err, stderr)
	}
	return git, "file://" + bare, bare
}

// noise is n incompressible bytes, the same for one seed on every run: a pack
// holding them is as large as they are.
func noise(seed uint64, n int) string {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n+8)
	for i := 0; i < n; i += 8 {
		v := r.Uint64()
		for j := range 8 {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	return string(b[:n])
}

// importInto runs `crewlet code import url web` against h with the hermetic
// git, and any more arguments.
func (h codeHost) importInto(t *testing.T, url string, more ...string) (string, string, error) {
	t.Helper()
	return cli(t, append([]string{"code", "import", url, "web", h.cfg, "-git", hermeticGit(t)}, more...)...)
}

// pushes is how many pushes web's history holds, read at the log's head.
func (h codeHost) pushes(t *testing.T) int {
	t.Helper()
	status, body := h.node.get(t, "/code/repos/web/pushes?limit=200&read_level=linearizable")
	var page struct {
		Pushes []json.RawMessage `json:"pushes"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil {
		t.Fatalf("GET /code/repos/web/pushes = %d %s", status, body)
	}
	return len(page.Pushes)
}

// importPush is one push of web's history as a case reads it.
type importPush struct {
	PackBytes int64 `json:"pack_bytes"`
	Commands  []struct {
		Ref    string `json:"ref"`
		New    string `json:"new"`
		Result string `json:"result"`
		Reason string `json:"reason"`
	} `json:"commands"`
}

// history is web's pushes, OLDEST FIRST — the order an import made them —
// read at the log's head.
func (h codeHost) history(t *testing.T) []importPush {
	t.Helper()
	status, body := h.node.get(t, "/code/repos/web/pushes?limit=200&read_level=linearizable")
	var page struct {
		Pushes []importPush `json:"pushes"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil {
		t.Fatalf("GET /code/repos/web/pushes = %d %s", status, body)
	}
	slices.Reverse(page.Pushes)
	return page.Pushes
}

// liveBytes is what web's packs hold, read at the log's head.
func (h codeHost) liveBytes(t *testing.T) int64 {
	t.Helper()
	status, body := h.node.get(t, "/code/repos/web?read_level=linearizable")
	var detail struct {
		Repo struct {
			LiveBytes int64 `json:"live_bytes"`
		} `json:"repo"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &detail) != nil {
		t.Fatalf("GET /code/repos/web = %d %s", status, body)
	}
	return detail.Repo.LiveBytes
}

// served is every ref web serves — peeled tags included, HEAD left out, which
// is the repository's default branch rather than anything an import moves —
// as a person's git lists it.
func (h codeHost) served(t *testing.T) map[string]string {
	t.Helper()
	refs := codetest.PersonWithToken(t, h.token).LsRemote(h.base+"/git/web.git", 2)
	delete(refs, "HEAD")
	return refs
}

// sourceRefs is every ref the source at bare holds, as git ls-remote lists it
// — peeled tags included, HEAD left out.
func sourceRefs(t *testing.T, git *gittest.Client, bare string) map[string]string {
	t.Helper()
	refs := map[string]string{}
	for line := range strings.Lines(git.Run(t, "", "ls-remote", bare)) {
		oid, ref, _ := strings.Cut(strings.TrimSpace(line), "\t")
		if ref != "HEAD" {
			refs[ref] = oid
		}
	}
	return refs
}

// wantSameRefs fails t unless web serves exactly the source's refs.
func wantSameRefs(t *testing.T, h codeHost, git *gittest.Client, bare string) {
	t.Helper()
	want, got := sourceRefs(t, git, bare), h.served(t)
	if !maps.Equal(want, got) {
		t.Errorf("web serves %v, the source holds %v", got, want)
	}
}

// AN IMPORT STEPS A LARGE HISTORY UNDER THE BUDGET. A first-parent main of
// sixty commits, each a new incompressible file of 52 KiB — about 3 MiB, three
// times what one push may carry — lands as fast-forwards of main, then its
// branch and its annotated tag, and EVERY PUSH IS HELD TO THE BUDGET: a push
// row's pack_bytes is the bundle the node stored, and a step is sent whole, so
// that bundle is exactly the pack the step sent and the .idx and .rev beside
// it, whose sizes follow from the objects the step carries — counted here in
// the source, from what the repository held before the push. Run under
// pack.threads=1, an operator's setting the measurement and the push share,
// the push builds its measurement's pack byte for byte, so the budget holds of
// what was stored exactly rather than within the drift between two threaded
// builds that the budget's tenth below max_push_bytes is for.
//
// And the steps are as LARGE as the budget allows: a step's estimate leaves out
// what the repository will hold already, so after the first step each takes
// another budget's worth of commits.
//
// Mutation: cut steps to the push limit rather than a tenth below it, and a
// push stores more than the budget and its indexes; cut them to twice the
// limit, and the first step is refused past receive.maxInputSize; estimate a
// step without what the repository will hold, and every step after the first
// is a single commit.
func TestImportStepsALargeHistoryUnderTheBudget(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	var tip, fork, tagged int
	for i := range 60 {
		tip = hist.commit("refs/heads/main", tip, map[string]string{
			fmt.Sprintf("data/%02d.bin", i): noise(uint64(i), 52<<10)})
		switch i {
		case 29:
			fork = tip
		case 44:
			tagged = tip
		}
	}
	feature := hist.commit("refs/heads/feature", fork, map[string]string{"feature/a.bin": noise(100, 30<<10)})
	hist.commit("refs/heads/feature", feature, map[string]string{"feature/b.bin": noise(101, 30<<10)})
	hist.tag("v1", tagged, "the first release")
	git, url, bare := hist.load(t)

	out, errOut, err := cli(t, "code", "import", url, "web", h.cfg, "-git", operatorsGit(t, "pack.threads=1"))
	if err != nil {
		t.Fatalf("code import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	steps := strings.Count(out, "\nimported step ")
	if steps < 3 || steps > 6 {
		t.Errorf("the import took %d steps, want the 3 MiB history in 4 or 5 — about 0.9 MiB each:\n%s", steps, out)
	}
	pushes := h.history(t)
	if len(pushes) != steps {
		t.Errorf("web records %d pushes, the import printed %d steps", len(pushes), steps)
	}
	held := map[string]string{}
	for i, p := range pushes {
		var revs strings.Builder
		for _, c := range p.Commands {
			fmt.Fprintln(&revs, c.New)
		}
		for _, oid := range held {
			fmt.Fprintln(&revs, "^"+oid)
		}
		listed, stderr, err := git.Try(t, "", strings.NewReader(revs.String()), "--git-dir="+bare, "rev-list",
			"--objects", "--stdin")
		if err != nil {
			t.Fatalf("the objects of push %d: %v\n%s", i+1, err, stderr)
		}
		objects := int64(strings.Count(listed, "\n"))
		if bound := importBudget + code.IndexBytesFor(objects) + code.ReverseBytesFor(objects); p.PackBytes > bound {
			t.Errorf("push %d of %d stored %d bytes for %d objects, past the budget and their indexes (%d)", i+1,
				len(pushes), p.PackBytes, objects, bound)
		}
		for _, c := range p.Commands {
			held[c.Ref] = c.New
		}
	}
	if !strings.Contains(out, "imported web: 3 refs, ") {
		t.Errorf("the import does not end saying what it imported:\n%s", out)
	}
	wantSameRefs(t, h, git, bare)
}

// AN IMPORT MEASURES EACH STEP BEFORE IT PUSHES IT — AND WHAT IT MEASURES IS
// WHAT THE NODE STORES. A history packed as a forge packs one stores the
// newest version of each file whole and every older version as a small delta
// against it, so a range of the older commits ESTIMATES at a few kilobytes and
// PACKS to every file whole — here sixteen files of 40 KiB growing by 1 KiB a
// commit, whose first nineteen commits estimate under the budget and pack past
// it. Measured, the step is cut shorter before anything is sent. And every
// step is sent whole, so what the import says its steps stored is what the
// repository holds once they land — within the drift between a measurement
// and the push's own build of the pack.
//
// Mutation: take a step's estimate for its size, and the first push is
// refused past receive.maxInputSize; measure and push thin packs, and the
// repository holds twice what the import says it stored.
func TestImportMeasuresEachStepBeforePushingIt(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	files := map[string]string{}
	for i := range 16 {
		files[fmt.Sprintf("f/%02d.bin", i)] = noise(uint64(1000+i), 40<<10)
	}
	tip := hist.commit("refs/heads/main", 0, maps.Clone(files))
	git, url, bare := forgePacked(t, &hist, files, tip)
	first19, err := diskUsage(t, git, bare, "main~1")
	if err != nil || first19 >= importBudget {
		t.Fatalf("the first nineteen commits estimate at %d bytes (%v), want them under the budget — the case "+
			"needs a history whose estimate understates its pack", first19, err)
	}

	out, errOut, err := h.importInto(t, url)
	if err != nil {
		t.Fatalf("code import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "cut shorter") {
		t.Errorf("no step was cut shorter by its measurement:\n%s", out)
	}
	wantSameRefs(t, h, git, bare)
	var said int64
	if i := strings.Index(out, " stored with the packs' indexes"); i >= 0 {
		start := strings.LastIndex(out[:i], ", ") + 2
		said = parseHumanBytes(t, out[start:i])
	}
	// To humanBytes' tenth of a unit, and the drift between a measurement and
	// the push's own build of the pack: far inside what completing thin packs
	// adds, which here doubles it.
	if held := h.liveBytes(t); said == 0 || math.Abs(float64(held-said)) > 0.05*float64(held) {
		t.Errorf("the import says its steps stored %s, and web holds %s:\n%s", humanBytes(said), humanBytes(held),
			out)
	}
}

// forgePacked loads h — files growing by a kibibyte a commit for nineteen
// commits after tip — and repacks it as a forge packs a history: every
// version of a file a delta against the largest, which is its newest.
func forgePacked(t *testing.T, hist *sourceHistory, files map[string]string, tip int) (*gittest.Client, string,
	string) {

	t.Helper()
	for c := range 19 {
		for i, path := range slices.Sorted(maps.Keys(files)) {
			files[path] += noise(uint64(2000+c*100+i), 1<<10)
		}
		tip = hist.commit("refs/heads/main", tip, maps.Clone(files))
	}
	git, url, bare := hist.load(t)
	git.Run(t, "", "--git-dir="+bare, "repack", "-a", "-d", "-f", "--window=50", "--depth=50", "-q")
	return git, url, bare
}

// parseHumanBytes reads back what humanBytes printed, to its precision.
func parseHumanBytes(t *testing.T, s string) int64 {
	t.Helper()
	var v float64
	var unit string
	if _, err := fmt.Sscanf(s, "%f %s", &v, &unit); err != nil {
		t.Fatalf("%q is no size humanBytes prints: %v", s, err)
	}
	scale := map[string]float64{"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30}[unit]
	if scale == 0 {
		t.Fatalf("%q is no size humanBytes prints", s)
	}
	return int64(v * scale)
}

// diskUsage is git's estimate of what rev is, from the source's own packs.
func diskUsage(t *testing.T, git *gittest.Client, bare, rev string) (int64, error) {
	t.Helper()
	var n int64
	_, err := fmt.Sscan(git.Run(t, "", "--git-dir="+bare, "rev-list", "--objects", "--disk-usage", rev), &n)
	return n, err
}

// AN IMPORT REFUSES AN OVERSIZED BLOB FIRST, pushing nothing: a 100 KiB file two
// commits back — since deleted — is named by its id, its path and its size, and
// by the limit it is past.
func TestImportRefusesAnOversizedBlobFirst(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	c := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	c = hist.commit("refs/heads/main", c, map[string]string{"assets/big.bin": noise(7, 100<<10)})
	c = hist.commit("refs/heads/main", c, map[string]string{"assets/big.bin": ""})
	hist.commit("refs/heads/main", c, map[string]string{"x.txt": "x\n"})
	git, url, bare := hist.load(t)
	blob := strings.TrimSpace(git.Run(t, "", "--git-dir="+bare, "rev-parse", "main~2:assets/big.bin"))

	out, _, err := h.importInto(t, url)
	if err == nil {
		t.Fatalf("an import of a 100 KiB blob under a 64 KiB limit succeeded:\n%s", out)
	}
	for _, want := range []string{"nothing was imported", blob, "assets/big.bin", "100.0 KiB",
		"code.limits.max_blob_bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if n := h.pushes(t); n != 0 {
		t.Errorf("a refused import pushed %d times", n)
	}
}

// AN IMPORT RESUMES AFTER A FAILED STEP. A branch code.repos freezes
// (direct_push: none) refuses its own import: the default branch lands, and
// the batch holding the frozen branch is refused WHOLE — its other branch too,
// since a step is --atomic — so the import stops naming the rule. With the rule
// lifted, the same command finds main already there and pushes the rest.
//
// Mutation: push a step without --atomic, and the frozen branch's neighbour
// lands from a step that failed.
func TestImportResumesAfterAFailedStep(t *testing.T) {
	t.Parallel()
	frozen := importRepo + "      protect:\n        - branch: frozen\n          direct_push: none\n"
	h := codeNodeWith(t, codeDeclaration{repos: frozen, settings: importLimits}, nil)
	var hist sourceHistory
	c := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	c = hist.commit("refs/heads/main", c, map[string]string{"a.txt": "a\n"})
	hist.commit("refs/heads/frozen", c, map[string]string{"frozen.txt": "frozen\n"})
	hist.commit("refs/heads/zeta", c, map[string]string{"zeta.txt": "zeta\n"})
	git, url, bare := hist.load(t)

	out, errOut, err := h.importInto(t, url)
	if err == nil || !strings.Contains(err.Error(), "the import stopped at step 2") ||
		!strings.Contains(errOut, "protected_direct") || !strings.Contains(err.Error(), "direct_push: none") {
		t.Fatalf("an import into a frozen branch = %v\nstdout:\n%s\nstderr:\n%s; want it stopped at step 2, "+
			"git's line naming protected_direct and the rule's remedy", err, out, errOut)
	}
	served := h.served(t)
	if served["refs/heads/main"] != sourceRefs(t, git, bare)["refs/heads/main"] {
		t.Errorf("main did not land before the refused step: %v", served)
	}
	if _, ok := served["refs/heads/zeta"]; ok {
		t.Errorf("zeta landed from a step that was refused: %v", served)
	}

	h.declare(t, codeDeclaration{repos: importRepo, settings: importLimits})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		_, body := h.node.get(t, "/code/repos/web?read_level=linearizable")
		var detail struct {
			Repo struct {
				Protections []json.RawMessage `json:"protections"`
			} `json:"repo"`
		}
		if json.Unmarshal([]byte(body), &detail) == nil && len(detail.Repo.Protections) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rule was never lifted: %s", body)
		}
	}
	out, errOut, err = h.importInto(t, url)
	if err != nil {
		t.Fatalf("the resumed import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "already on web: 1 of the source's refs") ||
		!strings.Contains(out, "imported step 1: 2 refs, refs/heads/frozen … refs/heads/zeta") ||
		strings.Contains(out, "imported step 2") {
		t.Errorf("the resumed import did not push exactly what was left:\n%s", out)
	}
	wantSameRefs(t, h, git, bare)
}

// AN IMPORT CONTINUES A BRANCH IT STOPPED PART WAY ALONG. The default branch is
// pushed along its first-parent history, so an import that failed after its
// first steps leaves the repository holding main at an EARLIER commit of the
// source's main — which is a resumed import of the same history, continued
// from there, never a repository that diverged.
//
// Mutation: hold every ref the repository has to the source's value, and the
// resume is refused as diverged.
func TestImportContinuesABranchItStoppedPartWayAlong(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	c := 0
	for i := range 8 {
		c = hist.commit("refs/heads/main", c, map[string]string{fmt.Sprintf("%d.txt", i): fmt.Sprintf("%d\n", i)})
	}
	git, url, bare := hist.load(t)
	earlier := strings.TrimSpace(git.Run(t, "", "--git-dir="+bare, "rev-parse", "main~3"))
	person := codetest.PersonWithToken(t, h.token)
	if _, stderr, err := person.Git().Try(t, "", nil, "--git-dir="+bare, "push", "-q", h.base+"/git/web.git",
		earlier+":refs/heads/main"); err != nil {
		t.Fatalf("the earlier step: %v\n%s", err, stderr)
	}

	out, errOut, err := h.importInto(t, url)
	if err != nil {
		t.Fatalf("an import of a branch the repository holds part way along: %v\nstdout:\n%s\nstderr:\n%s", err,
			out, errOut)
	}
	if !strings.Contains(out, "continuing refs/heads/main from the earlier commit web holds it at") {
		t.Errorf("the import does not say it continues main:\n%s", out)
	}
	if n := h.pushes(t); n != 2 {
		t.Errorf("web records %d pushes, want the earlier step and the one that finished main", n)
	}
	wantSameRefs(t, h, git, bare)
}

// AN IMPORT OF A SOURCE WHOSE DEFAULT BRANCH IS ANOTHER SAYS SO, and imports it
// first under its own name: a repository declared with main imports a source
// on master as master, saying the repository's default branch is not among
// what it imported — never renaming a branch, which would make the import a
// different history from the source.
func TestImportOfASourceWhoseDefaultBranchIsAnotherSaysSo(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	c := hist.commit("refs/heads/master", 0, map[string]string{"README.md": "# web\n"})
	hist.commit("refs/heads/topic", c, map[string]string{"t.txt": "t\n"})
	git, url, bare := hist.load(t)
	git.Run(t, "", "--git-dir="+bare, "symbolic-ref", "HEAD", "refs/heads/master")

	out, errOut, err := h.importInto(t, url)
	if err != nil {
		t.Fatalf("code import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	for _, want := range []string{
		"the source's default branch is master; code.repos says main — importing master as master; push a " +
			"main for the repository's HEAD",
		"imported step 1: refs/heads/master → ",
		"imported step 2: refs/heads/topic → ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the import does not say %q:\n%s", want, out)
		}
	}
	wantSameRefs(t, h, git, bare)
}

// AN IMPORT SKIPS A FORGE'S REFS. Pull and merge-request refs are the forge's,
// not the repository's — and the code host takes no push of a ref outside
// refs/heads/ and refs/tags/ — so they are listed as skipped, never pushed, and
// the history only they reach is never even downloaded: the pull ref's commit
// holds a blob past code.limits.max_blob_bytes, which the import refuses
// wherever it finds one in what it downloaded, so it imports only because the
// clone never held it.
//
// Mutation: clone every ref of the source (--mirror), and the import refuses
// the pull ref's blob.
func TestImportSkipsForgeRefs(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	c1 := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	c2 := hist.commit("refs/heads/main", c1, map[string]string{"a.txt": "a\n"})
	hist.commit("refs/pull/1/head", c2, map[string]string{"pr.bin": noise(11, 100<<10)})
	hist.point("refs/merge-requests/2/head", c1)
	git, url, bare := hist.load(t)

	out, errOut, err := h.importInto(t, url)
	if err != nil {
		t.Fatalf("code import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "skipping 2 refs outside refs/heads/ and refs/tags/ (refs/merge-requests/*, "+
		"refs/pull/*): a forge's, not the repository's") {
		t.Errorf("the import does not say what it skipped:\n%s", out)
	}
	served := h.served(t)
	if len(served) != 1 || served["refs/heads/main"] != sourceRefs(t, git, bare)["refs/heads/main"] {
		t.Errorf("web serves %v, want main alone", served)
	}
}

// AN IMPORT CUTS A BATCH TO WHAT ONE PUSH'S RECORD HOLDS. The node refuses a
// push whose ref updates are wider than its record may be, before anything of
// it runs, and a ref the code log spells long — `&` is six bytes there — fills
// a record long before a thousand of them: seven hundred tags of 240 `&` each
// are one push the node refuses whole. The import asks the domain's own rule
// of each batch, as the person the node records, so the tags land in more than
// one push, every one of them taken.
//
// Mutation: batch by count alone (drop recordHolds' cut), and the one batch of
// seven hundred is refused "push them in parts" on every run.
func TestImportCutsABatchToWhatOnePushsRecordHolds(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	c := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	const tags = 700
	for i := range tags {
		hist.point(fmt.Sprintf("refs/tags/%03d%s", i, strings.Repeat("&", 240)), c)
	}
	git, url, bare := hist.load(t)

	out, errOut, err := h.importInto(t, url)
	if err != nil {
		t.Fatalf("code import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	wantSameRefs(t, h, git, bare)
	batches, landed := 0, 0
	for _, push := range h.history(t) {
		if !strings.HasPrefix(push.Commands[0].Ref, "refs/tags/") {
			continue
		}
		batches++
		for _, cmd := range push.Commands {
			if cmd.Result != "ok" {
				t.Errorf("%s was %s (%s) in its push", cmd.Ref, cmd.Result, cmd.Reason)
			}
			landed++
		}
	}
	if batches < 2 || landed != tags {
		t.Errorf("the %d tags landed as %d updates in %d pushes, want every one of them in more than one",
			tags, landed, batches)
	}
}

// A BATCH IS CUT WHERE THE NODE'S OWN RULE CUTS IT: recordHolds answers the
// most refs, in order, whose updates the domain's rule takes as one push from
// this person through the widest node id config admits — one more is refused
// as too wide — and a thousand plain refs of the longest are one batch. A
// person whose own id fills the record is refused naming them, since no push
// of theirs can land.
//
// Mutation: ask the rule with an empty principal, and the wide case's batch
// is one the node may refuse, and the person whose id fills the record is
// never told. (A node id's 64 bytes are within one wide command's width, so
// no batch here tells the widest from the narrowest: importWidestNode is held
// to config's own rule instead.)
func TestABatchIsCutWhereThePushsRecordRuleCutsIt(t *testing.T) {
	t.Parallel()
	if !config.ValidNodeID(importWidestNode) || config.ValidNodeID(importWidestNode+"n") {
		t.Fatalf("importWidestNode is %d bytes, not the widest node id config admits", len(importWidestNode))
	}
	person := code.Principal{Kind: code.PrincipalOperator, OperatorID: strings.Repeat("o", 4<<10),
		Seat: strings.Repeat("s", 4<<10)}
	imp := &importer{principal: person}
	refs := func(fill string) []queuedRef {
		out := make([]queuedRef, code.MaxCommandsPerPush)
		for i := range out {
			name := fmt.Sprintf("refs/tags/%04d", i)
			name += strings.Repeat(fill, code.MaxRefNameBytes-len(name))
			out[i] = queuedRef{sourceRef: sourceRef{ref: name, oid: fmt.Sprintf("%040x", i+1), kind: "commit"}}
		}
		return out
	}
	asked := func(rs []queuedRef) error {
		cmds := make([]code.Command, len(rs))
		for i, r := range rs {
			cmds[i] = code.Command{Ref: r.ref, Old: code.ZeroOID, New: r.oid}
		}
		return code.Proposal{Principal: person, Via: code.ViaGit, Node: importWidestNode,
			Commands: cmds}.ValidateCommands()
	}

	if n, err := imp.recordHolds(refs("a")); err != nil || n != code.MaxCommandsPerPush {
		t.Errorf("a thousand plain refs of the longest: %d, %v — want one batch of all of them", n, err)
	}
	wide := refs("&")
	n, err := imp.recordHolds(wide)
	if err != nil {
		t.Fatalf("recordHolds of wide refs: %v", err)
	}
	var tooWide *code.TooWideError
	if n >= len(wide) || asked(wide[:n]) != nil || !errors.As(asked(wide[:n+1]), &tooWide) {
		t.Errorf("recordHolds cut %d wide refs at %d, which is not where the node's rule cuts them", len(wide), n)
	}

	huge := &importer{principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: strings.Repeat("o", 1<<20),
		Seat: "founder"}}
	_, err = huge.recordHolds(wide[:1])
	if err == nil || !errors.As(err, &tooWide) || !strings.Contains(err.Error(), "no push by this token can land") ||
		!strings.Contains(err.Error(), "founder (token ooo") {
		t.Errorf("a person whose id fills the record: %v", err)
	}
}

// AN IMPORT REFUSES A REPOSITORY THAT DIVERGED: a main the repository already
// holds at a commit the source's history does not hold is somebody else's
// push, not an earlier step of this import — refused naming the ref and both
// values, and nothing pushed.
func TestImportRefusesADivergedTarget(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	theirs := h.push(t, "web", map[string]string{"other.txt": "another history\n"}, "HEAD:refs/heads/main")
	var hist sourceHistory
	c := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	hist.commit("refs/heads/main", c, map[string]string{"a.txt": "a\n"})
	git, url, bare := hist.load(t)
	ours := sourceRefs(t, git, bare)["refs/heads/main"]

	_, _, err := h.importInto(t, url)
	if err == nil {
		t.Fatal("an import into a repository holding another history succeeded")
	}
	for _, want := range []string{"refs/heads/main", theirs, ours, "not a resumed import"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if n := h.pushes(t); n != 1 {
		t.Errorf("web records %d pushes, want the one that was there before the import", n)
	}
}

// A DRY RUN PLANS AND PUSHES NOTHING: every step it would take, sized by its
// estimate, and the repository left as it was.
func TestImportPlansWithoutPushingOnADryRun(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	tip := 0
	for i := range 30 {
		tip = hist.commit("refs/heads/main", tip, map[string]string{
			fmt.Sprintf("data/%02d.bin", i): noise(uint64(300+i), 52<<10)})
	}
	_, url, _ := hist.load(t)

	out, errOut, err := h.importInto(t, url, "-dry-run")
	if err != nil {
		t.Fatalf("code import -dry-run: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	for _, want := range []string{"plan: 2 steps", "step 1/2: refs/heads/main → ", "step 2/2: refs/heads/main → ",
		"(≈ ", "dry run: nothing was pushed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not say %q:\n%s", want, out)
		}
	}
	if n := h.pushes(t); n != 0 {
		t.Errorf("a dry run pushed %d times", n)
	}
	if served := h.served(t); len(served) != 0 {
		t.Errorf("a dry run left web holding %v", served)
	}
}

// AN IMPORT LANDS THE SOURCE'S REFS (R8): two branches, a merge commit, a
// lightweight and an annotated tag arrive as the source holds them, and a
// clone of the repository is whole by git's own strictest check.
func TestAnImportLandsTheSourcesRefs(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	c1 := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	c2 := hist.commit("refs/heads/main", c1, map[string]string{"a.txt": "a\n"})
	d1 := hist.commit("refs/heads/dev", c1, map[string]string{"dev.txt": "dev\n"})
	merged := hist.commit("refs/heads/main", c2, map[string]string{"dev.txt": "dev\n"}, d1)
	hist.commit("refs/heads/dev", d1, map[string]string{"dev2.txt": "more\n"})
	hist.tag("v1.0.0", merged, "the first release")
	hist.point("refs/tags/light", c2)
	git, url, bare := hist.load(t)

	out, errOut, err := h.importInto(t, url)
	if err != nil {
		t.Fatalf("code import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	wantSameRefs(t, h, git, bare)
	person := codetest.PersonWithToken(t, h.token)
	clone := filepath.Join(t.TempDir(), "clone.git")
	if _, stderr, err := person.Git().Try(t, "", nil, "clone", "-q", "--mirror", h.base+"/git/web.git",
		clone); err != nil {
		t.Fatalf("a clone of the imported repository: %v\n%s", err, stderr)
	}
	person.Git().Run(t, "", "--git-dir="+clone, "fsck", "--full", "--strict")
}

// AN IMPORT REPORTS THE STEP IN FLIGHT BEFORE A REFUSAL OF ITS PLAN. The next
// step is planned and measured while the step before it is pushed, so a
// refusal of the plan — a tag whose own history is past the push limit, which
// no step can make smaller — arrives while a push is in flight: that push is
// waited for and reported as landed before the refusal says where the import
// stopped, and nothing of it is left running.
//
// Mutation: answer the plan's refusal without waiting for the push in flight,
// and the step that landed is never said to have landed.
func TestAnImportReportsTheStepInFlightBeforeARefusal(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	huge := map[string]string{}
	for i := range 20 {
		huge[fmt.Sprintf("huge/%02d.bin", i)] = noise(uint64(500+i), 60<<10)
	}
	// A lightweight tag of a root commit of its own: a history no branch
	// reaches, 1.2 MiB of it in one commit.
	hist.commit("refs/tags/huge", 0, huge)
	_, url, _ := hist.load(t)

	out, _, err := h.importInto(t, url)
	if err == nil {
		t.Fatalf("an import of a tag past the push limit succeeded:\n%s", out)
	}
	if !strings.Contains(out, "imported step 1: refs/heads/main") {
		t.Errorf("the step in flight was never reported landed:\n%s", out)
	}
	for _, want := range []string{"refs/tags/huge", "alone packs to", "no branch's history reaches it",
		"code.limits.max_push_bytes", "stopped before step 2", "the 1 step of this run before it landed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if served := h.served(t); served["refs/heads/main"] == "" {
		t.Errorf("main did not land: %v", served)
	}
}

// AN IMPORT SAYS WHY GIT REFUSED A STEP, in git's own words. The line git
// prints for a refused ref names a class alone — `[remote rejected] (unpacker
// error)` for every object the node's fsck refuses, and for a pack past
// receive.maxInputSize — and the reason is on git's stderr, which only a
// person watching a terminal saw go by: an import run by a script or a CI job
// stopped telling its operator to fix a cause nothing had named. Here the
// source holds a commit whose author line carries a time zone of five digits,
// which the node's receive.fsckObjects refuses as badTimezone.
//
// Mutation: answer a refused step without git's stderr, and the refusal names
// no reason.
func TestAnImportSaysWhyGitRefusedAStep(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	git := gittest.NewClient(t)
	bare := filepath.Join(t.TempDir(), "source.git")
	git.Run(t, "", "init", "--bare", "-q", bare)
	tree, stderr, err := git.Try(t, "", strings.NewReader(""), "--git-dir="+bare, "hash-object", "-t", "tree", "-w",
		"--stdin")
	if err != nil {
		t.Fatalf("the empty tree: %v\n%s", err, stderr)
	}
	commit := "tree " + strings.TrimSpace(tree) + "\n" +
		"author A U Thor <author@example.com> 1767225600 +99999\n" +
		"committer A U Thor <author@example.com> 1767225600 +0000\n\n" +
		"a commit no fsck takes\n"
	oid, stderr, err := git.Try(t, "", strings.NewReader(commit), "--git-dir="+bare, "hash-object", "-t", "commit",
		"-w", "--literally", "--stdin")
	if err != nil {
		t.Fatalf("the malformed commit: %v\n%s", err, stderr)
	}
	git.Run(t, "", "--git-dir="+bare, "update-ref", "refs/heads/main", strings.TrimSpace(oid))
	git.Run(t, "", "--git-dir="+bare, "symbolic-ref", "HEAD", "refs/heads/main")

	out, errOut, err := h.importInto(t, "file://"+bare)
	if err == nil {
		t.Fatalf("an import of a commit the node's fsck refuses succeeded:\n%s", out)
	}
	if !strings.Contains(errOut, "(unpacker error)") || !strings.Contains(err.Error(), "badTimezone") {
		t.Errorf("the refusal does not say why git refused the step: %v\nstdout:\n%s\nstderr:\n%s", err, out,
			errOut)
	}
}

// AN IMPORT WITH A TOKEN THAT CANNOT PUSH STOPS BEFORE CLONING: the import
// pushes as the person its token is bound to, and a token bound to nobody
// fetches only — which the node says at a push's first request, so the import
// asks it that before it downloads the source, and says it in the node's words.
//
// Mutation: ask nothing before the clone, and the refusal comes after the
// source was downloaded.
func TestImportWithATokenThatCannotPushStopsBeforeCloning(t *testing.T) {
	t.Parallel()
	h := codeNodeWith(t, codeDeclaration{repos: importRepo, settings: importLimits},
		map[string]string{"ci": "a-ci-token-long-enough-for-tier-a"})
	var hist sourceHistory
	hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	_, url, _ := hist.load(t)

	out, _, err := cliIn(t, tokenEnviron("a-ci-token-long-enough-for-tier-a"), "code", "import", url, "web", "-url", h.base,
		"-git", hermeticGit(t))
	if err == nil || !strings.Contains(err.Error(), "not bound to a person") {
		t.Fatalf("an import with a token bound to nobody = %v, want the node's refusal", err)
	}
	if strings.Contains(out, "cloning") {
		t.Errorf("the import cloned the source before it learned the token cannot push:\n%s", out)
	}
}

// AN IMPORT REFUSES, BEFORE CLONING, REFS THE CODE HOST CANNOT HOLD: a branch
// whose name is past the 255 bytes a ref may be is git's to hold and never the
// code host's, so the source's advertisement alone refuses it, naming it.
func TestImportRefusesRefsTheCodeHostCannotHoldBeforeCloning(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	c := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	long := "refs/heads/" + strings.Repeat(strings.Repeat("a", 60)+"/", 4) + "end"
	hist.point(long, c)
	_, url, _ := hist.load(t)

	out, _, err := h.importInto(t, url)
	if err == nil || !strings.Contains(err.Error(), "nothing was imported") ||
		!strings.Contains(err.Error(), fmt.Sprintf("%q", long)) {
		t.Fatalf("an import of a %d-byte ref = %v, want a refusal naming it", len(long), err)
	}
	if strings.Contains(out, "cloning") {
		t.Errorf("the import cloned the source before refusing its refs:\n%s", out)
	}
}

// AN IMPORT REFUSES A BRANCH THE SEATS' NAMESPACE RESERVES — refs/heads/seats,
// and refs/heads/seats/<name> as a branch itself — before cloning, naming it
// for what it is rather than as a ref git's rules refuse, which it is not; a
// branch inside a seat's namespace is an ordinary branch, and kept.
func TestImportRefusesABranchTheSeatsNamespaceReserves(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	c := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	hist.point("refs/heads/seats/ana", c)
	_, url, _ := hist.load(t)

	out, _, err := h.importInto(t, url)
	if err == nil || !strings.Contains(err.Error(), "nothing was imported") ||
		!strings.Contains(err.Error(), `"refs/heads/seats/ana"`) ||
		!strings.Contains(err.Error(), "seats' own branches") || strings.Contains(err.Error(), "check-ref-format") {
		t.Fatalf("an import of a seat's namespace as a branch = %v, want a refusal naming it as reserved", err)
	}
	if strings.Contains(out, "cloning") {
		t.Errorf("the import cloned the source before refusing its refs:\n%s", out)
	}
}

// AN IMPORT REFUSES A SHALLOW SOURCE: its history stops at commits whose
// parents it lacks, which the code host refuses to be pushed — so it is said
// before anything is.
func TestImportRefusesAShallowSource(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	c := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	c = hist.commit("refs/heads/main", c, map[string]string{"a.txt": "a\n"})
	hist.commit("refs/heads/main", c, map[string]string{"b.txt": "b\n"})
	git, url, _ := hist.load(t)
	shallow := filepath.Join(t.TempDir(), "shallow.git")
	git.Run(t, "", "clone", "-q", "--bare", "--depth", "1", url, shallow)

	_, _, err := h.importInto(t, "file://"+shallow)
	if err == nil || !strings.Contains(err.Error(), "shallow") {
		t.Fatalf("an import of a shallow source = %v, want it refused as shallow", err)
	}
	if n := h.pushes(t); n != 0 {
		t.Errorf("a refused import pushed %d times", n)
	}
}

// A STEP'S SEARCH FINDS THE FURTHEST RANGE THAT FITS, asking few questions: for
// every place the answer can be and every place the search can start, the
// answer is the last index that fits — or one before the first where none
// does — in at most twice the logarithm of the history's length of estimates,
// which is what keeps a step's planning a handful of traversals on a history
// of a million commits.
func TestAStepsSearchFindsTheFurthestRangeThatFits(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 2, 3, 7, 64, 1000} {
		for _, last := range []int{-1, 0, n / 3, n / 2, n - 1} {
			for _, guess := range []int{0, 1, n / 4, n - 1, n + 5} {
				asked := 0
				got, err := furthest(0, n-1, guess, func(i int) (bool, error) {
					asked++
					if i < 0 || i >= n {
						t.Fatalf("asked about %d, outside [0, %d)", i, n)
					}
					return i <= last, nil
				})
				if err != nil || got != last {
					t.Errorf("n=%d last=%d guess=%d: furthest = %d (%v)", n, last, guess, got, err)
				}
				if bound := 2*bits.Len(uint(n)) + 2; asked > bound {
					t.Errorf("n=%d last=%d guess=%d: %d questions, more than %d", n, last, guess, asked, bound)
				}
			}
		}
	}
}

// A SEARCH WHOSE SMALLEST RANGE DOES NOT FIT ASKS TWO QUESTIONS: the guess and
// the smallest range, which every other range holds — a batch headed by a
// branch past the budget on its own, which galloping down from a full batch's
// guess asked about eleven times, each a traversal of that branch's whole
// history.
//
// Mutation: gallop down from the guess before asking about lo, and the search
// asks eleven.
func TestAStepsSearchAsksTheSmallestRangeOnceTheGuessFails(t *testing.T) {
	t.Parallel()
	asked := 0
	got, err := furthest(1, 1000, 1000, func(int) (bool, error) {
		asked++
		return false, nil
	})
	if err != nil || got != 0 || asked != 2 {
		t.Errorf("a search where nothing fits = %d (%v) in %d questions, want 0 in 2", got, err, asked)
	}
}

// GIT'S OUTPUT IS READ A LINE AT A TIME, however it arrives: a line split
// across writes is one line, a last line with no newline is handed on at the
// end, a line past its bound is refused, and a reader that has read all it
// needs stops git with no failure of its own.
func TestGitsOutputIsReadALineAtATime(t *testing.T) {
	t.Parallel()
	var lines []string
	w := &lineWriter{max: 16, fn: func(line []byte) error {
		lines = append(lines, string(line))
		return nil
	}}
	for _, piece := range []string{"one\ntw", "o\n", "", "thr", "ee\nfour"} {
		if _, err := w.Write([]byte(piece)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "two", "three", "four"}; !slices.Equal(lines, want) {
		t.Errorf("read %q, want %q", lines, want)
	}
	if _, err := w.Write([]byte(strings.Repeat("x", 17))); err == nil {
		t.Error("a line past its bound was read")
	}
	whole := &lineWriter{max: 16, fn: func([]byte) error { return nil }}
	if _, err := whole.Write([]byte(strings.Repeat("x", 17) + "\n")); err == nil {
		t.Error("a line past its bound, ended in the write that carried it, was read")
	}
	if _, err := whole.Write([]byte(strings.Repeat("x", 16) + "\n")); err != nil {
		t.Errorf("a line at its bound was refused: %v", err)
	}
	stop := &lineWriter{max: 16, fn: func([]byte) error { return errStopReading }}
	if _, err := stop.Write([]byte("a\nb\n")); !errors.Is(err, errStopReading) {
		t.Errorf("a reader that stopped answered %v", err)
	}
}

// runImport runs `crewlet code import url web` against h with git, h's own
// token the credential its environment holds, and a case's own say over the
// run first: tune changes the run prepareImport made — a bound the case scales
// down to what a test-sized history can reach.
func (h codeHost) runImport(t *testing.T, url, git string, tune func(*importer)) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	imp, dry, err := prepareImport(t.Context(), tokenEnviron(h.token),
		[]string{url, "web", h.cfg, "-git", git}, &out, &errOut)
	if err == nil {
		if tune != nil {
			tune(imp)
		}
		err = imp.run(t.Context(), dry)
	}
	return out.String(), errOut.String(), err
}

// AN IMPORT HOLDS EACH STEP TO WHAT ONE PUSH MAY STORE. The node refuses a
// push whose bundle — its pack, .idx and .rev — is past code.MaxPackBytes
// (pack_too_large), so a step whose stored bytes would pass it is cut shorter
// before it is sent, as one whose pack passes the budget is. That bound is a
// gibibyte and a quarter, which no case can reach, so the case lowers the
// import's own copy of it below the budget: every push the repository then
// records stored under it.
//
// Mutation: hold a step to its pack alone, and pushes past the bound land.
func TestImportHoldsEachStepToWhatOnePushMayStore(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	tip := 0
	for i := range 30 {
		tip = hist.commit("refs/heads/main", tip, map[string]string{
			fmt.Sprintf("data/%02d.bin", i): noise(uint64(700+i), 52<<10)})
	}
	git, url, bare := hist.load(t)
	const maxStored = 600 << 10

	out, errOut, err := h.runImport(t, url, hermeticGit(t), func(imp *importer) { imp.maxStored = maxStored })
	if err != nil {
		t.Fatalf("code import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	pushes := h.history(t)
	for i, p := range pushes {
		if p.PackBytes > maxStored {
			t.Errorf("push %d of %d stored %s, past the %s a push may store here", i+1, len(pushes),
				humanBytes(p.PackBytes), humanBytes(maxStored))
		}
	}
	if !strings.Contains(out, "cut shorter") {
		t.Errorf("no step was cut shorter for what it stores:\n%s", out)
	}
	wantSameRefs(t, h, git, bare)
}

// AN IMPORT STOPS BEFORE THE STEP THAT WOULD TAKE THE REPOSITORY PAST ITS LIMIT.
// A stepped history stores more than one pack of it — every step keeps whole
// the files its deltas cannot reach in the steps before it — so a source whose
// own pack is under code.limits.repo_max_bytes can still store past it: here
// under 1 MiB as one pack, against 1.5 MiB, in steps of about 0.9 MiB. The
// import adds up what each step stores and stops before the step that would
// cross, in its own words, with no push the node had to refuse. And a source
// whose one pack is already past the limit is refused before anything is
// pushed.
//
// Mutation: measure and push thin packs, and the node refuses a step
// repo_size — its completion stored what the import never counted.
func TestImportStopsBeforeTheStepThatWouldPassTheRepositoryLimit(t *testing.T) {
	t.Parallel()
	limited := func(repoMax int) codeDeclaration {
		return codeDeclaration{repos: importRepo, settings: importLimits + fmt.Sprintf("    repo_max_bytes: %d\n",
			repoMax)}
	}
	h := codeNodeWith(t, limited(3<<19), nil)
	var hist sourceHistory
	files := map[string]string{}
	for i := range 16 {
		files[fmt.Sprintf("f/%02d.bin", i)] = noise(uint64(1000+i), 40<<10)
	}
	_, url, _ := forgePacked(t, &hist, files, hist.commit("refs/heads/main", 0, maps.Clone(files)))

	out, errOut, err := h.importInto(t, url)
	if err == nil {
		t.Fatalf("an import storing past code.limits.repo_max_bytes succeeded:\n%s", out)
	}
	for _, want := range []string{"web would hold", "past code.limits.repo_max_bytes (1.5 MiB)",
		"the import stopped before step 2", "the 1 step of this run before it landed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v\nstdout:\n%s\nstderr:\n%s", want, err, out, errOut)
		}
	}
	pushes := h.history(t)
	if len(pushes) != 1 {
		t.Errorf("web records %d pushes, want the one step that fitted", len(pushes))
	}
	for _, p := range pushes {
		for _, c := range p.Commands {
			if c.Result != "ok" {
				t.Errorf("the node refused %s (%s): the import let through a step past the limit", c.Ref, c.Reason)
			}
		}
	}
	if held := h.liveBytes(t); held > 3<<19 {
		t.Errorf("web holds %s, past its limit", humanBytes(held))
	}

	// ONE PACK OF THE SOURCE PAST THE LIMIT — thirty incompressible commits,
	// 1.5 MiB as one pack, against 1 MiB: refused before any push.
	h.declare(t, limited(1<<20))
	var large sourceHistory
	tip := 0
	for i := range 30 {
		tip = large.commit("refs/heads/main", tip, map[string]string{
			fmt.Sprintf("data/%02d.bin", i): noise(uint64(1200+i), 52<<10)})
	}
	_, largeURL, _ := large.load(t)
	_, _, err = h.importInto(t, largeURL)
	if err == nil || !strings.Contains(err.Error(), "as one pack, past code.limits.repo_max_bytes (1.0 MiB)") {
		t.Errorf("an import of a source past the limit as one pack = %v, want it refused before any push", err)
	}
	if n := len(h.history(t)); n != 1 {
		t.Errorf("web records %d pushes after a refused import, want the 1 before it", n)
	}
}

// AN IMPORT CONTINUES A BRANCH THE REPOSITORY HOLDS AT A SECOND PARENT'S
// COMMIT. The repository holds main at a commit of side, which the source's
// main reaches only through a merge's second parent — a branch merged in since
// the last run — so the first-parent history past what the repository holds
// begins with three commits of main's own side, 0.9 MiB together, that do not
// descend from it. Every step is a fast-forward of main: the walk begins at
// the first commit that descends from the held one — the merge, which carries
// the three before it, under the push limit — and the history after the
// merge, larger than one step, goes in steps from there.
//
// Mutation: walk every first-parent commit the repository lacks, and the
// first step — two of the three, under the budget where all three are past it
// — is refused as non-fast-forward, on every run.
func TestImportContinuesABranchHeldAtASecondParentAncestor(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	base := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	ours := base
	for c := range 3 {
		files := map[string]string{}
		for i := range 6 {
			files[fmt.Sprintf("ours/%d-%d.bin", c, i)] = noise(uint64(800+c*10+i), 53<<10)
		}
		ours = hist.commit("refs/heads/main", ours, files)
	}
	side := hist.commit("refs/heads/side", base, map[string]string{"side.txt": "side\n"})
	tip := hist.commit("refs/heads/main", ours, map[string]string{"side.txt": "side\n"}, side)
	for i := range 30 {
		tip = hist.commit("refs/heads/main", tip, map[string]string{
			fmt.Sprintf("data/%02d.bin", i): noise(uint64(900+i), 52<<10)})
	}
	git, url, bare := hist.load(t)
	held := strings.TrimSpace(git.Run(t, "", "--git-dir="+bare, "rev-parse", "side"))
	person := codetest.PersonWithToken(t, h.token)
	if _, stderr, err := person.Git().Try(t, "", nil, "--git-dir="+bare, "push", "-q", h.base+"/git/web.git",
		held+":refs/heads/main"); err != nil {
		t.Fatalf("main at side's commit: %v\n%s", err, stderr)
	}

	out, errOut, err := h.importInto(t, url)
	if err != nil {
		t.Fatalf("an import of a main the repository holds at a merged commit: %v\nstdout:\n%s\nstderr:\n%s", err,
			out, errOut)
	}
	if !strings.Contains(out, "continuing refs/heads/main") || strings.Count(out, "\nimported step ") < 3 {
		t.Errorf("the import did not continue main in steps:\n%s", out)
	}
	wantSameRefs(t, h, git, bare)
}

// mergedHistory is a main whose second commit merges in an UNRELATED history
// past one step on its own — thirty commits of 52 KiB on a root of their own,
// a subtree added or a vendored repository — and goes on after it. With
// branch the source keeps refs/heads/vendor on that history's last commit; and
// it holds a branch a0, a1, … for each of descendants, one small commit on top
// of the merge each — sorting before vendor, and reaching the vendored history
// only THROUGH the merge. It answers the history's last commit and the merge.
func mergedHistory(t *testing.T, branch bool, descendants int) (git *gittest.Client, url, bare, vendor,
	merge string) {

	t.Helper()
	var hist sourceHistory
	root := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	other := 0
	for i := range 30 {
		other = hist.commit("refs/heads/vendor", other, map[string]string{
			fmt.Sprintf("vendor/%02d.bin", i): noise(uint64(1100+i), 52<<10)})
	}
	files := map[string]string{}
	for i := range 30 {
		files[fmt.Sprintf("vendor/%02d.bin", i)] = noise(uint64(1100+i), 52<<10)
	}
	merged := hist.commit("refs/heads/main", root, files, other)
	hist.commit("refs/heads/main", merged, map[string]string{"after.txt": "after\n"})
	for i := range descendants {
		hist.commit(fmt.Sprintf("refs/heads/a%d", i), merged, map[string]string{fmt.Sprintf("a%d.txt", i): "a\n"})
	}
	git, url, bare = hist.load(t)
	rev := func(name string) string {
		return strings.TrimSpace(git.Run(t, "", "--git-dir="+bare, "rev-parse", name))
	}
	vendor, merge = rev("vendor"), rev("main~1")
	if !branch {
		git.Run(t, "", "--git-dir="+bare, "update-ref", "-d", "refs/heads/vendor")
	}
	return git, url, bare, vendor, merge
}

// measuringGit is [hermeticGit] recording the first rev every pack-objects
// reads — the tip a step's measurement is built from, which the import names
// first — and measured counts the builds that named oid. git push runs its own
// pack-objects from git's exec path, never through this script, so only the
// import's measurements are counted.
func measuringGit(t *testing.T) (path string, measured func(oid string) int) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "measured")
	path = hermeticGitWith(t, "case \" $* \" in *\" pack-objects \"*)\n"+
		"  in=$(mktemp "+shellQuote(filepath.Join(dir, "revs.XXXXXX"))+") || exit 125\n"+
		"  cat > \"$in\" || exit 125\n"+
		"  head -n 1 \"$in\" >> "+shellQuote(log)+"\n"+
		"  exec < \"$in\"\n"+
		"  rm -f \"$in\";;\n"+
		"esac\n")
	return path, func(oid string) int {
		b, err := os.ReadFile(log)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the measurements: %v", err)
		}
		n := 0
		for line := range strings.Lines(string(b)) {
			if strings.TrimSpace(line) == oid {
				n++
			}
		}
		return n
	}
}

// AN IMPORT STEPS THE HISTORY A MERGE BRINGS IN FIRST. A first-parent walk of
// main meets the whole vendored history as the one commit that merges it,
// which no step of the walk can split — so the import steps that history, to
// the merge's other parent, on vendor — the branch of the source whose history
// it is — out of its turn, and the merge is then a step of what is left.
//
// Mutation: refuse the merge as one commit past the limit, and the import
// stops at it.
func TestImportStepsTheHistoryAMergeBringsInFirst(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	git, url, bare, vendor, merge := mergedHistory(t, true, 0)

	out, errOut, err := h.importInto(t, url)
	if err != nil {
		t.Fatalf("code import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if want := "stepping refs/heads/vendor first, to " + shortOID(vendor) + ": commit " + shortOID(merge) +
		" of refs/heads/main brings in its history"; !strings.Contains(out, want) {
		t.Errorf("the import does not say %q:\n%s", want, out)
	}
	wantSameRefs(t, h, git, bare)
}

// AN IMPORT STEPS A MERGE'S HISTORY ONCE, WHATEVER DESCENDS FROM THE MERGE.
// Five branches, a0 to a4, are one commit each on top of the merge: they sort
// before vendor and reach the vendored history only THROUGH the merge, so a
// walk of any of their own histories meets the same merge again — measured
// once more, at up to twice the budget, for each: at the 1 GiB ceiling a
// pack build of gibibytes for every branch created after the merge. What is
// stepped is the merge's other parent's OWN history, in which no walk can meet
// the merge: on vendor, where the source keeps it, and on a0 where it does not
// — every commit of it is an ancestor of a0's own — so a source with no branch
// on the merged-in history imports all the same. Either way the merge is
// measured ONCE, as the step past what one step carries — once its history
// has landed, main's next step goes past it to main's tip — the step's note
// names main's merge, and the five land in one batch once main is past it.
//
// Mutation: take the first branch that reaches the history whatever its path
// (no preference for one that reaches it without the merge), and vendor's
// history is stepped on a0 though vendor holds it; walk the chosen branch's
// own first-parent history rather than the merged-in parent's, and the merge
// is measured for every branch above it, each lands in a push of its own, and
// with no vendor the import is refused naming a4.
func TestImportStepsAMergesHistoryOnceWhateverDescendsFromTheMerge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		vendor  bool
		carrier string
	}{
		{name: "vendor kept", vendor: true, carrier: "refs/heads/vendor"},
		{name: "no branch on the history", vendor: false, carrier: "refs/heads/a0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := importNode(t)
			git, url, bare, vendor, merge := mergedHistory(t, tc.vendor, 5)
			path, measured := measuringGit(t)

			out, errOut, err := cli(t, "code", "import", url, "web", h.cfg, "-git", path)
			if err != nil {
				t.Fatalf("code import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
			}
			if want := "stepping " + tc.carrier + " first, to " + shortOID(vendor) + ": commit " +
				shortOID(merge) + " of refs/heads/main brings in its history"; !strings.Contains(out, want) {
				t.Errorf("the import does not say %q:\n%s", want, out)
			}
			if n := measured(merge); n != 1 {
				t.Errorf("the merge was measured %d times, want once — past one step:\n%s", n, out)
			}
			var batches []int
			for i, p := range h.history(t) {
				names := map[string]bool{}
				for _, c := range p.Commands {
					names[c.Ref] = true
				}
				if !names["refs/heads/a1"] && !names["refs/heads/a2"] && !names["refs/heads/a3"] &&
					!names["refs/heads/a4"] {
					continue
				}
				batches = append(batches, i+1)
				for k := range 5 {
					if ref := fmt.Sprintf("refs/heads/a%d", k); !names[ref] {
						t.Errorf("push %d moved %+v, which leaves %s of the branches above the merge out", i+1,
							p.Commands, ref)
					}
				}
			}
			if len(batches) != 1 {
				t.Errorf("the branches above the merge landed in pushes %v, want one batch:\n%s", batches, out)
			}
			wantSameRefs(t, h, git, bare)
		})
	}
}

// AN IMPORT REFUSES A MERGE NO BRANCH REACHES THE OTHER SIDE OF, WITH A REMEDY
// THAT WORKS. With no branch on the vendored history, nothing can push it in
// steps before the merge: the refusal — after ONE measurement of the merge —
// names the merge and the branch whose walk met it, the merged-in commit, what
// its history weighs and how to give it a branch — and doing what it says
// makes the same command import, the steps that landed kept.
func TestImportRefusesAMergeNoBranchReachesWithARemedyThatWorks(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	git, url, bare, vendor, merge := mergedHistory(t, false, 0)
	path, measured := measuringGit(t)

	out, _, err := cli(t, "code", "import", url, "web", h.cfg, "-git", path)
	if err == nil {
		t.Fatalf("an import of a merge past the limit no branch reaches succeeded:\n%s", out)
	}
	for _, want := range []string{"commit " + merge + " of refs/heads/main alone packs to ",
		"brings in the history of " + vendor, "no other branch of the source can carry it",
		"git clone --mirror", "branch import-" + shortOID(vendor) + " " + vendor} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if n := measured(merge); n != 1 {
		t.Errorf("the merge was measured %d times before its refusal, want once", n)
	}
	git.Run(t, "", "--git-dir="+bare, "branch", "import-"+shortOID(vendor), vendor)
	out, errOut, err := h.importInto(t, url)
	if err != nil {
		t.Fatalf("the import after its remedy: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	wantSameRefs(t, h, git, bare)
}

// AN IMPORT CARRIES A MERGE'S HISTORY ONLY ON A BRANCH IT CAN FAST-FORWARD
// THROUGH IT. Every step is a fast-forward, so a branch the repository holds
// carries the history only where it holds it at an ancestor of the merge's
// other parent: vendor, held part way along that history as an import that
// stopped there leaves it, goes on from where it is; a0, held at main's first
// commit — which the vendored history does not descend from — is passed over
// for a1, held at the history's second commit; and a1 is taken though a2,
// which the repository does not hold, is known able to carry it before a1's
// question is answered: the first in the source's order, however the
// questions asked side by side are answered.
//
// Mutation: take a held branch whatever it is held at, and a0's walk finds no
// commit of the history that descends from what it holds; take no branch the
// repository holds, and the history goes on a0 rather than on vendor, where
// the stopped import was carrying it; take the first candidate found able to
// carry it rather than the first in order, and it goes on a2.
func TestImportCarriesAMergesHistoryOnlyOnABranchItCanFastForward(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		vendor  bool
		held    map[string]string // ref → the source's revision the repository holds it at
		carrier string
	}{
		{name: "resumed part way along it", vendor: true, carrier: "refs/heads/vendor",
			held: map[string]string{"refs/heads/main": "main~2", "refs/heads/vendor": "vendor~25"}},
		{name: "held off it", vendor: false, carrier: "refs/heads/a1",
			held: map[string]string{"refs/heads/a0": "main~2", "refs/heads/a1": "vendor~28"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := importNode(t)
			git, url, bare, vendor, merge := mergedHistory(t, true, 3)
			var refspecs []string
			for ref, rev := range tc.held {
				refspecs = append(refspecs, strings.TrimSpace(git.Run(t, "", "--git-dir="+bare, "rev-parse",
					rev))+":"+ref)
			}
			if !tc.vendor {
				git.Run(t, "", "--git-dir="+bare, "update-ref", "-d", "refs/heads/vendor")
			}
			person := codetest.PersonWithToken(t, h.token)
			if _, stderr, err := person.Git().Try(t, "", nil, append([]string{"--git-dir=" + bare, "push", "-q",
				h.base + "/git/web.git"}, refspecs...)...); err != nil {
				t.Fatalf("the refs the repository holds: %v\n%s", err, stderr)
			}

			out, errOut, err := h.importInto(t, url)
			if err != nil {
				t.Fatalf("code import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
			}
			if want := "stepping " + tc.carrier + " first, to " + shortOID(vendor) + ": commit " +
				shortOID(merge) + " of refs/heads/main brings in its history"; !strings.Contains(out, want) {
				t.Errorf("the import does not say %q:\n%s", want, out)
			}
			wantSameRefs(t, h, git, bare)
		})
	}
}

// AN IMPORT STOPPED AT THE REPOSITORY LIMIT WHILE A BRANCH CARRIES A MERGE'S
// HISTORY COUNTS THAT BRANCH IN WHAT IS LEFT. a0, one commit of 600 KiB above
// the merge, carries the vendored history; the repository holds 950 KiB of a
// ref of its own, so the second step of that history would take it past
// code.limits.repo_max_bytes — 2.25 MiB, above the source's 2.1 MiB as one
// pack — and the import stops before it. What is left is that step's range,
// the merge and a0's own commit: a0 is out of the queue while it carries the
// history, and its 600 KiB was left out of the sum.
//
// Mutation: sum what is left without the carrier's own value, and the refusal
// says 679.9 KiB is left — the second step's range alone.
func TestImportCountsTheBranchCarryingAMergesHistoryInWhatIsLeft(t *testing.T) {
	t.Parallel()
	h := codeNodeWith(t, codeDeclaration{repos: importRepo, settings: importLimits +
		fmt.Sprintf("    repo_max_bytes: %d\n", 9<<18)}, nil)
	var theirs sourceHistory
	files := map[string]string{}
	for i := range 19 {
		files[fmt.Sprintf("theirs/%02d.bin", i)] = noise(uint64(1700+i), 50<<10)
	}
	theirs.commit("refs/heads/theirs", 0, files)
	_, _, theirsBare := theirs.load(t)
	person := codetest.PersonWithToken(t, h.token)
	if _, stderr, err := person.Git().Try(t, "", nil, "--git-dir="+theirsBare, "push", "-q",
		h.base+"/git/web.git", "refs/heads/theirs"); err != nil {
		t.Fatalf("the repository's own ref: %v\n%s", err, stderr)
	}
	git, _, bare, _, _ := mergedHistory(t, false, 1)
	a0 := map[string]string{}
	for i := range 12 {
		a0[fmt.Sprintf("a0/%02d.bin", i)] = noise(uint64(1800+i), 50<<10)
	}
	work := t.TempDir()
	git.Run(t, "", "clone", "-q", "--branch", "a0", bare, work)
	for path, content := range a0 {
		if err := os.MkdirAll(filepath.Join(work, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git.Run(t, work, "add", ".")
	git.Run(t, work, "commit", "-q", "-m", "a0's own")
	git.Run(t, work, "push", "-q", "origin", "a0")

	out, errOut, err := h.importInto(t, "file://"+bare)
	if err == nil {
		t.Fatalf("an import storing past code.limits.repo_max_bytes succeeded:\n%s", out)
	}
	if !strings.Contains(out, "stepping refs/heads/a0 first") ||
		!strings.Contains(err.Error(), "the import stopped before step 3") {
		t.Fatalf("the import did not stop during a0's carrying of the history: %v\nstdout:\n%s\nstderr:\n%s", err,
			out, errOut)
	}
	m := regexp.MustCompile(`holds about ([0-9.]+) (B|KiB|MiB) in the source's own packs`).FindStringSubmatch(
		err.Error())
	if m == nil {
		t.Fatalf("the refusal does not say what is left: %v", err)
	}
	left, _ := strconv.ParseFloat(m[1], 64)
	if m[2] != "MiB" || left < 1.1 {
		t.Errorf("the refusal says %s %s is left, want the second step's range, the merge and a0's 600 KiB — "+
			"more than 1.1 MiB: %v", m[1], m[2], err)
	}
}

// AN IMPORT STEPS A BRANCH MERGED BACK THAT MAIN WAS MERGED INTO SINCE. A
// long-lived feature branch, past one step, is merged into main; main goes on
// by more than a step; then main is merged into feature, to bring it up to
// date. Main's walk meets the first merge, and feature carries the history it
// brings in — to the commit main merged and no further: walked to its own
// value, feature's walk reached its own merge of main, which brings in what
// main's paused walk was about to push, more than a step that no branch still
// to be pushed reaches, and the import was refused there. Main goes on past
// its merge, and feature then goes on from where it was left, its merge of
// main a small step.
//
// Mutation: walk the carrier's own first-parent history rather than the
// merged-in parent's, and the import is refused at feature's merge of main.
func TestImportStepsABranchMergedBackThatMainWasMergedIntoSince(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	root := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	feature, featureFiles := root, map[string]string{}
	for i := range 30 {
		path, content := fmt.Sprintf("feature/%02d.bin", i), noise(uint64(1500+i), 52<<10)
		feature = hist.commit("refs/heads/feature", feature, map[string]string{path: content})
		featureFiles[path] = content
	}
	merged := feature
	tip := hist.commit("refs/heads/main", root, featureFiles, merged)
	mainFiles := map[string]string{}
	for i := range 24 {
		path, content := fmt.Sprintf("main/%02d.bin", i), noise(uint64(1600+i), 52<<10)
		tip = hist.commit("refs/heads/main", tip, map[string]string{path: content})
		mainFiles[path] = content
	}
	feature = hist.commit("refs/heads/feature", feature, mainFiles, tip)
	hist.commit("refs/heads/feature", feature, map[string]string{"feature.txt": "on\n"})
	hist.commit("refs/heads/main", tip, map[string]string{"after.txt": "after\n"})
	git, url, bare := hist.load(t)
	rev := func(name string) string {
		return strings.TrimSpace(git.Run(t, "", "--git-dir="+bare, "rev-parse", name))
	}

	out, errOut, err := h.importInto(t, url)
	if err != nil {
		t.Fatalf("code import: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if want := "stepping refs/heads/feature first, to " + shortOID(rev("feature~2")) + ": commit " +
		shortOID(rev("main~25")) + " of refs/heads/main brings in its history"; !strings.Contains(out, want) {
		t.Errorf("the import does not say %q:\n%s", want, out)
	}
	wantSameRefs(t, h, git, bare)
}

// A DRY RUN PLANS THE HISTORY A MERGE BRINGS IN FIRST, as the import would:
// the merge's estimate is past the push limit, so the plan steps vendor to the
// merge's other parent before it, says so as the import does, and plans no
// step past the limit.
//
// Mutation: plan a merge past the limit by its estimate as one step, and the
// plan marks it past the push limit where the import steps vendor first.
func TestADryRunPlansTheHistoryAMergeBringsInFirst(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	_, url, _, vendor, merge := mergedHistory(t, true, 0)

	out, errOut, err := h.importInto(t, url, "-dry-run")
	if err != nil {
		t.Fatalf("code import -dry-run: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if want := "  stepping refs/heads/vendor first, to " + shortOID(vendor) + ": commit " + shortOID(merge) +
		" of refs/heads/main brings in its history"; !strings.Contains(out, want) {
		t.Errorf("the plan does not say %q:\n%s", want, out)
	}
	if strings.Contains(out, "past the push limit") {
		t.Errorf("the plan holds a step past the push limit:\n%s", out)
	}
	if n := h.pushes(t); n != 0 {
		t.Errorf("a dry run pushed %d times", n)
	}
}

// operatorsGit is [hermeticGit] under push settings of the operator's own, in
// the environment as GIT_CONFIG_COUNT carries them — numbered after any the
// command set, as their shell would have exported them.
func operatorsGit(t *testing.T, settings ...string) string {
	t.Helper()
	more := "n=${GIT_CONFIG_COUNT:-0}\n"
	for i, kv := range settings {
		k, v, _ := strings.Cut(kv, "=")
		more += fmt.Sprintf("export \"GIT_CONFIG_KEY_$((n+%d))=%s\" \"GIT_CONFIG_VALUE_$((n+%d))=%s\"\n", i, k, i, v)
	}
	return hermeticGitWith(t, more+fmt.Sprintf("export GIT_CONFIG_COUNT=$((n+%d))\n", len(settings)))
}

// AN IMPORT PINS WHAT EACH STEP PUSHES, whatever the operator's push settings
// say: push.followTags would add the annotated tag on main's first commit to
// main's first step — a ref the plan never batched, objects it never measured
// — and push.gpgSign a push certificate the code host does not advertise,
// which git itself refuses. Each step moves exactly the refs it planned. (The
// operator's push options are TestAnImportOpensNoChange's.)
//
// Mutation: push without --no-follow-tags, and the first step carries the tag.
func TestImportPinsWhatEachStepPushes(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	tip := 0
	first := 0
	for i := range 25 {
		tip = hist.commit("refs/heads/main", tip, map[string]string{
			fmt.Sprintf("data/%02d.bin", i): noise(uint64(1300+i), 52<<10)})
		if i == 0 {
			first = tip
		}
	}
	hist.tag("v1", first, "the first commit")
	git, url, bare := hist.load(t)

	out, errOut, err := cli(t, "code", "import", url, "web", h.cfg, "-git", operatorsGit(t,
		"push.followTags=true", "push.gpgSign=true", "push.negotiate=true"))
	if err != nil {
		t.Fatalf("an import under the operator's push settings: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	pushes := h.history(t)
	if len(pushes) < 3 {
		t.Fatalf("web records %d pushes, want main's two steps and the tag's", len(pushes))
	}
	for i, p := range pushes[:len(pushes)-1] {
		if len(p.Commands) != 1 || p.Commands[0].Ref != "refs/heads/main" {
			t.Errorf("step %d moved %+v, want main alone", i+1, p.Commands)
		}
	}
	if last := pushes[len(pushes)-1]; len(last.Commands) != 1 || last.Commands[0].Ref != "refs/tags/v1" {
		t.Errorf("the last step moved %+v, want the tag", last.Commands)
	}
	wantSameRefs(t, h, git, bare)
}

// AN IMPORT OPENS NO CHANGE, whatever push options the operator keeps: a
// `change` in push.pushOption asks the code host to open a change from a push
// of one branch, which the step that brings topic is, and an import's steps
// are history, never a proposal — so every step pushes with the options
// cleared.
//
// Mutation: drop push.pushOption= from importPushConfig, and topic's step
// opens web!1.
func TestAnImportOpensNoChange(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	tip := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "one\n"})
	hist.commit("refs/heads/topic", tip, map[string]string{"TOPIC.md": "two\n"})
	git, url, bare := hist.load(t)

	out, errOut, err := cli(t, "code", "import", url, "web", h.cfg, "-git", operatorsGit(t,
		"push.pushOption=change"))
	if err != nil {
		t.Fatalf("an import under a push option asking for a change: %v\nstdout:\n%s\nstderr:\n%s", err, out,
			errOut)
	}
	wantSameRefs(t, h, git, bare)
	seam, ok := engine.OperatorCode(h.node.e)
	if !ok {
		t.Fatal("the node hosts no code")
	}
	read, err := seam.Read(t.Context(), code.ReadQuery{Kind: code.ReadChanges, Repo: "web",
		Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "ops", Seat: "founder"},
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}})
	if err != nil {
		t.Fatalf("list web's changes: %v", err)
	}
	if len(read.Changes) != 0 {
		t.Fatalf("an import opened %d changes: %+v", len(read.Changes), read.Changes)
	}
}

// AN IMPORT REPORTS A REFUSED STEP WITHOUT WAITING FOR THE NEXT ONE'S
// MEASUREMENT. The next step is measured while one is pushed; a push the node
// refuses at once — main, which code.repos freezes — ends that measurement
// rather than waiting for it, here held for two minutes, and nothing of it is
// left running.
//
// Mutation: read the push's outcome only once the next step is planned, and
// the refusal waits the measurement out.
func TestImportReportsARefusedStepWithoutWaitingForTheNextMeasurement(t *testing.T) {
	t.Parallel()
	frozen := importRepo + "      protect:\n        - branch: main\n          direct_push: none\n"
	h := codeNodeWith(t, codeDeclaration{repos: frozen, settings: importLimits}, nil)
	var hist sourceHistory
	c := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	hist.commit("refs/heads/other", c, map[string]string{"other.txt": "other\n"})
	_, url, _ := hist.load(t)
	// THE SECOND MEASUREMENT IS HELD: the first is main's step, the second
	// the next step's, planned while main is pushed.
	count := filepath.Join(t.TempDir(), "measurements")
	held := hermeticGitWith(t, "case \" $* \" in *\" pack-objects \"*)\n"+
		"  echo x >> "+shellQuote(count)+"\n"+
		"  if [ $(( $(wc -l < "+shellQuote(count)+") )) -ge 2 ]; then sleep 120; fi;;\n"+
		"esac\n")

	start := time.Now()
	out, errOut, err := cli(t, "code", "import", url, "web", h.cfg, "-git", held)
	if err == nil || !strings.Contains(err.Error(), "the import stopped at step 1") ||
		!strings.Contains(errOut, "protected_direct") {
		t.Fatalf("an import of a frozen main = %v\nstdout:\n%s\nstderr:\n%s; want it stopped at step 1", err, out,
			errOut)
	}
	if took := time.Since(start); took > time.Minute {
		t.Errorf("the refusal took %s: it waited for the next step's measurement", took)
	}
}

// AN IMPORT'S PROBE QUOTES GIT, AND NEVER SAYS WHAT GIT DID NOT. A node that
// serves no git routes — its roles leave git out — answers the probe's push
// with git's own "not found": the import stops before downloading anything
// and says what it asked and what git answered, never that the token was
// refused.
//
// Mutation: read every failure of the probe as the token's refusal.
func TestImportProbeQuotesGitAndNeverBlamesTheToken(t *testing.T) {
	t.Parallel()
	h := codeNodeAmended(t, codeDeclaration{repos: importRepo, settings: importLimits}, nil,
		func(b *config.Bootstrap) { b.Node.Roles = []string{"data", "ingress", "seats", "workers"} })
	var hist sourceHistory
	hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	_, url, _ := hist.load(t)

	out, _, err := h.importInto(t, url)
	if err == nil || !strings.Contains(err.Error(), "before downloading the source, the import asks") ||
		!strings.Contains(err.Error(), "git answered") || strings.Contains(err.Error(), "token") {
		t.Fatalf("an import through a node serving no git = %v, want git's answer quoted and no token blamed", err)
	}
	if strings.Contains(out, "cloning") {
		t.Errorf("the import cloned the source though the node takes no push:\n%s", out)
	}
}

// AN IMPORT INTO A COMPANY THAT HOSTS NO CODE SAYS SO, AND ONLY SO: the node
// answers not_found with not_hosted, where declaring a repository in
// code.repos — the remedy of a repository nobody declared — changes nothing.
//
// Mutation: hint code.repos at every not_found, and the case goes red.
func TestImportIntoACompanyThatHostsNoCodeSaysSo(t *testing.T) {
	t.Parallel()
	const token = "a-code-token-long-enough-for-tier-a"
	n := bootUnconfiguredWith(t, token, nil)
	n.reconcile()
	if err := n.settle(n.put(companyYAML)); err != nil {
		t.Fatalf("the company: %v", err)
	}
	var hist sourceHistory
	hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	_, url, _ := hist.load(t)

	_, _, err := cliIn(t, tokenEnviron(token), "code", "import", url, "web", "-url", n.base, "-git", hermeticGit(t))
	if err == nil || !strings.Contains(err.Error(), "code.native is not set") ||
		strings.Contains(err.Error(), "code.repos") {
		t.Errorf("an import into a company hosting no code = %v, want the node's not_hosted and no code.repos", err)
	}
}

// AN IMPORT REFUSES THE FIRST DIVERGED BRANCH IN THE SOURCE'S ORDER, however
// its questions run: the repository holds twelve branches at earlier commits,
// two of them at a commit their branch does not descend from, and the
// refusal names the first — whatever order the ancestry questions, asked side
// by side, are answered in — and nothing is pushed.
//
// Mutation: skip the questions before the first diverged branch found rather
// than after it, and an earlier branch is named.
func TestImportRefusesTheFirstDivergedBranchInTheSourcesOrder(t *testing.T) {
	t.Parallel()
	h := importNode(t)
	var hist sourceHistory
	base := hist.commit("refs/heads/main", 0, map[string]string{"README.md": "# web\n"})
	earlier := map[string]int{}
	for i := range 12 {
		ref := fmt.Sprintf("refs/heads/b%02d", i)
		earlier[ref] = hist.commit(ref, base, map[string]string{fmt.Sprintf("b%02d.txt", i): "1\n"})
		hist.commit(ref, earlier[ref], map[string]string{fmt.Sprintf("b%02d.txt", i): "2\n"})
	}
	git, url, bare := hist.load(t)
	at := func(ref string) string {
		return strings.TrimSpace(git.Run(t, "", "--git-dir="+bare, "rev-parse", ref+"~1"))
	}
	refspecs := []string{}
	for i := range 12 {
		ref := fmt.Sprintf("refs/heads/b%02d", i)
		value := at(ref)
		switch i {
		case 4, 9:
			// Its neighbour's earlier commit: one the source holds, and
			// this branch does not descend from.
			value = at(fmt.Sprintf("refs/heads/b%02d", i+1))
		}
		refspecs = append(refspecs, value+":"+ref)
	}
	person := codetest.PersonWithToken(t, h.token)
	if _, stderr, err := person.Git().Try(t, "", nil, append([]string{"--git-dir=" + bare, "push", "-q",
		h.base + "/git/web.git"}, refspecs...)...); err != nil {
		t.Fatalf("the earlier branches: %v\n%s", err, stderr)
	}

	_, _, err := h.importInto(t, url)
	if err == nil || !strings.Contains(err.Error(), "refs/heads/b04 is at") ||
		!strings.Contains(err.Error(), "not a resumed import") {
		t.Fatalf("an import into a repository with b04 and b09 diverged = %v, want b04 named", err)
	}
	if n := len(h.history(t)); n != 1 {
		t.Errorf("web records %d pushes, want the one that was there before the import", n)
	}
}
