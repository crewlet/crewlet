package assistantapi

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// A CONVERSATION'S FILES OVER HTTP: the person's upload of an image to ask
// about, their download of any file their conversation keeps, and an
// administrator's audited download of one.
//
// # The upload is the image's bytes, typed by its bytes
//
// The body is the raw file and its name rides the `name` query parameter, so
// nothing about the file is parsed out of a multipart envelope. The declared
// Content-Type must be an image type or application/octet-stream, none of which
// a cross-site form can send without a preflight, so the Origin gate in front
// of every guarded write is the second lock rather than the only one; and it is
// never what the file is kept as: the type is read from the bytes
// (assistant.SniffFile) against the image types a person may attach, so HTML
// named `.png` is refused. The body is bounded at wire.FileMaxBytes, refused by
// its declared length before a byte is read, and otherwise after one byte past
// the bound.
//
// # A download is never a page
//
// Every download, the owner's and the audited one alike, is served as an
// attachment of the type its bytes were sniffed as, beside
// `X-Content-Type-Options: nosniff`, `Content-Security-Policy: sandbox` and
// `Cache-Control: private, no-store`, so no browser renders a kept file as a
// document of this origin or keeps a copy of it.

// The file routes, as the API mounts them.
const (
	UploadPattern        = "POST /assistant/conversations/{id}/files"
	DownloadPattern      = "GET /assistant/conversations/{id}/files/{file}"
	AuditDownloadPattern = "POST /assistant/audit/conversations/{id}/files/{file}/read"
)

// fileNamespace is the namespace an uploaded file's id is derived in from the
// upload's operation and its conversation, so a retried upload is one file and
// a key sent again to another conversation is another. FIXED for the life of
// the format.
var fileNamespace = uuid.MustParse("6f1c2b9e-3a7d-5e40-b8f1-0d4c6a2e9b57")

// uploadTypes are the Content-Types an upload may declare: the attachable
// image types and the generic one a client sends when it does not know.
var uploadTypes = append(slices.Clone(wire.AttachmentMediaTypes), "application/octet-stream")

// upload serves `POST /assistant/conversations/{id}/files?name=<file name>`.
func (s *Surface) upload(w http.ResponseWriter, r *http.Request) {
	conversation, ok := pathID(w, r)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	if strings.TrimSpace(name) == "" {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			httpjson.Detail{"detail": "name is the file's name, which its download keeps"})
		return
	}
	if declared, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil ||
		!slices.Contains(uploadTypes, declared) {
		httpjson.FailWithFields(w, http.StatusUnsupportedMediaType,
			httpjson.CodeAssistantFileRefused, httpjson.Detail{"detail": "send the file's " +
				"bytes as the body, declared as one of " + words.Or(uploadTypes)})
		return
	}
	if r.ContentLength > wire.FileMaxBytes {
		httpjson.Refuse(w, httpjson.ErrTooLarge)
		return
	}
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	p, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	if !s.eligible(w, r, p) {
		return
	}
	raw, err := httpjson.ReadBody(w, r, wire.FileMaxBytes)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	mediaType, kept, err := assistant.SniffFile(name, raw, wire.AttachmentMediaTypes)
	if err != nil {
		httpjson.FailWithFields(w, http.StatusUnsupportedMediaType,
			httpjson.CodeAssistantFileRefused, httpjson.Detail{"detail": strings.TrimPrefix(
				err.Error(), assistant.ErrFileRefused.Error()+": ")})
		return
	}
	ref, err := s.files.Store(r.Context(), assistant.Write{Owner: p.ID, By: iam.ActorFor(p),
		Key: key, Steps: []string{"file"}}, assistant.NewFile{
		ID:           uuid.NewSHA1(fileNamespace, []byte(conversation.String()+"/"+key)),
		Conversation: conversation,
		Origin:       wire.FileAttached, Name: kept, MediaType: mediaType, Content: raw,
	})
	switch {
	case errors.Is(err, assistantdomain.ErrTooManyFiles):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeAssistantFileLimit,
			httpjson.Detail{"limit": assistantdomain.AttachmentsPerConversation})
		return
	case err != nil:
		s.failWrite(w, r, key, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, wire.FileStored{File: ref})
}

// download serves `GET /assistant/conversations/{id}/files/{file}`: one of the
// caller's own files. Anybody else's, or one of another conversation, is
// `404 not_found`.
func (s *Surface) download(w http.ResponseWriter, r *http.Request) {
	conversation, ok := pathID(w, r)
	if !ok {
		return
	}
	file, ok := pathFile(w, r)
	if !ok {
		return
	}
	p, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	stored, content, err := s.files.Open(r.Context(), p.ID, conversation, file)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	serveFile(w, stored.FileRef, content)
}

// pathFile is the `{file}` path value, a file id, or a 404 answered.
func pathFile(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("file"))
	if err != nil || id == uuid.Nil {
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return uuid.Nil, false
	}
	return id, true
}

// serveFile writes a kept file as a download, with every header that keeps a
// browser from rendering it or keeping it.
//
// A TYPE OFF THE KEPT LIST IS SERVED AS OPAQUE BYTES: every file is sniffed
// against that list before it is kept, so this is a row no build wrote, and it
// is never handed to a browser as anything it might render.
func serveFile(w http.ResponseWriter, f wire.FileRef, content []byte) {
	mediaType := f.MediaType
	if !slices.Contains(wire.FileMediaTypes, mediaType) {
		mediaType = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", mediaType)
	h.Set("Content-Disposition", ContentDisposition(f.Name))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// ContentDisposition is an attachment's header for a kept name: the name
// quoted, with every byte outside printable ASCII written as an underscore, and
// beside it RFC 5987's `filename*` carrying the name exactly where it is not
// plain ASCII. A kept name holds no quote, semicolon, backslash or control
// character (assistant.SniffFile refuses them), so the quoted form needs no
// escaping; one that did is answered with a generic name rather than escaped.
func ContentDisposition(name string) string {
	if name == "" || strings.ContainsAny(name, "\"\\;") || !utf8.ValidString(name) {
		name = "download"
	}
	var fallback strings.Builder
	plain := true
	for _, r := range name {
		if r < 0x20 || r > 0x7e {
			fallback.WriteByte('_')
			plain = false
			continue
		}
		fallback.WriteRune(r)
	}
	out := fmt.Sprintf(`attachment; filename="%s"`, fallback.String())
	if !plain {
		out += "; filename*=UTF-8''" + rfc5987(name)
	}
	return out
}

// rfc5987 percent-encodes s as RFC 5987's ext-value: every byte outside its
// attr-char set is %XX.
func rfc5987(s string) string {
	const attr = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			strings.IndexByte(attr, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
