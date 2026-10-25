package artifactserve

import (
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/objstore"
)

// THE ADOPTED BRAND ON THE ARTIFACT ORIGIN (ADR-0048): every file an adoption
// serves, at `/_crewlet/brand/<sha256>.<ext>` on every artifact host, and the
// one element the origin places in a framed page pinned to the brand.
//
// PUBLIC, AND ALLOWLISTED. The files are the company's public face, so no
// ticket opens them: a page in any mode, and a font a stylesheet loads with
// an opaque origin, reach them the way they reach the kit. But a hash is
// served only while an adoption names it (artifactsdomain's brand files,
// which the applier writes and prunes), never by looking the hash up in the
// object store, which would let anybody who learnt a private file's digest
// fetch it. An extension is served only as its own type, the one the adopted
// file is.
//
// IMMUTABLE, AND PER COMPANY. A hash names the same bytes for ever, so the
// answer is kept a year, as the kit is; unlike the kit, it is one company's,
// so a shared cache keys it with the host, which docs/guides/artifacts-serving.md
// tells a proxy and an edge to keep.

// brandPrefix is the brand's route on the artifact listener.
const brandPrefix = brandformat.BrandRoute

// brandLinkHash is the only value the origin writes into a page's link: a
// stylesheet's SHA-256, in hex, as the version recorded it.
var brandLinkHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// brandFile answers one file an adoption serves, named `<sha256>.<ext>`.
func (h *Handler) brandFile(w http.ResponseWriter, r *http.Request, name string) {
	raw, ext, ok := strings.Cut(name, ".")
	hash := objstore.Hash(raw)
	if !ok || !hash.Valid() || !slices.Contains(brandformat.ServedExts, ext) {
		h.refuse(w, http.StatusNotFound, 0)
		return
	}
	f, err := h.reads.BrandFile(r.Context(), hash)
	switch {
	case errors.Is(err, artifactsdomain.ErrNotFound):
		h.refuse(w, http.StatusNotFound, 0)
		return
	case err != nil:
		log.WarnContext(r.Context(), "artifacts_serve_brand_unavailable", "hash", raw, "error", err)
		ref := unavailable(err)
		h.refuse(w, ref.status, ref.retry)
		return
	}
	essence, _, _ := strings.Cut(f.ContentType, ";")
	if served, ok := brandformat.Ext(strings.TrimSpace(essence)); !ok || served != ext {
		h.refuse(w, http.StatusNotFound, 0)
		return
	}
	contentType, _ := brandformat.ContentType(ext)
	body, err := h.brandBody(r, f.Object)
	if err != nil {
		h.brandFailed(w, r, f.Object, err)
		return
	}
	header := w.Header()
	pagepolicy.ArtifactPublic(header, h.origins.Secure())
	header.Set("Cache-Control", cacheForever)
	header.Set("Content-Type", contentType)
	etag := `"` + raw + `"`
	if compressibleType(contentType) {
		header.Add("Vary", "Accept-Encoding")
		if pagepolicy.AcceptsGzip(r.Header) {
			if gz := h.brandGzip(f.Object, body); gz != nil {
				body, etag = gz, pagepolicy.GzipTag(etag)
				header.Set("Content-Encoding", "gzip")
			}
		}
	}
	header.Set("ETag", etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// brandBody is one brand file's bytes, read through the checking reader once
// and kept: a brand file is small (a stylesheet, a logo, a face of at most a
// few MiB) and asked for by every page that links it.
func (h *Handler) brandBody(r *http.Request, obj objstore.Object) ([]byte, error) {
	key := "b:" + string(obj.Hash)
	if body, ok := h.bodies.get(key); ok {
		return body, nil
	}
	body, err := h.readWhole(r.Context(), obj)
	if err != nil {
		return nil, err
	}
	h.bodies.put(key, body)
	return body, nil
}

// brandGzip is a brand file's gzip, computed once and kept, nil where gzip is
// not smaller.
func (h *Handler) brandGzip(obj objstore.Object, body []byte) []byte {
	key := "bz:" + string(obj.Hash)
	if gz, ok := h.bodies.get(key); ok {
		if len(gz) == 0 {
			return nil
		}
		return gz
	}
	gz := pagepolicy.GzipSmaller(body, gzip.DefaultCompression)
	if gz == nil {
		gz = []byte{}
	}
	h.bodies.put(key, gz)
	if len(gz) == 0 {
		return nil
	}
	return gz
}

// brandFailed answers a brand file whose bytes could not be read: damaged, or
// a store that did not answer.
func (h *Handler) brandFailed(w http.ResponseWriter, r *http.Request, obj objstore.Object, err error) {
	if errors.Is(err, objstore.ErrCorrupt) || errors.Is(err, objstore.ErrNotFound) {
		log.ErrorContext(r.Context(), "artifacts_serve_brand_damaged", "key", obj.Key.String(), "error", err)
		h.refuse(w, http.StatusInternalServerError, 0)
		return
	}
	log.WarnContext(r.Context(), "artifacts_serve_brand_content_unavailable", "key", obj.Key.String(),
		"error", err)
	h.refuse(w, http.StatusServiceUnavailable, retryUnavailable)
}

// withBrand is a page with the adopted stylesheet its version pinned linked
// at its opening, where the runtime is placed: the stylesheet defines only the
// kit's `--brand-*` inputs and the brand's faces, so linking it first lets
// every rule of the page's own, the kit's included, read it and win over it.
//
// NOTHING IS PLACED where the value is not a stylesheet's hash (a version
// records nothing else, and a value of another shape would be markup written
// into a page), or where the page keeps its own design
// (`<html data-crewlet-brand="off">`, read by the one function the publish
// reads it with).
func withBrand(page []byte, css string) []byte {
	if css == "" {
		return page
	}
	if !brandLinkHash.MatchString(css) {
		log.Warn("artifacts_serve_brand_unreadable", "brand", css)
		return page
	}
	if lint.BrandOff(page) {
		return page
	}
	tag := `<link rel="stylesheet" href="` + brandPrefix + css + `.css">`
	at := documentStart(page)
	out := make([]byte, 0, len(page)+len(tag))
	out = append(out, page[:at]...)
	out = append(out, tag...)
	return append(out, page[at:]...)
}
