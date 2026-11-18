package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// GET /git/{repo}/-/raw and /-/archive.zip (package doc, "A file's bytes and a
// commit's ZIP are served inert beside git"). A lookup runs in the read lane.

// The query keys each download takes, a closed set: any other key is refused.
var (
	rawKeys     = []string{"rev", "path", "download"}
	archiveKeys = []string{"rev"}
)

// zipNameMax is the longest name a ZIP entry carries (its 16-bit length);
// git refuses a longer one only once it reaches it, after writing what came before.
const zipNameMax = 65535

// Raw answers a file's bytes at a commit.
func (s *Server) Raw() http.Handler { return s.route(s.raw, s.admit) }

// Archive answers a commit's files as a ZIP.
func (s *Server) Archive() http.Handler { return s.route(s.archive, s.admit) }

func (s *Server) raw(w http.ResponseWriter, r *http.Request, admit admitter) {
	s.download(w, r, admit, types.FetchedRaw)
}

func (s *Server) archive(w http.ResponseWriter, r *http.Request, admit admitter) {
	s.download(w, r, admit, types.FetchedArchive)
}

// downloadQuery is a download's query, judged: a whole commit id, and for a
// raw file its path and whether it is asked as an attachment.
type downloadQuery struct {
	rev, path string
	attach    bool
}

// readDownloadQuery judges raw against kind's keys, answering why it is
// refused, or "".
func readDownloadQuery(raw, kind string) (downloadQuery, string) {
	keys := archiveKeys
	if kind == types.FetchedRaw {
		keys = rawKeys
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return downloadQuery{}, "the query could not be read: " + err.Error()
	}
	for key, vs := range values {
		switch {
		case !slices.Contains(keys, key):
			return downloadQuery{}, fmt.Sprintf("unknown parameter %q: this download takes %s", key,
				strings.Join(keys, ", "))
		case len(vs) > 1:
			return downloadQuery{}, fmt.Sprintf("parameter %q is given %d times: give it once", key, len(vs))
		}
	}
	q := downloadQuery{rev: values.Get("rev")}
	if !gitref.ValidOID(q.rev) || q.rev == gitlet.ZeroOID {
		return downloadQuery{}, "rev must name a commit by its whole id, 40 lowercase hex digits"
	}
	if kind != types.FetchedRaw {
		return q, ""
	}
	p, err := gitlet.GitPath("path", values.Get("path"), "", false)
	if err != nil {
		return downloadQuery{}, err.Error()
	}
	q.path = p
	switch d, ok := values["download"]; {
	case !ok:
	case d[0] == "1":
		q.attach = true
	default:
		return downloadQuery{}, "download is 1, or left out"
	}
	return q, ""
}

// download serves one download of kind (types.FetchedRaw, types.FetchedArchive).
func (s *Server) download(w http.ResponseWriter, r *http.Request, admit admitter, kind string) {
	start := s.now()
	if auth.GitAccess(r) != auth.GitDownload {
		// An escaped tail the mux matched but the guard and the drain read as
		// a push: nothing is served as a download unless all three agree.
		httpjson.NoRoute(w, r)
		return
	}
	if r.Method == http.MethodHead {
		// A download is a GET: answered before any repository read or git.
		w.Header().Set("Allow", http.MethodGet)
		auth.GitText(w, http.StatusMethodNotAllowed, "a download is a GET")
		return
	}
	ctx, end, ok := s.begin(r.Context(), w)
	if !ok {
		return
	}
	defer end()
	q, why := readDownloadQuery(r.URL.RawQuery, kind)
	if why != "" {
		auth.GitText(w, http.StatusBadRequest, sentence(why))
		return
	}
	adm, ok := admit(ctx, w, r, auth.GitDownload)
	if !ok {
		return
	}
	switch {
	case gone(adm.repo.Status):
		auth.GitText(w, http.StatusNotFound, s.goneSentence(adm.repo))
		return
	case !adm.grant.MayRead():
		auth.GitText(w, http.StatusForbidden, sentence(adm.grant.ReadRefusal()))
		return
	}
	synced, err := s.o.Cache.Sync(ctx, adm.repo.ID, adm.objects())
	if err != nil {
		s.syncFailed(w, r, adm.name, err)
		return
	}
	defer func() { synced.Release() }()

	at, err := s.lookedUp(ctx, r, adm, &synced, q)
	switch {
	case err != nil:
		s.syncFailed(w, r, adm.name, err)
		return
	case at.refusal != "":
		auth.GitText(w, at.status, sentence(at.refusal))
		return
	}
	d := &downloading{s: s, w: w, r: r, adm: adm, kind: kind, commit: at.commit, start: start,
		out: newSink(w, "", s.now)}
	// Counted however the answer ends: one git process read the
	// repository, and its bytes out (Server.served).
	defer func() { s.served(adm, d.out, cache.Use{Fetches: 1}) }()
	if kind == types.FetchedRaw {
		d.raw(ctx, &synced, q, at)
		return
	}
	d.archive(ctx, &synced)
}

// target is what a download's lookup found: the commit its rev peels to and,
// for a raw file, the blob at its path; or a refusal, with its status.
type target struct {
	commit, blob string
	size         int64
	status       int
	refusal      string
}

// lookedUp looks q up in the copy (lookup), syncing ONCE at the log's head
// where the copy lacks q's commit: a whole id the copy does not hold names a
// commit from a node further ahead, or none at all.
func (s *Server) lookedUp(ctx context.Context, r *http.Request, adm admission, synced *cache.Synced,
	q downloadQuery) (target, error) {

	short := q.rev[:gitlet.MinAbbrevDigits]
	var objs []lookedObject
	look := func() error {
		return s.repairing(ctx, r, adm.name, synced, inIncarnation, notStarted, func(in cache.Synced) (err error) {
			objs, err = s.lookup(ctx, in, q)
			return err
		})
	}
	if err := look(); err != nil {
		return target{}, err
	}
	if objs[0].missing {
		head, err := s.o.Cache.Sync(ctx, adm.repo.ID, statelog.Freshness{Level: statelog.ReadLinearizable})
		if err != nil {
			return target{}, err
		}
		synced.Release()
		*synced = head
		if err = look(); err != nil {
			return target{}, err
		}
	}
	if objs[0].missing || objs[1].missing {
		return target{status: http.StatusNotFound, refusal: "no commit " + q.rev + " in " + adm.name}, nil
	}
	if objs[1].Type != "commit" {
		return target{status: http.StatusBadRequest, refusal: fmt.Sprintf("rev %s names a %s: a download names "+
			"a commit, or a tag of one", q.rev, objs[1].Type)}, nil
	}
	f := target{commit: objs[1].ID}
	if q.path == "" {
		return f, nil
	}
	at := objs[2]
	switch {
	case at.missing:
		f.status, f.refusal = http.StatusNotFound, fmt.Sprintf("no file at %q in %s", q.path, short)
	case at.Type == "tree":
		f.status, f.refusal = http.StatusNotFound, fmt.Sprintf("%q in %s is a directory", q.path, short)
	case at.Type == "commit", at.Type == "submodule":
		f.status, f.refusal = http.StatusNotFound, fmt.Sprintf("%q in %s is a submodule", q.path, short)
	case at.Type == "blob":
		f.blob, f.size = at.ID, at.Size
	default:
		f.status, f.refusal = http.StatusNotFound, fmt.Sprintf("%q in %s names a %s, not a file", q.path, short,
			at.Type)
	}
	return f, nil
}

// lookedObject is one line of a lookup's answer: the object, or missing.
type lookedObject struct {
	gitproc.Object
	missing bool
}

// lookup is ONE cat-file --batch-check of q's rev, its peeled commit and its
// path, all on stdin. An answer read over damage is the damage's, since
// cat-file calls an unreadable object missing and exits 0 (cache.Missing).
func (s *Server) lookup(ctx context.Context, in cache.Synced, q downloadQuery) ([]lookedObject, error) {
	lines := []string{q.rev, q.rev + "^{}"}
	if q.path != "" {
		lines = append(lines, q.rev+":"+q.path)
	}
	var stdin bytes.Buffer
	for _, l := range lines {
		stdin.WriteString(l)
		stdin.WriteByte(0)
	}
	out := make([]lookedObject, 0, len(lines))
	recs := gitproc.NewRecords(func(rec []byte) error {
		i := len(out)
		if i >= len(lines) {
			return fmt.Errorf("host: cat-file answered %d records for %d lines", i+1, len(lines))
		}
		if string(rec) == lines[i]+" missing" {
			out = append(out, lookedObject{missing: true})
			return nil
		}
		o, ok := gitproc.ParseCheck(rec)
		if !ok {
			return fmt.Errorf("host: cat-file answered %q for %q, which is no object", rec, lines[i])
		}
		out = append(out, lookedObject{Object: o})
		return nil
	})
	res, err := s.runGit(ctx, gitproc.Cmd{
		Dir: in.Dir, Dropping: in.Dropping, Repairer: in.Repairer,
		Args: []string{"cat-file", "--batch-check", "-Z"}, Config: s.o.Cache.ServeConfig(),
		Stdin: &stdin, Stdout: recs, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
	})
	if res.Damage != nil {
		return nil, damaged("a download's lookup", res.Damage, err)
	}
	if err == nil {
		err = recs.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("host: a download's lookup: %w", err)
	}
	if len(out) != len(lines) {
		return nil, fmt.Errorf("host: cat-file answered %d records for %d lines", len(out), len(lines))
	}
	return out, nil
}

// damaged is what read over damage: the damage wrapped and the run's error only
// told, so repairing meets the damage before a deadline or a bound under it.
func damaged(what string, damage, err error) error {
	return fmt.Errorf("host: %s read a damaged copy: %w (%v)", what, damage, err) //nolint:errorlint // told, never wrapped
}

// downloading is one download whose lookup found what it asked for.
type downloading struct {
	s      *Server
	w      http.ResponseWriter
	r      *http.Request
	adm    admission
	kind   string
	commit string
	start  time.Time
	out    *sink
}

// raw answers a file's bytes: 304 where If-None-Match names its blob, else
// `cat-file blob` streamed in the transfer lane.
func (d *downloading) raw(ctx context.Context, synced *cache.Synced, q downloadQuery, f target) {
	tag := `"` + f.blob + `"`
	if httpjson.NoneMatch(d.r.Header.Get("If-None-Match"), tag) {
		h := d.w.Header()
		pagepolicy.Download(h)
		h.Set("ETag", tag)
		h.Set("Cache-Control", downloadCacheControl)
		d.w.WriteHeader(http.StatusNotModified)
		d.s.downloaded(ctx, d.adm, d.kind, 0)
		d.logged(http.StatusNotModified, 0)
		return
	}
	name := path.Base(q.path)
	d.out.holdOpening(codeindex.BinaryProbeBytes)
	d.out.bound(f.size)
	d.out.headers = func(h http.Header, opening []byte) {
		pagepolicy.Download(h)
		ctype, disposition := "application/octet-stream", "attachment"
		if !q.attach && !codeindex.Binary(opening) {
			ctype, disposition = "text/plain; charset=utf-8", "inline"
		}
		h.Set("Content-Type", ctype)
		h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
		h.Set("Content-Length", strconv.FormatInt(f.size, 10))
		h.Set("ETag", tag)
		h.Set("Cache-Control", downloadCacheControl)
	}
	var cpu time.Duration
	err := d.s.repairing(ctx, d.r, d.adm.name, synced, inIncarnation, d.out.Started, func(in cache.Synced) error {
		d.out.again()
		res, gerr := d.s.runGit(ctx, gitproc.Cmd{Dir: in.Dir, Dropping: in.Dropping, Repairer: in.Repairer,
			Args: []string{"cat-file", "blob", f.blob}, Config: d.s.o.Cache.ServeConfig(),
			Stdout: d.out, Class: gitproc.ClassTransfer, Share: d.adm.share(),
			Deadline: gitproc.DownloadDeadline(f.size), Stall: d.s.o.Timing.Stall})
		cpu = res.CPU
		return gerr
	})
	d.finish(ctx, err, cpu, 0)
}

// downloadCacheControl keeps a download out of every cache: a tab that opened
// it names no reader, and a code read for nobody is never kept.
const downloadCacheControl = "private, no-store"

// archive answers a commit's files as git's own ZIP: marked for its
// principal, measured, a place taken where it is large, then streamed.
func (d *downloading) archive(ctx context.Context, synced *cache.Synced) {
	s := d.s
	leave, err := s.archives.enter(d.adm.share())
	if err != nil {
		held, places := s.archives.inUse()
		s.log.Info("code_archive_busy", "repo", d.adm.name, "reason", "principal", "held", held,
			"places", places, "operator_id", d.adm.operatorID)
		retry(d.w, BusyRetryAfter)
		auth.GitText(d.w, http.StatusTooManyRequests, "you are already downloading a ZIP from this node: "+
			"retry in "+seconds(BusyRetryAfter)+", once it has ended")
		return
	}
	defer leave()
	prefix := d.adm.name + "-" + d.commit[:gitlet.MinAbbrevDigits] + "/"
	var m treeMeasure
	err = s.repairing(ctx, d.r, d.adm.name, synced, inIncarnation, notStarted, func(in cache.Synced) (merr error) {
		m, merr = s.measure(ctx, in, d.commit, prefix)
		return merr
	})
	var large *archiveTooLarge
	switch {
	case errors.As(err, &large):
		s.log.Info("code_archive_too_large", "repo", d.adm.name, "commit", d.commit, "bound", large.bound,
			"entries", large.m.entries, "bytes", large.m.bytes, "names", large.m.nameBytes)
		auth.GitText(d.w, http.StatusBadRequest, sentence(large.sentence()))
		return
	case err != nil:
		s.syncFailed(d.w, d.r, d.adm.name, err)
		return
	}
	bound := s.archiveBound(m)
	if bound > s.archiveFree {
		release, terr := s.archives.take(ctx)
		if terr != nil {
			if errors.Is(terr, errArchivesBusy) {
				held, places := s.archives.inUse()
				s.log.Info("code_archive_busy", "repo", d.adm.name, "reason", "places", "held", held,
					"places", places, "operator_id", d.adm.operatorID)
				retry(d.w, BusyRetryAfter)
				auth.GitText(d.w, http.StatusServiceUnavailable, "every place this node makes a large ZIP in "+
					"stayed busy for "+seconds(s.archives.wait)+": retry in "+seconds(BusyRetryAfter)+
					", or against another node")
				return
			}
			s.syncFailed(d.w, d.r, d.adm.name, terr)
			return
		}
		defer release()
	}
	file := d.adm.name + "-" + d.commit[:gitlet.MinAbbrevDigits] + ".zip"
	d.out.holdOpening(0)
	d.out.bound(bound)
	d.out.headers = func(h http.Header, _ []byte) {
		pagepolicy.Download(h)
		h.Set("Content-Type", "application/zip")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file}))
		h.Set("Cache-Control", downloadCacheControl)
	}
	var cpu time.Duration
	err = s.repairing(ctx, d.r, d.adm.name, synced, inIncarnation, d.out.Started, func(in cache.Synced) error {
		d.out.again()
		// --worktree-attributes: a bare copy then reads no tree's
		// .gitattributes, so every entry is its blob's bytes.
		res, gerr := s.runGit(ctx, gitproc.Cmd{Dir: in.Dir, Dropping: in.Dropping, Repairer: in.Repairer,
			Args:   []string{"archive", "--worktree-attributes", "--format=zip", "--prefix=" + prefix, d.commit},
			Config: s.o.Cache.ServeConfig(), Stdout: d.out, Class: gitproc.ClassTransfer, Share: d.adm.share(),
			Deadline: min(s.archiveDeadline, gitproc.DownloadDeadline(bound)), Stall: s.o.Timing.Stall})
		cpu = res.CPU
		return gerr
	})
	d.finish(ctx, err, cpu, bound)
}

// finish ends a download's stream: a failure before the first byte is
// answered with its status, one after it cuts the connection (a cleanly ended
// ZIP is taken as whole); a begun answer is recorded and logged with what it sent.
func (d *downloading) finish(ctx context.Context, err error, cpu time.Duration, bound int64) {
	if err == nil {
		err = d.out.release()
	}
	if !d.out.Started() {
		d.s.syncFailed(d.w, d.r, d.adm.name, err)
		return
	}
	d.s.downloaded(ctx, d.adm, d.kind, d.out.Sent())
	d.logged(http.StatusOK, cpu)
	if err == nil {
		d.out.End()
		return
	}
	switch {
	case d.s.endedBy(d.r) != "":
		d.s.log.Debug("code_git_abandoned", "repo", d.adm.name, "path", d.r.URL.Path, "by", d.s.endedBy(d.r),
			"error", err)
	case errors.Is(err, errPastBound):
		d.s.log.Warn("code_archive_past_bound", "repo", d.adm.name, "commit", d.commit, "bound", bound,
			"sent", d.out.Sent(), "operator_id", d.adm.operatorID, "error", err)
	default:
		d.s.log.Warn(failedLine(d.r), "repo", d.adm.name, "kind", d.kind, "commit", d.commit,
			"reason", stopReason(err), "sent", d.out.Sent(), "error", err)
	}
	panic(http.ErrAbortHandler)
}

// stopReason is how a git process past its first byte ended, as gitStopped
// names it, or "git" where git failed by itself.
func stopReason(err error) string {
	switch {
	case errors.Is(err, gitproc.ErrDeadline):
		return "deadline"
	case errors.Is(err, gitproc.ErrStalled):
		return "stall"
	case errors.Is(err, gitproc.ErrOutput):
		return "output"
	}
	return "git"
}

// logged is a download's line: INFO for a ZIP, DEBUG for a raw file, whose
// one view costs what a REST read does. The request's fetch metadata says
// whether a page elsewhere started it; no path is logged.
func (d *downloading) logged(status int, cpu time.Duration) {
	level := "debug"
	if d.kind == types.FetchedArchive {
		level = "info"
	}
	attrs := []any{"repo", d.adm.name, "kind", d.kind, "status", status, "commit", d.commit,
		"bytes", d.out.Sent(), "took_ms", d.s.now().Sub(d.start).Milliseconds(), "cpu_ms", cpu.Milliseconds(),
		"operator_id", d.adm.operatorID, "seat", d.adm.seat,
		"sec_fetch_site", fetchMeta(d.r.Header, "Sec-Fetch-Site", "same-origin", "same-site", "cross-site", "none"),
		"sec_fetch_user", fetchMeta(d.r.Header, "Sec-Fetch-User", "?1")}
	if level == "info" {
		d.s.log.Info("code_download", attrs...)
		return
	}
	d.s.log.Debug("code_download", attrs...)
}

// fetchMeta is a Fetch Metadata header as a log carries it: one of the values
// the spec defines, "" where absent and "other" otherwise, so no client's text
// reaches the line.
func fetchMeta(h http.Header, name string, known ...string) string {
	v, ok := h[http.CanonicalHeaderKey(name)]
	switch {
	case !ok || len(v) == 0:
		return ""
	case len(v) == 1 && slices.Contains(known, v[0]):
		return v[0]
	}
	return "other"
}

// downloaded files a download of a unit's repository: one read, the bytes it
// was sent (0 for a 304, which confirms the file's content as a read would).
func (s *Server) downloaded(ctx context.Context, adm admission, kind string, bytes int64) {
	p := adm.principal()
	if adm.repo.Visibility != gitlet.VisibilityUnit || p == (gitlet.Principal{}) {
		return
	}
	s.o.Fetched(ctx, types.CodeFetched{RepoID: adm.repo.ID, RepoName: adm.name,
		PrincipalKind: types.CodeReaderKind(p.Kind), Principal: p.Actor(), Seat: p.Seat,
		Via: types.FetchedViaGit, Kind: kind, Reads: 1, Bytes: bytes})
}

// treeMeasure is what a ZIP of a commit is made of, counted exactly: its
// entries (the prefix's own directory among them), its files' bytes, and its
// entries' names as the ZIP spells them.
type treeMeasure struct {
	entries, bytes, nameBytes int64
}

// archiveTooLarge is a commit a ZIP is refused for before its first byte: past
// one of the measure's bounds, or holding a name a ZIP cannot carry.
type archiveTooLarge struct {
	bound        string // entries, bytes, names or name
	limit, count int64
	m            treeMeasure
}

func (e *archiveTooLarge) Error() string {
	return fmt.Sprintf("host: the commit's ZIP passes its %s bound (%d, at most %d)", e.bound, e.count, e.limit)
}

// sentence is the refusal's words: what was passed and the remedy.
func (e *archiveTooLarge) sentence() string {
	const remedy = "; clone it instead: git clone --depth 1"
	switch e.bound {
	case "entries":
		return fmt.Sprintf("this commit has more than %d entries, the most a ZIP is made of here%s", e.limit, remedy)
	case "bytes":
		return fmt.Sprintf("this commit has more than %d bytes of files, the most a ZIP is made of here%s",
			e.limit, remedy)
	case "names":
		return fmt.Sprintf("this commit's paths come to more than %d bytes, the most a ZIP is made of here%s",
			e.limit, remedy)
	}
	return fmt.Sprintf("this commit holds a path of more than %d bytes (with the ZIP's folder), which a ZIP "+
		"cannot carry%s", e.limit, remedy)
}

// measure is ONE `ls-tree -r -t -l -z` of commit in the copy in, counted as a
// ZIP prefixed prefix spells it, and ended at the first bound it passes.
func (s *Server) measure(ctx context.Context, in cache.Synced, commit, prefix string) (treeMeasure, error) {
	m := treeMeasure{entries: 1, nameBytes: int64(len(prefix))}
	recs := gitproc.NewRecords(func(rec []byte) error {
		e, err := gitproc.ParseTreeEntry(rec)
		if err != nil {
			return err
		}
		name := int64(len(prefix) + len(e.Path))
		if e.Type == "tree" || e.Type == "commit" {
			name++
		}
		if e.Type == "blob" {
			m.bytes += e.Size
		}
		m.entries++
		m.nameBytes += name
		switch {
		case name > s.archiveMaxName:
			return &archiveTooLarge{bound: "name", limit: s.archiveMaxName, count: name, m: m}
		case m.entries > s.archiveMaxEntries:
			return &archiveTooLarge{bound: "entries", limit: s.archiveMaxEntries, count: m.entries, m: m}
		case m.bytes > s.archiveMaxBytes:
			return &archiveTooLarge{bound: "bytes", limit: s.archiveMaxBytes, count: m.bytes, m: m}
		case m.nameBytes > s.archiveMaxNames:
			return &archiveTooLarge{bound: "names", limit: s.archiveMaxNames, count: m.nameBytes, m: m}
		}
		return nil
	})
	res, err := s.runGit(ctx, gitproc.Cmd{Dir: in.Dir, Dropping: in.Dropping, Repairer: in.Repairer,
		Args: []string{"ls-tree", "-r", "-t", "-l", "-z", commit}, Config: s.o.Cache.ServeConfig(),
		Stdout: recs, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline})
	if res.Damage != nil {
		return treeMeasure{}, damaged("an archive's measure", res.Damage, err)
	}
	if err == nil {
		err = recs.Close()
	}
	var large *archiveTooLarge
	switch {
	case errors.As(err, &large):
		return treeMeasure{}, large
	case errors.Is(err, gitproc.ErrLineTooLong):
		// A record past gitproc.MaxLine holds a path within its header's 80
		// bytes of a ZIP name's limit, or past it.
		return treeMeasure{}, &archiveTooLarge{bound: "name", limit: zipNameMax, count: gitproc.MaxLine, m: m}
	case err != nil:
		return treeMeasure{}, fmt.Errorf("host: an archive's measure: %w", err)
	}
	return m, nil
}

// archivePlaces are a node's places to make a large ZIP in, and the
// principals making one now.
type archivePlaces struct {
	mu     sync.Mutex
	held   map[gitproc.Share]int
	free   chan struct{}
	places int
	wait   time.Duration
}

// errArchiving is a principal already making ArchivesPerPrincipal ZIPs here;
// errArchivesBusy is every place staying busy for the wait.
var (
	errArchiving    = errors.New("host: this principal is already making a ZIP here")
	errArchivesBusy = errors.New("host: every archive place stayed busy")
)

// newArchivePlaces is places places, each waited for up to wait.
func newArchivePlaces(places int, wait time.Duration) *archivePlaces {
	p := &archivePlaces{held: map[gitproc.Share]int{}, free: make(chan struct{}, places), places: places,
		wait: wait}
	for range places {
		p.free <- struct{}{}
	}
	return p
}

// enter marks who as making a ZIP NOW, before anything waits, so two of one
// principal's requests never both wait; leave clears the mark.
func (p *archivePlaces) enter(who gitproc.Share) (leave func(), err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held[who] >= ArchivesPerPrincipal {
		return nil, errArchiving
	}
	p.held[who]++
	return sync.OnceFunc(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.held[who]--; p.held[who] <= 0 {
			delete(p.held, who)
		}
	}), nil
}

// take waits up to p.wait for a place, or until ctx ends; release gives it back.
func (p *archivePlaces) take(ctx context.Context) (release func(), err error) {
	t := time.NewTimer(p.wait)
	defer t.Stop()
	select {
	case <-p.free:
		return sync.OnceFunc(func() { p.free <- struct{}{} }), nil
	case <-t.C:
		return nil, errArchivesBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// inUse is how many places are held now, and how many the node has.
func (p *archivePlaces) inUse() (held, places int) {
	return p.places - len(p.free), p.places
}
