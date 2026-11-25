package jsonscan

import "io"

// NewSized is New with a read buffer of size bytes, at least MinBufferSize: a test scans with a small one to
// move the buffer's edge through every byte of a short text.
func NewSized(r io.Reader, size int) *Scanner {
	return newScanner(r, size)
}

// MinBufferSize is the least buffer a Scanner reads with, which slides its edge most often.
const MinBufferSize = minBufferSize

// StringRun is the word-at-a-time run of a string's bytes as they stand, held to the byte-at-a-time rule.
var StringRun = stringRun

// TextRun is the word-at-a-time run of the bytes decoding copies, held to the byte-at-a-time rule.
var TextRun = textRun
