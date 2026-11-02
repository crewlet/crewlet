package mediatype

import (
	"errors"
	"fmt"
	"mime"
	"path"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/words"
)

// Class is the family a type belongs to, which decides the size limit a file
// is held to and how the serving origin treats it.
type Class string

// The classes a media type can fall in.
const (
	// ClassDocument is a page or a text document: HTML, Markdown, plain
	// text, CSV, XML.
	ClassDocument Class = "document"

	// ClassScript is JavaScript a page loads.
	ClassScript Class = "script"

	// ClassStyle is a stylesheet.
	ClassStyle Class = "style"

	// ClassData is structured data a page fetches: JSON, a source map,
	// WebAssembly.
	ClassData Class = "data"

	// ClassImage is a raster or vector image.
	ClassImage Class = "image"

	// ClassFont is a web font.
	ClassFont Class = "font"

	// ClassAudio and ClassVideo are media a page plays.
	ClassAudio Class = "audio"
	ClassVideo Class = "video"

	// ClassModel is a 3D model a page renders.
	ClassModel Class = "model"

	// ClassOther is a document a browser renders with its own viewer: PDF.
	ClassOther Class = "other"
)

// Classes are every class, in the order the table lists them.
var Classes = []Class{
	ClassDocument, ClassScript, ClassStyle, ClassData, ClassImage, ClassFont,
	ClassAudio, ClassVideo, ClassModel, ClassOther,
}

// Valid reports whether c is a class this build knows.
func (c Class) Valid() bool { return slices.Contains(Classes, c) }

// Type is one served type an extension may be published as.
type Type struct {
	// Served is the Content-Type the serving origin sends, parameters
	// included (`text/html; charset=utf-8`). It is what a manifest entry
	// records, and never a client's spelling.
	Served string

	// Class is the type's family.
	Class Class

	// Text reports a type whose bytes are UTF-8 text: what an edit may be
	// applied to, and what is held to the text size limit rather than the
	// binary one.
	Text bool

	// Compressible reports a type the serving origin gzips on the way out.
	// Already-compressed formats (images other than SVG, fonts, media,
	// WebAssembly, PDF) gain nothing and cost a CPU per request.
	Compressible bool

	// Scriptable reports a type a browser may execute: HTML, SVG, XML and
	// JavaScript. Serving one is safe ONLY because the artifact origin holds
	// no credential; the dashboard's origin never renders any of them.
	Scriptable bool
}

// Essence is the served type without parameters, lower case: the form a
// declared type is compared in.
func (t Type) Essence() string { return essence(t.Served) }

// Entry is one extension's row: its default type, and the alternates a client
// may name to disambiguate it.
type Entry struct {
	// Extension is the lower-case extension with its dot (`.html`).
	Extension string

	// Types are the types the extension may be published as, the default
	// first. More than one only where the extension genuinely names two
	// formats (an `.mp4` of audio alone, an `.ogg` of video).
	Types []Type
}

// THE TABLE. One row per extension, read by the publish validator, the serving
// handler and the asset upload, so the three can never disagree about a file.
//
// WHY THESE AND NOT MORE: it is the set a web page an author or an agent
// publishes actually uses, and the parity contract an artifact author already
// knows. A type outside it is refused naming the path, so the origin never
// serves a file it cannot describe and never sniffs one.
var table = []Entry{
	// Documents.
	{".html", []Type{{Served: "text/html; charset=utf-8", Class: ClassDocument, Text: true, Compressible: true, Scriptable: true}}},
	{".htm", []Type{{Served: "text/html; charset=utf-8", Class: ClassDocument, Text: true, Compressible: true, Scriptable: true}}},
	{".md", []Type{{Served: "text/markdown; charset=utf-8", Class: ClassDocument, Text: true, Compressible: true}}},
	{".txt", []Type{{Served: "text/plain; charset=utf-8", Class: ClassDocument, Text: true, Compressible: true}}},
	{".csv", []Type{{Served: "text/csv; charset=utf-8", Class: ClassDocument, Text: true, Compressible: true}}},
	// XML IS SCRIPTABLE: a browser navigated to an XML document renders an
	// XHTML namespace in it and runs its scripts.
	{".xml", []Type{{Served: "application/xml; charset=utf-8", Class: ClassDocument, Text: true, Compressible: true, Scriptable: true}}},

	// Scripts, styles and data.
	{".js", []Type{{Served: "text/javascript; charset=utf-8", Class: ClassScript, Text: true, Compressible: true, Scriptable: true}}},
	{".mjs", []Type{{Served: "text/javascript; charset=utf-8", Class: ClassScript, Text: true, Compressible: true, Scriptable: true}}},
	{".css", []Type{{Served: "text/css; charset=utf-8", Class: ClassStyle, Text: true, Compressible: true}}},
	{".json", []Type{{Served: "application/json", Class: ClassData, Text: true, Compressible: true}}},
	{".map", []Type{{Served: "application/json", Class: ClassData, Text: true, Compressible: true}}},
	{".wasm", []Type{{Served: "application/wasm", Class: ClassData}}},

	// Images. SVG is text, compressible and SCRIPTABLE.
	{".png", []Type{{Served: "image/png", Class: ClassImage}}},
	{".jpg", []Type{{Served: "image/jpeg", Class: ClassImage}}},
	{".jpeg", []Type{{Served: "image/jpeg", Class: ClassImage}}},
	{".gif", []Type{{Served: "image/gif", Class: ClassImage}}},
	{".webp", []Type{{Served: "image/webp", Class: ClassImage}}},
	{".avif", []Type{{Served: "image/avif", Class: ClassImage}}},
	{".ico", []Type{{Served: "image/x-icon", Class: ClassImage}}},
	{".svg", []Type{{Served: "image/svg+xml", Class: ClassImage, Text: true, Compressible: true, Scriptable: true}}},

	// Fonts.
	{".woff", []Type{{Served: "font/woff", Class: ClassFont}}},
	{".woff2", []Type{{Served: "font/woff2", Class: ClassFont}}},
	{".ttf", []Type{{Served: "font/ttf", Class: ClassFont}}},
	{".otf", []Type{{Served: "font/otf", Class: ClassFont}}},

	// Audio and video. The container formats that carry either name both,
	// and a client may name the one it holds.
	{".mp3", []Type{{Served: "audio/mpeg", Class: ClassAudio}}},
	{".wav", []Type{{Served: "audio/wav", Class: ClassAudio}}},
	{".oga", []Type{{Served: "audio/ogg", Class: ClassAudio}}},
	{".m4a", []Type{{Served: "audio/mp4", Class: ClassAudio}}},
	{".ogg", []Type{{Served: "audio/ogg", Class: ClassAudio}, {Served: "video/ogg", Class: ClassVideo}}},
	{".mp4", []Type{{Served: "video/mp4", Class: ClassVideo}, {Served: "audio/mp4", Class: ClassAudio}}},
	{".webm", []Type{{Served: "video/webm", Class: ClassVideo}, {Served: "audio/webm", Class: ClassAudio}}},
	{".ogv", []Type{{Served: "video/ogg", Class: ClassVideo}}},

	// Models and other documents.
	{".glb", []Type{{Served: "model/gltf-binary", Class: ClassModel}}},
	{".gltf", []Type{{Served: "model/gltf+json", Class: ClassModel, Text: true, Compressible: true}}},
	{".pdf", []Type{{Served: "application/pdf", Class: ClassOther}}},
}

// Table is every row, in the order it is declared: what a reference page and
// a test walk.
func Table() []Entry {
	out := make([]Entry, len(table))
	for i, e := range table {
		out[i] = Entry{Extension: e.Extension, Types: slices.Clone(e.Types)}
	}
	return out
}

// ErrRefused reports a file whose type this table does not serve: an extension
// it has no row for, or a declared type that is not one of its row's.
var ErrRefused = errors.New("mediatype: the file's type is not one an artifact may publish")

// For is the default type of the file at p, and false for an extension the
// table has no row for. The extension is read case-insensitively from the last
// path segment, so `Logo.PNG` is a PNG.
func For(p string) (Type, bool) {
	row, ok := rowFor(p)
	if !ok {
		return Type{}, false
	}
	return row.Types[0], true
}

// Resolve is the type the file at p is served as, given the type a client
// declared for it (empty when it declared none).
//
// THE TABLE DECIDES. A declared type only chooses between the types the
// extension's own row lists, compared without parameters and case; anything
// else is [ErrRefused] naming the path, never accepted and never coerced. A
// file with no extension the table knows is refused whatever it declares.
func Resolve(p, declared string) (Type, error) {
	row, ok := rowFor(p)
	if !ok {
		return Type{}, fmt.Errorf("%w: %q has no extension an artifact can publish. "+
			"Use %s", ErrRefused, p, words.Or(extensions()))
	}
	if strings.TrimSpace(declared) == "" {
		return row.Types[0], nil
	}
	want := essence(declared)
	for _, t := range row.Types {
		if t.Essence() == want {
			return t, nil
		}
	}
	served := make([]string, 0, len(row.Types))
	for _, t := range row.Types {
		served = append(served, t.Essence())
	}
	return Type{}, fmt.Errorf("%w: %q is declared %q, and a %s file is served as %s",
		ErrRefused, p, declared, row.Extension, words.Or(served))
}

// rowFor is the row for the extension of p's last segment.
func rowFor(p string) (Entry, bool) {
	ext := strings.ToLower(path.Ext(p))
	if ext == "" {
		return Entry{}, false
	}
	for _, row := range table {
		if row.Extension == ext {
			return row, true
		}
	}
	return Entry{}, false
}

// extensions is every extension the table serves, for a refusal to list.
func extensions() []string {
	out := make([]string, 0, len(table))
	for _, row := range table {
		out = append(out, row.Extension)
	}
	return out
}

// essence is a media type without its parameters, lower case. A type the mime
// parser cannot read is compared as its lower-cased text before any `;`, so a
// malformed declaration is simply one that matches no row.
func essence(t string) string {
	if parsed, _, err := mime.ParseMediaType(t); err == nil {
		return strings.ToLower(parsed)
	}
	head, _, _ := strings.Cut(t, ";")
	return strings.ToLower(strings.TrimSpace(head))
}
