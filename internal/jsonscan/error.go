package jsonscan

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Kind is what a token is, as [Scanner.Next] answers it.
type Kind string

const (
	// ObjectStart is an object's opening brace.
	ObjectStart Kind = "object_start"
	// ObjectEnd is an object's closing brace.
	ObjectEnd Kind = "object_end"
	// ArrayStart is an array's opening bracket.
	ArrayStart Kind = "array_start"
	// ArrayEnd is an array's closing bracket.
	ArrayEnd Kind = "array_end"
	// Name is an object member's name, its decoded text read with [Scanner.Read].
	Name Kind = "name"
	// String is a string value, its decoded text read with [Scanner.Read].
	String Kind = "string"
	// Number is a number, its literal as written read with [Scanner.Read].
	Number Kind = "number"
	// True is the literal true.
	True Kind = "true"
	// False is the literal false.
	False Kind = "false"
	// Null is the literal null.
	Null Kind = "null"
)

// Valid reports whether k is one of the ten kinds.
func (k Kind) Valid() bool {
	switch k {
	case ObjectStart, ObjectEnd, ArrayStart, ArrayEnd, Name, String, Number, True, False, Null:
		return true
	}
	return false
}

// beginsValue reports whether a token of kind k is a value's first: a container's start or a scalar.
func (k Kind) beginsValue() bool {
	switch k {
	case ObjectStart, ArrayStart, String, Number, True, False, Null:
		return true
	}
	return false
}

// Fault is why a text is no JSON value ([Error]). THE FIVE STAY APART because a reader maps each onto a
// class of its own: the Claude Code thread layer reads a line of blanks alone as its faultSyntax, a value
// cut at the line's break as faultPrefix, more after the value as faultTrailing, a NUL or a byte order mark
// first as faultLead (a [FaultByte] at offset 0 whose [Error.Rune] is U+0000 or U+FEFF) and a value nested
// too deep as faultDepth, each a different reading of the line by Claude Code's own load.
type Fault string

const (
	// FaultNoValue is a text that ended before any value began: it is empty, or blanks alone.
	FaultNoValue Fault = "no_value"
	// FaultCut is a text that ended inside its value: a line cut at its break.
	FaultCut Fault = "cut"
	// FaultByte is a byte the grammar refuses where it stands, a NUL or a byte order mark before the value
	// among them.
	FaultByte Fault = "byte"
	// FaultTrailing is more than blanks after the value.
	FaultTrailing Fault = "trailing"
	// FaultDepth is a container opened past [MaxDepth].
	FaultDepth Fault = "depth"
)

// Valid reports whether f is one of the five faults.
func (f Fault) Valid() bool {
	switch f {
	case FaultNoValue, FaultCut, FaultByte, FaultTrailing, FaultDepth:
		return true
	}
	return false
}

// Error is a text the scanner refuses: where, and why. Once a scan meets one, every call of its scanner
// answers the same Error.
type Error struct {
	// Offset is where in the text the fault was met, from 0: the refused byte ([FaultByte]), the first byte
	// after the value that is no blank ([FaultTrailing]), the bracket or brace that opened past the bound
	// ([FaultDepth]), or the text's length, where it ended ([FaultNoValue], [FaultCut]).
	Offset int64
	// Fault is why.
	Fault Fault
	// Byte is the refused byte, for [FaultByte] alone.
	Byte byte
	// Rune is the character the refused byte begins, as utf8.DecodeRune reads the text from it (U+FFFD where
	// it begins none), for [FaultByte] alone: what tells a byte order mark, U+FEFF, from another byte 0xEF.
	Rune rune
	// where is what the grammar takes at a refused byte, for the message.
	where string
}

// Error says where the text stops being one JSON value, and why.
func (e *Error) Error() string {
	switch e.Fault {
	case FaultNoValue:
		return fmt.Sprintf("jsonscan: the text ends at byte %d before any value begins: it is empty or blanks alone",
			e.Offset)
	case FaultCut:
		return fmt.Sprintf("jsonscan: the text ends at byte %d inside its value, which is cut short", e.Offset)
	case FaultTrailing:
		return fmt.Sprintf("jsonscan: more than blanks follows the value, from byte %d", e.Offset)
	case FaultDepth:
		return fmt.Sprintf("jsonscan: byte %d opens a container past %d levels deep", e.Offset, MaxDepth)
	}
	char := fmt.Sprintf("%q", e.Rune)
	if e.Rune == utf8.RuneError {
		char = fmt.Sprintf("0x%02x", e.Byte)
	}
	return fmt.Sprintf("jsonscan: byte %d, %s, is refused %s", e.Offset, char, e.where)
}

// ErrNoText is [Scanner.Read]'s answer at a token that has no text: read a Name's, a String's or a Number's.
var ErrNoText = errors.New("jsonscan: the current token has no text to read: Read reads a Name, a String or a Number")

// ErrNoValue is [Scanner.Skip]'s, [Scanner.Capture]'s and [Scanner.Tee]'s answer where the current token
// begins no value they can take: before the first token, or at a container's end; and for Capture and Tee,
// at a Name too, or once a Read has taken part of the value's text or read it to its end. A Read that
// answers io.ErrShortBuffer takes nothing, and Capture and Tee still take the value after it.
var ErrNoValue = errors.New("jsonscan: the current token begins no value to pass over, capture or tee: " +
	"call Skip, Capture or Tee right after the Next that answers the value")

// ErrTeeing is [Scanner.Tee]'s answer while a tee writes the value the current token is in: one writes at a
// time.
var ErrTeeing = errors.New("jsonscan: a tee already writes the value the current token is in: " +
	"tee a value once its tee has ended")

// TeeError is a tee's writer's first error ([Scanner.Tee]), which the call that ends the teed value answers.
// UNLIKE A SCAN'S ERROR IT ENDS NOTHING: every call after it goes on with the text past the value.
type TeeError struct {
	// Offset is where in the text the teed value begins.
	Offset int64
	// Err is what the writer answered.
	Err error
}

// Error says which value's tee failed, and how.
func (e *TeeError) Error() string {
	return fmt.Sprintf("jsonscan: writing the value at byte %d through its tee: %v", e.Offset, e.Err)
}

// Unwrap is the writer's error, so errors.Is and errors.As reach it.
func (e *TeeError) Unwrap() error { return e.Err }
