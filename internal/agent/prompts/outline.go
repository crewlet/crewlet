package prompts

import (
	"strings"
	"unicode/utf8"
)

// Prompt is a built prompt: the text a model is sent, and the OUTLINE of it —
// the parts it was assembled from, in order, each with the byte length it
// occupies in the text.
//
// # Why the builder states the outline rather than a reader deriving it
//
// A reader that wants to show a prompt as its parts has one thing to go on
// without this: the text's own `##` lines. That is right for the builder's
// headings and wrong for every heading INSIDE embedded content — a chat
// trigger's own "## Triage — decide BEFORE replying" and "## Thread context", a
// pull request description's "# Title", a model's "## Summary" quoted back in
// review evidence or a ledger reply. Split on those, an embedded heading
// escapes the section it belongs to, or swallows the rest of the prompt under
// a title nobody wrote. Only the frame that appended a part knows where the
// part begins and ends, so the frame that appends it records it.
//
// # The invariants a reader may rely on
//
// The sections TILE Text exactly and in order: Text is valid UTF-8, their byte
// lengths sum to len(Text), every boundary falls on a rune boundary, every
// section is at least one byte, and no key repeats within one prompt. A
// separator between two parts belongs to the section BEFORE it, so a headed
// section starts with its own heading line — and [Section.Headed] says which
// sections those are. A reader that finds a map breaking any of these must
// fall back to the text's own headings rather than slice by it; [Valid] is
// that check.
type Prompt struct {
	Text     string
	Sections []Section
}

// Section is one part of a [Prompt].
type Section struct {
	// Key is a stable snake_case identifier the builder chose for this
	// part, unique within the prompt: what a reader keys a part on across
	// two prompts of the same phase, where Title may carry a seat's own
	// words (a unit's name).
	Key string

	// Title is the part's heading text without its leading #'s, inline
	// markdown kept — or, for a part with no heading of its own (the
	// identity line, a worker's mandated rules), a title the builder chose.
	Title string

	// Bytes is the part's length in Text, in bytes of UTF-8.
	Bytes int

	// Headed is true when the part BEGINS WITH ITS OWN HEADING LINE, whose
	// text is Title: a part opened by [Builder.Heading]. False for one
	// opened by [Builder.Lead], whatever its text begins with.
	//
	// A reader cannot tell the two apart from the text, and that is the
	// point. A lead part carrying somebody else's markdown — a worker's
	// persona, a task prompt the executor wrote, a trigger's body — very
	// often OPENS with a heading ("## Goal", "# Fix the login bug"), and a
	// reader that took that line for the part's own heading drew the
	// builder's title in its place and lost the quoted heading's words.
	// Only the builder knows which kind of part it appended.
	Headed bool
}

// Valid reports whether the outline tiles the text — see [Prompt]. A prompt
// with no outline at all is valid: there is simply nothing to slice by.
//
// INVALID UTF-8 FAILS IT, though every byte count would still add up. A
// prompt carries external content — a vendor's webhook body, a quoted reply —
// and the text travels as JSON, whose encoder rewrites each invalid byte to
// U+FFFD, three bytes for one: the text a reader receives is then longer than
// the map that claims to tile it, and every section after the bad byte is cut
// in the wrong place.
func (p Prompt) Valid() bool {
	if len(p.Sections) == 0 {
		return true
	}
	if !utf8.ValidString(p.Text) {
		return false
	}
	seen := make(map[string]bool, len(p.Sections))
	at := 0
	for _, s := range p.Sections {
		if s.Bytes <= 0 || s.Key == "" || seen[s.Key] {
			return false
		}
		seen[s.Key] = true
		if at+s.Bytes > len(p.Text) {
			return false
		}
		at += s.Bytes
		if at < len(p.Text) && !utf8.RuneStart(p.Text[at]) {
			return false
		}
	}
	return at == len(p.Text)
}

// Builder assembles a [Prompt] from parts joined by one separator, recording
// where each section the caller opens begins.
//
// It produces EXACTLY the text strings.Join(parts, sep) would: the outline is
// carried beside the prompt and never changes a byte of it, because a prompt's
// bytes are its prefix-cache key and its behaviour (see the package doc).
//
// A section is opened by the part that begins it — [Builder.Heading] for a
// part that starts with its own heading line, [Builder.Lead] for one with none
// — and runs until the next one opens; [Builder.Add] continues the section in
// progress. The boundary is placed at the opening part's first byte that is
// not a newline, so the blank line a builder writes before a heading ("\n##")
// stays with the section before it, as every separator does.
type Builder struct {
	sep   string
	parts []string
	opens []opening
}

// opening is a section a part opened.
type opening struct {
	part   int
	key    string
	title  string
	headed bool
}

// NewBuilder starts a prompt whose parts are joined by sep.
func NewBuilder(sep string) *Builder { return &Builder{sep: sep} }

// Heading opens a section whose first part begins with its heading line, and
// appends parts. The title is that heading's text.
//
// NO PARTS, NO SECTION: a section builder that returns nil for "does not
// apply" leaves nothing behind, in the outline exactly as in the text — and
// parts holding only newlines open nothing either (see [blank]).
func (b *Builder) Heading(key string, parts ...string) {
	if blank(parts) {
		b.parts = append(b.parts, parts...)
		return
	}
	b.open(key, headingTitle(parts[0]), true, parts)
}

// Lead opens a section with no heading of its own, under the title given, and
// appends parts. Parts that are all empty open nothing, so an absent identity
// line does not become a section of blank lines (see [blank]).
func (b *Builder) Lead(key, title string, parts ...string) {
	if blank(parts) {
		b.parts = append(b.parts, parts...)
		return
	}
	b.open(key, title, false, parts)
}

// blank reports whether parts hold nothing but newlines. Such parts are
// appended — the text is the text — but open no section: a section of blank
// lines has no content of its own to name, and its newlines are separators,
// which belong to the section before them.
func blank(parts []string) bool {
	return strings.Trim(strings.Join(parts, ""), "\n") == ""
}

// Add appends parts to the section in progress.
func (b *Builder) Add(parts ...string) { b.parts = append(b.parts, parts...) }

func (b *Builder) open(key, title string, headed bool, parts []string) {
	b.opens = append(b.opens, opening{part: len(b.parts), key: key, title: title, headed: headed})
	b.parts = append(b.parts, parts...)
}

// Build joins the parts and measures the outline.
//
// The first section always starts at byte 0, so anything appended before it —
// an empty part a builder keeps for its separator — belongs to it rather than
// to no section at all. A section that would measure zero bytes is dropped;
// [blank] already keeps a newline-only part from opening one, so this is the
// guarantee rather than a case any builder reaches.
func (b *Builder) Build() Prompt {
	text := strings.Join(b.parts, b.sep)
	if len(b.opens) == 0 {
		return Prompt{Text: text}
	}
	// Where every part starts in the joined text.
	offsets := make([]int, len(b.parts))
	at := 0
	for i, part := range b.parts {
		if i > 0 {
			at += len(b.sep)
		}
		offsets[i] = at
		at += len(part)
	}
	starts := make([]int, len(b.opens))
	for i, o := range b.opens {
		if i == 0 {
			continue
		}
		part := b.parts[o.part]
		starts[i] = offsets[o.part] + len(part) - len(strings.TrimLeft(part, "\n"))
	}
	sections := make([]Section, 0, len(b.opens))
	for i, o := range b.opens {
		end := len(text)
		if i+1 < len(b.opens) {
			end = starts[i+1]
		}
		if end <= starts[i] {
			continue
		}
		sections = append(sections, Section{
			Key: o.key, Title: o.title, Bytes: end - starts[i], Headed: o.headed,
		})
	}
	return Prompt{Text: text, Sections: sections}
}

// headingTitle is a heading part's title: its first non-blank line with the
// leading #'s and the space after them removed.
func headingTitle(part string) string {
	line := strings.TrimLeft(part, "\n")
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	return strings.TrimSpace(strings.TrimLeft(line, "#"))
}
