package host_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/gitref"
)

// dlCase is a solo fleet holding RepoName under settings, its main pushed by
// ceo with one README: the case, ceo, ceo's work tree and main's commit.
type dlCase struct {
	*gitlettest.Case
	p    *gitlettest.Person
	work string
	head string
}

func newDlCase(t *testing.T, o gitlettest.SoloOptions, settings gitlet.RepoSettings) *dlCase {
	t.Helper()
	return seeded(t, newCase(t, o, settings))
}

// seeded is c with ceo's first commit pushed.
func seeded(t *testing.T, c *gitlettest.Case) *dlCase {
	t.Helper()
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	return &dlCase{Case: c, p: p, work: work, head: head}
}

// git runs ceo's git in the work tree, its standard input in.
func (d *dlCase) git(in string, args ...string) string {
	d.T.Helper()
	out, stderr, err := d.p.Git().Try(d.T, d.work, strings.NewReader(in), args...)
	if err != nil {
		d.T.Fatalf("git %q: %v\n%s", args, err, stderr)
	}
	return out
}

// blob writes content as a blob, converted by nothing, and answers its id.
func (d *dlCase) blob(content string) string {
	d.T.Helper()
	return strings.TrimSpace(d.git(content, "hash-object", "-w", "--stdin"))
}

// entry is one path of a commit built by commit: its mode and object.
type entry struct{ mode, id string }

// file is a regular file's entry holding content.
func (d *dlCase) file(content string) entry { return entry{"100644", d.blob(content)} }

// commit is a commit of exactly entries, no work tree's attributes applied,
// pushed to a branch of its own so the repository holds it.
func (d *dlCase) commit(entries map[string]entry) string {
	d.T.Helper()
	return d.commitOn("", entries)
}

// commitOn is commit with parent as its parent ("" for none).
func (d *dlCase) commitOn(parent string, entries map[string]entry) string {
	d.T.Helper()
	var info strings.Builder
	for path, e := range entries {
		info.WriteString(e.mode + " " + e.id + "\t" + path + "\x00")
	}
	g := d.p.Git().Env("GIT_INDEX_FILE=" + filepath.Join(d.T.TempDir(), "index"))
	run := func(in string, args ...string) string {
		out, stderr, err := g.Try(d.T, d.work, strings.NewReader(in), args...)
		if err != nil {
			d.T.Fatalf("git %q: %v\n%s", args, err, stderr)
		}
		return strings.TrimSpace(out)
	}
	run(info.String(), "update-index", "-z", "--index-info")
	args := []string{"commit-tree", "-m", "built", run("", "write-tree", "--missing-ok")}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit := run("", args...)
	d.MustPush(d.p, d.work, commit+":refs/heads/b-"+commit[:12])
	return commit
}

// files is a commit of each path holding its content.
func (d *dlCase) files(files map[string]string) string {
	d.T.Helper()
	entries := map[string]entry{}
	for path, content := range files {
		entries[path] = d.file(content)
	}
	return d.commit(entries)
}

// host is node 0's host.
func (d *dlCase) host() *host.Server { return d.Fleet.Nodes()[0].Host() }

// rawPath is a raw download's path of RepoName, each extra a key=value pair.
func rawPath(rev, path string, extra ...string) string {
	q := "rev=" + rev + "&path=" + url.QueryEscape(path)
	for _, e := range extra {
		q += "&" + e
	}
	return "/git/" + gitlettest.RepoName + "/-/raw?" + q
}

// zipPath is an archive download's path of RepoName.
func zipPath(rev string) string { return "/git/" + gitlettest.RepoName + "/-/archive.zip?rev=" + rev }

// prefixOf is the folder a ZIP of commit holds its files in.
func prefixOf(commit string) string {
	return gitlettest.RepoName + "-" + commit[:gitlet.MinAbbrevDigits] + "/"
}

// answer is one download's answer: its response, what arrived, and the read's
// error where the answer was cut.
type answer struct {
	resp *http.Response
	body []byte
	cut  error
}

// ask sends method target to node 0 as who's token, with headers (name, value
// pairs), and reads the answer whole.
func (d *dlCase) ask(method, target, who string, headers ...string) answer {
	d.T.Helper()
	resp := d.send(method, target, who, headers...)
	defer func() { _ = resp.Body.Close() }()
	body, cut := io.ReadAll(resp.Body)
	return answer{resp: resp, body: body, cut: cut}
}

// send is ask's request, its answer's body left to the caller.
func (d *dlCase) send(method, target, who string, headers ...string) *http.Response {
	d.T.Helper()
	n := d.Fleet.Nodes()[0]
	req, err := http.NewRequestWithContext(d.Ctx, method, n.URL()+target, nil)
	if err != nil {
		d.T.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+d.Fleet.Token(d.T, who))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := n.Client().Do(req)
	if err != nil {
		d.T.Fatalf("%s %s: %v", method, target, err)
	}
	return resp
}

// get is a GET as ceo that must answer 200, read whole.
func (d *dlCase) get(target string, headers ...string) answer {
	d.T.Helper()
	a := d.ask(http.MethodGet, target, "ceo", headers...)
	if a.resp.StatusCode != http.StatusOK || a.cut != nil {
		d.T.Fatalf("GET %s answered %d %q (cut %v), want 200", target, a.resp.StatusCode, short(string(a.body)),
			a.cut)
	}
	return a
}

// assertInert holds h to a download's headers: the inert policy and the
// browser headers beside it, kept out of every cache.
func assertInert(t *testing.T, what string, h http.Header) {
	t.Helper()
	for name, want := range map[string]string{"Content-Security-Policy": pagepolicy.Inert,
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin", "Cache-Control": "private, no-store"} {
		if got := h.Get(name); got != want {
			t.Errorf("%s: %s = %q, want %q", what, name, got, want)
		}
	}
	for name, values := range h {
		for _, v := range values {
			if strings.ContainsAny(v, "\r\n") {
				t.Errorf("%s: header %s carries a line break: %q", what, name, v)
			}
		}
	}
}

// assertLine holds a to a refusal of status in one text/plain line.
func assertLine(t *testing.T, a answer, status int, what string) {
	t.Helper()
	if a.resp.StatusCode != status || a.resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" ||
		strings.Count(string(a.body), "\n") != 1 || !strings.HasSuffix(string(a.body), "\n") {
		t.Errorf("%s answered %d %q (%s), want %d in one text/plain line", what, a.resp.StatusCode, a.body,
			a.resp.Header.Get("Content-Type"), status)
	}
}

// zipOf reads body as a ZIP, failing the case where it is none.
func zipOf(t *testing.T, body []byte) *zip.Reader {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("the answer is no ZIP: %v", err)
	}
	return r
}

// zipMeasure is what a ZIP is made of, read from the ZIP itself: the oracle
// the server's own measure of the commit must equal.
func zipMeasure(r *zip.Reader) host.ArchiveMeasure {
	m := host.ArchiveMeasure{Entries: int64(len(r.File))}
	for _, f := range r.File {
		m.Names += int64(len(f.Name))
		m.Bytes += int64(f.UncompressedSize64)
	}
	return m
}

// unitSettings are gitlettest.Settings with the repository visible to its
// unit alone, whose reads are recorded.
func unitSettings() gitlet.RepoSettings {
	s := gitlettest.Settings()
	s.Visibility = gitlet.VisibilityUnit
	return s
}

// random is n bytes no compressor shrinks.
func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return string(b)
}

// A REPOSITORY'S BYTES ARE SERVED INERT (ADR-0153's gate): every file, an
// HTML page, an SVG, a script, a stylesheet and a PDF among them, is its bytes
// exactly as text/plain inline where it is text (git's own rule: no NUL in its
// first 8 000 bytes) and as an attachment of application/octet-stream
// otherwise, never a type read from its name, and every name round-trips
// through its Content-Disposition. Asked to download, every file is an
// attachment; the ZIP carries the same inert headers.
//
// Mutation: type a file by its extension, drop sandbox, or build the
// disposition by concatenation (quote".txt breaks it).
func TestADownloadIsServedInert(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"index.html": "<script>alert(1)</script>\n", "logo.svg": "<svg onload=alert(1)></svg>\n",
		"app.js": "alert(1)\n", "style.css": "body{}\n", "page.xhtml": "<html/>\n", "doc.pdf": "%PDF-1.4\n",
		"data.bin": "head\x00tail", "late.bin": strings.Repeat("a", 8000) + "\x00tail", "empty.txt": "",
		`quote".txt`: "q\n", "caf\xe9.txt": "latin\n", "nl\nname.txt": "nl\n", "a\tb.txt": "tab\n",
		"plus+and&.txt": "query\n",
	}
	d := newDlCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	commit := d.files(files)
	for name, content := range files {
		blob := strings.TrimSpace(d.git("", "rev-parse", commit+":"+name))
		for _, attach := range []bool{false, true} {
			var extra []string
			if attach {
				extra = []string{"download=1"}
			}
			a := d.get(rawPath(commit, name, extra...))
			what := strconv.Quote(name)
			if string(a.body) != content {
				t.Errorf("%s: body %q, want the blob's bytes", what, short(string(a.body)))
			}
			h := a.resp.Header
			assertInert(t, what, h)
			if h.Get("Content-Length") != strconv.Itoa(len(content)) || h.Get("ETag") != `"`+blob+`"` {
				t.Errorf("%s: Content-Length %q, ETag %q, want %d and the blob %s", what,
					h.Get("Content-Length"), h.Get("ETag"), len(content), blob)
			}
			wantType, wantDisp := "text/plain; charset=utf-8", "inline"
			if name == "data.bin" || attach {
				wantType, wantDisp = "application/octet-stream", "attachment"
			}
			if got := h.Get("Content-Type"); got != wantType {
				t.Errorf("%s (download %v): Content-Type %q, want %q", what, attach, got, wantType)
			}
			disp, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
			if err != nil || disp != wantDisp || params["filename"] != name {
				t.Errorf("%s (download %v): Content-Disposition %q reads as %q %q (%v), want %s naming it",
					what, attach, h.Get("Content-Disposition"), disp, params["filename"], err, wantDisp)
			}
		}
	}
	z := d.get(zipPath(commit))
	assertInert(t, "the ZIP", z.resp.Header)
	if got := z.resp.Header.Get("Content-Type"); got != "application/zip" {
		t.Errorf("the ZIP's Content-Type is %q", got)
	}
	want := gitlettest.RepoName + "-" + commit[:gitlet.MinAbbrevDigits] + ".zip"
	if disp, params, err := mime.ParseMediaType(z.resp.Header.Get("Content-Disposition")); err != nil ||
		disp != "attachment" || params["filename"] != want {
		t.Errorf("the ZIP's Content-Disposition is %q, want an attachment named %s",
			z.resp.Header.Get("Content-Disposition"), want)
	}
}

// A RAW FILE ANSWERS ITS ETAG: If-None-Match naming its blob (weak, or `*`)
// is 304 with the inert headers and no body, running no `cat-file blob`;
// another tag is the file. A 304 of a unit repository is one read of 0 bytes,
// since it confirms the file's content; a company repository's is none.
//
// Mutation: record only a download whose body began.
func TestARawFileAnswersItsETag(t *testing.T) {
	t.Parallel()
	for _, settings := range []gitlet.RepoSettings{unitSettings(), gitlettest.Settings()} {
		d := newDlCase(t, gitlettest.SoloOptions{}, settings)
		f := d.Fleet.(*gitlettest.SoloFleet)
		blob := strings.TrimSpace(d.git("", "rev-parse", d.head+":README.md"))
		ran := host.RecordGit(d.host())
		before := len(f.Fetched())
		for _, tag := range []string{`"` + blob + `"`, `W/"` + blob + `"`, "*"} {
			a := d.ask(http.MethodGet, rawPath(d.head, "README.md"), "ceo", "If-None-Match", tag)
			if a.resp.StatusCode != http.StatusNotModified || len(a.body) != 0 {
				t.Fatalf("If-None-Match %s answered %d %q, want 304 and no body", tag, a.resp.StatusCode, a.body)
			}
			assertInert(t, "a 304", a.resp.Header)
			if a.resp.Header.Get("ETag") != `"`+blob+`"` {
				t.Errorf("a 304's ETag is %q", a.resp.Header.Get("ETag"))
			}
		}
		for _, c := range ran() {
			if slices.Equal(c.Args[:min(2, len(c.Args))], []string{"cat-file", "blob"}) {
				t.Errorf("a 304 ran %q", c.Args)
			}
		}
		if a := d.get(rawPath(d.head, "README.md"), "If-None-Match", `"`+randomOID()+`"`); string(a.body) != "one\n" {
			t.Errorf("another tag answered %q, want the file", a.body)
		}
		if settings.Visibility == gitlet.VisibilityUnit {
			// A 200's record is filed after its last byte reaches the client.
			eventually(t, "the read's record", func() bool { return len(f.Fetched()) >= before+4 })
		}
		recorded := f.Fetched()[before:]
		if settings.Visibility != gitlet.VisibilityUnit {
			if len(recorded) != 0 {
				t.Errorf("a company repository's downloads were recorded: %+v", recorded)
			}
			continue
		}
		if len(recorded) != 4 {
			t.Fatalf("a unit repository's three 304s and a read were recorded %+v, want four reads", recorded)
		}
		for i, r := range recorded {
			want := int64(0)
			if i == 3 {
				want = 4
			}
			if r.Kind != types.FetchedRaw || r.Via != types.FetchedViaGit || r.Reads != 1 || r.Bytes != want {
				t.Errorf("read %d is recorded %+v, want one raw read of %d bytes", i, r, want)
			}
		}
	}
}

// A DOWNLOAD NAMES A COMMIT AND A PATH EXACTLY: every malformed query is 400,
// every name of nothing 404 (a commit the copy lacks only after one sync at the
// log's head), HEAD 405 with no git run, each one text/plain line. A tail
// spelled with an escape, which the guard reads as a push, is the mux's 404.
//
// Mutation: accept an abbreviation, let HEAD run git, drop the lookup's
// commit arm (the held submodule is no 404), or serve an escaped tail.
func TestADownloadNamesACommitAndAPathExactly(t *testing.T) {
	t.Parallel()
	e := &recording{}
	d := newDlCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: e.wrap}}, gitlettest.Settings())
	readme := d.file("one\n")
	subs := d.commit(map[string]entry{"README.md": readme, "dir/f": d.file("f\n"),
		"held": {"160000", d.head}, "lost": {"160000", randomOID()}})
	tree := strings.TrimSpace(d.git("", "rev-parse", subs+"^{tree}"))
	ran := host.RecordGit(d.host())
	base := "/git/" + gitlettest.RepoName + "/-/raw?"
	for _, q := range []string{
		"rev=" + subs + "&path=README.md&color=1", "rev=" + subs + "&rev=" + subs + "&path=README.md",
		"rev=" + subs + "&path=a%zz", "rev=" + subs[:12] + "&path=README.md",
		"rev=" + strings.ToUpper(subs) + "&path=README.md", "rev=" + gitlet.ZeroOID + "&path=README.md",
		"rev=" + tree + "&path=README.md", "rev=" + readme.id + "&path=README.md", "rev=" + subs + "&path=",
		"rev=" + subs + "&path=..%2Fx", "rev=" + subs + "&path=%2Fx", "rev=" + subs + "&path=a%2F%2Fb",
		"rev=" + subs + "&path=a%00b", "rev=" + subs + "&path=" + strings.Repeat("a", 4097),
		"rev=" + subs + "&path=README.md&download=2", "path=README.md",
	} {
		assertLine(t, d.ask(http.MethodGet, base+q, "ceo"), http.StatusBadRequest, base+q)
	}
	for _, q := range []string{"rev=" + subs + "&path=x", "rev=" + subs + "&download=1", ""} {
		target := "/git/" + gitlettest.RepoName + "/-/archive.zip?" + q
		assertLine(t, d.ask(http.MethodGet, target, "ceo"), http.StatusBadRequest, target)
	}
	short := subs[:gitlet.MinAbbrevDigits]
	lacks := randomOID()
	linear := e.linearizable()
	for target, says := range map[string]string{
		"/git/nope/-/raw?rev=" + subs + "&path=README.md": `"nope" not found`,
		rawPath(lacks, "README.md"):                       "no commit " + lacks,
		zipPath(lacks):                                    "no commit " + lacks,
		rawPath(subs, "no/such"):                          `no file at "no/such" in ` + short,
		rawPath(subs, "dir"):                              `"dir" in ` + short + " is a directory",
		rawPath(subs, "held"):                             `"held" in ` + short + " is a submodule",
		rawPath(subs, "lost"):                             `"lost" in ` + short + " is a submodule",
	} {
		a := d.ask(http.MethodGet, target, "ceo")
		assertLine(t, a, http.StatusNotFound, target)
		if !strings.Contains(string(a.body), says) {
			t.Errorf("%s said %q, want it to say %q", target, a.body, says)
		}
	}
	if got := e.linearizable() - linear; got != 2 {
		t.Errorf("the lookups paid %d syncs at the log's head, want one for each request naming a commit "+
			"the copy lacked", got)
	}
	before := len(ran())
	for _, target := range []string{rawPath(subs, "README.md"), zipPath(subs)} {
		a := d.ask(http.MethodHead, target, "ceo")
		if a.resp.StatusCode != http.StatusMethodNotAllowed || a.resp.Header.Get("Allow") != http.MethodGet {
			t.Errorf("HEAD %s answered %d Allow %q, want 405 Allow GET", target, a.resp.StatusCode,
				a.resp.Header.Get("Allow"))
		}
	}
	if after := ran(); len(after) != before {
		t.Errorf("a HEAD ran git: %q", after[before].Args)
	}
	repo := "/git/" + gitlettest.RepoName
	for _, target := range []string{repo + "/-/r%61w?rev=" + subs + "&path=README.md",
		repo + "/%2D/raw?rev=" + subs + "&path=README.md", repo + "/-/archive%2Ezip?rev=" + subs} {
		a := d.ask(http.MethodGet, target, "ceo")
		if a.resp.StatusCode != http.StatusNotFound || !strings.Contains(string(a.body), `"no_route"`) {
			t.Errorf("GET %s answered %d %q, want the mux's 404 no_route", target, a.resp.StatusCode, a.body)
		}
	}
	if after := ran(); len(after) != before {
		t.Errorf("an escaped tail ran git: %q", after[before].Args)
	}
}

// A DOWNLOAD REACHES GIT ONLY AS DATA: files whose names are options, refs or
// revision syntax are read raw and listed in the ZIP, every process recorded
// fits one of the four templates, and a path reaches git only on the lookup's
// standard input.
//
// Mutation: hand the path to cat-file as <rev>:<path> in its arguments, or add
// --output to the archive.
func TestADownloadReachesGitOnlyAsData(t *testing.T) {
	t.Parallel()
	names := []string{"--output=x/f", "-o", "-r", "a:b", "nl\nname", "--prefix=evil", "HEAD", "@{-1}"}
	files := map[string]string{}
	for _, n := range names {
		files[n] = "content of " + n + "\n"
	}
	d := newDlCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	commit := d.files(files)
	ins := host.RecordGitInputs(d.host())
	for _, n := range names {
		if a := d.get(rawPath(commit, n)); string(a.body) != files[n] {
			t.Errorf("%q read %q", n, a.body)
		}
	}
	prefix := prefixOf(commit)
	var listed []string
	for _, f := range zipOf(t, d.get(zipPath(commit)).body).File {
		listed = append(listed, f.Name)
	}
	for _, n := range names {
		if !slices.Contains(listed, prefix+n) {
			t.Errorf("the ZIP lists %q, without %q", listed, prefix+n)
		}
	}
	fed := map[string]bool{}
	for _, in := range ins() {
		switch a := in.Args; {
		case slices.Equal(a, []string{"cat-file", "--batch-check", "-Z"}):
			for rec := range strings.SplitSeq(strings.TrimSuffix(string(in.Stdin), "\x00"), "\x00") {
				if path, ok := strings.CutPrefix(rec, commit+":"); ok {
					fed[path] = true
				}
			}
			continue
		case len(a) == 3 && a[0] == "cat-file" && a[1] == "blob" && gitref.ValidOID(a[2]):
		case slices.Equal(a, []string{"ls-tree", "-r", "-t", "-l", "-z", commit}):
		case slices.Equal(a, []string{"archive", "--worktree-attributes", "--format=zip", "--prefix=" + prefix, commit}):
		default:
			t.Errorf("a download ran git %q, which fits no template", a)
		}
		if len(in.Stdin) > 0 {
			t.Errorf("git %q was fed %q", in.Args, in.Stdin)
		}
	}
	for _, n := range names {
		if !fed[n] {
			t.Errorf("path %q never reached the lookup's standard input", n)
		}
	}
}

// AN ARCHIVE IS GIT'S OWN ZIP of the commit's files as stored: byte for byte
// what `git archive --worktree-attributes --format=zip --prefix=<repo>-<short>/`
// writes in a bare clone on the same git, whatever the commit's .gitattributes
// say (export-ignore, export-subst, ident, eol, working-tree-encoding, a
// filter), and whatever the default branch's say of a commit without them.
// It lists the prefix's directory, every directory, a link as a link and a
// submodule as an empty directory, its comment the commit's id.
//
// Mutation: drop --worktree-attributes (every converted file changes), or the
// prefix.
func TestAnArchiveIsGitsOwnZip(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		".gitattributes": "ignored.txt export-ignore\nsubst.txt export-subst\nident.txt ident\n" +
			"eol.txt text eol=crlf\nenc.txt working-tree-encoding=UTF-16LE\nfilter.txt filter=evil\n",
		"ignored.txt": "kept\n", "subst.txt": "$Format:%H$\n", "ident.txt": "$Id$\n", "eol.txt": "a\nb\n",
		"enc.txt": "plain\n", "filter.txt": "raw\n", "dir/f": "f\n",
	}
	d := newDlCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	entries := map[string]entry{"link": {"120000", d.blob("dir/f")}, "sub": {"160000", d.head}}
	for path, content := range files {
		entries[path] = d.file(content)
	}
	withAttrs := d.commit(entries)
	delete(entries, ".gitattributes")
	plain := d.commit(entries)
	// The default branch, the copy's HEAD, substitutes every file.
	main := d.commitOn(d.head, map[string]entry{".gitattributes": d.file("* export-subst\n"),
		"subst.txt": d.file("$Format:%H$\n")})
	d.MustPush(d.p, d.work, main+":refs/heads/main")

	d.git("", "update-ref", "refs/heads/main", main)
	d.git("", "update-ref", "refs/heads/with-attributes", withAttrs)
	d.git("", "update-ref", "refs/heads/plain", plain)
	bare := filepath.Join(t.TempDir(), "bare.git")
	d.p.Git().Run(t, "", "clone", "-q", "--bare", d.work, bare)
	theirs := func(commit string) []byte {
		out, stderr, err := d.p.Git().Env("GIT_ATTR_NOSYSTEM=1").Try(t, bare, nil, "-c", "filter.evil.smudge=false",
			"archive", "--worktree-attributes", "--format=zip", "--prefix="+prefixOf(commit), commit)
		if err != nil {
			t.Fatalf("git archive in a bare clone: %v\n%s", err, stderr)
		}
		return []byte(out)
	}
	for _, commit := range []string{withAttrs, plain} {
		ours := d.get(zipPath(commit)).body
		if want := theirs(commit); !bytes.Equal(ours, want) {
			t.Errorf("the ZIP of %s is %d bytes, not git's own %d", commit, len(ours), len(want))
		}
		r := zipOf(t, ours)
		if r.Comment != commit {
			t.Errorf("the ZIP's comment is %q, want the commit %s", r.Comment, commit)
		}
		prefix := prefixOf(commit)
		got := map[string]*zip.File{}
		for _, f := range r.File {
			got[f.Name] = f
		}
		for _, name := range []string{prefix, prefix + "dir/", prefix + "sub/"} {
			if got[name] == nil {
				t.Errorf("the ZIP of %s lists no %s", commit, name)
			}
		}
		if l := got[prefix+"link"]; l == nil || l.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the link is no link in the ZIP of %s", commit)
		}
		for path, content := range files {
			f := got[prefix+path]
			if f == nil {
				if commit == withAttrs || path != ".gitattributes" {
					t.Errorf("the ZIP of %s holds no %s", commit, path)
				}
				continue
			}
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil || string(body) != content {
				t.Errorf("%s in the ZIP of %s holds %q (%v), want its blob's %q", path, commit, body, err, content)
			}
		}
	}
}

// AN ARCHIVE IS MEASURED BEFORE ITS FIRST BYTE: with the bounds scaled, a
// commit past each is 400 naming its bound and the remedy, logged, after an
// ls-tree ended early and no archive process; a path a ZIP cannot carry is
// 400, past git's record bound and, the name bound scaled, inside it; a
// measure ended by its deadline is 503, never 400.
//
// Mutation: count only files (the directories' names pass unbounded), drop
// the measure's name arm, or answer a deadline 400.
func TestAnArchiveIsMeasuredBeforeItsFirstByte(t *testing.T) {
	t.Parallel()
	var l logs
	d := newDlCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
	s := d.host()
	empty := d.blob("")
	// Two trees name 10 000 empty files: a hundred directories of a hundred.
	var inner, outer strings.Builder
	for i := range 100 {
		inner.WriteString("100644 blob " + empty + "\tf" + strconv.Itoa(i) + "\n")
	}
	sub := strings.TrimSpace(d.git(inner.String(), "mktree"))
	for i := range 100 {
		outer.WriteString("040000 tree " + sub + "\td" + strconv.Itoa(i) + "\n")
	}
	many := strings.TrimSpace(d.git("", "commit-tree", "-m", "many", strings.TrimSpace(d.git(outer.String(), "mktree"))))
	d.MustPush(d.p, d.work, many+":refs/heads/many")
	big := d.commit(map[string]entry{"big": d.file(strings.Repeat("x", 4096))})
	// Five levels of 200-byte names: the file's own name is short of the
	// names bound, its directories' names together are past it.
	deep := d.commit(map[string]entry{strings.Repeat(strings.Repeat("n", 200)+"/", 5) + "f": {"100644", empty}})
	ran := host.RecordGit(s)
	const no = 1 << 40
	for _, tc := range []struct {
		commit, bound         string
		entries, bytes, names int64
	}{
		{many, "1000 entries", 1000, no, no}, {big, "4000 bytes", no, 4000, no}, {deep, "2000 bytes", no, no, 2000},
	} {
		commit, bound := tc.commit, tc.bound
		host.SetArchiveBounds(s, tc.entries, tc.bytes, tc.names)
		before := len(ran())
		a := d.ask(http.MethodGet, zipPath(commit), "ceo")
		assertLine(t, a, http.StatusBadRequest, "a ZIP past "+bound)
		if !strings.Contains(string(a.body), strings.Fields(bound)[0]) ||
			!strings.Contains(string(a.body), "git clone --depth 1") {
			t.Errorf("a ZIP past %s said %q, want the bound and the remedy", bound, a.body)
		}
		for _, c := range ran()[before:] {
			if c.Args[0] == "archive" {
				t.Errorf("a ZIP past %s ran git archive", bound)
			}
		}
	}
	for _, bound := range []string{"entries", "bytes", "names"} {
		if got := l.count("code_archive_too_large", `"bound":"`+bound+`"`); got != 1 {
			t.Errorf("code_archive_too_large bound=%s was logged %d times, want once", bound, got)
		}
	}
	host.SetArchiveBounds(s, host.MaxArchiveEntries, host.MaxArchiveTreeBytes, host.MaxArchiveNameBytes)

	// A NAME A ZIP CANNOT CARRY: 270 levels of 250-byte names.
	long := d.commit(map[string]entry{strings.Repeat(strings.Repeat("m", 250)+"/", 270) + "f": {"100644", empty}})
	before := len(ran())
	a := d.ask(http.MethodGet, zipPath(long), "ceo")
	assertLine(t, a, http.StatusBadRequest, "a ZIP holding a name past 65,535 bytes")
	if !strings.Contains(string(a.body), "a ZIP cannot carry") {
		t.Errorf("a ZIP holding a name past 65,535 bytes said %q", a.body)
	}
	for _, c := range ran()[before:] {
		if c.Args[0] == "archive" {
			t.Errorf("a ZIP holding a name past 65,535 bytes ran git archive")
		}
	}
	// A NAME PAST THE BOUND in a record git lists whole: deep's third level.
	host.SetArchiveNameMax(s, 500)
	before = len(ran())
	a = d.ask(http.MethodGet, zipPath(deep), "ceo")
	assertLine(t, a, http.StatusBadRequest, "a ZIP holding a name past 500 bytes")
	if !strings.Contains(string(a.body), "more than 500 bytes") || !strings.Contains(string(a.body), "a ZIP cannot carry") {
		t.Errorf("a ZIP holding a name past 500 bytes said %q", a.body)
	}
	for _, c := range ran()[before:] {
		if c.Args[0] == "archive" {
			t.Errorf("a ZIP holding a name past 500 bytes ran git archive")
		}
	}
	if got := l.count("code_archive_too_large", `"bound":"name"`); got != 2 {
		t.Errorf("code_archive_too_large bound=name was logged %d times, want twice", got)
	}
	host.SetArchiveNameMax(s, host.ZipNameMax)

	// A MEASURE ENDED BY ITS DEADLINE is the node's, never the commit's.
	host.FailGitOnce(s, gitproc.ErrDeadline, "ls-tree")
	a = d.ask(http.MethodGet, zipPath(d.head), "ceo")
	assertLine(t, a, http.StatusServiceUnavailable, "a measure ended by its deadline")
	if got := a.resp.Header.Get("Retry-After"); got != "10" {
		t.Errorf("a measure ended by its deadline answered Retry-After %q, want 10", got)
	}
	d.get(zipPath(d.head))
}

// AN ARCHIVE STAYS INSIDE ITS BOUND: commits at the measure's edges are each
// served whole and no longer than archiveBound of what they are made of (an
// incompressible file, ten thousand empty files, long nested names, a link, a
// submodule, a name that is no UTF-8, an empty tree), and the archive runs
// for min(ArchiveDeadline, DownloadDeadline(bound)). With the bound scaled to
// half a ZIP, the answer is cut short of a whole ZIP, logged once, and a unit
// repository's read is recorded with the bytes it was sent.
//
// Mutation: name each entry once in the bound, or end the cut answer cleanly.
func TestAnArchiveStaysInsideItsBound(t *testing.T) {
	t.Parallel()
	var l logs
	d := newDlCase(t, gitlettest.SoloOptions{Logger: l.logger()}, unitSettings())
	s := d.host()
	f := d.Fleet.(*gitlettest.SoloFleet)
	empty := d.blob("")
	tenThousand := map[string]entry{}
	for i := range 10_000 {
		tenThousand["d"+strconv.Itoa(i/100)+"/f"+strconv.Itoa(i%100)] = entry{"100644", empty}
	}
	emptyCommit := strings.TrimSpace(d.git("", "commit-tree", "-m", "empty", strings.TrimSpace(d.git("", "mktree"))))
	d.MustPush(d.p, d.work, emptyCommit+":refs/heads/empty")
	incompressible := d.commit(map[string]entry{"noise": d.file(random(3 << 20))})
	edges := []string{
		incompressible,
		d.commit(tenThousand),
		d.commit(map[string]entry{strings.Repeat(strings.Repeat("l", 200)+"/", 30) + "f": d.file("deep\n")}),
		d.commit(map[string]entry{"link": {"120000", d.blob("x")}, "sub": {"160000", d.head},
			"caf\xe9": d.file("latin\n")}),
		emptyCommit,
	}
	ran := host.RecordGit(s)
	for _, commit := range edges {
		before := len(ran())
		body := d.get(zipPath(commit)).body
		m := zipMeasure(zipOf(t, body))
		bound := host.ArchiveBoundOf(m)
		if int64(len(body)) > bound {
			t.Errorf("the ZIP of %s is %d bytes, past its bound %d of %+v", commit, len(body), bound, m)
		}
		for _, c := range ran()[before:] {
			if c.Args[0] != "archive" {
				continue
			}
			if want := min(host.ArchiveDeadline, gitproc.DownloadDeadline(bound)); c.Deadline != want {
				t.Errorf("the archive of %s runs for %v, want %v: the measure is not the ZIP's own", commit,
					c.Deadline, want)
			}
		}
	}

	whole := d.get(zipPath(incompressible)).body
	half := int64(len(whole) / 2)
	host.SetArchiveBound(s, func(host.ArchiveMeasure) int64 { return half })
	before := len(f.Fetched())
	a := d.ask(http.MethodGet, zipPath(incompressible), "ceo")
	if a.resp.StatusCode != http.StatusOK || a.cut == nil || int64(len(a.body)) > half {
		t.Errorf("a ZIP past its bound answered %d with %d bytes (cut %v), want it cut within %d", a.resp.StatusCode,
			len(a.body), a.cut, half)
	}
	if _, err := zip.NewReader(bytes.NewReader(a.body), int64(len(a.body))); err == nil {
		t.Errorf("a ZIP cut at its bound reads as a whole ZIP")
	}
	eventually(t, "the cut ZIP's line", func() bool { return l.count("code_archive_past_bound") == 1 })
	got := f.Fetched()[before:]
	if len(got) != 1 || got[0].Kind != types.FetchedArchive || got[0].Bytes < int64(len(a.body)) ||
		got[0].Bytes > half {
		t.Errorf("the cut ZIP is recorded %+v, want one ZIP of the %d to %d bytes it was sent", got, len(a.body),
			half)
	}
}

// A LARGE ARCHIVE HOLDS A PLACE: a node makes one for every ArchiveShare
// read slots, two at least. On a node of two, two principals' large ZIPs run,
// a third principal's is 503 once the wait has passed, and is served once one
// ends; a small ZIP takes no place and is served at once. One principal makes
// one ZIP at a time, marked before it waits: its second, while its first
// waits for a place or streams, is 429 at once.
//
// Mutation: mark the principal only once it holds a place (both wait), let a
// small ZIP take a place, or no per-principal mark.
func TestALargeArchiveHoldsAPlace(t *testing.T) {
	t.Parallel()
	for slots, want := range map[int]int{1: 2, 2: 2, 3: 2, 4: 2, 7: 2, 8: 2, 12: 3, 16: 4, 64: 16} {
		if got := host.Archives(slots); got != want {
			t.Errorf("Archives(%d) = %d, want %d", slots, got, want)
		}
	}
	var l logs
	d := newDlCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
	s := d.host()
	large := d.commit(map[string]entry{"noise": d.file(random(200 << 10))})
	const wait = 2 * time.Second
	host.SetArchivePlaces(s, 2, wait)
	host.SetArchiveFree(s, 100<<10)
	_, release := host.HoldGit(s, "archive", "--worktree-attributes", "--format=zip", "--prefix="+prefixOf(large))
	defer release()

	type result struct {
		who    string
		status int
		cut    error
	}
	results := make(chan result, 8)
	start := func(who string) {
		go func() {
			a := d.ask(http.MethodGet, zipPath(large), who)
			results <- result{who, a.resp.StatusCode, a.cut}
		}()
	}
	held := func(places, principals int) func() bool {
		return func() bool {
			p, q := host.ArchivesHeld(s)
			return p == places && q == principals
		}
	}
	start("ceo")
	eventually(t, "ceo's place", held(1, 1))
	start("dev")
	eventually(t, "dev's place", held(2, 2))

	// Both places held: a small ZIP is served at once, ceo's second is 429.
	if a := d.ask(http.MethodGet, zipPath(d.head), ""); a.resp.StatusCode != http.StatusOK {
		t.Errorf("a small ZIP at a full node answered %d %q, want it served", a.resp.StatusCode, a.body)
	}
	if a := d.ask(http.MethodGet, zipPath(d.head), "ceo"); a.resp.StatusCode != http.StatusTooManyRequests ||
		a.resp.Header.Get("Retry-After") != "10" {
		t.Errorf("ceo's second ZIP while its first streams answered %d Retry-After %q, want 429 and 10",
			a.resp.StatusCode, a.resp.Header.Get("Retry-After"))
	}
	start(gitlettest.FixturePat)
	eventually(t, "a third principal waiting", held(2, 3))
	if a := d.ask(http.MethodGet, zipPath(large), gitlettest.FixturePat); a.resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a principal's second ZIP while its first waits answered %d, want 429", a.resp.StatusCode)
	}
	// Answered while the first still waits for a place: it never waited itself.
	if len(results) != 0 || !held(2, 3)() {
		t.Errorf("a principal's second ZIP was answered only once its first stopped waiting")
	}
	if r := <-results; r.who != gitlettest.FixturePat || r.status != http.StatusServiceUnavailable {
		t.Errorf("%s's large ZIP at a full node answered %d, want the third principal's 503", r.who, r.status)
	}
	if n := l.count("code_archive_busy", `"reason":"places"`); n != 1 {
		t.Errorf("code_archive_busy reason=places was logged %d times, want once", n)
	}
	if n := l.count("code_archive_busy", `"reason":"principal"`); n != 2 {
		t.Errorf("code_archive_busy reason=principal was logged %d times, want twice", n)
	}
	release()
	for range 2 {
		if r := <-results; r.status != http.StatusOK || r.cut != nil {
			t.Errorf("%s's large ZIP answered %d (cut %v) once released, want it whole", r.who, r.status, r.cut)
		}
	}
	if a := d.ask(http.MethodGet, zipPath(large), gitlettest.FixturePat); a.resp.StatusCode != http.StatusOK {
		t.Errorf("the third principal's large ZIP once a place is free answered %d, want it served",
			a.resp.StatusCode)
	}
	eventually(t, "every place given back", held(0, 0))
}

// A DOWNLOAD RUNS IN THE TRANSFER LANE: a file's bytes and an archive are
// transfers under the credential's share (a person by seat, a token bound to
// nobody by its name), held to the stall; the lookup and the measure are
// reads with the read deadline and no share; and no download takes a unit of
// the seat git-work share.
//
// Mutation: stream in the read lane, or take a unit.
func TestADownloadRunsInTheTransferLane(t *testing.T) {
	t.Parallel()
	d := newDlCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	s := d.host()
	ran := host.RecordGit(s)
	reached, release := host.HoldGit(s, "archive")
	done := make(chan answer, 1)
	go func() { done <- d.ask(http.MethodGet, zipPath(d.head), "ceo") }()
	reachedFirst(t, reached, done)
	if held := d.Fleet.Nodes()[0].Share().Held(); held != 0 {
		t.Errorf("an archive holds %d units of the seat git-work share, want none", held)
	}
	release()
	if a := <-done; a.resp.StatusCode != http.StatusOK {
		t.Fatalf("the ZIP answered %d", a.resp.StatusCode)
	}
	for who, want := range map[string]gitproc.ShareClass{"ceo": gitproc.SharePerson, "": gitproc.ShareMachine} {
		before := len(ran())
		for _, target := range []string{rawPath(d.head, "README.md"), zipPath(d.head)} {
			if a := d.ask(http.MethodGet, target, who); a.resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s as %q answered %d", target, who, a.resp.StatusCode)
			}
		}
		cmds := ran()[before:]
		if len(cmds) != 5 {
			t.Errorf("the two downloads ran %d processes, want two lookups, a measure and two streams", len(cmds))
		}
		for _, c := range cmds {
			switch c.Args[0] + " " + c.Args[1] {
			case "cat-file blob", "archive --worktree-attributes":
				if c.Class != gitproc.ClassTransfer || c.Share.Class != want || c.Share.Principal == "" ||
					(who != "" && c.Share.Principal != who) || c.Stall != host.Production.Stall || c.Deadline <= 0 {
					t.Errorf("%q as %q runs in %s under %+v, stall %v, want the transfer lane under its "+
						"credential's share", c.Args, who, c.Class, c.Share, c.Stall)
				}
			default:
				if c.Class != gitproc.ClassRead || c.Share != (gitproc.Share{}) || c.Deadline != gitproc.ReadDeadline ||
					c.Loads != 0 {
					t.Errorf("%q as %q runs in %s under %+v for %v loading %v, want a read with no share",
						c.Args, who, c.Class, c.Share, c.Deadline, c.Loads)
				}
			}
		}
	}
}

// A UNIT REPOSITORY'S DOWNLOAD IS RECORDED: a raw file and a ZIP are each one
// read via git of its kind and the bytes it was sent; a company repository's
// and a refused one are none; a download whose client hangs up part way is
// recorded with what it was sent. The download's line names the request's
// fetch metadata, held to the values the spec defines.
//
// Mutation: record only a download that ended whole.
func TestAUnitRepositorysDownloadIsRecorded(t *testing.T) {
	t.Parallel()
	var l logs
	d := newDlCase(t, gitlettest.SoloOptions{Logger: l.logger()}, unitSettings())
	f := d.Fleet.(*gitlettest.SoloFleet)
	const size = 16 << 20
	big := d.commit(map[string]entry{"noise": d.file(random(size))})
	before := len(f.Fetched())
	// A 200's record and line are filed after its last byte reaches the client.
	raw := d.get(rawPath(d.head, "README.md"), "Sec-Fetch-Site", "cross-site", "Sec-Fetch-User", "?1")
	eventually(t, "the raw read's record", func() bool { return len(f.Fetched()) > before })
	z := d.get(zipPath(d.head), "Sec-Fetch-Site", "<script>")
	eventually(t, "the ZIP's record", func() bool { return len(f.Fetched()) > before+1 })
	eventually(t, "the downloads' lines", func() bool { return l.count("code_download") == 2 })
	assertLine(t, d.ask(http.MethodGet, rawPath(d.head, "nothing"), "ceo"), http.StatusNotFound, "no such file")
	got := f.Fetched()[before:]
	want := []types.CodeFetched{
		{Kind: types.FetchedRaw, Bytes: int64(len(raw.body))}, {Kind: types.FetchedArchive, Bytes: int64(len(z.body))},
	}
	if len(got) != len(want) {
		t.Fatalf("the downloads were recorded %+v, want a raw read and a ZIP", got)
	}
	for i, r := range got {
		if r.Kind != want[i].Kind || r.Bytes != want[i].Bytes || r.Via != types.FetchedViaGit || r.Reads != 1 ||
			r.RepoName != gitlettest.RepoName || r.Seat != "ceo" {
			t.Errorf("download %d is recorded %+v, want one %s read of %d bytes by ceo", i, r, want[i].Kind,
				want[i].Bytes)
		}
	}
	if n := l.count("code_download", `"kind":"raw"`, `"sec_fetch_site":"cross-site"`, `"sec_fetch_user":"?1"`); n != 1 {
		t.Errorf("the raw read's code_download names its fetch metadata %d times, want once: %q", n,
			l.lines("code_download"))
	}
	if n := l.count("code_download", `"kind":"archive"`, `"sec_fetch_site":"other"`, `"sec_fetch_user":""`); n != 1 {
		t.Errorf("the ZIP's code_download carries a client's text: %q", l.lines("code_download"))
	}

	// A CLIENT THAT HANGS UP after its first mebibyte: recorded, and logged
	// with its fetch metadata beside its failure.
	before = len(f.Fetched())
	resp := d.send(http.MethodGet, rawPath(big, "noise"), "ceo", "Sec-Fetch-Site", "same-site")
	if _, err := io.ReadFull(resp.Body, make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	eventually(t, "the hung-up download's record", func() bool { return len(f.Fetched()) > before })
	if r := f.Fetched()[before]; r.Kind != types.FetchedRaw || r.Bytes < 1<<20 || r.Bytes >= size {
		t.Errorf("a download cut by its client is recorded %+v, want what it was sent", r)
	}
	eventually(t, "the hung-up download's line", func() bool {
		return l.count("code_download", `"status":200`, `"sec_fetch_site":"same-site"`) == 1
	})

	// A company repository's download is no read of a unit's.
	open := newDlCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	open.get(rawPath(open.head, "README.md"))
	open.get(zipPath(open.head))
	if got := open.Fleet.(*gitlettest.SoloFleet).Fetched(); len(got) != 0 {
		t.Errorf("a company repository's downloads were recorded %+v", got)
	}
}

// A LARGE BLOB IS PACED: it arrives whole at its Content-Length, and with the
// server's clock running past the pace as the answer is written, the answer
// stops short, the connection is cut, `cat-file blob` is ended
// (code_download_failed, reason output) and the read is recorded with what
// it was sent.
//
// Mutation: pace the answer on a clock of its own (it arrives whole), or
// record only a download that ended whole.
func TestALargeBlobIsPaced(t *testing.T) {
	t.Parallel()
	var l logs
	c := newCase(t, gitlettest.SoloOptions{Logger: l.logger()}, unitSettings())
	// The clock is set before the host's first request, which reads it.
	clock := &steppedClock{at: time.Now(), step: time.Microsecond}
	host.SetClock(c.Fleet.Nodes()[0].Host(), clock.now)
	d := seeded(t, c)
	f := d.Fleet.(*gitlettest.SoloFleet)
	const size = 3 << 20
	content := random(size)
	commit := d.commit(map[string]entry{"noise": d.file(content)})
	first := len(f.Fetched())
	if a := d.get(rawPath(commit, "noise")); string(a.body) != content || a.resp.ContentLength != size {
		t.Fatalf("a %d-byte file arrived as %d bytes of %d declared", size, len(a.body), a.resp.ContentLength)
	}
	eventually(t, "the whole read's record", func() bool { return len(f.Fetched()) > first })
	// Twenty seconds pass at every reading of the clock, so the pace's thirty
	// seconds a mebibyte are spent within the answer's first two writes.
	clock.setStep(20 * time.Second)
	before := len(f.Fetched())
	resp := d.send(http.MethodGet, rawPath(commit, "noise"), "ceo")
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err == nil || len(body) >= size {
		t.Errorf("a client slower than the pace read %d of %d bytes (%v), want the answer cut", len(body), size, err)
	}
	eventually(t, "the slow download's failure", func() bool {
		return l.count("code_download_failed", `"reason":"output"`) == 1
	})
	if got := f.Fetched()[before:]; len(got) != 1 || got[0].Bytes < int64(len(body)) || got[0].Bytes >= size {
		t.Errorf("the cut download is recorded %+v, want one read of what it was sent", got)
	}
}

// A DOWNLOAD FOLLOWS THE REPOSITORY'S STATUS: an old name's download is served
// as the repository under its current name, never redirected; a deleted or
// purging one is no download; an archived one, one being imported and a pull
// mirror are served, since a download is no push.
//
// Mutation: answer a download at an old name as a push is answered.
func TestADownloadFollowsTheRepositorysStatus(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	as := func(ref gitlet.RepoRef) gitlet.RepoRef { return ref }
	e := &recording{resolve: func(ctx context.Context, inner host.Estate, q gitlet.ResolveQuery) (gitlet.RepoRef, error) {
		switch q.Name {
		case "old-name":
			q.Name = gitlettest.RepoName
			ref, err := inner.Resolve(ctx, q)
			ref.Redirect, ref.RedirectUntil = gitlettest.RepoName, time.Now().Add(gitlet.RedirectWindow)
			return ref, err
		case gitlettest.RepoName:
			ref, err := inner.Resolve(ctx, q)
			mu.Lock()
			defer mu.Unlock()
			return as(ref), err
		}
		return inner.Resolve(ctx, q)
	}}
	d := newDlCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: e.wrap}}, gitlettest.Settings())
	old := "/git/old-name/-/"
	if a := d.get(old + "raw?rev=" + d.head + "&path=README.md"); string(a.body) != "one\n" {
		t.Errorf("the old name's raw read answered %q", a.body)
	}
	z := d.get(old + "archive.zip?rev=" + d.head)
	if _, params, _ := mime.ParseMediaType(z.resp.Header.Get("Content-Disposition")); params["filename"] !=
		gitlettest.RepoName+"-"+d.head[:gitlet.MinAbbrevDigits]+".zip" {
		t.Errorf("the old name's ZIP is named %q, want the current name's", params["filename"])
	}
	for _, file := range zipOf(t, z.body).File {
		if !strings.HasPrefix(file.Name, prefixOf(d.head)) {
			t.Errorf("the old name's ZIP holds %q, outside the current name's folder", file.Name)
		}
	}
	for _, tc := range []struct {
		name   string
		status gitlet.RepoStatus
		mirror bool
		want   int
	}{
		{"deleted", gitlet.RepoStatusDeleted, false, http.StatusNotFound},
		{"purging", gitlet.RepoStatusPurging, false, http.StatusNotFound},
		{"archived", gitlet.RepoStatusArchived, false, http.StatusOK},
		{"importing", gitlet.RepoStatusImporting, false, http.StatusOK},
		{"a pull mirror", gitlet.RepoStatusActive, true, http.StatusOK},
	} {
		mu.Lock()
		as = func(ref gitlet.RepoRef) gitlet.RepoRef {
			ref.Status, ref.Mirrored = tc.status, tc.mirror
			if tc.status == gitlet.RepoStatusDeleted || tc.status == gitlet.RepoStatusPurging {
				ref.DeletedAt = time.Now()
			}
			return ref
		}
		mu.Unlock()
		for _, target := range []string{rawPath(d.head, "README.md"), zipPath(d.head)} {
			a := d.ask(http.MethodGet, target, "ceo")
			if a.resp.StatusCode != tc.want {
				t.Errorf("%s: %s answered %d %q, want %d", tc.name, target, a.resp.StatusCode, short(string(a.body)),
					tc.want)
			}
			if tc.want == http.StatusNotFound {
				assertLine(t, a, http.StatusNotFound, tc.name)
			}
		}
	}
}

// A DAMAGED COPY IS REPAIRED UNDER A DOWNLOAD: with the copy's pack index
// cut short, the lookup's cat-file answers `missing` over the damage and
// exits 0; the answer is the damage's, so the copy is repaired and the file
// and the ZIP are served whole, never 404. Damage met after the first byte
// cuts the connection: nothing can be run again under a begun answer.
//
// Mutation: take the lookup's `missing` at face value (the raw read is 404),
// or end the answer cleanly where git failed after its first byte.
func TestADamagedCopyIsRepairedUnderADownload(t *testing.T) {
	t.Parallel()
	t.Run("after the first byte", func(t *testing.T) {
		t.Parallel()
		var l logs
		d := newDlCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
		text := strings.Repeat("line of text\n", 5000)
		commit := d.files(map[string]string{"long.txt": text})
		for target, args := range map[string][]string{rawPath(commit, "long.txt"): {"cat-file", "blob"},
			zipPath(commit): {"archive"}} {
			host.AnswerGitOnce(d.host(), []byte(text[:len(text)/2]), fmt.Errorf("%w: a pack went missing",
				gitproc.ErrMissingObject), args...)
			a := d.ask(http.MethodGet, target, "ceo")
			if a.resp.StatusCode != http.StatusOK || a.cut == nil || len(a.body) > len(text)/2 {
				t.Errorf("%s: damage after the first byte answered %d with %d bytes (cut %v), want the "+
					"connection cut", target, a.resp.StatusCode, len(a.body), a.cut)
			}
		}
		if n := l.count("code_download_retried"); n != 0 {
			t.Errorf("a begun download was run again (%d code_download_retried)", n)
		}
	})
	for _, kind := range []string{"raw", "archive"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			var l logs
			d := newDlCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
			truncateNewestIndex(t, d.Case)
			target := rawPath(d.head, "README.md")
			if kind == "archive" {
				target = zipPath(d.head)
			}
			a := d.get(target)
			if kind == "raw" && string(a.body) != "one\n" {
				t.Errorf("the repaired raw read answered %q", a.body)
			}
			if kind == "archive" {
				zipOf(t, a.body)
			}
			if n := l.count("code_download_retried"); n < 1 {
				t.Errorf("code_download_retried was never logged: the damage was not repaired under the download")
			}
		})
	}
}

// A BAD SIZE IS DAMAGE: ls-tree prints BAD for a blob it cannot read and exits
// 0, so a measure that meets one answers the copy's damage, which is repaired
// as a missing object before the ZIP is measured again and served.
//
// Mutation: read BAD as a size of 0 (the ZIP is served over the damage).
func TestABadSizeIsDamage(t *testing.T) {
	t.Parallel()
	var l logs
	d := newDlCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
	host.AnswerGitOnce(d.host(), []byte("100644 blob "+randomOID()+"     BAD\tgone\x00"), nil, "ls-tree")
	zipOf(t, d.get(zipPath(d.head)).body)
	if n := l.count("code_download_retried", `"reason":"missing"`); n != 1 {
		t.Errorf("code_download_retried reason=missing was logged %d times, want once: %q", n,
			l.lines("code_download_retried"))
	}
}
