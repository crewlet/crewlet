package jsonscan

import (
	"encoding/binary"
	"unicode/utf8"
)

// CountUnits turns the unit count on or off from here on ([Scanner.Units], [Scanner.TextUnits]): what it
// counted is kept while it is off. Off after New and after Reset, which zeroes both counts.
func (s *Scanner) CountUnits(on bool) {
	switch {
	case on && !s.counting:
		s.onAt, s.wide = s.offset(), 0
	case !on && s.counting:
		s.units = s.Units()
	}
	s.counting = on
}

// Units is how long, in UTF-16 code units, every byte the scan passed while counting is AS WRITTEN: a
// character past the basic plane two, each byte that begins no UTF-8 character one (utf8.DecodeRune's
// reading), an escape its bytes, and the blanks, commas and colons between tokens among them. Next takes a
// bracket, a brace, a string's or a name's opening quote and a whole literal, nothing of a number; Read,
// Skip and Capture take the rest. A value's length as written is the difference across it.
func (s *Scanner) Units() int64 {
	if !s.counting {
		return s.units
	}
	return s.units + s.offset() - s.onAt - s.wide
}

// TextUnits is how long, in UTF-16 code units, the current Name's or String's DECODED text taken while
// counting is, read or passed over by Skip or Capture: an escape its character's units (a pair two, a lone
// surrogate's one), an invalid byte one, as U+FFFD; whole once its closing quote is taken. Zero at every
// other token: each token Next answers starts it again.
func (s *Scanner) TextUnits() int64 {
	return s.textUnits
}

// countChar counts a character of width bytes, read whole from a text: its units into the text's, and the
// bytes it takes past them.
func (s *Scanner) countChar(width int) {
	units := 1 + int64(width/utf8.UTFMax)
	s.textUnits += units
	s.wide += int64(width) - units
}

// countRun counts run, bytes a string holds as they stand, a character at a time as utf8.DecodeRune reads
// it, and answers how many of them it counted: all, but where open (the run reaches what is read so far and
// more may come), a last character the bytes after it could still complete, held back for the next fill so
// it is never read as the invalid bytes it would be cut. ASCII is counted eight bytes at a time.
func (s *Scanner) countRun(run []byte, open bool) int {
	i, units := 0, int64(0)
	for i < len(run) {
		if len(run)-i >= 8 && binary.LittleEndian.Uint64(run[i:])&highs == 0 {
			i, units = i+8, units+8
			continue
		}
		if run[i] < utf8.RuneSelf {
			i, units = i+1, units+1
			continue
		}
		width := charWidth(run[i:])
		if width == 0 {
			if open {
				break
			}
			width = 1
		}
		i, units = i+width, units+1+int64(width/utf8.UTFMax)
	}
	s.textUnits += units
	s.wide += int64(i) - units
	return i
}

// charWidth is how many bytes the character b begins takes as utf8.DecodeRune reads it, 1 for a byte that
// begins none; 0 where b ends before it says which, as utf8.FullRune reads it. Inlined rather than calling
// DecodeRune, which a text past ASCII would pay a call a character for.
func charWidth(b []byte) int {
	c := b[0]
	var n int
	switch {
	case c < 0xc2 || c >= 0xf5:
		return 1
	case c < 0xe0:
		n = 2
	case c < 0xf0:
		n = 3
	default:
		n = 4
	}
	// The second byte's range: narrower after the leads that would otherwise
	// make an overlong form, a surrogate, or a character past U+10FFFF.
	lo, hi := byte(0x80), byte(0xbf)
	switch c {
	case 0xe0:
		lo = 0xa0
	case 0xed:
		hi = 0x9f
	case 0xf0:
		lo = 0x90
	case 0xf4:
		hi = 0x8f
	}
	for k := 1; k < n; k++ {
		if k == len(b) {
			return 0
		}
		if b[k] < lo || b[k] > hi {
			return 1
		}
		lo, hi = 0x80, 0xbf
	}
	return n
}
