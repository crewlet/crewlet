package artifacts

import (
	"fmt"
	"slices"
	"strings"
)

// ---- the kind table ------------------------------------------------------ //
//
// ONE TABLE, BECAUSE A KIND IS A SET OF ANSWERS, NOT A NAME. Every surface
// that treats an artifact differently by kind (the manifest's entry check, the
// agent tools' default entry, the artifact origin's serving decision, the
// frame and the ticket the dashboard embeds, the page runtime's contract, the
// linter's dark-mode warning, the search text, the CLI's inference, the
// dashboard's viewer, compare and Data tab) asks the kind's [KindSpec] for the
// one facet it needs, rather than switching on the name. A kind added here is
// a kind every one of those surfaces answers for, or the build fails, and a
// switch nobody remembered cannot quietly treat a new kind as the old default.
//
// THE ORGANISING RULE IS NATIVE OR FRAMED (adr/0042). A framed kind runs
// author code, and only on the credential-less artifact origin (adr/0035). A
// native kind carries no author code: the dashboard draws it from a closed,
// validated format with its own primitives, so nothing it holds can become
// script on the dashboard's origin, and a publish outside the format is
// refused with a named error rather than silently dropped on read.
//
// A KIND ENTERS [Kinds] ONLY IN THE CHANGE THAT MAKES IT WORK END TO END: a kind
// this build does not serve is refused at create, so no build ships a kind
// that publishes and cannot be opened.

// Render is how a kind is drawn.
type Render string

const (
	// RenderFramed runs the version's own files as a page, inside a
	// sandboxed frame on the artifact origin.
	RenderFramed Render = "framed"

	// RenderNative is drawn by the dashboard from a closed format, with no
	// author code.
	RenderNative Render = "native"
)

// Renders are every way a kind is drawn.
var Renders = []Render{RenderFramed, RenderNative}

// Valid reports whether r is a way this build draws a kind.
func (r Render) Valid() bool { return slices.Contains(Renders, r) }

// Store is what a kind's datastore is to it.
type Store string

const (
	// StoreDeclared is a datastore a version reaches only by declaring the
	// `db` capability: a page's own data.
	StoreDeclared Store = "declared"

	// StoreContent is a datastore that IS the content: a native sheet is
	// its collections, keeps one from its first version, and publishes no
	// files at create.
	StoreContent Store = "content"

	// StoreImplied is a native kind's own datastore beside its files: a
	// dashboard's store datasets, which agents and people write and every
	// open board reads live. It declares nothing, since a native kind has
	// no page to grant a capability to, and it is never versioned with the
	// files.
	StoreImplied Store = "implied"

	// StoreNone is a kind whose content is its files alone, which keeps no
	// datastore.
	StoreNone Store = "none"
)

// Stores are every relation a kind has to its datastore.
var Stores = []Store{StoreDeclared, StoreContent, StoreImplied, StoreNone}

// Valid reports whether s is a store relation this build knows.
func (s Store) Valid() bool { return slices.Contains(Stores, s) }

// Search is what a version contributes to the text the gallery searches.
type Search string

const (
	// SearchEntry indexes the entry's readable text (a page's text without
	// its markup), and nothing when the version names no entry.
	SearchEntry Search = "entry"

	// SearchFormat indexes the readable text the kind's own format extracts
	// from every file it holds (a deck's slides and their speaker notes, a
	// board's titles and notes, never a figure), not the entry alone, whose
	// text is only the index.
	SearchFormat Search = "format"
)

// Searches are every search extraction this build knows.
var Searches = []Search{SearchEntry, SearchFormat}

// Valid reports whether s is a search extraction this build knows.
func (s Search) Valid() bool { return slices.Contains(Searches, s) }

// Format is the closed format a kind's own files are validated against at
// publish, beyond the per-file bounds every version is held to.
type Format string

const (
	// FormatNone holds a version's files to the per-file bounds alone: a
	// framed page is linted as a page instead, and a sheet's content is its
	// datastore.
	FormatNone Format = "none"

	// FormatSlides is the slide format (internal/artifacts/slideformat): a
	// deck index naming one slide file each, every slide parsed into the
	// closed element and style subset, refused at publish when outside it.
	FormatSlides Format = "slides"

	// FormatDashboard is the board format (internal/artifacts/dashspec):
	// the spec's filters, datasets and widgets held to their closed sets,
	// every file dataset read against its declared columns, refused at
	// publish with every finding.
	FormatDashboard Format = "dashboard"
)

// Formats are every format this build validates.
var Formats = []Format{FormatNone, FormatSlides, FormatDashboard}

// Valid reports whether f is a format this build validates.
func (f Format) Valid() bool { return slices.Contains(Formats, f) }

// EntryRule is what a kind's entry, the file a version opens on, may be.
type EntryRule struct {
	// Optional reports a version may name no entry at all: a native sheet's
	// content is its datastore.
	Optional bool

	// Essences are the media type essences the entry may be served as, as
	// [mediatype] spells them (`text/html`).
	Essences []string

	// Defaults are the entry paths a publish that names none looks for, in
	// preference order. The first is where a create's single `content` lands.
	Defaults []string

	// Fixed reports the entry may only be one of [EntryRule.Defaults]: a
	// format whose index is a file of a common type (a deck's `deck.json`)
	// is told apart by its path, never by its type alone.
	Fixed bool
}

// Admits reports whether a file served as essence may be this kind's entry.
func (r EntryRule) Admits(essence string) bool { return slices.Contains(r.Essences, essence) }

// AdmitsAt reports whether the file at path, served as essence, may be this
// kind's entry: a type the rule admits, at one of its paths where the rule
// fixes them.
func (r EntryRule) AdmitsAt(path, essence string) bool {
	return r.Admits(essence) && (!r.Fixed || slices.Contains(r.Defaults, path))
}

// KindSpec is everything the engine and the dashboard decide by kind.
type KindSpec struct {
	// Kind is the kind's name.
	Kind Kind

	// Summary is the kind's purpose in one clause, which the agent tools'
	// descriptions list from this table.
	Summary string

	// Render is how the kind is drawn.
	Render Render

	// Entry is what the kind's entry may be.
	Entry EntryRule

	// Store is what the kind's datastore is to it.
	Store Store

	// Format is the closed format the kind's files are validated against.
	Format Format

	// Search is what a version contributes to the gallery's search text.
	Search Search
}

// Framed reports the kind runs author code on the artifact origin: the only
// kind a view ticket is minted for, a version may declare capabilities and pin
// a runtime contract on, and the linter's page warnings apply to.
func (s KindSpec) Framed() bool { return s.Render == RenderFramed }

// Capabilities are the runtime capabilities a version of the kind may
// declare: every one this build serves for a framed kind, and none for a
// native kind, which has no page to grant them to.
func (s KindSpec) Capabilities() []Capability {
	if !s.Framed() {
		return nil
	}
	return slices.Clone(Capabilities)
}

// kindSpecs is the table, in the order every listing of kinds follows.
var kindSpecs = []KindSpec{
	{
		Kind:    KindHTML,
		Summary: "runs the page's own code in a sandbox",
		Render:  RenderFramed,
		Entry:   EntryRule{Essences: []string{"text/html"}, Defaults: []string{"index.html"}},
		Store:   StoreDeclared,
		Format:  FormatNone,
		Search:  SearchEntry,
	},
	{
		Kind:    KindMarkdown,
		Summary: "is a document the dashboard draws",
		Render:  RenderNative,
		Entry:   EntryRule{Essences: []string{"text/markdown"}, Defaults: []string{"index.md"}},
		Store:   StoreNone,
		Format:  FormatNone,
		Search:  SearchEntry,
	},
	{
		Kind:    KindSlides,
		Summary: "is a deck of 16:9 slides the dashboard draws and presents",
		Render:  RenderNative,
		Entry: EntryRule{Essences: []string{"application/json"}, Defaults: []string{"deck.json"},
			Fixed: true},
		Store:  StoreNone,
		Format: FormatSlides,
		Search: SearchFormat,
	},
	{
		Kind:    KindDashboard,
		Summary: "is a board of figures over live data the dashboard draws, each answered as its viewer",
		Render:  RenderNative,
		Entry: EntryRule{Essences: []string{"application/json"}, Defaults: []string{"dashboard.json"},
			Fixed: true},
		Store:  StoreImplied,
		Format: FormatDashboard,
		Search: SearchFormat,
	},
	{
		Kind:    KindSheet,
		Summary: "is a table drawn from its datastore",
		Render:  RenderNative,
		Entry:   EntryRule{Optional: true},
		Store:   StoreContent,
		Format:  FormatNone,
		Search:  SearchEntry,
	},
}

// Kinds are every kind this build serves, in the table's order.
var Kinds = func() []Kind {
	out := make([]Kind, len(kindSpecs))
	for i, s := range kindSpecs {
		out[i] = s.Kind
	}
	return out
}()

// KindSpecs is the table: every kind this build serves, in order, each a copy
// a caller may keep.
func KindSpecs() []KindSpec {
	out := make([]KindSpec, len(kindSpecs))
	for i, s := range kindSpecs {
		out[i] = s.clone()
	}
	return out
}

// Spec is k's row of the table, and false for a kind this build does not
// serve.
func (k Kind) Spec() (KindSpec, bool) {
	i := slices.IndexFunc(kindSpecs, func(s KindSpec) bool { return s.Kind == k })
	if i < 0 {
		return KindSpec{}, false
	}
	return kindSpecs[i].clone(), true
}

// Valid reports whether k is a kind this build serves.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// Framed reports k is a kind this build serves that runs author code on the
// artifact origin; a kind this build does not serve is never served there.
func (k Kind) Framed() bool {
	s, ok := k.Spec()
	return ok && s.Framed()
}

// KindFor is the kind whose entry the file at path, served as essence, may
// be, and false when no kind opens on such a file. It is how a publish that
// names no kind infers one from its entry.
//
// A FIXED PATH WINS: `deck.json` is a deck's index whatever else opens on
// JSON, so a kind that fixes its entry's path is asked first, and a kind that
// takes any path of its type only after.
func KindFor(path, essence string) (Kind, bool) {
	for _, fixed := range []bool{true, false} {
		for _, s := range kindSpecs {
			if s.Entry.Fixed == fixed && s.Entry.AdmitsAt(path, essence) {
				return s.Kind, true
			}
		}
	}
	return "", false
}

// DefaultEntries are every kind's default entry paths, in the table's order:
// the files a publish that names no entry looks for.
func DefaultEntries() []string {
	var out []string
	for _, s := range kindSpecs {
		for _, d := range s.Entry.Defaults {
			if !slices.Contains(out, d) {
				out = append(out, d)
			}
		}
	}
	return out
}

// KindSummaries is the table as one sentence an agent tool's description
// carries: "`html` runs the page's own code in a sandbox, ...".
func KindSummaries() string {
	parts := make([]string, len(kindSpecs))
	for i, s := range kindSpecs {
		parts[i] = fmt.Sprintf("`%s` %s", s.Kind, s.Summary)
	}
	return strings.Join(parts, ", ") + "."
}

func (s KindSpec) clone() KindSpec {
	s.Entry.Essences = slices.Clone(s.Entry.Essences)
	s.Entry.Defaults = slices.Clone(s.Entry.Defaults)
	return s
}
