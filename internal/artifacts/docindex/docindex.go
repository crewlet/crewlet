// Package docindex is the format a `doc` artifact holds: one Markdown
// document, or a document of several tabs, each its own Markdown file, ordered
// by a `doc.json` index (D55).
//
// THE FILES. A doc of one tab is a version whose entry is a Markdown file
// (`index.md` by default). A doc of several is a version whose entry is
// `doc.json`:
//
//	{ "v": 1, "tabs": [ { "path": "tabs/plan.md", "title": "Plan" }, ... ] }
//
// naming, in order, the Markdown file of every tab. Images and any other file
// are files of the same version, named by their path from the version's root.
// A Markdown file the index does not name is a file of the version and not a
// tab, which a tab may link to.
//
// REFUSED AT PUBLISH, NEVER DROPPED ON READ. An index that does not resolve (a
// tab naming no file, a file that is not Markdown, a path twice, a title past
// its bound, more tabs than a doc holds) is refused with a [Finding] naming the
// rule, so an author fixes what they wrote rather than learning on read that a
// tab never shows.
//
// NOTHING HERE IS RENDERED. The Markdown is never executed and never parsed
// into markup: the dashboard draws each tab with its own element-constructing
// renderer (`dashboard/src/lib/markdown.ts`), which also decides what a heading
// is, so a heading has one definition and the outline and the page it points
// into cannot disagree. This package answers a doc's tabs and their text.
//
// PURE. It reads no store and holds no clock: the publish pipeline hands it the
// version's files, and the read routes call it on the same files.
package docindex

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/jsonform"
)

// IndexPath is a tabbed doc's index, its entry.
const IndexPath = "doc.json"

// MarkdownEssence is the one media type a tab is.
const MarkdownEssence = "text/markdown"

// IndexVersion is the index format this build reads.
const IndexVersion = 1

// MaxTabs bounds one doc.
const MaxTabs = 32

// MaxTitleBytes bounds a tab's title.
const MaxTitleBytes = 120

// MaxTextBytes bounds every tab's text together: the bound of one entry, so a
// tabbed doc is read whole in one answer exactly as a doc of one file is.
const MaxTextBytes = int(artifacts.MaxEntryBytes)

// CoverBytes bounds the opening of the first tab the gallery's cover draws.
const CoverBytes = 1024

// coverLines bounds the same opening by lines, so a cover of short lines stops
// where a card has room.
const coverLines = 12

// MaxFindings bounds the findings one refusal carries.
const MaxFindings = 50

// ---- the index ------------------------------------------------------------ //

// Index is `doc.json` as an author writes it.
type Index struct {
	V    int        `json:"v"`
	Tabs []IndexTab `json:"tabs"`
}

// IndexTab is one tab of the index.
type IndexTab struct {
	Path  string `json:"path"`
	Title string `json:"title"`
}

// ---- the doc -------------------------------------------------------------- //

// Doc is a version's doc as the dashboard draws it: every tab, in order, with
// its text.
type Doc struct {
	// Indexed reports the version opens on `doc.json` rather than on one
	// Markdown file.
	Indexed bool  `json:"indexed"`
	Tabs    []Tab `json:"tabs"`
}

// Tab is one tab of a doc.
type Tab struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

// Outline is a doc without its text: what the gallery's cover and an agent
// reading a doc one tab at a time need.
type Outline struct {
	Tabs  []OutlineTab `json:"tabs"`
	Cover Cover        `json:"cover"`
}

// OutlineTab is one tab of an outline.
type OutlineTab struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Bytes int    `json:"bytes"`
}

// Cover is the opening of the first tab, Markdown cut at a line boundary, which
// the gallery draws at card size with the dashboard's own renderer.
type Cover struct {
	Tab      string `json:"tab"`
	Markdown string `json:"markdown"`
}

// Outline is the doc's outline.
func (d Doc) Outline() Outline {
	out := Outline{Tabs: make([]OutlineTab, len(d.Tabs))}
	for i, t := range d.Tabs {
		out.Tabs[i] = OutlineTab{Path: t.Path, Title: t.Title, Bytes: len(t.Text)}
	}
	if len(d.Tabs) > 0 {
		out.Cover = Cover{Tab: d.Tabs[0].Path, Markdown: opening(d.Tabs[0].Text)}
	}
	return out
}

// Text is the doc's readable text, for the gallery's search: each tab's title,
// then its text, in order.
func (d Doc) Text() string {
	var b strings.Builder
	for i, t := range d.Tabs {
		if i > 0 {
			b.WriteString("\n")
		}
		if d.Indexed {
			b.WriteString(t.Title)
			b.WriteString("\n")
		}
		b.WriteString(t.Text)
	}
	return b.String()
}

// Tab is the tab at path, and false for a path that is not one of the doc's
// tabs.
func (d Doc) Tab(p string) (Tab, bool) {
	i := slices.IndexFunc(d.Tabs, func(t Tab) bool { return t.Path == p })
	if i < 0 {
		return Tab{}, false
	}
	return d.Tabs[i], true
}

// opening is the start of a tab, at most [CoverBytes] and [coverLines], cut at
// a line boundary (or, for one long first line, at a rune boundary).
func opening(text string) string {
	var b strings.Builder
	for i, line := range strings.SplitAfter(text, "\n") {
		if i >= coverLines || b.Len()+len(line) > CoverBytes {
			if b.Len() == 0 {
				return strings.TrimRight(cutRunes(line, CoverBytes), "\n")
			}
			break
		}
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// cutRunes is s cut to at most n bytes without splitting a rune.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ---- reading -------------------------------------------------------------- //

// Rule is what a finding broke.
type Rule string

// The rules a doc can break.
const (
	RuleIndex     Rule = "index"     // doc.json itself
	RuleReference Rule = "reference" // a tab naming a file that is not there, or not Markdown
	RuleBound     Rule = "bound"     // a count or size past its bound
	RuleEncoding  Rule = "encoding"  // a tab that is not UTF-8 text
)

// Finding is one way a doc is outside the format.
type Finding struct {
	Path    string `json:"path"`
	Rule    Rule   `json:"rule"`
	Message string `json:"message"`
}

func (f Finding) String() string { return fmt.Sprintf("%s: %s: %s", f.Path, f.Rule, f.Message) }

// Refused is a doc outside the format: every finding, in index order, so an
// author fixes them in one pass.
type Refused struct {
	Findings []Finding
}

func (r *Refused) Error() string {
	lines := make([]string, len(r.Findings))
	for i, f := range r.Findings {
		lines[i] = f.String()
	}
	return "the doc is outside the doc format: " + strings.Join(lines, "; ")
}

// ErrRefused is every refusal this package answers, wrapped by [*Refused].
var ErrRefused = errors.New("docindex: refused")

// Unwrap makes every refusal an [ErrRefused].
func (r *Refused) Unwrap() error { return ErrRefused }

// Read reads a whole doc from the version whose entry is entry: `doc.json` and
// every tab it names, or the one Markdown file. A doc outside the format is a
// [*Refused]; an error reading a file is answered as itself.
//
// The index is read as stored ([jsonform.Stored]), so a doc accepted
// before ADR-0059 tightened its keys reads as it did; [Check] is the publish's.
func Read(src artifacts.FileSource, entry string) (Doc, error) {
	return read(src, entry, jsonform.Stored(jsonform.UntilEOF))
}

// Check is [Read] as a publish holds a doc whose index it writes.
func Check(src artifacts.FileSource, entry string) (Doc, error) {
	return read(src, entry, jsonform.Decode)
}

func read(src artifacts.FileSource, entry string, decode jsonform.Reader) (Doc, error) {
	if entry != IndexPath {
		return readOne(src, entry)
	}
	raw, err := src.Read(IndexPath)
	if err != nil {
		return Doc{}, fmt.Errorf("docindex: read %s: %w", IndexPath, err)
	}
	idx, found := parseIndex(raw, decode)
	if idx == nil {
		return Doc{}, &Refused{Findings: found}
	}
	doc := Doc{Indexed: true, Tabs: make([]Tab, 0, len(idx.Tabs))}
	total := 0
	for _, t := range idx.Tabs {
		if problem := tabFile(src, t.Path); problem != nil {
			found = append(found, *problem)
			continue
		}
		body, err := src.Read(t.Path)
		if err != nil {
			return Doc{}, fmt.Errorf("docindex: read %s: %w", t.Path, err)
		}
		if !utf8.Valid(body) {
			found = append(found, Finding{Path: t.Path, Rule: RuleEncoding,
				Message: "a tab is UTF-8 text"})
			continue
		}
		total += len(body)
		doc.Tabs = append(doc.Tabs, Tab{Path: t.Path, Title: t.Title, Text: string(body)})
	}
	if total > MaxTextBytes {
		found = append(found, Finding{Path: IndexPath, Rule: RuleBound, Message: fmt.Sprintf(
			"the tabs total %d bytes and a doc holds at most %d", total, MaxTextBytes)})
	}
	if len(found) > 0 {
		return Doc{}, &Refused{Findings: bounded(found)}
	}
	return doc, nil
}

// readOne reads a doc of one tab: its entry, a Markdown file.
func readOne(src artifacts.FileSource, entry string) (Doc, error) {
	if problem := tabFile(src, entry); problem != nil {
		return Doc{}, &Refused{Findings: []Finding{*problem}}
	}
	body, err := src.Read(entry)
	if err != nil {
		return Doc{}, fmt.Errorf("docindex: read %s: %w", entry, err)
	}
	if !utf8.Valid(body) {
		return Doc{}, &Refused{Findings: []Finding{{Path: entry, Rule: RuleEncoding,
			Message: "a doc is UTF-8 text"}}}
	}
	title := strings.TrimSuffix(path.Base(entry), path.Ext(entry))
	return Doc{Tabs: []Tab{{Path: entry, Title: title, Text: string(body)}}}, nil
}

// tabFile is why the file at p cannot be a tab, and nil when it can.
func tabFile(src artifacts.FileSource, p string) *Finding {
	essence, ok := src.Essence(p)
	switch {
	case !ok:
		return &Finding{Path: IndexPath, Rule: RuleReference, Message: fmt.Sprintf(
			"tab %q is not one of the version's files", p)}
	case essence != MarkdownEssence:
		return &Finding{Path: p, Rule: RuleReference, Message: fmt.Sprintf(
			"a tab is Markdown (%s), and %q is %s", MarkdownEssence, p, essence)}
	}
	return nil
}

// IndexForm is every key `doc.json` may carry and the type of each, derived
// from [Index] ([jsonform.Of]).
var IndexForm = jsonform.Of[Index]()

// ParseIndex reads `doc.json` as a publish holds it, answering every problem
// with it. A nil index is one that could not be read at all.
func ParseIndex(raw []byte) (*Index, []Finding) { return parseIndex(raw, jsonform.Decode) }

func parseIndex(raw []byte, decode jsonform.Reader) (*Index, []Finding) {
	var found []Finding
	bad := func(format string, args ...any) {
		found = append(found, Finding{Path: IndexPath, Rule: RuleIndex,
			Message: fmt.Sprintf(format, args...)})
	}
	var idx Index
	// EVERY STRUCTURAL FAULT AT ONCE ([jsonform.Decode]).
	d := decode(raw, IndexForm, jsonform.Subject{File: IndexPath, Format: "the doc index"}, &idx)
	for _, f := range d.Faults {
		found = append(found, Finding{Path: IndexPath, Rule: RuleIndex, Message: f.Message})
	}
	if len(d.Faults) > 0 {
		return nil, found
	}
	if idx.V != IndexVersion {
		bad("v is %d, and the doc index this build reads is %d", idx.V, IndexVersion)
		return nil, found
	}
	switch {
	case len(idx.Tabs) == 0:
		bad("tabs is empty: a doc holds at least one tab")
	case len(idx.Tabs) > MaxTabs:
		bad("tabs names %d tabs and a doc holds at most %d", len(idx.Tabs), MaxTabs)
	}
	seen := map[string]bool{}
	for i, t := range idx.Tabs {
		n := i + 1
		switch {
		case t.Path == "":
			bad("tab %d names no path", n)
		case seen[t.Path]:
			bad("tab %d names %q, which an earlier tab already names", n, t.Path)
		}
		seen[t.Path] = true
		switch {
		case strings.TrimSpace(t.Title) == "":
			bad("tab %d has no title", n)
		case len(t.Title) > MaxTitleBytes:
			bad("tab %d's title is %d bytes and a title is at most %d", n, len(t.Title),
				MaxTitleBytes)
		case !utf8.ValidString(t.Title) || strings.ContainsFunc(t.Title, unicode.IsControl):
			bad("tab %d's title holds a control character or is not UTF-8", n)
		}
	}
	if len(found) > 0 {
		return nil, bounded(found)
	}
	return &idx, nil
}

func bounded(found []Finding) []Finding {
	if len(found) > MaxFindings {
		return found[:MaxFindings]
	}
	return found
}
