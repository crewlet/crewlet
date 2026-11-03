package code

import (
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Sentence is a refusal of this domain worded for whoever reads it: a
// person's write surface, a seat's tool, the assistant. The state log's own
// refusals, which the code log's writer returns as the framework wrote them,
// are said in the framework's words for a person ([statelog.Sentence]) — here
// as well as by every surface that asks the state log first
// (internal/agent/builtin's WriterSentence), so a caller that asks only this
// one cannot print the framework's words after the mark.
//
// THE PREFIX IS FOR A LOG. Every error this package writes opens with "code:"
// and states its fact after it, often with a sentence of the caller's own
// after that ([ErrNotPermitted]'s "…: <who reads it>"); that sentence is the
// one a reader is told where there is one, and otherwise the fact without the
// package. An error a caller wrapped in context of its own is its whole
// message.
func Sentence(err error) string {
	if said, ok := statelog.Sentence(err); ok {
		return said
	}
	return strings.TrimPrefix(err.Error(), "code: ")
}
