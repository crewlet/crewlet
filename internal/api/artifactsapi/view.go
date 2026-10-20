package artifactsapi

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/paced"
	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// HOW A VERSION IS VIEWED FROM THE API ORIGIN, which never renders one: a frame
// minted for the artifact origin (an HTML page or a design's artboard runs
// there, sandboxed, and nowhere else), a doc's source or a design's canvas as
// JSON for the dashboard to draw itself (it runs no author code), and a file's
// raw bytes as an attachment, text as plain text, under the API's own policy.

// ticketBody names the version a frame opens (zero is the head) and the page
// it opens: empty for a page's entry, one artboard's path for a design.
type ticketBody struct {
	Version  int    `json:"version,omitempty"`
	Document string `json:"document,omitempty"`
}

// ticket mints the frame the dashboard embeds a version in (`POST
// /artifacts/{id}/tickets`), decided by `get_artifact`: a view ticket bound to
// the caller's credential and to the one page it opens (a design's artboard is
// a frame of its own, with a ticket of its own), the artifact origin's URL
// under it, and the sandbox and allow attributes the dashboard sets verbatim. It records that the caller
// opened the artifact (unless it is a gallery's preview, [purposeOf]) and, for
// a read only the admin path admitted, the elevated read, before the frame is
// handed over.
func (s *Service) ticket(w http.ResponseWriter, r *http.Request) {
	if _, ok := pathID(w, r); !ok {
		return
	}
	var body ticketBody
	if !decode(w, r, &body, true) {
		return
	}
	purpose, ok := purposeOf(w, r)
	if !ok {
		return
	}
	if body.Version < 0 {
		invalid(w, "version is a version number, or absent for the head")
		return
	}
	a, ok := s.admitContent(w, r, body.Version)
	if !ok {
		return
	}
	switch {
	case !a.Head.Kind.Framed():
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeArtifactNotFramed, map[string]string{
			"detail": "a " + string(a.Head.Kind) + " artifact is rendered by the dashboard: read " +
				"its source, or its data"})
		return
	case s.frames == nil:
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeArtifactOriginUnset, map[string]string{
			"detail": "api.artifacts.external_url is unset on this deployment, so no frame " +
				"can name where a page loads from"})
		return
	}
	number := body.Version
	if number == 0 {
		number = a.Head.HeadNumber
	}
	v, err := s.reads.Version(r.Context(), a.Head.ID, number)
	if err != nil {
		readFailed(w, r, err, "version "+strconv.Itoa(number))
		return
	}
	lineage, err := auth.LineageOf(a.Principal)
	if err != nil {
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeForbidden, map[string]string{
			"detail": "this credential cannot open a page: a frame is bound to the sign-in " +
				"or the token it was opened through, and this request names none"})
		return
	}
	until, _ := auth.Lifetime(r.Context())
	frame, err := s.frames.For(artifactserve.FrameRequest{
		Principal: a.Principal, Lineage: lineage, Until: until, Head: a.Head,
		Version: v, Document: body.Document, Granted: grant(v.Capabilities, purpose),
		Purpose: purpose,
	})
	switch {
	case errors.Is(err, artifactserve.ErrCredentialEnding):
		httpjson.FailWith(w, http.StatusUnauthorized, httpjson.CodeSessionRevoked,
			map[string]string{"detail": say(err)})
		return
	case errors.Is(err, artifactserve.ErrNoLineage):
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeForbidden,
			map[string]string{"detail": say(err)})
		return
	case errors.Is(err, artifactserve.ErrNoDocument):
		invalid(w, say(err))
		return
	case errors.Is(err, artifactserve.ErrNotServed):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeArtifactNotFramed,
			map[string]string{"detail": say(err)})
		return
	case err != nil:
		log.ErrorContext(r.Context(), "artifacts_frame_unminted", "artifact", a.Head.ID.String(),
			"version", number, "error", err.Error())
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	if purpose != artifacts.TicketPurposePreview {
		s.gate.markOpened(r.Context(), a)
	}
	httpjson.Write(w, http.StatusCreated, frame)
}

// grant is what a frame minted for purpose is granted of what its version
// declares: all of it for an open, and for a preview only what drawing the
// page needs ([artifacts.PreviewCapabilities]: its assets, a datastore the
// dashboard's preview host answers with reads alone, and a newcomer's state
// the host keeps for the frame), so a preview's page offers no viewer and no
// room, and never the reader's own state, whatever the version declares.
func grant(declared []artifacts.Capability, purpose artifacts.TicketPurpose) []artifacts.Capability {
	if purpose != artifacts.TicketPurposePreview {
		return declared
	}
	out := make([]artifacts.Capability, 0, len(artifacts.PreviewCapabilities))
	for _, c := range declared {
		if slices.Contains(artifacts.PreviewCapabilities, c) {
			out = append(out, c)
		}
	}
	return out
}

// purposeOf is why a content read is made (`?purpose=`), and whether it names
// one this build knows: none, for a viewer opening the artifact, or
// `preview`, for a gallery card drawing it small. A purpose this build does
// not know is refused as invalid rather than read as an open, so a client is
// never recorded, granted or reported as something it did not ask to be.
//
// A PREVIEW IS THE SAME READ under the same authorization (admitContent
// decides it before this is asked, and an administrator's elevated read
// through a preview is recorded as any other is), and it records no open: a
// gallery draws a dozen artifacts small as the reader scrolls past them, and
// recording each would fill every Seen by list with people who only browsed
// and reorder the reader's Recently opened.
func purposeOf(w http.ResponseWriter, r *http.Request) (artifacts.TicketPurpose, bool) {
	purpose := artifacts.TicketPurpose(r.URL.Query().Get("purpose"))
	if !purpose.Valid() {
		invalid(w, "purpose is preview, or absent for a viewer opening the artifact")
		return "", false
	}
	return purpose, true
}

// versionEntry is the version the path names and one of its files, for a
// caller a admitted, rendering a refusal itself.
func (s *Service) versionOf(w http.ResponseWriter, r *http.Request, a Admission, n int) (
	artifactsdomain.Version, bool) {

	v, err := s.reads.Version(r.Context(), a.Head.ID, n)
	if err != nil {
		readFailed(w, r, err, "version "+strconv.Itoa(n))
		return artifactsdomain.Version{}, false
	}
	return v, true
}

// downloadHead is how much of a file is read before the response starts:
// enough to know the content can be read at all, so a file whose bytes are
// unreachable answers 503 rather than a 200 with an empty body.
const downloadHead = 32 << 10

// file answers one file of a version as raw bytes (`GET
// /artifacts/{id}/versions/{n}/files/{path...}`), decided by `get_artifact`.
//
// THE API ORIGIN NEVER RENDERS AN ARTIFACT: the answer is an attachment, any
// text and anything a browser could run (HTML, SVG, a script) is served as
// `text/plain`, and the API's own policy (nothing may load, nothing may frame
// it) is set on it explicitly. A page is viewed on the artifact origin, in a
// frame, and nowhere else.
func (s *Service) file(w http.ResponseWriter, r *http.Request) {
	n, ok := pathVersion(w, r)
	if !ok {
		return
	}
	a, ok := s.admitContent(w, r, n)
	if !ok {
		return
	}
	v, ok := s.versionOf(w, r, a, n)
	if !ok {
		return
	}
	want := r.PathValue("path")
	var entry artifacts.Entry
	found := false
	for _, e := range v.Manifest.Files {
		if e.Path == want {
			entry, found = e, true
			break
		}
	}
	if !found {
		gone(w, "version "+strconv.Itoa(n)+" holds no file at "+want)
		return
	}
	body, err := s.objects.Open(r.Context(), entry.Object)
	if err == nil {
		defer body.Close()
		head := make([]byte, min(int64(downloadHead), entry.Object.Size))
		if _, err = io.ReadFull(body, head); err == nil {
			s.sendFile(w, r, a.Head.ID, entry, io.MultiReader(bytes.NewReader(head), body))
			return
		}
	}
	contentFailed(w, r, a.Head.ID, entry.Path, err)
}

// contentFailed renders a file whose bytes could not be read: damaged (the
// store answered with other bytes than the manifest names, which a retry reads
// again, so no 503) or unreachable (which clears when the store answers).
func contentFailed(w http.ResponseWriter, r *http.Request, id artifacts.ID, p string, err error) {
	if errors.Is(err, objstore.ErrCorrupt) {
		log.WarnContext(r.Context(), "artifacts_content_corrupt", "artifact", id.String(),
			"path", p, "error", err.Error())
		httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeContentCorrupt,
			map[string]string{"detail": p + " is named by the version and the object store " +
				"holds content for it that is not what the version records"})
		return
	}
	log.WarnContext(r.Context(), "artifacts_content_unavailable", "artifact", id.String(),
		"path", p, "error", err.Error())
	httpjson.UnavailableWith(w, httpjson.CodeContentUnavailable,
		httpjson.RetrySeconds(statelog.ElectionRetryHint), httpjson.Detail{
			"detail": p + " is named by the version and its content could not be read right now",
		})
}

// rawType is the Content-Type a file is downloaded under on the API origin:
// `text/plain` for every text file and every type a browser could run, the
// media table's own served type for the rest, so nothing downloaded here can
// render or execute on the API's origin even if it is opened.
func rawType(p string) string {
	t, known := mediatype.For(p)
	switch {
	case !known:
		return "application/octet-stream"
	case t.Text || t.Scriptable:
		return "text/plain; charset=utf-8"
	}
	return t.Served
}

// sendFile writes a file's headers and then its content, from src.
func (s *Service) sendFile(w http.ResponseWriter, r *http.Request, id artifacts.ID, e artifacts.Entry,
	src io.Reader) {

	h := w.Header()
	pagepolicy.Set(h, pagepolicy.API)
	h.Set("Content-Type", rawType(e.Path))
	h.Set("Content-Length", strconv.FormatInt(e.Object.Size, 10))
	h.Set("ETag", strconv.Quote(string(e.Object.Hash)))
	h.Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(e.Path)}))
	// A PRIVATE FILE, kept by no cache: the bytes of a version never change,
	// and whether this caller may read them can, the next request decides.
	h.Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	if err := paced.Send(w, src, s.pace); err != nil {
		// THE STATUS IS SENT, so the one honest signal left is a body
		// shorter than its Content-Length: aborting the handler closes the
		// connection rather than ending the response cleanly. The store
		// holds back the byte that would complete a file whose content it
		// could not verify, so a damaged object reaches here too, never a
		// whole response.
		log.WarnContext(r.Context(), "artifacts_download_cut", "artifact", id.String(),
			"path", e.Path, "error", err.Error())
		panic(http.ErrAbortHandler)
	}
}

// Source is a version's entry as text, for the dashboard to render itself: a
// doc's Markdown (which runs no author code, and so is shown on the
// dashboard rather than in a frame), or an HTML page's source.
type Source struct {
	Artifact    artifacts.ID   `json:"artifact"`
	Kind        artifacts.Kind `json:"kind"`
	Version     int            `json:"version"`
	Path        string         `json:"path"`
	ContentType string         `json:"content_type"`
	Size        int64          `json:"size"`
	Text        string         `json:"text"`
}

// source answers a version's entry as text (`GET
// /artifacts/{id}/versions/{n}/source`), decided by `get_artifact`: read back
// through the object store, checked against its digest, at most
// [artifacts.MaxEntryBytes] (the bound every entry is published under). An
// open of a native kind's source records that the caller opened it, as a frame's
// ticket does for a page.
func (s *Service) source(w http.ResponseWriter, r *http.Request) {
	n, ok := pathVersion(w, r)
	if !ok {
		return
	}
	a, ok := s.admitContent(w, r, n)
	if !ok {
		return
	}
	v, ok := s.versionOf(w, r, a, n)
	if !ok {
		return
	}
	entry, named := entryOf(v)
	if !named {
		gone(w, "version "+strconv.Itoa(n)+" names no entry: a native sheet's content is its data")
		return
	}
	text, _, err := s.readText(r.Context(), entry, artifacts.MaxEntryBytes)
	switch {
	case errors.Is(err, errNotText):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeContentRefused, map[string]string{
			"detail": entry.Path + " is not text"})
		return
	case err != nil:
		contentFailed(w, r, a.Head.ID, entry.Path, err)
		return
	}
	// AN ENTRY THE DASHBOARD DRAWS IS OPENED BY READING IT: a native kind,
	// for which no ticket is minted, and a design's canvas, which may be
	// read with no artboard in view and so no ticket minted. The open is
	// recorded here, as a frame's ticket records a page's.
	if spec, _ := a.Head.Kind.Spec(); !spec.OpensEntry() {
		s.gate.markOpened(r.Context(), a)
	}
	httpjson.Write(w, http.StatusOK, Source{
		Artifact: a.Head.ID, Kind: a.Head.Kind, Version: n, Path: entry.Path,
		ContentType: entry.ContentType, Size: entry.Object.Size, Text: text,
	})
}
