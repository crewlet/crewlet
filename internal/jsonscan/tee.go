package jsonscan

import (
	"errors"
	"io"
)

// errTeeEnded is what the scan's own steps answer where they end a teed value whose writer failed, so the
// call they serve answers that writer's [*TeeError] in its place and goes on ([Scanner.stop]). Never a
// call's answer, it is in no reader's error, so a reader's error ends the scan even where its chain holds a
// TeeError, another scanner's.
var errTeeEnded = errors.New("jsonscan: a teed value whose writer failed has ended")

// Tee writes the current value's bytes as written to w, from its first byte (a string's opening quote
// included) to its last, as the scan passes them: whether the caller reads the value's tokens (Next, Read)
// or passes it (Skip, Capture, or Next over a text left unread). The tee ends with the value, and holds
// nothing: it writes what the read buffer held of the value each time the buffer slides, and the rest where
// the value ends. A WRITER'S ERROR ENDS THE WRITING, NEVER THE SCAN: the tee writes nothing more, the scan
// validates every byte on, and the call that ends the value answers a [*TeeError] for it, the scan standing
// just past the value as after Skip (Next answers no token; Read answers the bytes it read with it). The
// current token must begin a value, none of its text read, else [ErrNoValue]; one tee writes at a time, else
// [ErrTeeing]. At a literal, which Next passed whole, Tee writes it and answers for the writer itself.
func (s *Scanner) Tee(w io.Writer) error {
	if s.err != nil {
		return s.err
	}
	if !s.whole || !s.kind.beginsValue() {
		return ErrNoValue
	}
	if s.tw != nil {
		return ErrTeeing
	}
	// The token's first byte is still in buf, as for Capture ([Scanner.slide]).
	s.tw, s.teeErr, s.teeAt, s.teeFrom, s.teeDepth = w, nil, s.at, int(s.at-s.base), s.depth
	switch s.kind {
	case ObjectStart, ArrayStart:
		s.teeDepth--
	case True, False, Null:
		s.endTee()
		return s.takeTee()
	}
	return nil
}

// teeWrite writes b, the next bytes of the teed value, unless the writer has failed: a write that takes less
// than b and says nothing failed is a short write.
func (s *Scanner) teeWrite(b []byte) {
	if s.teeErr != nil || len(b) == 0 {
		return
	}
	n, err := s.tw.Write(b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	s.teeErr = err
}

// endTee ends the tee at the value's end, at pos, writing what is left of it. A writer's error stays in
// teeErr, with the tee off, for the call that ended the value to answer ([Scanner.takeTee]).
func (s *Scanner) endTee() {
	s.teeWrite(s.buf[s.teeFrom:s.pos])
	s.tw = nil
}

// teeEnded answers errTeeEnded where a teed value has ended and its writer's failure is not yet answered.
func (s *Scanner) teeEnded() error {
	if s.tw != nil || s.teeErr == nil {
		return nil
	}
	return errTeeEnded
}

// takeTee answers, once, the error of the writer of a teed value that has ended; nil while the tee writes,
// or where its writer took every byte.
func (s *Scanner) takeTee() error {
	if s.tw != nil || s.teeErr == nil {
		return nil
	}
	err := &TeeError{Offset: s.teeAt, Err: s.teeErr}
	s.teeErr = nil
	return err
}
