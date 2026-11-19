package redact

import (
	"io"
	"sync"
)

// prefixWholeBytes is the longest text [Prefix] redacts whole, as [Secrets]
// does: 32 MiB, the most the engine reads of one text whole anywhere (a box's
// file, one line of a run's stream: internal/sandbox's MaxFileBytes), and where
// a stream stops costing more. Over log-shaped text from 256 KiB to 32 MiB,
// Secrets took a quarter to a tenth of a stream's time, a fiftieth of its
// allocation and no more peak heap; at 32 MiB the two peaks met, about 110
// MiB, and past it only the stream's stays bounded.
const prefixWholeBytes = 32 << 20

// Prefix is the first n bytes of what [Secrets] returns for text, and how long
// all of it is: an opening of the whole redaction, read in bounded memory past
// [prefixWholeBytes] (the package doc's "A text that arrives in pieces"). An n
// below 0 is 0. The opening may end inside a rune; a caller that shows it cuts
// it at a rune's edge.
func Prefix(text string, n int) (opening string, whole int) {
	return prefix(text, n, prefixWholeBytes, holdBytes())
}

// LongestMarker is how long the longest marker the redaction writes is: a
// rule's ([ruleMarkers]) or a key's block's ([keyMarker]). A reader that cuts a
// redacted text reads this far past its bound to see whether a marker crosses
// it.
func LongestMarker() int { return longestWrittenMarker() }

var longestWrittenMarker = sync.OnceValue(func() int {
	n := len(keyMarker)
	for _, m := range ruleMarkers() {
		n = max(n, len(m))
	}
	return n
})

// prefix is [Prefix] with the length up to which a text is redacted whole and
// the stream's hold as arguments, so a test can stream a short text at a hold
// that refuses it. A text the stream refuses ([ErrDecidedEarly], the only error
// a stream into an [opening] answers) is redacted whole: the opening is then
// exact at Secrets' cost, and nothing the stream wrote is read.
func prefix(text string, n, readWhole, hold int) (string, int) {
	n = max(n, 0)
	if len(text) > readWhole {
		o := &opening{keep: n}
		s := newStreamHolding(o, hold)
		err := writePieces(s, text)
		if err == nil {
			err = s.Close()
		}
		if err == nil {
			return string(o.kept), o.whole
		}
	}
	whole := Secrets(text)
	return whole[:min(n, len(whole))], len(whole)
}

// writePieces writes text to w in pieces of at most [pieceBytes], each copied
// into one buffer, so a text held as a string is never copied whole.
func writePieces(w io.Writer, text string) error {
	buf := make([]byte, 0, min(len(text), pieceBytes))
	for len(text) > 0 {
		k := min(len(text), pieceBytes)
		buf = append(buf[:0], text[:k]...)
		if _, err := w.Write(buf); err != nil {
			return err
		}
		text = text[k:]
	}
	return nil
}

// opening keeps the first keep bytes written to it and counts them all. It
// never fails, so a stream into it answers only its own refusal.
type opening struct {
	keep, whole int
	kept        []byte
}

func (o *opening) Write(p []byte) (int, error) {
	if room := o.keep - len(o.kept); room > 0 {
		o.kept = append(o.kept, p[:min(room, len(p))]...)
	}
	o.whole += len(p)
	return len(p), nil
}
