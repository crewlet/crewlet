package artifactserve

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve/ticket"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE BYTES: one file of a ticket's version, every byte checked against the
// row that names it.

// bodyCacheBytes bounds what the origin keeps of bytes it has read and checked:
// document templates and the gzip of small compressible files, keyed by the
// object's digest, so a popular artifact is read from the object store once
// per node rather than once per viewer. Sixty-four mebibytes holds hundreds of
// typical pages and their bundles beside the engine's other caches.
const bodyCacheBytes = 64 << 20

// gzipKeepMax is the largest compressible file whose gzip is computed once and
// kept; a larger one is compressed as it streams and kept nowhere.
const gzipKeepMax = 4 << 20

// headUnit is how much of a file is read before the status is written: enough
// to know the store answers at all, so an unreachable object is a 503 rather
// than a 200 with nothing after it.
const headUnit = 32 << 10

// maxSubresourceAge is the longest a browser keeps a ticketed file: an hour,
// the ticket's own lifetime, and never past the ticket.
const maxSubresourceAge = 3600

// ticketed answers one request under a ticket, and the destination it was
// served as (empty for a refusal, which is counted where the request was).
func (h *Handler) ticketed(w http.ResponseWriter, r *http.Request, token, file string) string {
	ctx := r.Context()
	claims, opened, err := h.tickets.Open(token)
	switch opened {
	case ticket.Valid:
	case ticket.Expired:
		h.ticketRefused(w, &refusal{status: http.StatusUnauthorized, reason: reasonExpired})
		return ""
	default:
		log.DebugContext(ctx, "artifacts_serve_ticket_invalid", "error", err)
		h.ticketRefused(w, &refusal{status: http.StatusNotFound, reason: reasonInvalid})
		return ""
	}
	if !h.origins.Serves(r.Host, claims.Artifact) {
		h.ticketRefused(w, &refusal{status: http.StatusNotFound, reason: reasonHost})
		return ""
	}
	dest := fetchDest(r)
	if dest == "document" {
		// A TOP-LEVEL OPEN OF A TICKET URL is refused, so a ticket cannot be
		// pasted into a tab and presented as a standalone page: a frame's
		// document arrives as `iframe`. A browser that sends no Fetch
		// Metadata is admitted, which costs nothing the policy's sandbox
		// does not already cover.
		h.ticketRefused(w, &refusal{status: http.StatusNotFound, reason: reasonNavigation})
		return ""
	}
	documentDest := dest == "iframe" || dest == "frame" || dest == "embed" || dest == "object"
	asset, isAsset := strings.CutPrefix(file, manifest.ReservedAssetsPrefix)
	path := ""
	if !isAsset && file != "" {
		clean, err := manifest.CleanPath(file)
		if err != nil || clean != file {
			h.refuse(w, http.StatusNotFound, 0)
			return ""
		}
		path = clean
	}
	got, v, ref := h.authorize(ctx, claims, documentDest)
	if ref != nil {
		h.ticketRefused(w, ref)
		return ""
	}
	if isAsset {
		return h.asset(w, r, claims, asset, got, documentDest)
	}
	if path == "" {
		path = v.Manifest.Entry
	}
	entry, ok := manifest.EntryAt(v.Manifest, path)
	if !ok {
		h.refuse(w, http.StatusNotFound, 0)
		return ""
	}
	if documentType(entry.ContentType, documentDest) {
		// ANY DOCUMENT IS DECIDED AGAIN, whatever its destination said: a
		// page fetched as data is still a page.
		if !got.fresh {
			if _, _, ref := h.authorize(ctx, claims, true); ref != nil {
				h.ticketRefused(w, ref)
				return ""
			}
		}
		h.serveDocument(w, r, claims, entry.Object, entry.ContentType, entry.Script)
		return destDocument
	}
	h.serveFile(w, r, claims, entry.Object, entry.ContentType)
	return destSubresource
}

// asset answers one file of the artifact's asset store, which the ticket must
// grant ([artifacts.CapabilityAssets]).
func (h *Handler) asset(w http.ResponseWriter, r *http.Request, claims artifacts.TicketClaims,
	raw string, got verdict, documentDest bool) string {

	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw ||
		!slices.Contains(claims.Capabilities, artifacts.CapabilityAssets) {
		h.refuse(w, http.StatusNotFound, 0)
		return ""
	}
	assets, err := h.reads.Assets(r.Context(), claims.Artifact)
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		h.ticketRefused(w, refused())
		return ""
	case err != nil:
		log.WarnContext(r.Context(), "artifacts_serve_rows_unavailable",
			"artifact", claims.Artifact.String(), "error", err)
		h.ticketRefused(w, unavailable(err))
		return ""
	}
	i := slices.IndexFunc(assets, func(a artifactsdomain.Asset) bool { return a.ID == id })
	if i < 0 {
		h.refuse(w, http.StatusNotFound, 0)
		return ""
	}
	a := assets[i]
	if documentType(a.ContentType, documentDest) {
		if !got.fresh {
			if _, _, ref := h.authorize(r.Context(), claims, true); ref != nil {
				h.ticketRefused(w, ref)
				return ""
			}
		}
		// AN ASSET CARRIES NO SCRIPT FACTS: the linter reads a version's
		// files, never the asset store, so a document there runs no script.
		h.serveDocument(w, r, claims, a.Object, a.ContentType, nil)
		return destDocument
	}
	h.serveFile(w, r, claims, a.Object, a.ContentType)
	return destSubresource
}

// ticketRefused answers a refusal and counts it under its reason.
func (h *Handler) ticketRefused(w http.ResponseWriter, ref *refusal) {
	if ref.reason != "" {
		h.metrics.Add(metrics.ArtifactsTicketRefusals, 1, metrics.Attrs{"reason": ref.reason})
	}
	h.refuse(w, ref.status, ref.retry)
}

// serveDocument answers a document: its stored bytes with a fresh nonce
// substituted for the placeholder the linter wrote into every script element,
// under the document policy built for that nonce.
//
// NO PARSE, NO ETAG, NO RANGE. The substitution is a byte replacement on a
// template read and checked once per node; the body differs per response, so
// no validator could name it, and the response is never stored anywhere.
func (h *Handler) serveDocument(w http.ResponseWriter, r *http.Request, c artifacts.TicketClaims,
	obj objstore.Object, contentType string, script *artifacts.ScriptPolicy) {

	template, err := h.template(r.Context(), obj)
	if err != nil {
		h.contentFailed(w, r, c, obj, err)
		return
	}
	nonce, err := randomToken()
	if err != nil {
		log.ErrorContext(r.Context(), "artifacts_serve_nonce_failed", "error", err)
		h.refuse(w, http.StatusInternalServerError, 0)
		return
	}
	body := bytes.ReplaceAll(template, []byte(artifacts.NoncePlaceholder), []byte(nonce))
	header := w.Header()
	header.Set("Content-Type", contentType)
	pagepolicy.ArtifactDocument(header, h.origins.Policy(c.Artifact), nonce, script)
	if compressibleType(contentType) {
		header.Add("Vary", "Accept-Encoding")
		if pagepolicy.AcceptsGzip(r.Header) {
			if gz := pagepolicy.GzipSmaller(body, gzip.DefaultCompression); gz != nil {
				body = gz
				header.Set("Content-Encoding", "gzip")
			}
		}
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := newPacedWriter(w).Write(body); err != nil {
		log.InfoContext(r.Context(), "artifacts_serve_cut", "artifact", c.Artifact.String(),
			"error", err)
		panic(http.ErrAbortHandler)
	}
}

// template is a document's stored bytes, read and checked once and kept.
func (h *Handler) template(ctx context.Context, obj objstore.Object) ([]byte, error) {
	key := "t:" + string(obj.Hash)
	if body, ok := h.bodies.get(key); ok {
		return body, nil
	}
	body, err := h.readWhole(ctx, obj)
	if err != nil {
		return nil, err
	}
	h.bodies.put(key, body)
	return body, nil
}

// gzipped is a small compressible file's gzip, computed once and kept: nil,
// and nil error, where gzip is not smaller.
func (h *Handler) gzipped(ctx context.Context, obj objstore.Object) ([]byte, error) {
	key := "g:" + string(obj.Hash)
	if body, ok := h.bodies.get(key); ok {
		return body, nil
	}
	whole, err := h.readWhole(ctx, obj)
	if err != nil {
		return nil, err
	}
	body := pagepolicy.GzipSmaller(whole, gzip.DefaultCompression)
	if body == nil {
		body = []byte{}
	}
	h.bodies.put(key, body)
	return body, nil
}

// readWhole reads one object to its end through the checking reader: every
// byte hashed and counted, and nothing returned unless the whole matches the
// row.
func (h *Handler) readWhole(ctx context.Context, obj objstore.Object) ([]byte, error) {
	rc, err := h.objects.Open(ctx, obj)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	body := make([]byte, 0, obj.Size)
	buf := bytes.NewBuffer(body)
	if _, err := buf.ReadFrom(io.LimitReader(rc, obj.Size+1)); err != nil {
		return nil, err
	}
	if int64(buf.Len()) != obj.Size {
		return nil, fmt.Errorf("%w: %s read %d of %d bytes", objstore.ErrCorrupt, obj.Key,
			buf.Len(), obj.Size)
	}
	return buf.Bytes(), nil
}

// serveFile answers a file a document loads.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, c artifacts.TicketClaims,
	obj objstore.Object, contentType string) {

	header := w.Header()
	pagepolicy.ArtifactSubresource(header, h.origins.Policy(c.Artifact))
	header.Set("Content-Type", contentType)
	header.Set("Cache-Control", h.subresourceCache(c))
	identity := entityTag(obj)
	compress := compressibleType(contentType)
	if compress {
		header.Add("Vary", "Accept-Encoding")
	}
	gz := compress && pagepolicy.AcceptsGzip(r.Header)
	if gz && obj.Size <= gzipKeepMax {
		body, err := h.gzipped(r.Context(), obj)
		if err != nil {
			h.contentFailed(w, r, c, obj, err)
			return
		}
		if len(body) > 0 {
			header.Set("Content-Encoding", "gzip")
			header.Set("ETag", pagepolicy.GzipTag(identity))
			// http.ServeContent for its conditional and ranged answers
			// over the kept body, written at the object store's pace.
			http.ServeContent(newPacedWriter(w), r, "", time.Time{}, bytes.NewReader(body))
			return
		}
		gz = false
	}
	ranged := r.Header.Get("Range") != ""
	if gz && ranged {
		// A RANGE IS SERVED FROM THE IDENTITY BYTES of a file too large to
		// keep compressed, since a range of a stream compressed as it
		// goes has no stable offsets.
		gz = false
	}
	etag := identity
	if gz {
		etag = pagepolicy.GzipTag(identity)
	}
	header.Set("ETag", etag)
	if noneMatch(r.Header.Get("If-None-Match"), etag) {
		header.Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if gz {
		h.stream(w, r, c, obj, 0, obj.Size, http.StatusOK, true)
		return
	}
	start, length, status := int64(0), obj.Size, http.StatusOK
	if ranged && ifRange(r.Header.Get("If-Range"), identity) {
		s, n, ok, satisfiable := parseRange(r.Header.Get("Range"), obj.Size)
		switch {
		case ok && !satisfiable:
			header.Set("Content-Range", "bytes */"+strconv.FormatInt(obj.Size, 10))
			header.Del("Content-Type")
			header.Set("Content-Length", "0")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		case ok:
			start, length, status = s, n, http.StatusPartialContent
			header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, s+n-1, obj.Size))
		}
	}
	header.Set("Accept-Ranges", "bytes")
	h.stream(w, r, c, obj, start, length, status, false)
}

// subresourceCache is a ticketed file's Cache-Control: private, so no shared
// cache keeps it; immutable, since a version never changes; and never past
// the ticket, so a browser does not keep serving a file its viewer's ticket
// has stopped opening.
func (h *Handler) subresourceCache(c artifacts.TicketClaims) string {
	age := int64(c.Expires.Sub(h.now()) / time.Second)
	age = max(0, min(age, maxSubresourceAge))
	return "private, max-age=" + strconv.FormatInt(age, 10) + ", immutable"
}

// stream sends length bytes of obj from start, read through the checking
// reader, at the object store's pace, gzipped as it goes where gz is set.
//
// THE STATUS WAITS FOR THE FIRST BYTES, so a store that cannot answer is a 503
// rather than a 200 with nothing after it. After that the only honest signal
// left for a failure is a body shorter than promised, so a failure cuts the
// connection: the checking reader holds back the byte that would complete a
// file whose content it could not verify, so a damaged object always reads
// short of its length rather than whole.
//
// A RANGE IS CHECKED FOR ITS LENGTH, and for the whole digest only where it
// reaches the end of the object: the bytes before it are read and discarded
// rather than fetched as a range, which keeps every response's memory
// constant (a video's open-ended range from many viewers would otherwise be a
// whole file per viewer in memory) at the cost of reading at most the file's
// size from the store, and every file is at most sixteen mebibytes.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, c artifacts.TicketClaims,
	obj objstore.Object, start, length int64, status int, gz bool) {

	ctx := r.Context()
	rc, err := h.objects.Open(ctx, obj)
	if err != nil {
		h.contentFailed(w, r, c, obj, err)
		return
	}
	defer func() { _ = rc.Close() }()
	if start > 0 {
		if _, skipErr := io.CopyN(io.Discard, rc, start); skipErr != nil {
			h.contentFailed(w, r, c, obj, skipErr)
			return
		}
	}
	src := io.LimitReader(rc, length)
	head := make([]byte, min(int64(headUnit), length))
	n, err := io.ReadFull(src, head)
	if err != nil && length > 0 {
		h.contentFailed(w, r, c, obj, err)
		return
	}
	header := w.Header()
	if gz {
		header.Set("Content-Encoding", "gzip")
	} else {
		header.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	body := io.MultiReader(bytes.NewReader(head[:n]), endAt(src, length-int64(n)))
	if gz {
		err = copyPacedGzip(w, body)
	} else {
		err = copyPaced(w, body)
	}
	if err != nil {
		log.InfoContext(ctx, "artifacts_serve_cut", "artifact", c.Artifact.String(),
			"key", obj.Key.String(), "error", err)
		panic(http.ErrAbortHandler)
	}
}

// endAt is src, which must hold exactly n more bytes, ending in io.EOF itself
// after them and failing if it ends before: a LimitReader ends early without
// saying so, and a cut that read as an end would close a response short as
// though it were whole.
func endAt(src io.Reader, n int64) io.Reader { return &exactly{r: src, left: n} }

type exactly struct {
	r    io.Reader
	left int64
}

func (e *exactly) Read(p []byte) (int, error) {
	if e.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.r.Read(p)
	e.left -= int64(n)
	switch {
	case e.left == 0:
		return n, io.EOF
	case err == io.EOF: //nolint:errorlint // the end is io.EOF itself
		return n, fmt.Errorf("%w: the object ended %d bytes early", objstore.ErrCorrupt, e.left)
	}
	return n, err
}

// contentFailed answers a read of the object store that failed before the
// status was written.
//
// DAMAGED IS NOT UNREACHABLE. An object whose bytes do not match the row, or
// one a retained version names that the store no longer holds, is the
// engine's own fault, which no retry clears: 500. One the store did not
// answer for is a 503 with the hint a broker election takes.
func (h *Handler) contentFailed(w http.ResponseWriter, r *http.Request, c artifacts.TicketClaims,
	obj objstore.Object, err error) {

	switch {
	case errors.Is(err, objstore.ErrCorrupt), errors.Is(err, objstore.ErrNotFound):
		log.ErrorContext(r.Context(), "artifacts_serve_content_damaged",
			"artifact", c.Artifact.String(), "key", obj.Key.String(), "error", err)
		h.refuse(w, http.StatusInternalServerError, 0)
	default:
		log.WarnContext(r.Context(), "artifacts_serve_content_unavailable",
			"artifact", c.Artifact.String(), "key", obj.Key.String(), "error", err)
		h.refuse(w, http.StatusServiceUnavailable, statelog.ElectionRetryHint)
	}
}

// entityTag is a file's strong entity tag: the first twenty bytes of its
// digest, which names its content and nothing else.
func entityTag(obj objstore.Object) string {
	return `"` + string(obj.Hash)[:40] + `"`
}

// noneMatch reports whether an If-None-Match header matches etag under weak
// comparison (RFC 9110 §13.1.2): a list, `*`, or a weak tag of the same value.
func noneMatch(header, etag string) bool {
	if header == "" {
		return false
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, member := range strings.Split(header, ",") {
		member = strings.TrimSpace(member)
		if member == "*" || strings.TrimPrefix(member, "W/") == want {
			return true
		}
	}
	return false
}

// ifRange reports whether a Range may be honoured under an If-Range header: no
// header, or one naming exactly this strong tag. A date or another tag serves
// the whole file, which is always a correct answer.
func ifRange(header, etag string) bool {
	return header == "" || header == etag
}

// parseRange reads a single byte range. ok is false for a header this serves
// whole (more than one range, another unit, or bad syntax, each of which RFC
// 9110 lets a server ignore); satisfiable is false for a well-formed range
// that names no byte of the file.
func parseRange(header string, size int64) (start, length int64, ok, satisfiable bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false, false
	}
	first, last, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false, false
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false, false
		}
		if n == 0 || size == 0 {
			return 0, 0, true, false
		}
		n = min(n, size)
		return size - n, n, true, true
	}
	s, err := strconv.ParseInt(first, 10, 64)
	if err != nil || s < 0 {
		return 0, 0, false, false
	}
	e := size - 1
	if last != "" {
		if e, err = strconv.ParseInt(last, 10, 64); err != nil || e < s {
			return 0, 0, false, false
		}
		e = min(e, size-1)
	}
	if s >= size {
		return 0, 0, true, false
	}
	return s, e - s + 1, true, true
}

// The media table's facts by served type: what is gzipped, and what a browser
// would run if it navigated to it.
var (
	compressible = map[string]bool{}
	scriptable   = map[string]bool{}
)

func init() {
	for _, row := range mediatype.Table() {
		for _, t := range row.Types {
			compressible[t.Essence()] = t.Compressible
			scriptable[t.Essence()] = t.Scriptable
		}
	}
}

// essenceOf is a served type without parameters, lower case.
func essenceOf(contentType string) string {
	if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
		return strings.ToLower(parsed)
	}
	head, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(head))
}

func compressibleType(contentType string) bool { return compressible[essenceOf(contentType)] }

// documentType reports a file served as a document: HTML whatever asked for
// it, and any other type a browser runs script in (SVG, XML) when the frame
// itself navigates to it.
func documentType(contentType string, documentDest bool) bool {
	essence := essenceOf(contentType)
	return essence == "text/html" || (documentDest && scriptable[essence])
}

// ---- pacing ------------------------------------------------------------- //
//
// ON THE WALL CLOCK, ALWAYS: a write deadline is the connection's, read by the
// network stack against real time, so the handler's injectable clock (which
// decides tickets and verdict ages) has no say in it.

// copyPaced writes src to the client a mebibyte at a time, each under its own
// [objstore.MiBPace] write deadline, set once the mebibyte is in hand, so the
// time spent waiting on the store for it is never the client's. Done only where
// src ends in io.EOF itself ([objstore.Fill]).
//
// THE OBJECT STORE'S ONE FLOOR, the pace every leg a file's bytes cross is
// held to: the server's write timeout is deliberately unset, so a route that
// streams a body bounds it itself, and a client that opened a download and
// never read it would otherwise hold a handler and a connection for ever.
func copyPaced(w http.ResponseWriter, src io.Reader) error {
	rc := http.NewResponseController(w)
	buf := make([]byte, objstore.MiB)
	for {
		n, end, err := objstore.Fill(src, buf)
		if err != nil {
			return err
		}
		if n > 0 {
			if err := deadline(rc); err != nil {
				return err
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
		}
		if end {
			return nil
		}
	}
}

// copyPacedGzip is [copyPaced] for a body compressed as it streams: each
// mebibyte read from the store is compressed into memory, and what that
// produced is written under a fresh deadline.
func copyPacedGzip(w http.ResponseWriter, src io.Reader) error {
	rc := http.NewResponseController(w)
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	buf := make([]byte, objstore.MiB)
	send := func() error {
		if out.Len() == 0 {
			return nil
		}
		if err := deadline(rc); err != nil {
			return err
		}
		_, err := w.Write(out.Bytes())
		out.Reset()
		return err
	}
	for {
		n, end, err := objstore.Fill(src, buf)
		if err != nil {
			return err
		}
		if _, err := zw.Write(buf[:n]); err != nil {
			return err
		}
		if end {
			if err := zw.Close(); err != nil {
				return err
			}
			return send()
		}
		if err := zw.Flush(); err != nil {
			return err
		}
		if err := send(); err != nil {
			return err
		}
	}
}

// deadline arms the connection's write deadline for one mebibyte. A writer that
// cannot carry one has no connection to bound.
func deadline(rc *http.ResponseController) error {
	err := rc.SetWriteDeadline(time.Now().Add(objstore.MiBPace))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// pacedWriter is a response writer whose body goes out a mebibyte at a time
// under the same deadline: for a body already in memory, written by
// http.ServeContent, where nothing waits on the store between writes.
type pacedWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func newPacedWriter(w http.ResponseWriter) *pacedWriter {
	return &pacedWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
}

func (p *pacedWriter) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		n := min(len(b), objstore.MiB)
		if err := deadline(p.rc); err != nil {
			return written, err
		}
		m, err := p.ResponseWriter.Write(b[:n])
		written += m
		if err != nil {
			return written, err
		}
		b = b[n:]
	}
	return written, nil
}

// Unwrap is what lets http.ResponseController reach the connection through
// this writer.
func (p *pacedWriter) Unwrap() http.ResponseWriter { return p.ResponseWriter }
