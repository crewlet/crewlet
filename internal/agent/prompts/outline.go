package prompts

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// Prompt is a built prompt: the text a model is sent, and its OUTLINE — the
// parts it was assembled from, in order, each with its byte length in the text
// (package doc, "Every builder returns its outline beside its text").
//
// The sections TILE Text exactly and in order: Text is valid UTF-8, their byte
// lengths sum to len(Text), every boundary falls on a rune boundary, every
// section is at least one byte, and no key repeats. A separator belongs to the
// section BEFORE it, so a headed section ([Section.Headed]) starts with its own
// heading line. A reader finding a map that breaks any of these falls back to
// the text's own headings rather than slice by it; [Valid] is that check.
type Prompt struct {
	Text     string
	Sections []Section
}

// Section is one part of a [Prompt].
type Section struct {
	// Key is a stable snake_case identifier, unique within the prompt: what a
	// reader keys a part on across two prompts of one phase, since Title may
	// carry a seat's own words (a unit's name).
	Key string

	// Title is the part's heading text without its leading #'s, inline
	// markdown kept — or, for a part with no heading of its own (the
	// identity line, a worker's mandated rules), a title the builder chose.
	Title string

	// Bytes is the part's length in Text, in bytes of UTF-8.
	Bytes int

	// Headed is true when the part BEGINS WITH ITS OWN HEADING LINE, whose
	// text is Title: a part opened by [Builder.Heading]. False for one opened
	// by [Builder.Lead], whatever its text begins with: a lead part carrying
	// somebody else's markdown (a persona, a task prompt, a trigger's body)
	// often opens with a heading of its own, which a reader must not mistake
	// for the builder's. Only the builder knows which kind it appended.
	Headed bool
}

// Valid reports whether the outline tiles the text — see [Prompt]. A prompt
// with no outline at all is valid: there is simply nothing to slice by.
//
// INVALID UTF-8 FAILS IT, though the byte counts add up: the text travels as
// JSON, whose encoder rewrites each invalid byte to U+FFFD (three bytes for
// one), so every section after an invalid byte in external content would be
// cut in the wrong place.
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
// It produces EXACTLY the text strings.Join(parts, sep) would, because a
// prompt's bytes are its cache key and its behaviour (package doc).
//
// A section is opened by [Builder.Heading] or [Builder.Lead] and runs until
// the next opens; [Builder.Add] continues it. The boundary is the opening
// part's first byte that is not a newline, so a blank line before a heading
// ("\n##") stays with the section before it, as every separator does.
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
// NO PARTS, NO SECTION: a builder's nil for "does not apply" leaves nothing in
// the outline as in the text, and newline-only parts open nothing ([blank]).
func (b *Builder) Heading(key string, parts ...string) {
	if blank(parts) {
		b.parts = append(b.parts, parts...)
		return
	}
	b.open(key, headingTitle(parts[0]), true, parts)
}

// Lead opens a section with no heading of its own, under the title given, and
// appends parts. Blank parts open nothing ([blank]).
func (b *Builder) Lead(key, title string, parts ...string) {
	if blank(parts) {
		b.parts = append(b.parts, parts...)
		return
	}
	b.open(key, title, false, parts)
}

// blank reports whether parts hold nothing but newlines. Such parts are
// appended but open no section: their newlines are separators, which belong to
// the section before them.
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
// The first section always starts at byte 0, so anything appended before it
// belongs to it rather than to no section. A zero-byte section is dropped:
// [blank] already prevents one, so this is the guarantee, not a reached case.
func (b *Builder) Build() Prompt {
	text := strings.Join(b.parts, b.sep)
	if len(b.opens) == 0 {
		return Prompt{Text: text}
	}
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

// Then is p with part, a headed section keyed key, appended after a blank line:
// what a [Builder] joining on "\n\n" would have built with part last. A prompt
// with no outline gains none, and a blank part adds nothing.
func (p Prompt) Then(key, part string) Prompt {
	switch {
	case blank([]string{part}):
		return p
	case p.Text == "":
		return Prompt{Text: part, Sections: []Section{{Key: key, Title: headingTitle(part),
			Bytes: len(part), Headed: true}}}
	}
	const sep = "\n\n"
	out := Prompt{Text: p.Text + sep + part}
	if len(p.Sections) == 0 {
		return out
	}
	out.Sections = append(slices.Clone(p.Sections), Section{Key: key, Title: headingTitle(part),
		Bytes: len(part), Headed: true})
	out.Sections[len(p.Sections)-1].Bytes += len(sep)
	return out
}

// Nested is p told under a heading of another prompt: each heading its builder
// opened a section with goes one level down, and NOT ANOTHER BYTE MOVES
// (internal/agent/runner's leadAgain).
//
// Only the outline says which lines are the builder's headings. The text
// holds somebody else's words too (a trigger's "## Triage", a code block's
// "#include"), and demoting every '#' line would rewrite them inside the fence
// that says who wrote them. So only a [Section.Headed] section's first line
// moves; headings within a section's content keep their level. A prompt whose
// outline is absent or does not tile its text is returned as it is.
func (p Prompt) Nested() Prompt {
	total := 0
	for _, s := range p.Sections {
		if s.Bytes <= 0 {
			return p
		}
		total += s.Bytes
	}
	if len(p.Sections) == 0 || total != len(p.Text) {
		return p
	}
	out := Prompt{Sections: slices.Clone(p.Sections)}
	var text strings.Builder
	text.Grow(len(p.Text) + len(p.Sections))
	at := 0
	for i, s := range p.Sections {
		part := p.Text[at : at+s.Bytes]
		at += s.Bytes
		// The first section starts at byte 0 ([Builder.Build]), so its
		// heading is its first non-blank line.
		if heading := len(part) - len(strings.TrimLeft(part, "\n")); s.Headed &&
			strings.HasPrefix(part[heading:], "#") {
			part = part[:heading] + "#" + part[heading:]
			out.Sections[i].Bytes++
		}
		text.WriteString(part)
	}
	out.Text = text.String()
	return out
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
