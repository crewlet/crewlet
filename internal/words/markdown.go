package words

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Inert is text Markdown draws as exactly its characters: every ASCII
// punctuation mark is escaped, so nothing in it opens a link, picture,
// heading, list, quote, fence, table or emphasis, or reads as a reference or
// address. For words a person reads on an approval that an agent wrote.
func Inert(text string) string {
	var b strings.Builder
	b.Grow(len(text) + len(text)/4)
	for _, r := range text {
		if r < utf8.RuneSelf && (unicode.IsPunct(r) || unicode.IsSymbol(r)) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// CodeBlock is text in a fenced code block whose fence is longer than any run
// of backticks inside it, so no text can close it early.
func CodeBlock(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "text\n" + strings.TrimSuffix(text, "\n") + "\n" + fence
}
