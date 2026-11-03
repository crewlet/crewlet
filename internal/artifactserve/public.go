package artifactserve

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
)

// THE EMBEDDED RUNTIME AND KIT: the same bytes on every artifact host, in the
// binary, under names that are versions.
//
// The runtime (`runtime/<contract>/`) is what installs `window.crewlet` in a
// page and speaks to its host over the message port; the kit (`kit/<version>/`)
// is the design system's artifact build. Both are build output committed under
// static/artifacts and embedded, so `go build` needs no Node, exactly as the
// dashboard is. A contract or kit version this binary does not hold is a 404:
// nothing is resolved to a nearby version, because a page pinned to one
// contract must run the contract it was written against.

// RuntimeScript is the runtime's entry file within its contract's directory.
const RuntimeScript = "runtime.js"

// RuntimePath is the runtime of one contract at its CONTRACT'S name, on the
// page's own artifact origin: answered for a page or a cache that still names
// it, and revalidated on every use ([cacheRevalidate]), because a contract
// names what the runtime promises and never its bytes. A fix to the runtime
// ships under the same contract, so a year-long cache of this name kept a
// browser on the runtime it first fetched long after the fix.
func RuntimePath(contract string) string {
	return "/_crewlet/runtime/" + contract + "/" + RuntimeScript
}

// runtimeAddressHex is how many hex digits of the runtime's SHA-256 name its
// bytes: 64 bits, so two builds of one contract never share a name by
// accident, and nothing else ever has to.
const runtimeAddressHex = 16

// runtimeAddressed is a content-addressed runtime file: `runtime.<digest>.js`.
var runtimeAddressed = regexp.MustCompile(`^runtime\.([0-9a-f]{16})\.js$`)

// RuntimeAddress is the runtime of one contract at the name of its BYTES,
// `/_crewlet/runtime/<contract>/runtime.<sha256 prefix>.js`: what the origin
// writes on the element it places in every page, and the one name of the
// runtime served immutable for a year, since another build's runtime is
// another name.
func RuntimeAddress(contract string, runtime []byte) string {
	sum := sha256.Sum256(runtime)
	return "/_crewlet/runtime/" + contract + "/runtime." +
		hex.EncodeToString(sum[:])[:runtimeAddressHex] + ".js"
}

// versionPattern is an exact version: three numbers, as a contract and a kit
// release are both named. Nothing looser reaches the tree. Which contracts the
// tree holds is the publish pipeline's reading of the same directories
// (internal/artifactupload's ReleasesIn), which resolves a version's pin to one
// of them; this side only ever serves a name of that shape.
// Compiled at first use, not at init: the code host's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
var versionPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$`)
})

// cacheForever is a versioned public file's Cache-Control: the name is the
// version, so the bytes behind it never change, `public` because the file is in
// the binary and is nobody's, and a year because that is the longest lifetime a
// cache is conventionally asked to honour.
const cacheForever = "public, max-age=31536000, immutable"

// cacheRevalidate is a public file's Cache-Control where its name is not its
// bytes (the runtime at its contract's name, [RuntimePath]): any cache may
// keep it, and must ask again before every use, which the ETag answers with a
// 304 while the bytes are the same.
const cacheRevalidate = "public, no-cache, must-revalidate"

// publicFiles serves the embedded tree, each file read, hashed and compressed
// once for the life of the process: the tree is in the binary, so none of
// those answers can change while it runs.
type publicFiles struct {
	tree fs.FS

	// files holds every file that has been served; a probe for a name the
	// tree does not hold is refused every time and kept nowhere, so a
	// crawler asking for a million names grows nothing here.
	mu    sync.Mutex
	files map[string]*publicFile
}

type publicFile struct {
	body        []byte
	digest      string
	etag        string
	contentType string
	gzipped     func() []byte
}

func newPublicFiles(tree fs.FS) *publicFiles {
	return &publicFiles{tree: tree, files: map[string]*publicFile{}}
}

// serve answers `<kind>/<version>/<file>` from the tree, or reports that it
// holds no such file. It reads no ticket and no host: the bytes are public and
// identical on every artifact host, which is what lets an edge cache key them
// without the host.
//
// A KIT is named by its release, whose bytes never change, so every kit file
// is kept a year. A RUNTIME is named by its contract, whose bytes do change
// with a fix, so only its content-addressed name ([RuntimeAddress]) is kept a
// year, an address naming other bytes than this build's is absent, and every
// other name in a contract's directory is revalidated on every use.
func (p *publicFiles) serve(w http.ResponseWriter, r *http.Request, kind, rest string, secure bool) bool {
	version, file, ok := strings.Cut(rest, "/")
	if !ok || !versionPattern().MatchString(version) || file == "" {
		return false
	}
	cache := cacheForever
	address := ""
	if kind == destRuntime {
		cache = cacheRevalidate
		if m := runtimeAddressed.FindStringSubmatch(file); m != nil {
			file, address, cache = RuntimeScript, m[1], cacheForever
		}
	}
	name := kind + "/" + version + "/" + file
	typ, known := mediatype.For(file)
	if !known || !fs.ValidPath(name) {
		return false
	}
	f, ok := p.lookup(name, typ)
	if !ok || (address != "" && f.digest[:runtimeAddressHex] != address) {
		return false
	}
	h := w.Header()
	pagepolicy.ArtifactPublic(h, secure)
	h.Set("Cache-Control", cache)
	h.Set("Content-Type", f.contentType)
	body, etag := f.body, f.etag
	if gz := f.gzipped(); gz != nil {
		h.Add("Vary", "Accept-Encoding")
		if pagepolicy.AcceptsGzip(r.Header) {
			body, etag = gz, pagepolicy.GzipTag(f.etag)
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("ETag", etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	return true
}

// runtimeAddress is the content-addressed path of one contract's runtime as
// this binary holds it ([RuntimeAddress]), or false for a contract it does
// not hold.
func (p *publicFiles) runtimeAddress(contract string) (string, bool) {
	if !versionPattern().MatchString(contract) {
		return "", false
	}
	typ, _ := mediatype.For(RuntimeScript)
	f, ok := p.lookup(destRuntime+"/"+contract+"/"+RuntimeScript, typ)
	if !ok {
		return "", false
	}
	return RuntimeAddress(contract, f.body), true
}

// lookup is one file, read and classified on first use.
func (p *publicFiles) lookup(name string, typ mediatype.Type) (*publicFile, bool) {
	p.mu.Lock()
	f, ok := p.files[name]
	p.mu.Unlock()
	if ok {
		return f, true
	}
	data, err := fs.ReadFile(p.tree, name)
	if err != nil {
		return nil, false
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	built := &publicFile{
		body:        data,
		digest:      digest,
		etag:        `"` + digest[:40] + `"`,
		contentType: typ.Served,
		gzipped: sync.OnceValue(func() []byte {
			if !typ.Compressible {
				return nil
			}
			return pagepolicy.GzipSmaller(data, gzip.BestCompression)
		}),
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if f, ok := p.files[name]; ok {
		return f, true
	}
	p.files[name] = built
	return built, true
}
