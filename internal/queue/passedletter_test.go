package queue_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
)

// A PASSED COPY'S HEADER CARRIES ITS REASON WHOLE, AND NOTHING ELSE READS AS
// ONE.
//
// A reader of a group's dead letters handles again only the copies a pass
// retired, found by the header they were kept under ([queue.PassedLetter]).
// So the header has to carry any reason a handler can give as one line the
// transport carries unchanged ([queue.LetterHeader]) and read back exactly;
// and an entry kept with no header — a failure's poison — a wake feed's JSON
// header and a header that only begins like a pass's must never read as one,
// or a replay would handle poison as a delivery somebody passed.
//
// Mutation: write the reason unquoted, and a reason holding a line ending
// makes a header the transport refuses; read a header past its prefix without
// the quote, and `pass newer_record` reads as a pass.
func TestAPassedCopysHeaderCarriesItsReasonWhole(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{
		"newer_record", "", `a "quoted" reason`, "line\nending", "café ☕",
		"del\x7f", "\xff not utf-8", " spaced ",
	} {
		header := queue.PassedLetter(reason)
		if err := queue.LetterHeader(header); err != nil {
			t.Errorf("the header of a copy passed for %q is %q, which no dead letter "+
				"can be kept under: %v", reason, header, err)
		}
		if got, ok := queue.PassedReason(header); !ok || got != reason {
			t.Errorf("the header %q reads back as %q (a pass: %v), want the reason %q whole",
				header, got, ok, reason)
		}
	}
	for _, header := range []string{
		"", `{"v":1,"source":"work","reason":"newer_record"}`, "pass", "pass ",
		"pass newer_record", "pass `newer_record`", "pass 'n'", `pass "unterminated`,
		`pass "newer_record" and more`, `passed "newer_record"`,
	} {
		if reason, ok := queue.PassedReason([]byte(header)); ok {
			t.Errorf("the header %q reads as a pass for %q: a replay would handle "+
				"an entry nobody passed", header, reason)
		}
	}
}

// A DEAD LETTER FITS UP TO THE CEILING LESS ITS FRAMING, AND NOT A BYTE PAST
// IT, because [queue.LetterFits] is the one rule every keep and every passed
// copy's header is held to: a backend that kept one byte more than the broker
// carries would mark a copy the broker then refuses.
//
// Mutation: compare with `<` rather than `<=`, and a letter exactly at the
// ceiling does not fit.
func TestALetterFitsUpToTheCeilingLessItsFraming(t *testing.T) {
	t.Parallel()
	header := queue.PassedLetter("newer_record")
	room := queue.MaxPayloadBytes - queue.LetterFraming - len(header)
	if !queue.LetterFits(header, make([]byte, room)) {
		t.Errorf("a %d-byte copy beside a %d-byte header does not fit, though the two "+
			"and the framing come to the ceiling exactly", room, len(header))
	}
	if queue.LetterFits(header, make([]byte, room+1)) {
		t.Errorf("a %d-byte copy beside a %d-byte header fits, a byte past the ceiling",
			room+1, len(header))
	}
}
